// Package parse runs a harness entry's parse script: starlark that pats runs
// once for each line of the harness's stdout, and that says what the line
// means by calling out (docs/spec/harness-parse.md).
package parse

import (
	"errors"
	"fmt"
	"math"
	"strings"

	starjson "go.starlark.net/lib/json"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"
)

// maxSteps bounds one run of a script, which is one line. it bounds steps,
// not memory or time: a builtin that builds a huge value is one step.
const maxSteps = 1_000_000

// the names a script is handed. anything else it names must be its own.
var predeclared = map[string]bool{"line": true, "out": true, "json": true, "re": true}

// a script is its body: control flow at the top level, and a name set twice.
var fileOptions = &syntax.FileOptions{TopLevelControl: true, GlobalReassign: true}

// Program is a compiled parse script, safe for concurrent passes.
type Program struct {
	prog *starlark.Program
}

// Compile checks a script: its syntax, and that every name it uses is its own
// or one it is handed. name is what an error calls the script.
func Compile(name, src string) (*Program, error) {
	_, prog, err := starlark.SourceProgramOptions(fileOptions, name, src, func(n string) bool { return predeclared[n] })
	if err != nil {
		return nil, err
	}
	if prog.NumLoads() > 0 {
		return nil, fmt.Errorf("%s: a parse script cannot load another", name)
	}
	return &Program{prog: prog}, nil
}

// Result is what a pass has heard so far.
type Result struct {
	Tools     map[string]int // tool calls by name
	ToolCalls int

	Turns, Input, Output, Reasoning, CacheRead, CacheWrite int
	PremiumRequests, TTFTms, APIms                         int
	CostUSD                                                float64
	HasCost                                                bool // cost_usd was passed at all, 0 included
	IsError                                                bool
	ErrorMsg                                               string // what the harness said of its error, "" when it said nothing

	// Said is whether a call that feeds the summary was made: without one,
	// and with no failed line, the output was empty or not this format's.
	Said bool

	// Errors counts the lines whose run failed. FirstError is the first of
	// them, on the 1-based line FirstErrorLine.
	Errors         int
	FirstError     string
	FirstErrorLine int

	finals    []string
	texts     []string
	clearNext bool // the next piece of text starts the reply afresh
}

// Replies is the reply a judge is read from, newest first: the final
// candidates when there are any, else the pieces of text joined by newlines.
func (r *Result) Replies() []string {
	if len(r.finals) > 0 {
		out := make([]string, len(r.finals))
		for i, f := range r.finals {
			out[len(r.finals)-1-i] = f
		}
		return out
	}
	if len(r.texts) > 0 {
		return []string{strings.Join(r.texts, "\n")}
	}
	return nil
}

// Pass is one reading of a harness's output from its first line, by one
// goroutine.
type Pass struct {
	prog  *starlark.Program
	res   Result
	out   starlark.Value
	lines int
}

// NewPass starts a reading with fresh totals.
func (p *Program) NewPass() *Pass {
	ps := &Pass{prog: p.prog, res: Result{Tools: map[string]int{}}}
	ps.out = starlarkstruct.FromStringDict(starlark.String("out"), starlark.StringDict{
		"tool":  starlark.NewBuiltin("out.tool", ps.outTool),
		"usage": starlark.NewBuiltin("out.usage", ps.outUsage),
		"error": starlark.NewBuiltin("out.error", ps.outError),
		"text":  starlark.NewBuiltin("out.text", ps.outText),
		"clear": starlark.NewBuiltin("out.clear", ps.outClear),
		"final": starlark.NewBuiltin("out.final", ps.outFinal),
	})
	return ps
}

// Line runs the script on one line, given without its newline; one carriage
// return before it is dropped. a run that fails ends there, is counted and
// is returned: what it said before failing stands.
func (ps *Pass) Line(line []byte) error {
	ps.lines++
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	th := &starlark.Thread{Name: "parse"}
	th.SetMaxExecutionSteps(maxSteps)
	_, err := ps.prog.Init(th, starlark.StringDict{
		"line": starlark.String(line),
		"out":  ps.out,
		"json": starjson.Module,
		"re":   reModule,
	})
	if err == nil {
		return nil
	}
	err = errors.New(errText(err))
	ps.res.Errors++
	if ps.res.Errors == 1 {
		ps.res.FirstError, ps.res.FirstErrorLine = err.Error(), ps.lines
	}
	return err
}

// Result is what the pass has heard so far. the caller must not change it.
func (ps *Pass) Result() *Result { return &ps.res }

// errText is an evaluation error without its backtrace.
func errText(err error) string {
	var ee *starlark.EvalError
	if errors.As(err, &ee) {
		return ee.Msg
	}
	return err.Error()
}

func (ps *Pass) outTool(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "name", &name); err != nil {
		return nil, err
	}
	ps.res.Tools[name]++
	ps.res.ToolCalls++
	ps.res.Said = true
	return starlark.None, nil
}

func (ps *Pass) outError(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	v, msg := true, ""
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "v?", &v, "msg?", &msg); err != nil {
		return nil, err
	}
	if !v {
		msg = "" // no error has no message
	}
	ps.res.IsError, ps.res.ErrorMsg = v, msg
	ps.res.Said = true
	return starlark.None, nil
}

