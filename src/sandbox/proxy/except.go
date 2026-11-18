package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	pathpkg "path"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// RawEntry is one entry of an allow or a deny list as a policy writes it: a
// line, or a mapping of the line and the requests it leaves alone
// (docs/spec/egress-except.md). yaml and json both carry an entry with no
// exception as the line itself.
type RawEntry struct {
	Match  string
	Except []string
}

// Lines are entries with no exception, one for each line.
func Lines(lines ...string) []RawEntry {
	if lines == nil {
		return nil
	}
	out := make([]RawEntry, len(lines))
	for i, l := range lines {
		out[i] = RawEntry{Match: l}
	}
	return out
}

// Matches are the entries' lines, without their exceptions.
func Matches(entries []RawEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Match
	}
	return out
}

// Same reports whether two entries are written alike: the match, and the
// exceptions in order.
func (r RawEntry) Same(o RawEntry) bool {
	if r.Match != o.Match || len(r.Except) != len(o.Except) {
		return false
	}
	for i := range r.Except {
		if r.Except[i] != o.Except[i] {
			return false
		}
	}
	return true
}

// String is the entry as an error names it.
func (r RawEntry) String() string { return r.Match }

// rawMapping is the mapping form on the wire.
type rawMapping struct {
	Match  string   `yaml:"match" json:"match"`
	Except []string `yaml:"except" json:"except"`
}

func (r RawEntry) MarshalYAML() (any, error) {
	if len(r.Except) == 0 {
		return r.Match, nil
	}
	return rawMapping(r), nil
}

func (r RawEntry) MarshalJSON() ([]byte, error) {
	if len(r.Except) == 0 {
		return json.Marshal(r.Match)
	}
	return json.Marshal(rawMapping(r))
}

// UnmarshalYAML reads a line or a mapping. the mapping's keys are checked
// here: a decoder's own check for unknown fields stops at a type that
// unmarshals itself.
func (r *RawEntry) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*r = RawEntry{Match: node.Value}
		if node.Tag == "!!null" {
			r.Match = ""
		}
		return nil
	case yaml.MappingNode:
		var m RawEntry
		var hasMatch, hasExcept bool
		for i := 0; i+1 < len(node.Content); i += 2 {
			k, v := node.Content[i], node.Content[i+1]
			if (k.Value == "match" && hasMatch) || (k.Value == "except" && hasExcept) {
				return fmt.Errorf("line %d: %s is written twice in one entry", k.Line, k.Value)
			}
			switch k.Value {
			case "match":
				if v.Kind != yaml.ScalarNode || v.Tag == "!!null" || v.Value == "" {
					return fmt.Errorf("line %d: match: want an entry, as a line writes one", v.Line)
				}
				m.Match, hasMatch = v.Value, true
			case "except":
				if v.Kind != yaml.SequenceNode || len(v.Content) == 0 {
					return fmt.Errorf("line %d: except: want a list of one or more url parts -- with none, write the entry as a line", v.Line)
				}
				for _, item := range v.Content {
					if item.Kind != yaml.ScalarNode || item.Tag == "!!null" || item.Value == "" {
						return fmt.Errorf("line %d: except: an item is a url part: a path glob (/...) or a regexp (~...)", item.Line)
					}
					m.Except = append(m.Except, item.Value)
				}
				hasExcept = true
			default:
				return fmt.Errorf("line %d: unknown field %q in an entry (it takes match and except)", k.Line, k.Value)
			}
		}
		switch {
		case !hasMatch:
			return fmt.Errorf("line %d: an entry written as a mapping needs a match", node.Line)
		case !hasExcept:
			return fmt.Errorf("line %d: an entry written as a mapping needs an except -- with none, write it as a line", node.Line)
		}
		*r = m
		return nil
	}
	return fmt.Errorf("line %d: an entry is a line, or a mapping of match and except", node.Line)
}

// UnmarshalJSON reads what MarshalJSON writes, as strictly as the yaml. the
// object is read a key at a time: encoding/json's own decoding of a struct
// takes a key in any case and the last of two alike.
func (r *RawEntry) UnmarshalJSON(data []byte) error {
	var line string
	if err := json.Unmarshal(data, &line); err == nil {
		*r = RawEntry{Match: line}
		return nil
	}
	shape := errors.New("an entry is a line, or a mapping of match and except")
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return shape
	}
	var m RawEntry
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		key, _ := tok.(string)
		if err != nil {
			return shape
		}
		if seen[key] {
			return fmt.Errorf("%s is written twice in one entry", key)
		}
		seen[key] = true
		switch key {
		case "match":
			err = dec.Decode(&m.Match)
		case "except":
			err = dec.Decode(&m.Except)
		default:
			return fmt.Errorf("unknown field %q in an entry (it takes match and except)", key)
		}
		if err != nil {
			return fmt.Errorf("%w: %s", shape, key)
		}
	}
	if m.Match == "" || len(m.Except) == 0 || slices.Contains(m.Except, "") {
		return errors.New("an entry written as a mapping needs a match and an except")
	}
	*r = m
	return nil
}

