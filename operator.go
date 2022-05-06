package runn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/antonmedv/expr"
	"github.com/fatih/color"
	"github.com/goccy/go-yaml"
	"github.com/hashicorp/go-multierror"
	"github.com/k1LoW/expand"
)

var (
	cyan     = color.New(color.FgCyan).SprintFunc()
	yellow   = color.New(color.FgYellow).SprintFunc()
	expandRe = regexp.MustCompile(`"?{{\s*([^}]+)\s*}}"?`)
	numberRe = regexp.MustCompile(`^[+-]?\d+(?:\.\d+)?$`)
)

type step struct {
	key           string
	runnerKey     string
	httpRunner    *httpRunner
	httpRequest   map[string]interface{}
	dbRunner      *dbRunner
	dbQuery       map[string]interface{}
	execRunner    *execRunner
	execCommand   map[string]interface{}
	testRunner    *testRunner
	testCond      string
	dumpRunner    *dumpRunner
	dumpCond      string
	bindRunner    *bindRunner
	bindCond      map[string]string
	includeRunner *includeRunner
	includePath   string
	debug         bool
}

const (
	storeVarsKey  = "vars"
	storeStepsKey = "steps"
)

type store struct {
	steps    []map[string]interface{}
	stepMaps map[string]interface{}
	vars     map[string]interface{}
	bindVars map[string]interface{}
	useMaps  bool
}

func (s *store) toMap() map[string]interface{} {
	store := map[string]interface{}{}
	store[storeVarsKey] = s.vars
	if s.useMaps {
		store[storeStepsKey] = s.stepMaps
	} else {
		store[storeStepsKey] = s.steps
	}
	for k, v := range s.bindVars {
		store[k] = v
	}
	return store
}

type operator struct {
	httpRunners map[string]*httpRunner
	dbRunners   map[string]*dbRunner
	steps       []*step
	store       store
	desc        string
	useMaps     bool
	debug       bool
	interval    time.Duration
	root        string
	t           *testing.T
	failFast    bool
	included    bool
	cond        string
	skipped     bool
	out         io.Writer
}

func (o *operator) record(v map[string]interface{}) {
	if o.useMaps && len(o.steps) > 0 {
		o.store.stepMaps[o.steps[len(o.store.stepMaps)].key] = v
		return
	}
	o.store.steps = append(o.store.steps, v)
}

