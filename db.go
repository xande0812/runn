package runn

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"unsafe"

	"github.com/araddon/dateparse"
	"github.com/golang-sql/sqlexp"
	"github.com/golang-sql/sqlexp/nest"
	_ "github.com/googleapis/go-sql-spanner"
	"github.com/xo/dburl"
	"modernc.org/sqlite"
)

const (
	dbStoreLastInsertIDKey = "last_insert_id"
	dbStoreRowsAffectedKey = "rows_affected"
	dbStoreRowsKey         = "rows"
)

type Querier interface {
	sqlexp.Querier
}

type TxQuerier interface {
	nest.Querier
	BeginTx(ctx context.Context, opts *nest.TxOptions) (*nest.Tx, error)
}

type dbRunner struct {
	name     string
	client   TxQuerier
	operator *operator
}

type dbQuery struct {
	stmt string
}

type DBResponse struct {
	LastInsertID int64
	RowsAffected int64
	Columns      []string
	Rows         []map[string]interface{}
}

func newDBRunner(name, dsn string) (*dbRunner, error) {
	var db *sql.DB
	var err error
	if strings.HasPrefix(dsn, "sp://") || strings.HasPrefix(dsn, "spanner://") {
		d := strings.Split(strings.Split(dsn, "://")[1], "/")
		db, err = sql.Open("spanner", fmt.Sprintf(`projects/%s/instances/%s/databases/%s`, d[0], d[1], d[2]))
	} else {
		db, err = dburl.Open(normalizeDSN(dsn))
	}
	if err != nil {
		return nil, err
	}
	nx, err := nestTx(db)
	if err != nil {
		return nil, err
	}
	return &dbRunner{
		name:   name,
		client: nx,
	}, nil
}

var dsnRep = strings.NewReplacer("sqlite://", "moderncsqlite://", "sqlite3://", "moderncsqlite://", "sq://", "moderncsqlite://")

func normalizeDSN(dsn string) string {
	if !contains(sql.Drivers(), "sqlite3") { // sqlite3 => github.com/mattn/go-sqlite3
		return dsnRep.Replace(dsn)
	}
	return dsn
}

func (rnr *dbRunner) Run(ctx context.Context, q *dbQuery) error {
	stmts := separateStmt(q.stmt)
	out := map[string]interface{}{}
	tx, err := rnr.client.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	for _, stmt := range stmts {
		rnr.operator.capturers.captureDBStatement(rnr.name, stmt)
		err := func() error {
			if !strings.HasPrefix(strings.ToUpper(stmt), "SELECT") {
				// exec
				r, err := tx.ExecContext(ctx, stmt)
				if err != nil {
					return err
				}
				id, _ := r.LastInsertId()
				a, _ := r.RowsAffected()
				out = map[string]interface{}{
					string(dbStoreLastInsertIDKey): id,
					string(dbStoreRowsAffectedKey): a,
				}

				rnr.operator.capturers.captureDBResponse(rnr.name, &DBResponse{
					LastInsertID: id,
					RowsAffected: a,
				})

				return nil
			}

			// query
			rows := []map[string]interface{}{}
			r, err := tx.QueryContext(ctx, stmt)
			if err != nil {
				return err
			}
			defer r.Close()

			columns, err := r.Columns()
			if err != nil {
				return err
			}
			types, err := r.ColumnTypes()
			if err != nil {
				return err
			}
			for r.Next() {
				row := map[string]interface{}{}
				vals := make([]interface{}, len(columns))
				valsp := make([]interface{}, len(columns))
				for i := range columns {
					valsp[i] = &vals[i]
				}
				if err := r.Scan(valsp...); err != nil {
					return err
				}
				for i, c := range columns {
					switch v := vals[i].(type) {
					case []byte:
						s := string(v)
						t := strings.ToUpper(types[i].DatabaseTypeName())
						switch {
						case strings.Contains(t, "TEXT") || strings.Contains(t, "CHAR") || t == "TIME": // MySQL8: ENUM = CHAR
							row[c] = s
						case t == "DECIMAL" || t == "FLOAT" || t == "DOUBLE": // MySQL: NUMERIC = DECIMAL
							num, err := strconv.ParseFloat(s, 64)
							if err != nil {
								return fmt.Errorf("invalid column: evaluated %s, but got %s(%v): %w", c, t, s, err)
							}
							row[c] = num
						case t == "DATE" || t == "TIMESTAMP" || t == "DATETIME": // MySQL(SSH port fowarding)
							d, err := dateparse.ParseStrict(s)
							if err != nil {
								return fmt.Errorf("invalid column: evaluated %s, but got %s(%v): %w", c, t, s, err)
							}
							row[c] = d
						case strings.Contains(t, "JSONB"): // PostgreSQL JSONB
							var jsonColumn map[string]interface{}
							err = json.Unmarshal(v, &jsonColumn)
							if err != nil {
								return fmt.Errorf("invalid column: evaluated %s, but got %s(%v): %w", c, t, s, err)
							}
							row[c] = jsonColumn
						default: // MySQL: BOOLEAN = TINYINT
							num, err := strconv.Atoi(s)
							if err != nil {
								return fmt.Errorf("invalid column: evaluated %s, but got %s(%v): %w", c, t, s, err)
							}
							row[c] = num
						}
					default:
						// MySQL8: DATE, TIMESTAMP, DATETIME
						row[c] = v
					}
				}
				rows = append(rows, row)
			}
			if err := r.Err(); err != nil {
				return err
			}

			rnr.operator.capturers.captureDBResponse(rnr.name, &DBResponse{
				Columns: columns,
				Rows:    rows,
			})

			out = map[string]interface{}{
				string(dbStoreRowsKey): rows,
			}
			return nil
		}()
		if err != nil {
			if err := tx.Rollback(); err != nil {
				return err
			}
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	rnr.operator.record(out)
	return nil
}

func nestTx(client Querier) (TxQuerier, error) {
	switch c := client.(type) {
	case *sql.DB:
		return nest.Wrap(c), nil
	case *sql.Tx:
		if c == nil {
			return nil, fmt.Errorf("invalid db client: %v", c)
		}
		var v reflect.Value = reflect.ValueOf(c).Elem()
		var psv reflect.Value = v.FieldByName("db").Elem()
		db := (*sql.DB)(unsafe.Pointer(psv.UnsafeAddr()))
		return nest.Wrap(db), nil
	default:
		return nil, fmt.Errorf("invalid db client: %v", c)
	}
}

func separateStmt(stmt string) []string {
	if !strings.Contains(stmt, ";") {
		return []string{stmt}
	}
	stmts := []string{}
	s := []rune{}
	ins := false
	ind := false
	for _, c := range stmt {
		s = append(s, c)
		switch c {
		case '\'':
			ins = !ins
		case '"':
			ind = !ind
		case ';':
			if !ins && !ind {
				stmts = append(stmts, strings.Trim(string(s), " \n"))
				s = []rune{}
			}
		}
	}
	if len(s) > 0 {
		cutset := " \n\\n\"" // When I receive a multi-line query with `key: |`, I get an unexplained string at the end. Therefore, remove it as a workaround.
		l := strings.TrimRight(string(s), cutset)
		if len(l) > 0 {
			stmts = append(stmts, l)
		}
	}
	return stmts
}

func init() {
	if !contains(sql.Drivers(), "moderncsqlite") {
		sql.Register("moderncsqlite", &sqlite.Driver{})
	}
}