// ParseRawEntry parses an entry with its exceptions. an exception is about
// paths, so the entry must be a url entry, and each item is a url part with
// an optional method prefix and no host: it applies wherever the entry does.
//
// an item is read the closed way for the list it is in, which is the other
// way to the entry it sits on. a deny's spares a request, so it is matched
// as an allow's url part is and tighter (sparesURL). an allow's withholds
// one, so it is matched as a deny's url part is.
func ParseRawEntry(raw RawEntry, deny bool) (Entry, error) {
	e, err := ParseEntry(raw.Match, deny)
	if err != nil || len(raw.Except) == 0 {
		return e, err
	}
	if !e.URL() {
		return e, fmt.Errorf("an except needs a url entry: write %s/**", e.Raw)
	}
	for _, item := range raw.Except {
		x, err := parseExcept(item, deny)
		if err != nil {
			return e, fmt.Errorf("except %q: %w", item, err)
		}
		e.except = append(e.except, x)
	}
	return e, nil
}

// parseExcept parses one item of an entry's except list. deny says which
// list the entry is in.
func parseExcept(item string, deny bool) (Entry, error) {
	x := Entry{Raw: strings.TrimSpace(item), deny: !deny}
	s := x.Raw
	if first, rest, ok := strings.Cut(s, " "); ok && methodsRe.MatchString(first) {
		x.methods = strings.Split(first, ",")
		s = strings.TrimSpace(rest)
	}
	var err error
	switch {
	case strings.HasPrefix(s, "~"):
		re := s[1:]
		if re == "" {
			return x, errors.New("empty regexp after ~")
		}
		if deny {
			// the whole path: unanchored, it would spare every path that
			// holds the text.
			re = "^(?:" + re + ")$"
		} else {
			re = "(?i)" + re
		}
		if x.urlRe, err = regexp.Compile(re); err != nil {
			return x, fmt.Errorf("regexp: %w", err)
		}
		x.urlText, x.urlIsRe = re, true
	case strings.HasPrefix(s, "/"):
		// a deny's item must match every reading of a path, and a glob that
		// writes what a reading rewrites can never match its own.
		if deny && (strings.ContainsAny(s, `;\%`) || lookalikes.Replace(s) != s) {
			return x, errors.New(`a glob here cannot write a ";", a backslash, a "%" or a look-alike of a slash or a dot: no path it names is read one way`)
		}
		if x.urlRe, err = globRe(s, !deny); err != nil {
			return x, fmt.Errorf("glob: %w", err)
		}
		x.urlText = s
	default:
		return x, errors.New("want a path glob (/...) or a regexp (~...), with no host: an item applies wherever its entry does")
	}
	if deny {
		if x.queryRe, err = spelledQuery(x.urlRe); err != nil {
			return x, fmt.Errorf("regexp: %w", err)
		}
	}
	return x, nil
}

// spared is the item of a deny entry's except that spares the request, or
// nil. an item spares only when every reading of the path matches it, with
// none let off: a waiver would spare "/a;b/key" where an origin that drops
// parameters serves "/a/key", a path the entry denies.
func (e Entry) spared(method, urlPath, rawQuery string) *Entry {
	for i, x := range e.except {
		if x.matchesMethod(method) && x.sparesURL(urlPath, rawQuery) {
			return &e.except[i]
		}
	}
	return nil
}

// sparesURL holds the item to every reading of the path, and to each with
// its trailing slash dropped, as a deny is tried against the path: "/a/b/.."
// resolves to "/a/", which an origin may serve as "/a". a reading that holds
// a "?" of its own (sent as "%3F") is spared by nothing: it would read as
// the query an item spells.
func (e Entry) sparesURL(urlPath, rawQuery string) bool {
	for _, r := range readings(urlPath) {
		for _, p := range []string{r.path, pathpkg.Clean(r.path)} {
			if p == "." && r.path == "" {
				continue // path.Clean("") is ".", which no request asked for
			}
			if strings.ContainsRune(p, '?') || !e.admitsPath(p, rawQuery) {
				return false
			}
		}
	}
	return true
}

// withheld is the item of an allow entry's except that takes the request
// out of what the entry admits, or nil. an item withholds as a deny takes:
// on any reading of the path, case-insensitively.
func (e Entry) withheld(method, urlPath, rawQuery string) *Entry {
	for i, x := range e.except {
		if x.matchesMethod(method) && x.matchesURL(urlPath, rawQuery) {
			return &e.except[i]
		}
	}
	return nil
}

// Left is an entry that matched a request and left it alone: a deny that
// spared it, or an allow that withheld it, and the item that did.
type Left struct {
	Deny  bool
	Entry string
	Item  string
}
