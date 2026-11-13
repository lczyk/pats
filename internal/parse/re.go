package parse

import (
	"regexp"
	"sync"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
)

// reModule is the script's `re`: go's regexp, whose matching is linear in the
// input whatever an agent printed into it. no lookaround, no backreferences.
var reModule = &starlarkstruct.Module{
	Name: "re",
	Members: starlark.StringDict{
		"search":  starlark.NewBuiltin("re.search", reSearch),
		"findall": starlark.NewBuiltin("re.findall", reFindall),
	},
}

// patterns caches compiled patterns, which a script states once and uses on
// every line. it is capped: a script that builds a pattern per line compiles
// each time past the cap.
var patterns struct {
	sync.Mutex
	m map[string]*regexp.Regexp
}

const maxPatterns = 256

func compilePattern(p string) (*regexp.Regexp, error) {
	patterns.Lock()
	defer patterns.Unlock()
	if re := patterns.m[p]; re != nil {
		return re, nil
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, err
	}
	if patterns.m == nil {
		patterns.m = map[string]*regexp.Regexp{}
	}
	if len(patterns.m) < maxPatterns {
		patterns.m[p] = re
	}
	return re, nil
}

// match is one match as a script sees it: the whole match, then each group,
// None for a group that took no part.
func match(s string, loc []int) starlark.Tuple {
	out := make(starlark.Tuple, 0, len(loc)/2)
	for i := 0; i+1 < len(loc); i += 2 {
		if loc[i] < 0 {
			out = append(out, starlark.None)
			continue
		}
		out = append(out, starlark.String(s[loc[i]:loc[i+1]]))
	}
	return out
}

// reSearch is re.search(pattern, s): the first match, or None.
func reSearch(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var pattern, s string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "pattern", &pattern, "s", &s); err != nil {
		return nil, err
	}
	re, err := compilePattern(pattern)
	if err != nil {
		return nil, err
	}
	loc := re.FindStringSubmatchIndex(s)
	if loc == nil {
		return starlark.None, nil
	}
	return match(s, loc), nil
}

// reFindall is re.findall(pattern, s): every match, in order.
func reFindall(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var pattern, s string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "pattern", &pattern, "s", &s); err != nil {
		return nil, err
	}
	re, err := compilePattern(pattern)
	if err != nil {
		return nil, err
	}
	var out []starlark.Value
	for _, loc := range re.FindAllStringSubmatchIndex(s, -1) {
		out = append(out, match(s, loc))
	}
	return starlark.NewList(out), nil
}
