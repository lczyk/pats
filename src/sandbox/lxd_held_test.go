package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// heldScript on a made-up instance: a fake stat answers for each dir, a
// fake readlink resolves links an earlier bind brought.
func TestHeldScript(t *testing.T) {
	run := func(t *testing.T, stats map[string]string, links map[string]string, args ...string) (int, string) {
		bin := t.TempDir()
		statSh := "#!/bin/sh\ncase \"$4\" in\n"
		for d, v := range stats {
			statSh += "  " + d + ") echo '" + v + "';;\n"
		}
		statSh += "  *) echo '0 0 755';;\nesac\n"
		linkSh := "#!/bin/sh\ncase \"$2\" in\n"
		for from, to := range links {
			linkSh += "  " + from + ") echo '" + to + "';;\n"
		}
		linkSh += "  *) echo \"$2\";;\nesac\n"
		require.NoError(t, os.WriteFile(filepath.Join(bin, "stat"), []byte(statSh), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(bin, "readlink"), []byte(linkSh), 0o755))
		// every dir the script asks about is a dir here: it tests [ -d ].
		root := t.TempDir()
		cmd := exec.Command("/bin/sh", append([]string{"-c", heldScript, "held"}, args...)...)
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		cmd.Dir = root
		out, _ := cmd.CombinedOutput()
		code := 0
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		return code, string(out)
	}
	cases := []struct {
		name  string
		stats map[string]string
		links map[string]string
		code  int
		at    string
	}{
		{"root's dirs", nil, nil, 0, ""},
		{"agent-owned dir", map[string]string{"/tmp": "1001 0 755"}, nil, 3, "/tmp"},
		{"world-writable dir", map[string]string{"/tmp": "0 0 777"}, nil, 3, "/tmp"},
		{"sticky world-writable dir", map[string]string{"/tmp": "0 0 1777"}, nil, 0, ""},
		{"group-writable dir of the agent's group", map[string]string{"/tmp": "0 1001 770"}, nil, 3, "/tmp"},
		{"group-writable dir of another group", map[string]string{"/tmp": "0 50 770"}, nil, 0, ""},
		{"a link under a writable bind", nil, map[string]string{"/tmp/tree/escape": "/workspace"}, 3, "/workspace"},
		{"a link from an agent's dir", map[string]string{"/tmp": "1001 1001 755"}, map[string]string{"/tmp/tree/escape": "/srv/safe"}, 3, "/tmp"},
		{"a writable bind with a space", nil, map[string]string{"/tmp/tree/escape": "/srv/my work"}, 3, "/srv/my work"},
		{"a writable bind reached through a link", nil, map[string]string{"/srv/alias/data": "/srv/data", "/tmp/tree/escape": "/srv/data"}, 3, "/srv/data"},
		{"the bind itself a link into an agent's dir", map[string]string{"/usr": "1001 1001 755"}, map[string]string{"/tmp/tree/escape/ref": "/usr/ref"}, 3, "/usr"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := "/tmp/tree/ref"
			if c.links != nil {
				p = "/tmp/tree/escape/ref"
			}
			code, out := run(t, c.stats, c.links, "1001", "/workspace", "/srv/my work", "/srv/alias/data", "--", p)
			assert.Equal(t, code, c.code, "output: %s", out)
			if c.at != "" {
				assert.ContainsString(t, out, c.at)
			}
		})
	}
}
