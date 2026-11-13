package eval

import (
	"errors"
	"sync"
)

// the budget (docs/spec/budget.md): a run-wide cap stops new pairs once the
// reported spend reaches it, and an agent's budget stops one pair.

// errBudget cancels an agent whose pair passed its agent's budget.
var errBudget = errors.New("budget passed")

// ErrBudget is a run that left pairs unstarted at its --budget.
var ErrBudget = errors.New("budget reached")

// ledger is a run's spend, under one lock: what finished pairs cost, and
// what running ones have reported so far. a nil ledger counts nothing and
// admits everything.
type ledger struct {
	mu         sync.Mutex
	limit      float64 // 0: no run-wide cap
	finished   float64
	live       map[*pairStat]float64
	notStarted int
	// hit latches once the spend reaches the cap: float sums regroup as
	// pairs finish, so a sum just over the cap can read just under it later.
	hit bool
}

func newLedger(limit float64) *ledger {
	return &ledger{limit: limit, live: map[*pairStat]float64{}}
}

// reachedLocked says whether the cap is reached, and latches it.
func (l *ledger) reachedLocked() bool {
	if l.limit > 0 && !l.hit && l.spentLocked() >= l.limit {
		l.hit = true
	}
	return l.hit
}

// spentLocked is the spend so far.
func (l *ledger) spentLocked() float64 {
	s := l.finished
	for _, c := range l.live {
		s += c
	}
	return s
}

// admit starts a pair, or says no once the spend has reached the cap; a
// pair told no is counted as not started.
func (l *ledger) admit(s *pairStat) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.reachedLocked() {
		l.notStarted++
		return false
	}
	l.live[s] = 0
	return true
}

// add records a step's cost for a running pair and returns the pair's spend.
func (l *ledger) add(s *pairStat, c float64) float64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.live[s] += c
	l.reachedLocked()
	return l.live[s]
}

// finish moves a pair from live to finished in one step: its final spend is
// its summary's cost when known, else what it reported live, so a step is
// never counted twice and a spend seen is kept when the summary is missing.
func (l *ledger) finish(s *pairStat) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.live[s]
	if s.costKnown {
		c = s.cost
	}
	l.finished += c
	delete(l.live, s)
	l.reachedLocked()
}

// reached says whether the run-wide cap is reached.
func (l *ledger) reached() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reachedLocked()
}

// totals is the spend and the pairs not started so far.
func (l *ledger) totals() (spent float64, notStarted int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.spentLocked(), l.notStarted
}
