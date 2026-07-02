package eval

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

func TestGenerateNameDeterministic(t *testing.T) {
	a, b := generateName("001-20260702"), generateName("001-20260702")
	assert.Equal(t, a, b)
	// different seed -> (almost certainly) different name; at minimum valid shape.
	c := generateName("002-20260702")
	assert.That(t, a != "" && c != "", "names non-empty", a, c)
}

func TestSplitRunName(t *testing.T) {
	cases := []struct {
		name string
		date string
		n    int
		ok   bool
	}{
		{"001-20260702-fluffy-bunny", "20260702", 1, true},
		{"012-20260703-woven-sock", "20260703", 12, true},
		{"latest", "", 0, false}, // the symlink
		{"garbage", "", 0, false},
	}
	for _, c := range cases {
		date, n, ok := splitRunName(c.name)
		if ok != c.ok || (ok && (date != c.date || n != c.n)) {
			t.Errorf("splitRunName(%q) = (%q, %d, %v), want (%q, %d, %v)",
				c.name, date, n, ok, c.date, c.n, c.ok)
		}
	}
}

func TestResolveRunDir(t *testing.T) {
	base := t.TempDir()
	mk := func(name string) { require.NoError(t, os.MkdirAll(filepath.Join(base, name), 0o755)) }
	mk("002-20260702-fluffy-bunny")
	mk("003-20260703-woven-sock")

	// words alone resolve.
	got, err := resolveRunDir(base, "fluffy-bunny")
	require.NoError(t, err)
	assert.Equal(t, filepath.Base(got), "002-20260702-fluffy-bunny")

	// full name and explicit path still work.
	got, err = resolveRunDir(base, "002-20260702-fluffy-bunny")
	require.NoError(t, err)
	assert.Equal(t, filepath.Base(got), "002-20260702-fluffy-bunny")

	// "" -> latest by (date, n).
	got, err = resolveRunDir(base, "")
	require.NoError(t, err)
	assert.Equal(t, filepath.Base(got), "003-20260703-woven-sock")

	// unknown -> error.
	_, err = resolveRunDir(base, "no-such-run")
	assert.Error(t, err, "not found")
}