func New(opts ...Option) (*operator, error) {
	bk := newBook()
	for _, opt := range opts {
		if err := opt(bk); err != nil {
			return nil, err
		}
	}

	useMaps := false
	if len(bk.stepKeys) == len(bk.Steps) {
		useMaps = true
	}

	o := &operator{
		httpRunners: map[string]*httpRunner{},
		dbRunners:   map[string]*dbRunner{},
		store: store{
			steps:    []map[string]interface{}{},
			stepMaps: map[string]interface{}{},
			vars:     bk.Vars,
			bindVars: map[string]interface{}{},
			useMaps:  useMaps,
		},
		useMaps:  useMaps,
		desc:     bk.Desc,
		debug:    bk.Debug,
		interval: bk.interval,
		t:        bk.t,
		failFast: bk.failFast,
		included: bk.included,
		cond:     bk.If,
		out:      os.Stderr,
	}

	if bk.path != "" {
		o.root = filepath.Dir(bk.path)
	} else {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		o.root = wd
	}

	for k, v := range bk.Runners {
		if k == includeRunnerKey || k == testRunnerKey || k == dumpRunnerKey || k == execRunnerKey || k == bindRunnerKey || k == ifSectionKey || k == descSectionKey {
			return nil, fmt.Errorf("runner name '%s' is reserved for built-in runner", k)
		}
		delete(bk.runnerErrs, k)

		switch vv := v.(type) {
		case string:
			switch {
			case strings.Index(vv, "https://") == 0 || strings.Index(vv, "http://") == 0:
				hc, err := newHTTPRunner(k, vv, o)
				if err != nil {
					bk.runnerErrs[k] = err
					continue
				}
				o.httpRunners[k] = hc
			default:
				dc, err := newDBRunner(k, vv, o)
				if err != nil {
					bk.runnerErrs[k] = err
					continue
				}
				o.dbRunners[k] = dc
			}
		case map[string]interface{}:
			tmp, err := yaml.Marshal(vv)
			if err != nil {
				bk.runnerErrs[k] = err
				continue
			}
			c := &RunnerConfig{}
			if err := yaml.Unmarshal(tmp, c); err != nil {
				bk.runnerErrs[k] = err
				continue
			}

			if c.OpenApi3DocLocation != "" && !strings.HasPrefix(c.OpenApi3DocLocation, "https://") && !strings.HasPrefix(c.OpenApi3DocLocation, "http://") && !strings.HasPrefix(c.OpenApi3DocLocation, "/") {
				c.OpenApi3DocLocation = filepath.Join(o.root, c.OpenApi3DocLocation)
			}

			if c.Endpoint != "" {
				// httpRunner
				hc, err := newHTTPRunner(k, c.Endpoint, o)
				if err != nil {
					bk.runnerErrs[k] = err
					continue
				}
				hv, err := NewHttpValidator(c)
				if err != nil {
					bk.runnerErrs[k] = err
					continue
				}
				hc.validator = hv
				o.httpRunners[k] = hc
			}
		}
	}
	for k, v := range bk.httpRunners {
		delete(bk.runnerErrs, k)
		v.operator = o
		o.httpRunners[k] = v
	}
	for k, v := range bk.dbRunners {
		delete(bk.runnerErrs, k)
		v.operator = o
		o.dbRunners[k] = v
	}

	keys := map[string]struct{}{}
	for k := range o.httpRunners {
		keys[k] = struct{}{}
	}
	for k := range o.dbRunners {
		if _, ok := keys[k]; ok {
			return nil, fmt.Errorf("duplicate runner names: %s", k)
		}
		keys[k] = struct{}{}
	}

	var merr error
	for k, err := range bk.runnerErrs {
		merr = multierror.Append(merr, fmt.Errorf("runner %s error: %w", k, err))
	}
	if merr != nil {
		return nil, merr
	}

	for i, s := range bk.Steps {
		if err := validateStepKeys(s); err != nil {
			return nil, fmt.Errorf("invalid steps[%d]. %w: %s", i, err, s)
		}
		key := ""
		if len(bk.stepKeys) == len(bk.Steps) {
			key = bk.stepKeys[i]
		}
		if err := o.AppendStep(key, s); err != nil {
			return nil, err
		}
	}

	return o, nil
}

func validateStepKeys(s map[string]interface{}) error {
	if len(s) == 0 {
		return errors.New("step must specify at least one runner")
	}
	custom := 0
	for k := range s {
		if k == testRunnerKey || k == dumpRunnerKey || k == bindRunnerKey {
			continue
		}
		custom += 1
	}
	if custom > 1 {
		return errors.New("runners that cannot be running at the same time are specified")
	}
	return nil
}

func (o *operator) AppendStep(key string, s map[string]interface{}) error {
	if o.t != nil {
		o.t.Helper()
	}
	step := &step{key: key, debug: o.debug}
	// test runner
	if v, ok := s[testRunnerKey]; ok {
		tr, err := newTestRunner(o)
		if err != nil {
			return err
		}
		step.testRunner = tr
		vv, ok := v.(string)
		if !ok {
			return fmt.Errorf("invalid test condition: %v", v)
		}
		step.testCond = vv
		delete(s, testRunnerKey)
	}
	// dump runner
	if v, ok := s[dumpRunnerKey]; ok {
		dr, err := newDumpRunner(o)
		if err != nil {
			return err
		}
		step.dumpRunner = dr
		vv, ok := v.(string)
		if !ok {
			return fmt.Errorf("invalid dump condition: %v", v)
		}
		step.dumpCond = vv
		delete(s, dumpRunnerKey)
	}
	// bind runner
	if v, ok := s[dumpRunnerKey]; ok {
		br, err := newBindRunner(o)
		if err != nil {
			return err
		}
		step.bindRunner = br
		vv, ok := v.(map[string]interface{})
		if !ok {
			return fmt.Errorf("invalid bind condition: %v", v)
		}
		cond := map[string]string{}
		for k, vvv := range vv {
			s, ok := vvv.(string)
			if !ok {
				return fmt.Errorf("invalid bind condition: %v", v)
			}
			cond[k] = s
		}
		step.bindCond = cond
		delete(s, bindRunnerKey)
	}

	k, v, ok := pop(s)
	if ok {
		step.runnerKey = k
		switch {
		case k == includeRunnerKey:
			ir, err := newIncludeRunner(o)
			if err != nil {
				return err
			}
			step.includeRunner = ir
			vv, ok := v.(string)
			if !ok {
				return fmt.Errorf("invalid include path: %v", v)
			}
			step.includePath = vv
		case k == execRunnerKey:
			er, err := newExecRunner(o)
			if err != nil {
				return err
			}
			step.execRunner = er
			vv, ok := v.(map[string]interface{})
			if !ok {
				return fmt.Errorf("invalid exec command: %v", v)
			}
			step.execCommand = vv
		default:
			h, ok := o.httpRunners[k]
			if ok {
				step.httpRunner = h
				vv, ok := v.(map[string]interface{})
				if !ok {
					return fmt.Errorf("invalid http request: %v", v)
				}
				step.httpRequest = vv
			} else {
				db, ok := o.dbRunners[k]
				if ok {
					step.dbRunner = db
					vv, ok := v.(map[string]interface{})
					if !ok {
						return fmt.Errorf("invalid db query: %v", v)
					}
					step.dbQuery = vv
				} else {
					return fmt.Errorf("can not find client: %s", k)
				}
			}
		}
	}
	o.steps = append(o.steps, step)
	return nil
}

