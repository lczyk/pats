package eval

import (
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

func TestSplitArgs(t *testing.T) {
	ok := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"\t \n", nil},
		{"a", []string{"a"}},
		{"a b c", []string{"a", "b", "c"}},
		{"  a   b  ", []string{"a", "b"}},
		{"a\tb\nc", []string{"a", "b", "c"}},
		{"a\rb c\r", []string{"a", "b", "c"}}, // carriage return is whitespace too
		{"scorers/scorer_x.py --debug", []string{"scorers/scorer_x.py", "--debug"}},
		{"prepare.sh base-passwd ubuntu-24.04", []string{"prepare.sh", "base-passwd", "ubuntu-24.04"}},

		// single quotes: fully literal, no escapes.
		{"'a b'", []string{"a b"}},
		{"'a\\b'", []string{"a\\b"}},
		{"'a\"b'", []string{"a\"b"}},

		// double quotes.
		{"\"a b\"", []string{"a b"}},
		{"\"a\\\"b\"", []string{"a\"b"}}, // \" -> "
		{"\"a\\\\b\"", []string{"a\\b"}}, // \\ -> \
		{"\"a\\nb\"", []string{"a\\nb"}}, // \n stays literal backslash-n
		{"\"a$b\"", []string{"a$b"}},     // no expansion
		{"\"a\\$b\"", []string{"a$b"}},   // \$ -> $

		// adjacency / concatenation.
		{"a\"b\"c", []string{"abc"}},
		{"'a'b\"c\"", []string{"abc"}},
		{"ab'cd'", []string{"abcd"}},

		// empty-quote -> empty arg.
		{"''", []string{""}},
		{"\"\"", []string{""}},
		{"a''b", []string{"ab"}},
		{"a '' b", []string{"a", "", "b"}},

		// unquoted escapes.
		{"a\\ b", []string{"a b"}},
		{"\\'", []string{"'"}},
		{"\\\"", []string{"\""}},
		{"a\\\\b", []string{"a\\b"}},

		// mixed.
		{"cmd --flag=\"a b\" 'c d' e", []string{"cmd", "--flag=a b", "c d", "e"}},
	}
	for _, tc := range ok {
		got, err := splitArgs(tc.in)
		require.NoError(t, err, tc.in)
		assert.EqualArrays(t, got, tc.want, tc.in)
	}

	bad := []string{
		"'abc",    // unterminated single quote
		"\"abc",   // unterminated double quote
		"a'b",     // unterminated single quote mid-word
		"abc\\",   // trailing backslash
		"a \"b\\", // trailing backslash inside dquote (consumed escape, then EOF)
	}
	for _, in := range bad {
		_, err := splitArgs(in)
		assert.Error(t, err, assert.AnyError, in)
	}
}
