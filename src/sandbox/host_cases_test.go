//go:build !nohost

package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// the host's rows of the cross-driver tables, and its exec tests, so a build
// without the host driver drops them with it.

func init() {
	conformDrivers = append(conformDrivers, conformDriver{"host", hostConform})
}

// hostConform is a host session, which has no fake: its commands are real
// processes and its sandbox is the host, so the paths of the table are dirs
// under a root of the test's. the proxy child is TestHostEgress's.
func hostConform(t *testing.T, spec Spec) (Session, *conformFake) {
	root := t.TempDir()
	at := func(p string) string { return filepath.Join(root, p) }
	spec.Egress = Egress{None: true}
	sess, err := hostOn(t, Options{}).Start(context.Background(), spec)
	require.NoError(t, err)
	return sess, &conformFake{id: "host", at: at,
		exits: func(code int, out, errOut string) []string {
			return []string{"/bin/sh", "-c", fmt.Sprintf("printf %%s %s; printf %%s %s >&2; exit %d", shQuote(out), shQuote(errOut), code)}
		},
		write: func(p, content string) {
			require.NoError(t, os.MkdirAll(filepath.Dir(at(p)), 0o755))
			require.NoError(t, os.WriteFile(at(p), []byte(content), 0o644))
		},
		read: func(p string) (string, bool) {
			b, err := os.ReadFile(at(p))
			return string(b), err == nil
		},
		// a host session makes nothing to leave but what it runs.
		left: func() []string {
			if _, err := sess.Exec(context.Background(), Cmd{Argv: []string{"true"}}, io.Discard, io.Discard); !errors.Is(err, ErrLaunch) {
				return []string{"a closed session that still runs commands"}
			}
			return nil
		},
	}
}

// TestExecCancelHost holds the host, with no client and no runner to fake,
// to TestExecCancel's results on real processes.
func TestExecCancelHost(t *testing.T) {
	host := func(t *testing.T) (Session, string) {
		dir := t.TempDir()
		s, err := hostOn(t, Options{}).Start(context.Background(), Spec{Workdir: dir, Egress: Egress{None: true}})
		require.NoError(t, err)
		t.Cleanup(func() { s.Close() })
		return s, dir
	}
	t.Run("host/agent", func(t *testing.T) {
		s, _ := host(t)
		// the command's stdout is a pipe of the test's: it ends once nothing
		// of the command's group holds it, a child in the background (as an
		// agent's tools are) included, alive or not yet reaped.
		pr, pw, err := os.Pipe()
		require.NoError(t, err)
		defer pr.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		type result struct {
			code int
			err  error
		}
		done := make(chan result, 1)
		go func() {
			code, err := s.Exec(ctx, Cmd{Argv: []string{"/bin/sh", "-c", "sleep 300 & echo started; wait"}}, pw, io.Discard)
			pw.Close()
			done <- result{code, err}
		}()
		require.NoError(t, pr.SetReadDeadline(time.Now().Add(10*time.Second)))
		out := bufio.NewReader(pr)
		line, err := out.ReadString('\n')
		require.NoError(t, err, "the command never started")
		require.Equal(t, line, "started\n")
		cancel()
		select {
		case got := <-done:
			require.NoError(t, got.err)
			assert.Equal(t, got.code, -1)
		case <-time.After(5 * time.Second):
			t.Fatal("Exec did not return")
		}
		require.NoError(t, pr.SetReadDeadline(time.Now().Add(5*time.Second)))
		_, err = io.ReadAll(out)
		assert.NoError(t, err, "the cancelled command's child still holds its stdout")
	})
	t.Run("host/done before", func(t *testing.T) {
		s, dir := host(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		code, err := execBounded(t, s, ctx, Cmd{Argv: []string{"/bin/sh", "-c", "touch ran"}})
		require.Error(t, err, "context canceled")
		assert.ErrorIs(t, err, ErrLaunch)
		assert.Equal(t, code, -1)
		_, err = os.Stat(filepath.Join(dir, "ran"))
		assert.That(t, os.IsNotExist(err), "the command ran")
	})
	t.Run("host/finishes", func(t *testing.T) {
		s, _ := host(t)
		code, err := execBounded(t, s, context.Background(), Cmd{Argv: []string{"true"}})
		require.NoError(t, err)
		assert.Equal(t, code, 0)
	})
}