func (o *operator) Run(ctx context.Context) error {
	if o.t != nil {
		o.t.Helper()
		var err error
		o.t.Run(o.desc, func(t *testing.T) {
			t.Helper()
			err = o.run(ctx)
			if err != nil {
				t.Error(err)
			}
		})
		return err
	}
	return o.run(ctx)
}

func (o *operator) run(ctx context.Context) error {
	if o.cond != "" {
		store := o.store.toMap()
		store["included"] = o.included
		tf, err := expr.Eval(fmt.Sprintf("(%s) == true", o.cond), store)
		if err != nil {
			return err
		}
		if !tf.(bool) {
			o.Debugf(yellow("Skip %s\n"), o.desc)
			o.skipped = true
			return nil
		}
	}

	for i, s := range o.steps {
		if i != 0 {
			time.Sleep(o.interval)
		}
		if i != 0 {
			o.Debugln("")
		}
		if s.runnerKey != "" {
			o.Debugf(cyan("Run '%s' on %s\n"), s.runnerKey, o.stepName(i))
		}
		switch {
		case s.httpRunner != nil && s.httpRequest != nil:
			e, err := o.expand(s.httpRequest)
			if err != nil {
				return err
			}
			r, ok := e.(map[string]interface{})
			if !ok {
				return fmt.Errorf("invalid %s: %v", o.stepName(i), e)
			}
			req, err := parseHTTPRequest(r)
			if err != nil {
				return err
			}
			if err := s.httpRunner.Run(ctx, req); err != nil {
				return fmt.Errorf("http request failed on %s: %v", o.stepName(i), err)
			}
		case s.dbRunner != nil && s.dbQuery != nil:
			e, err := o.expand(s.dbQuery)
			if err != nil {
				return err
			}
			q, ok := e.(map[string]interface{})
			if !ok {
				return fmt.Errorf("invalid %s: %v", o.stepName(i), e)
			}
			query, err := parseDBQuery(q)
			if err != nil {
				return fmt.Errorf("invalid %s: %v", o.stepName(i), q)
			}
			if err := s.dbRunner.Run(ctx, query); err != nil {
				return fmt.Errorf("db query failed on %s: %v", o.stepName(i), err)
			}
		case s.execRunner != nil && s.execCommand != nil:
			e, err := o.expand(s.execCommand)
			if err != nil {
				return err
			}
			cmd, ok := e.(map[string]interface{})
			if !ok {
				return fmt.Errorf("invalid %s: %v", o.stepName(i), e)
			}
			command, err := parseExecCommand(cmd)
			if err != nil {
				return fmt.Errorf("invalid %s: %v", o.stepName(i), cmd)
			}
			if err := s.execRunner.Run(ctx, command); err != nil {
				return fmt.Errorf("exec command failed on %s: %v", o.stepName(i), err)
			}
		case s.includeRunner != nil && s.includePath != "":
			if err := s.includeRunner.Run(ctx, s.includePath); err != nil {
				return fmt.Errorf("include failed on %s: %v", o.stepName(i), err)
			}
		}
		// test runner
		if s.testRunner != nil && s.testCond != "" {
			o.Debugf(cyan("Run '%s' on %s\n"), testRunnerKey, o.stepName(i))
			if err := s.testRunner.Run(ctx, s.testCond); err != nil {
				return fmt.Errorf("test failed on %s: %v", o.stepName(i), err)
			}
			if len(o.store.steps) < i+1 {
				o.record(nil)
			}
		}
		// dump runner
		if s.dumpRunner != nil && s.dumpCond != "" {
			o.Debugf(cyan("Run '%s' on %s\n"), dumpRunnerKey, o.stepName(i))
			if err := s.dumpRunner.Run(ctx, s.dumpCond); err != nil {
				return fmt.Errorf("dump failed on %s: %v", o.stepName(i), err)
			}
			if len(o.store.steps) < i+1 {
				o.record(nil)
			}
		}
		// bind runner
		if s.bindRunner != nil && s.bindCond != nil {
			o.Debugf(cyan("Run '%s' on %s\n"), bindRunnerKey, o.stepName(i))
			if err := s.bindRunner.Run(ctx, s.bindCond); err != nil {
				return fmt.Errorf("bind failed on %s: %v", o.stepName(i), err)
			}
			if len(o.store.steps) < i+1 {
				o.record(nil)
			}
		}
	}
	return nil
}

