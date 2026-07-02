package eval

import "fmt"

// splitArgs splits a command-line string into argv using POSIX sh quoting +
// splitting rules -- the shlex(posix=True) recipe -- but performs NO expansion
// (no $var, globbing, or `command` substitution; those belong inside the
// invoked script, and pats deliberately runs fields without a shell).
//
// rules: unquoted whitespace separates words; '...' is fully literal; "..."
// is literal except a backslash before " \ $ or `; an unquoted backslash
// escapes the next char. adjacent quoted/unquoted runs concatenate, and empty
// quotes yield an empty argument (two adjacent quotes -> one empty arg).
//
// NOTE: no line-continuation (backslash-newline) -- field values are single
// line. unterminated quotes and a trailing backslash are errors.
func splitArgs(s string) ([]string, error) {
	var args []string
	var buf []rune
	started := false // a word has begun (tracked separately so '' yields "")
	r := []rune(s)

	for i := 0; i < len(r); {
		c := r[i]
		switch c {
		case '\'':
			started = true
			i++
			for i < len(r) && r[i] != '\'' {
				buf = append(buf, r[i])
				i++
			}
			if i >= len(r) {
				return nil, fmt.Errorf("unterminated single quote")
			}
			i++ // closing '

		case '"':
			started = true
			i++
			for i < len(r) && r[i] != '"' {
				if r[i] == '\\' && i+1 < len(r) {
					switch r[i+1] {
					case '"', '\\', '$', '`':
						buf = append(buf, r[i+1])
						i += 2
						continue
					}
				}
				buf = append(buf, r[i]) // backslash literal before anything else
				i++
			}
			if i >= len(r) {
				return nil, fmt.Errorf("unterminated double quote")
			}
			i++ // closing "

		case '\\':
			if i+1 >= len(r) {
				return nil, fmt.Errorf("trailing backslash")
			}
			started = true
			buf = append(buf, r[i+1])
			i += 2

		case ' ', '\t', '\n', '\r':
			if started {
				args = append(args, string(buf))
				buf = buf[:0]
				started = false
			}
			i++

		default:
			started = true
			buf = append(buf, c)
			i++
		}
	}
	if started {
		args = append(args, string(buf))
	}
	return args, nil
}
