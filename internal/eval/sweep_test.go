package eval

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/src/sandbox"
)

// the sweep lists each place the selection uses once, and a place it
// cannot reach is a warning, never a stop; PATS_NO_SWEEP lists none.
func TestSweepAtRunStart(t *testing.T) {
	var opened []string
	old := openDriver
	openDriver = func(o sandbox.Options) (sandbox.Driver, error) {
		opened = append(opened, o.Driver+" "+o.Remote)
		return nil, errors.New("no daemon")
	}
	t.Cleanup(func() { openDriver = old })

	fakes := threeFakes()
	t.Setenv(NoSweepEnv, "")
	_, err, log := runAgents(t, fakes, Options{})
	require.NoError(t, err)
	assert.Equal(t, len(opened), 1) // three sandboxes, one place
	assert.ContainsString(t, log, "sweep: docker: no daemon")

	opened = nil
	t.Setenv(NoSweepEnv, "1")
	_, err, _ = runAgents(t, fakes, Options{})
	require.NoError(t, err)
	assert.Equal(t, len(opened), 0)
}

// the sweep's bound covers its lock wait: a lock held past it is a
// warning, and the run goes on.
func TestSweepBound(t *testing.T) {
	t.Setenv(NoSweepEnv, "")
	unlock, err := machineLock(t.Context())
	require.NoError(t, err)
	defer unlock()
	old := sweepBound
	sweepBound = 50 * time.Millisecond
	t.Cleanup(func() { sweepBound = old })
	var out bytes.Buffer
	sweepDead(t.Context(), parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
`), []string{"sxx"}, false, logw{w: &out})
	assert.ContainsString(t, out.String(), "sweep: the machine lock")

	// a dry run takes no lock, so the held one does not stop it.
	oldOpen := openDriver
	openDriver = func(o sandbox.Options) (sandbox.Driver, error) { return nil, errors.New("no daemon") }
	t.Cleanup(func() { openDriver = oldOpen })
	out.Reset()
	sweepDead(t.Context(), parseCfg(t, `
sxx: {kind: sandbox, items: {sxx: {image: s}}}
`), []string{"sxx"}, true, logw{w: &out})
	assert.That(t, !strings.Contains(out.String(), "sweep: the machine lock"), "dry run waited on the lock: %s", out.String())
}

// hangingRuns is a place whose listing never answers.
type hangingRuns struct{ sandbox.Driver }

func (hangingRuns) Runs(ctx context.Context) ([]sandbox.RunRecord, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// a listing that hangs ends at the bound; the places of the selection are
// each listed once, and a sandbox not selected is not.
func TestSweepPlaces(t *testing.T) {
	t.Setenv(NoSweepEnv, "")
	var opened []string
	old := openDriver
	openDriver = func(o sandbox.Options) (sandbox.Driver, error) {
		opened = append(opened, o.Driver+" "+o.Remote)
		if o.Remote != "one" {
			return nil, errors.New("no server")
		}
		return hangingRuns{}, nil
	}
	t.Cleanup(func() { openDriver = old })
	oldB := sweepBound
	sweepBound = 50 * time.Millisecond
	t.Cleanup(func() { sweepBound = oldB })
	cfg := parseCfg(t, `
boxes: {kind: sandbox, items: {
  ax: {driver: lxd, remote: one, image: i},
  bx: {driver: lxd, remote: one, image: j},
  cx: {driver: lxd, remote: two, image: k},
  dx: {driver: lxd, remote: three, image: l}}}
`)
	var out bytes.Buffer
	sweepDead(t.Context(), cfg, []string{"cx", "ax", "bx"}, false, logw{w: &out})
	assert.Equal(t, strings.Join(opened, ","), "lxd two,lxd one")
	assert.ContainsString(t, out.String(), "sweep: lxd two: no server")
	assert.ContainsString(t, out.String(), "sweep: cut short after 50ms at lxd one")
}

// judge sweeps before its preflight.
func TestSweepBeforeJudge(t *testing.T) {
	t.Setenv(NoSweepEnv, "")
	var order []string
	old := openDriver
	openDriver = func(o sandbox.Options) (sandbox.Driver, error) {
		order = append(order, "sweep")
		return nil, errors.New("no daemon")
	}
	t.Cleanup(func() { openDriver = old })
	fake := &fakeSandbox{preflightCode: 1}
	useFake(t, fake)
	dir, _, cfg := judgeFixture(t)
	_ = Judge(cfg, JudgeOptions{ConfigDir: dir, Out: io.Discard, Offline: true})
	assert.Equal(t, strings.Join(order, ","), "sweep")
	assert.Equal(t, fake.preflights, 1)
}