func (o *operator) stepName(i int) string {
	if o.useMaps {
		return fmt.Sprintf("'%s'.steps.%s", o.desc, o.steps[i].key)
	}
	return fmt.Sprintf("'%s'.steps[%d]", o.desc, i)
}

func (o *operator) expand(in interface{}) (interface{}, error) {
	store := o.store.toMap()
	store["string"] = func(in interface{}) string { return fmt.Sprintf("%v", in) }
	b, err := yaml.Marshal(in)
	if err != nil {
		return nil, err
	}
	var reperr error
	replacefunc := func(in string) string {
		if !strings.Contains(in, "{{") {
			return in
		}
		matches := expandRe.FindAllStringSubmatch(in, -1)
		oldnew := []string{}
		for _, m := range matches {
			o, err := expr.Eval(m[1], store)
			if err != nil {
				reperr = err
			}
			var s string
			switch v := o.(type) {
			case string:
				if numberRe.MatchString(v) {
					s = fmt.Sprintf("'%s'", v)
				} else {
					s = v
				}
			case int64:
				s = strconv.Itoa(int(v))
			case int:
				s = strconv.Itoa(v)
			default:
				reperr = fmt.Errorf("invalid format: %v\n%s", o, string(b))
			}
			oldnew = append(oldnew, m[0], s)
		}
		rep := strings.NewReplacer(oldnew...)
		return rep.Replace(in)
	}
	e := expand.ReplaceYAML(string(b), replacefunc, true)
	if reperr != nil {
		return nil, reperr
	}
	var out interface{}
	if err := yaml.Unmarshal([]byte(e), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (o *operator) Debugln(a string) {
	if !o.debug {
		return
	}
	_, _ = fmt.Fprintln(o.out, a)
}

func (o *operator) Debugf(format string, a ...interface{}) {
	if !o.debug {
		return
	}
	_, _ = fmt.Fprintf(o.out, format, a...)
}

func (o *operator) Skipped() bool {
	return o.skipped
}

type operators struct {
	ops []*operator
	t   *testing.T
}

func Load(pathp string, opts ...Option) (*operators, error) {
	ops := &operators{}
	books, err := Books(pathp)
	if err != nil {
		return nil, err
	}
	for _, b := range books {
		o, err := New(append(opts, b)...)
		if err != nil {
			return nil, err
		}
		if o.t != nil {
			ops.t = o.t
		}
		ops.ops = append(ops.ops, o)
	}
	return ops, nil
}

func (ops *operators) RunN(ctx context.Context) error {
	if ops.t != nil {
		ops.t.Helper()
	}
	for _, o := range ops.ops {
		if err := o.Run(ctx); err != nil && o.failFast {
			return err
		}
	}
	return nil
}

func pop(s map[string]interface{}) (string, interface{}, bool) {
	for k, v := range s {
		delete(s, k)
		return k, v, true
	}
	return "", nil, false
}
