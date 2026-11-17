package eval

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/lczyk/pats/internal/config"
)

// the budget (docs/spec/budget.md, budget-kinds.md): a run-wide cap stops
// new pairs once what they reported reaches it, and an agent's budget stops
// one pair.

// ErrBudget is a run that left pairs unstarted at its --budget.
var ErrBudget = errors.New("budget reached")

// budgetStop cancels an agent whose pair passed a cap of its agent's
// budget: kind is the cap.
type budgetStop struct{ kind string }

func (b budgetStop) Error() string { return b.kind + " budget passed" }

// usage is what a pair reported of the kinds a budget caps, but time.
type usage struct {
	cost          float64
	tokens, tools int64
}

func (u usage) plus(v usage) usage {
	return usage{u.cost + v.cost, u.tokens + v.tokens, u.tools + v.tools}
}

// passed is the first kind of b that u is past, "" for none. the order is
// config.BudgetKinds', so one line passing two caps names the same one.
func passed(b config.Budget, u usage) string {
	switch {
	case b.Cost > 0 && u.cost > b.Cost:
		return "cost"
	case b.Tokens > 0 && u.tokens > b.Tokens:
		return "tokens"
	case b.Tools > 0 && u.tools > b.Tools:
		return "tools"
	}
	return ""
}

// stopMessage is a budget-stopped pair's error: the kind, what the pair
// reached and its cap. ran is how long the agent ran.
func stopMessage(kind string, b config.Budget, u usage, ran time.Duration) string {
	switch kind {
	case "cost":
		return fmt.Sprintf("cost %s passed its budget %s", usd(u.cost), usd(b.Cost))
	case "tokens":
		return fmt.Sprintf("tokens %d passed its budget %d", u.tokens, b.Tokens)
	case "tools":
		return fmt.Sprintf("tools %d passed its budget %d", u.tools, b.Tools)
	}
	return fmt.Sprintf("time %s reached its budget %s", roundDur(ran), b.Time)
}

// roundDur is a duration as a log line shows it: to the second, or the
// millisecond under one.
func roundDur(d time.Duration) time.Duration {
	if d >= time.Second {
		return d.Round(time.Second)
	}
	return d.Round(time.Millisecond)
}

// ledger is a run's use, under one lock: what finished pairs reported, and
// what running ones have so far. a nil ledger counts nothing and admits
// everything.
type ledger struct {
	mu         sync.Mutex
	limit      config.Budget // zero: no run-wide cap
	start      time.Time     // the time cap's clock starts here
	now        func() time.Time
	finished   usage
	live       map[*pairStat]usage
	notStarted int
	// hit latches the first kind found reached: float sums regroup as pairs
	// finish, and a summary may say less than the live counts, so a sum just
	// over a cap can read just under it later.
	hit   string
	hitAt time.Duration // the clock when a time cap latched
}

func newLedger(limit config.Budget) *ledger {
	return &ledger{limit: limit, start: time.Now(), now: time.Now, live: map[*pairStat]usage{}}
}

// reachedLocked says whether a cap is reached, and latches the first.
func (l *ledger) reachedLocked() bool {
	if l.hit != "" {
		return true
	}
	u := l.usedLocked()
	switch {
	case l.limit.Cost > 0 && u.cost >= l.limit.Cost:
		l.hit = "cost"
	case l.limit.Tokens > 0 && u.tokens >= l.limit.Tokens:
		l.hit = "tokens"
	case l.limit.Tools > 0 && u.tools >= l.limit.Tools:
		l.hit = "tools"
	case l.limit.Time > 0:
		if el := l.now().Sub(l.start); el >= l.limit.Time {
			l.hit, l.hitAt = "time", el
		}
	}
	return l.hit != ""
}

// usedLocked is the use so far.
func (l *ledger) usedLocked() usage {
	u := l.finished
	for _, v := range l.live {
		u = u.plus(v)
	}
	return u
}

// admit starts a pair, or says no once a cap is reached; a pair told no is
// counted as not started.
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
	l.live[s] = usage{}
	return true
}

// add records a running pair's change in use, which may be negative: a
// harness that restates a total can restate it lower.
func (l *ledger) add(s *pairStat, d usage) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.live[s] = l.live[s].plus(d)
	l.reachedLocked()
}

// finish moves a pair from live to finished in one step: per kind, its
// summary's count when there is one, else what it reported live, so a step
// is never counted twice and a count seen is kept when the summary is
// missing.
func (l *ledger) finish(s *pairStat) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	u := l.live[s]
	if s.costKnown {
		u.cost = s.cost
	}
	if s.tokensKnown {
		u.tokens = s.tokensUsed
	}
	if s.toolsKnown {
		u.tools = s.toolsUsed
	}
	l.finished = l.finished.plus(u)
	delete(l.live, s)
	l.reachedLocked()
}

// reached says whether a run-wide cap is reached.
func (l *ledger) reached() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reachedLocked()
}

// totals is the use and the pairs not started so far.
func (l *ledger) totals() (used usage, notStarted int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.usedLocked(), l.notStarted
}

// reachedAt names the cap that was reached and where it stood: the sum now
// for a count, the clock when it latched for time.
func (l *ledger) reachedAt() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	u := l.usedLocked()
	switch l.hit {
	case "cost":
		return fmt.Sprintf("budget $%.2f reached at $%.2f", l.limit.Cost, u.cost)
	case "tokens":
		return fmt.Sprintf("budget tokens %d reached at %d", l.limit.Tokens, u.tokens)
	case "tools":
		return fmt.Sprintf("budget tools %d reached at %d", l.limit.Tools, u.tools)
	case "time":
		return fmt.Sprintf("budget time %s reached at %s", l.limit.Time, roundDur(l.hitAt))
	}
	return "budget " + l.limit.String() + " reached"
}

// usd prints a dollar amount to the cent, and one under a cent to its first
// two digits: a cap of a tenth of a cent is not $0.00.
func usd(x float64) string {
	if x <= 0 || x >= 0.01 {
		return fmt.Sprintf("$%.2f", x)
	}
	return fmt.Sprintf("$%.*f", 1-int(math.Floor(math.Log10(x))), x)
}

// estMark goes before an amount that is an estimate.
func estMark(estimated bool) string {
	if estimated {
		return "~"
	}
	return ""
}