func (ps *Pass) outText(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	s, sep := "", "\n"
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "s", &s, "sep?", &sep); err != nil {
		return nil, err
	}
	if ps.res.clearNext {
		ps.res.texts, ps.res.clearNext = nil, false
	}
	// the pieces join by newlines. another sep joins this one to the piece
	// before it instead: "" for a reply that streams a token at a time.
	if n := len(ps.res.texts); sep != "\n" && n > 0 {
		ps.res.texts[n-1] += sep + s
	} else {
		ps.res.texts = append(ps.res.texts, s)
	}
	return starlark.None, nil
}

func (ps *Pass) outClear(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	next := false
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "next?", &next); err != nil {
		return nil, err
	}
	// next=True keeps the pieces until another arrives: a step that ended
	// is still the reply until a later one says something.
	if next {
		ps.res.clearNext = true
	} else {
		ps.res.texts, ps.res.clearNext = nil, false
	}
	return starlark.None, nil
}

func (ps *Pass) outFinal(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var s starlark.Value = starlark.None
	add := false
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "s?", &s, "add?", &add); err != nil {
		return nil, err
	}
	var reply string
	switch v := s.(type) {
	case starlark.String:
		reply = string(v)
	case starlark.NoneType:
		// no reply given: the pieces of text so far are it, and are spent.
		// with none there is nothing to keep.
		if len(ps.res.texts) == 0 {
			return starlark.None, nil
		}
		reply = strings.Join(ps.res.texts, "\n")
		ps.res.texts, ps.res.clearNext = nil, false
	default:
		return nil, fmt.Errorf("%s: for parameter s: got %s, want string", b.Name(), s.Type())
	}
	if !add {
		ps.res.finals = nil
	}
	ps.res.finals = append(ps.res.finals, reply)
	return starlark.None, nil
}

// outUsage adds its keywords to the totals, or with total=True replaces those
// totals. every number is checked, and the totals it would make, before any
// is changed: a bad one is an error that changes nothing.
func (ps *Pass) outUsage(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) > 0 {
		return nil, fmt.Errorf("%s: takes keywords only", b.Name())
	}
	r := &ps.res
	counts := []struct {
		key string
		dst *int
		v   starlark.Value
	}{
		{"turns", &r.Turns, nil}, {"input_tokens", &r.Input, nil}, {"output_tokens", &r.Output, nil},
		{"reasoning_tokens", &r.Reasoning, nil}, {"cache_read_tokens", &r.CacheRead, nil}, {"cache_write_tokens", &r.CacheWrite, nil},
		{"premium_requests", &r.PremiumRequests, nil}, {"ttft_ms", &r.TTFTms, nil}, {"api_duration_ms", &r.APIms, nil},
	}
	var cost starlark.Value
	total := false
	pairs := []any{"cost_usd?", &cost, "total?", &total}
	for i := range counts {
		pairs = append(pairs, counts[i].key+"?", &counts[i].v)
	}
	if err := starlark.UnpackArgs(b.Name(), nil, kwargs, pairs...); err != nil {
		return nil, err
	}

	next := make([]int, len(counts))
	for i, c := range counts {
		next[i] = *c.dst
		if c.v == nil {
			continue
		}
		n, err := whole(c.v)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %v", b.Name(), c.key, err)
		}
		if total {
			next[i] = n
		} else if next[i] > math.MaxInt-n {
			return nil, fmt.Errorf("%s: %s: the total would not fit", b.Name(), c.key)
		} else {
			next[i] += n
		}
	}
	nextCost := r.CostUSD
	if cost != nil {
		c, ok := starlark.AsFloat(cost)
		if !ok || math.IsNaN(c) || math.IsInf(c, 0) || c < 0 {
			return nil, fmt.Errorf("%s: cost_usd: %s is not a finite number of at least 0", b.Name(), cost)
		}
		if total {
			nextCost = c
		} else if nextCost += c; math.IsInf(nextCost, 0) {
			return nil, fmt.Errorf("%s: cost_usd: the total would not be finite", b.Name())
		}
	}

	for i, c := range counts {
		*c.dst = next[i]
	}
	if cost != nil {
		r.CostUSD, r.HasCost = nextCost, true
	}
	r.Said = true
	return starlark.None, nil
}

// whole reads a count: a whole number of at least 0 that fits an int.
func whole(v starlark.Value) (int, error) {
	switch v := v.(type) {
	case starlark.Int:
		n, ok := v.Int64()
		if !ok || n < 0 || n > math.MaxInt {
			return 0, fmt.Errorf("%s is not a whole number of at least 0 that fits", v)
		}
		return int(n), nil
	case starlark.Float:
		// past 2^53 a float no longer holds every whole number.
		f := float64(v)
		if math.IsNaN(f) || f < 0 || f > float64(1<<53) || f > math.MaxInt || f != math.Trunc(f) {
			return 0, fmt.Errorf("%s is not a whole number of at least 0 that fits", v)
		}
		return int(f), nil
	}
	return 0, fmt.Errorf("got %s, want a number", v.Type())
}
