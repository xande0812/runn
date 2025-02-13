package runn

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/fatih/color"
)

type result string

const (
	resultSuccess result = "success"
	resultFailure result = "failure"
	resultSkipped result = "skipped"
)

type RunResult struct {
	Desc        string
	Path        string
	Skipped     bool
	Err         error
	StepResults []*StepResult
	Store       map[string]interface{}
}

type StepResult struct {
	Key     string
	Desc    string
	Skipped bool
	Err     error
}

type runNResult struct {
	Total      atomic.Int64
	RunResults []*RunResult
	mu         sync.Mutex
}

type runNResultSimplified struct {
	Total   int64                 `json:"total"`
	Success int64                 `json:"success"`
	Failure int64                 `json:"failure"`
	Skipped int64                 `json:"skipped"`
	Results []runResultSimplified `json:"results"`
}

type runResultSimplified struct {
	Path   string                 `json:"path"`
	Result result                 `json:"result"`
	Steps  []stepResultSimplified `json:"steps"`
}

type stepResultSimplified struct {
	Key    string `json:"key"`
	Result result `json:"result"`
}

func newRunResult(desc, path string) *RunResult {
	return &RunResult{
		Desc: desc,
		Path: path,
	}
}

func (r *runNResult) HasFailure() bool {
	for _, rr := range r.RunResults {
		if rr.Err != nil {
			return true
		}
	}
	return false
}

func (r *runNResult) Simplify() runNResultSimplified {
	s := runNResultSimplified{
		Total: r.Total.Load(),
	}
	for _, rr := range r.RunResults {
		switch {
		case rr.Err != nil:
			s.Failure += 1
			s.Results = append(s.Results, runResultSimplified{
				Path:   rr.Path,
				Result: resultFailure,
				Steps:  simplifyStepResults(rr.StepResults),
			})
		case rr.Skipped:
			s.Skipped += 1
			s.Results = append(s.Results, runResultSimplified{
				Path:   rr.Path,
				Result: resultSkipped,
				Steps:  simplifyStepResults(rr.StepResults),
			})
		default:
			s.Success += 1
			s.Results = append(s.Results, runResultSimplified{
				Path:   rr.Path,
				Result: resultSuccess,
				Steps:  simplifyStepResults(rr.StepResults),
			})
		}
	}
	return s
}

func (r *runNResult) Out(out io.Writer, verbose bool) error {
	var ts, fs string
	green := color.New(color.FgGreen).SprintFunc()
	red := color.New(color.FgRed).SprintFunc()

	_, _ = fmt.Fprintln(out, "")
	if !verbose && r.HasFailure() {
		_, _ = fmt.Fprintln(out, "")
		i := 1
		for _, r := range r.RunResults {
			if r.Err == nil {
				continue
			}
			_, _ = fmt.Fprintf(out, "%d) %s\n", i, ShortenPath(r.Path))
			for _, sr := range r.StepResults {
				if sr.Err == nil {
					continue
				}
				_, _ = fmt.Fprintf(out, SprintMultilinef("  %s\n", "%v", red(fmt.Sprintf("Failure/Error: %s", strings.TrimRight(sr.Err.Error(), "\n")))))
			}
			i++
		}
	}
	_, _ = fmt.Fprintln(out, "")

	rs := r.Simplify()
	if rs.Total == 1 {
		ts = fmt.Sprintf("%d scenario", rs.Total)
	} else {
		ts = fmt.Sprintf("%d scenarios", rs.Total)
	}
	ss := fmt.Sprintf("%d skipped", rs.Skipped)
	if rs.Failure == 1 {
		fs = fmt.Sprintf("%d failure", rs.Failure)
	} else {
		fs = fmt.Sprintf("%d failures", rs.Failure)
	}
	if r.HasFailure() {
		if _, err := fmt.Fprintf(out, red("%s, %s, %s\n"), ts, ss, fs); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(out, green("%s, %s, %s\n"), ts, ss, fs); err != nil {
			return err
		}
	}
	return nil
}

func (r *runNResult) OutJSON(out io.Writer) error {
	s := r.Simplify()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if _, err := out.Write(b); err != nil {
		return err
	}
	if _, err := fmt.Fprint(out, "\n"); err != nil {
		return err
	}
	return nil
}

func simplifyStepResults(stepResults []*StepResult) []stepResultSimplified {
	simplified := []stepResultSimplified{}
	for _, sr := range stepResults {
		switch {
		case sr.Err != nil:
			simplified = append(simplified, stepResultSimplified{
				Key:    sr.Key,
				Result: resultFailure,
			})
		case sr.Skipped:
			simplified = append(simplified, stepResultSimplified{
				Key:    sr.Key,
				Result: resultSkipped,
			})
		default:
			simplified = append(simplified, stepResultSimplified{
				Key:    sr.Key,
				Result: resultSuccess,
			})
		}
	}
	return simplified
}

func SprintMultilinef(lineformat, format string, a ...any) string {
	lines := strings.Split(fmt.Sprintf(format, a...), "\n")
	var formatted string
	for _, l := range lines {
		formatted += fmt.Sprintf(lineformat, l)
	}
	return formatted
}
