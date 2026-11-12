package eval

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// deadlineBox records how long preflight gives each step, and its agent
// prints say.
type deadlineBox struct {
	start, exec time.Duration
	say         string
}

func (b *deadlineBox) Run(ctx context.Context, spec sandbox.Spec, stdout, stderr io.Writer) (int, error) {
	return 0, nil
}

func (b *deadlineBox) Start(ctx context.Context, spec sandbox.Spec) (sandbox.Session, error) {
	d, _ := ctx.Deadline()
	b.start = time.Until(d)
	return &deadlineSession{b: b}, nil
}

type deadlineSession struct{ b *deadlineBox }

func (s *deadlineSession) Push(ctx context.Context, hostPath, path string) error { return nil }
func (s *deadlineSession) Pull(ctx context.Context, path, hostDir string) error  { return nil }
func (s *deadlineSession) HandOver(ctx context.Context, paths ...string) error   { return nil }
func (s *deadlineSession) Sync(ctx context.Context) error                        { return nil }
func (s *deadlineSession) Close() error                                          { return nil }
func (s *deadlineSession) Exec(ctx context.Context, cmd sandbox.Cmd, stdout, stderr io.Writer) (int, error) {
	d, _ := ctx.Deadline()
	s.b.exec = time.Until(d)
	_, _ = io.WriteString(stdout, s.b.say)
	return 0, nil
}

// the sandbox gets a pair's setup budget to come up (a cold lxd server takes
// minutes); the agent's reply alone is held to preflightTimeout.
func TestPreflightBudgets(t *testing.T) {
	box := &deadlineBox{}
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return box, nil }
	defer func() { newSandbox = old }()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("p"), 0o644))
	cfg := parseCfg(t, "boxes: {kind: sandbox, items: {bxx: {image: x}}}\n"+
		"agents: {kind: agent, defaults: {harness: opencode-openrouter, model: m}, items: {axx: {sandbox: bxx}}}\n")
	var out bytes.Buffer
	require.NoError(t, Preflight(context.Background(), cfg, Options{ConfigDir: dir, Out: &out}, []config.TestPair{{Agent: "axx"}}))
	assert.That(t, box.start > preflightTimeout && box.start <= setupTimeout, "start budget %s", box.start)
	assert.That(t, box.exec > 0 && box.exec <= preflightTimeout, "exec budget %s", box.exec)
}

// a harness that exits 0 on a reply that failed does not pass: its stream
// says so, as a refused key does in pi's.
func TestPreflightHearsAnError(t *testing.T) {
	box := &deadlineBox{say: piFailed}
	old := newSandbox
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) { return box, nil }
	defer func() { newSandbox = old }()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("p"), 0o644))
	cfg := parseCfg(t, "boxes: {kind: sandbox, items: {bxx: {image: x}}}\n"+
		"agents: {kind: agent, defaults: {harness: pi-openrouter, model: m}, items: {axx: {sandbox: bxx}}}\n")
	var out bytes.Buffer
	err := Preflight(context.Background(), cfg, Options{ConfigDir: dir, Out: &out}, []config.TestPair{{Agent: "axx"}})
	assert.Error(t, err, "harness reported an error")
	assert.Error(t, err, "User not found")

	box.say = piLog
	require.NoError(t, Preflight(context.Background(), cfg, Options{ConfigDir: dir, Out: &out}, []config.TestPair{{Agent: "axx"}}))
}
