package eval

import (
	"sort"
	"time"

	"github.com/lczyk/pats/internal/config"
)

// the dispatcher's order (docs/spec/repeats.md, "dispatch order"): the
// pairs it expects to run longest start first, so a long one does not start
// last and run alone while the other slots sit idle.

// queued is a pair of a repeat waiting in the dispatcher's queue.
type queued struct {
	rep, idx int // its repeat (1-based) and its place in the selection
	pair     config.TestPair
	key      historyKey
	sandbox  string        // the sandbox it runs in, for max_parallel
	limit    time.Duration // the agent's effective timeout; 0 is none
}

// estimates is how long the pairs of each key are expected to run.
type estimates struct {
	hist    map[historyKey][]float64 // past runs' durations, seconds
	seen    map[historyKey][]float64 // this invocation's, seconds
	running map[historyKey][]time.Time
}

func newEstimates(hist map[historyKey]*pastPairs) *estimates {
	e := &estimates{hist: map[historyKey][]float64{}, seen: map[historyKey][]float64{}, running: map[historyKey][]time.Time{}}
	for k, p := range hist {
		if len(p.durations) > 0 {
			e.hist[k] = p.durations
		}
	}
	return e
}

// start notes a pair of k running since at.
func (e *estimates) start(k historyKey, at time.Time) {
	e.running[k] = append(e.running[k], at)
}

// end notes the pair of k that started at as ended, and its duration as a
// sample when it ran the agent (any status but error).
func (e *estimates) end(k historyKey, at time.Time, status string, dur float64) {
	rs := e.running[k]
	for i, t := range rs {
		if t.Equal(at) {
			e.running[k] = append(rs[:i:i], rs[i+1:]...)
			break
		}
	}
	if status != "" && status != "error" {
		e.seen[k] = append(e.seen[k], dur)
	}
}

// of is q's estimate at now, and whether there is one: this invocation's
// median, else history's, raised to what its running pairs have run, capped
// at its timeout.
func (e *estimates) of(q *queued, now time.Time) (time.Duration, bool) {
	var est time.Duration
	known := false
	if xs := e.seen[q.key]; len(xs) > 0 {
		est, known = secs(median(xs)), true
	} else if xs := e.hist[q.key]; len(xs) > 0 {
		est, known = secs(median(xs)), true
	}
	for _, t := range e.running[q.key] {
		est, known = max(est, now.Sub(t)), true
	}
	if q.limit > 0 {
		est = min(est, q.limit)
	}
	return est, known
}

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// order sorts the queue into dispatch order: unknown first, then the
// longest estimate, then the earlier repeat, then the selection's order.
func (e *estimates) order(queue []*queued, now time.Time) {
	type ranked struct {
		est   time.Duration
		known bool
	}
	r := make(map[*queued]ranked, len(queue))
	for _, q := range queue {
		est, known := e.of(q, now)
		r[q] = ranked{est, known}
	}
	sort.SliceStable(queue, func(i, j int) bool {
		a, b := r[queue[i]], r[queue[j]]
		switch {
		case a.known != b.known:
			return !a.known
		case a.est != b.est:
			return a.est > b.est
		case queue[i].rep != queue[j].rep:
			return queue[i].rep < queue[j].rep
		}
		return queue[i].idx < queue[j].idx
	})
}

// effectiveTimeout is how long the agent may run: the task's timeout,
// shortened by the agent's time budget; 0 is no limit.
func effectiveTimeout(a config.Agent, t config.Task) time.Duration {
	tt := t.TimeoutDuration()
	if bt := a.Budget.Time; bt > 0 && (tt == 0 || bt < tt) {
		return bt
	}
	return tt
}
