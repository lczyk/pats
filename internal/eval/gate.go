package eval

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// preflightGate holds every pair between its setup and its agent until every
// credential preflight has settled (docs/spec/preflight-overlap.md, as
// docs/spec/preflight-per-agent.md amends it). it opens for the agents that
// passed; a pair of an agent that failed, or of any agent when the failure
// is fatal, is discarded.
type preflightGate struct {
	ch      chan struct{} // closed once settled
	once    sync.Once
	dropped map[string]bool
	fatal   bool
	at      time.Time // when it settled
}

func newPreflightGate() *preflightGate { return &preflightGate{ch: make(chan struct{})} }

// settle opens the gate for every agent but dropped, or for none.
func (g *preflightGate) settle(dropped map[string]bool, fatal bool, at time.Time) {
	g.once.Do(func() {
		g.dropped, g.fatal, g.at = dropped, fatal, at
		close(g.ch)
	})
}

// settled is closed once the gate has settled.
func (g *preflightGate) settled() <-chan struct{} {
	if g == nil {
		return nil
	}
	return g.ch
}

// pass says whether agent's pair goes on, once settled; false when ctx ends
// first. a nil gate always passes (judge, a run with nothing to wait for).
func (g *preflightGate) pass(ctx context.Context, agent string) bool {
	if g == nil {
		return true
	}
	select {
	case <-g.ch:
		return g.passes(agent)
	default:
	}
	select {
	case <-g.ch:
		return g.passes(agent)
	case <-ctx.Done():
		return false
	}
}

func (g *preflightGate) passes(agent string) bool { return !g.fatal && !g.dropped[agent] }

// open says whether the gate settled and passes agent, without waiting.
func (g *preflightGate) open(agent string) bool {
	if g == nil {
		return true
	}
	select {
	case <-g.ch:
		return g.passes(agent)
	default:
		return false
	}
}

// errLocal marks a write to the run dir that failed: the run's own error,
// whatever becomes of the pair.
var errLocal = errors.New("the run dir")

// local marks err as errLocal.
func local(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", errLocal, err)
}

// errDiscard is a pair whose agent was dropped at the gate: nothing of it
// is recorded, as if it never started.
var errDiscard = errors.New("discarded at the preflight gate")
