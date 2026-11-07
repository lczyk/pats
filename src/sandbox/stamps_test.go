package sandbox

import (
	"context"
	"io"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// each cli's --version line, as the clis print them, reduces to its version;
// a cli the image lacks has no stamp.
func TestParseStamps(t *testing.T) {
	out := "claude=2.1.319 (Claude Code)\n\ncodex=codex-cli 0.167.0\n\ncopilot=GitHub Copilot CLI 1.0.107.\n\nopencode=\n\n"
	assert.EqualMaps(t, parseStamps(out), map[string]string{"claude": "2.1.319", "codex": "0.167.0", "copilot": "1.0.107"})
	assert.EqualMaps(t, parseStamps("x=1.0.0-beta.4 build\n"), map[string]string{"x": "1.0.0-beta.4"})
}

// a provisioned lxd image is published with the harness versions inside it
// as properties, which LXDImageStamps reads back.
func TestLXDImageStampsOnServer(t *testing.T) {
	f := newFakeServer(t)
	f.agent = func(ctx context.Context, inst string, argv []string, stdout, stderr io.Writer) error {
		if len(argv) == 3 && argv[2] == stampScript {
			io.WriteString(stdout, "claude=2.1.319 (Claude Code)\ncodex=\n")
		}
		return nil
	}
	l := lxdOn(t, f, Options{})
	alias, _, err := l.image(context.Background(), LXDImageSpec{Base: "ubuntu:26.04", Argv: []string{"true"}}, io.Discard)
	require.NoError(t, err)
	fp, err := l.fingerprint(context.Background(), alias)
	require.NoError(t, err)
	stamps, err := l.imageStamps(context.Background(), fp)
	require.NoError(t, err)
	assert.EqualMaps(t, stamps, map[string]string{"claude": "2.1.319"})
}
