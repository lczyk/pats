// Package sched orders tasks so the longest start first.
//
// a Scheduler holds a queue of tasks and a running estimate of how long
// the tasks of each key take. it starts nothing and times nothing: its
// caller asks for the next task to start (Peek), says it started it and
// when (Start), and reports it ended with how long it took (Done). every
// call that ranks takes the caller's now, so the caller owns the clock. the order is longest first, so a long task does not start
// last and run alone at the tail while the other slots sit idle:
//
//   - a key with no estimate (nothing primed, nothing ended, nothing
//     running) goes first: it may be the longest, and starting it learns
//     its length soonest.
//   - then the longest estimate. a key's estimate is the median of the
//     durations its tasks reported here, else of those it was primed with;
//     a task of it still running raises it to at least how long that task
//     has run; and a task's limit caps it.
//   - then the lower round, then the lower index.
//
// every task of a key shares its estimate, so of two tasks differing only
// in round the lower always starts first.
//
// a Scheduler is safe for use from several goroutines, though its order is
// only meaningful when one goroutine drives it.
package sched

import (
	"slices"
	"sort"
	"sync"
	"time"
)

// Task is a unit of work waiting to start, or running.
type Task[K comparable, V any] struct {
	Key   K             // tasks of one key share an estimate
	Group string        // the task counts against this group's cap; "" is none
	Round int           // lower first, among equal estimates
	Index int           // lower first, among equal estimates and rounds
	Limit time.Duration // the longest it can run, a cap on its estimate; 0 is none
	Value V             // the caller's

	started time.Time
}

// Options configures a Scheduler.
type Options struct {
	// Slots is how many tasks may run at once; below 1 is one.
	Slots int
	// Caps is how many tasks of a group may run at once; a group not in it
	// has no cap of its own.
	Caps map[string]int
}

// Scheduler is a queue of tasks in longest-first order.
type Scheduler[K comparable, V any] struct {
	mu      sync.Mutex
	opts    Options
	queue   []*Task[K, V]
	running map[*Task[K, V]]bool
	inGroup map[string]int
	primed  map[K][]time.Duration
	seen    map[K][]time.Duration
}

// New is an empty Scheduler.
func New[K comparable, V any](opts Options) *Scheduler[K, V] {
	if opts.Slots < 1 {
		opts.Slots = 1
	}
	return &Scheduler[K, V]{
		opts: opts, running: map[*Task[K, V]]bool{}, inGroup: map[string]int{},
		primed: map[K][]time.Duration{}, seen: map[K][]time.Duration{},
	}
}

// Prime gives key durations from before, which its estimate uses until a
// task of it reports one here.
func (s *Scheduler[K, V]) Prime(key K, durations ...time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.primed[key] = append(s.primed[key], durations...)
}

// Add queues tasks. a task already queued or running is not queued again.
func (s *Scheduler[K, V]) Add(tasks ...*Task[K, V]) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range tasks {
		if t != nil && !s.running[t] && !slices.Contains(s.queue, t) {
			s.queue = append(s.queue, t)
		}
	}
}

// Peek is the task to start next at now: the first queued in order with a
// free slot and room in its group. nil when every slot is taken, the queue
// is empty, or every queued task waits on its group's cap. it stays queued
// until Start.
func (s *Scheduler[K, V]) Peek(now time.Time) *Task[K, V] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.running) >= s.opts.Slots {
		return nil
	}
	s.sortLocked(now)
	for _, t := range s.queue {
		if s.roomLocked(t) {
			return t
		}
	}
	return nil
}

// roomLocked says whether t's group has room for one more.
func (s *Scheduler[K, V]) roomLocked(t *Task[K, V]) bool {
	c, capped := s.opts.Caps[t.Group]
	return !capped || s.inGroup[t.Group] < c
}

// Start takes t off the queue as running since at, and says whether it
// did: not when t is not queued, every slot is taken, or its group is at its
// cap. the caller starts t only on true.
func (s *Scheduler[K, V]) Start(t *Task[K, V], at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.Index(s.queue, t)
	if i < 0 || len(s.running) >= s.opts.Slots || !s.roomLocked(t) {
		return false
	}
	s.queue = slices.Delete(s.queue, i, i+1)
	t.started = at
	s.running[t] = true
	s.inGroup[t.Group]++
	return true
}

// Done ends a running task. when ok, took is a duration of its key, which
// replaces what it was primed with.
func (s *Scheduler[K, V]) Done(t *Task[K, V], took time.Duration, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running[t] {
		return
	}
	delete(s.running, t)
	s.inGroup[t.Group]--
	if ok {
		s.seen[t.Key] = append(s.seen[t.Key], took)
	}
}

// Drop empties the queue and returns what was in it, in its order at now.
func (s *Scheduler[K, V]) Drop(now time.Time) []*Task[K, V] {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sortLocked(now)
	q := s.queue
	s.queue = nil
	return q
}

// Queued is how many tasks wait; Running how many run.
func (s *Scheduler[K, V]) Queued() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

func (s *Scheduler[K, V]) Running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.running)
}

// Primed says whether key was primed with any duration.
func (s *Scheduler[K, V]) Primed(key K) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.primed[key]) > 0
}

// Estimate is how long t is expected to run, as of now, and whether there
// is an estimate at all.
func (s *Scheduler[K, V]) Estimate(t *Task[K, V], now time.Time) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.estimateLocked(t, now)
}

func (s *Scheduler[K, V]) estimateLocked(t *Task[K, V], now time.Time) (time.Duration, bool) {
	var est time.Duration
	known := false
	if xs := s.seen[t.Key]; len(xs) > 0 {
		est, known = median(xs), true
	} else if xs := s.primed[t.Key]; len(xs) > 0 {
		est, known = median(xs), true
	}
	for r := range s.running {
		if r.Key == t.Key {
			est, known = max(est, now.Sub(r.started)), true
		}
	}
	if t.Limit > 0 {
		est = min(est, t.Limit)
	}
	return est, known
}

// sortLocked puts the queue in order at now.
func (s *Scheduler[K, V]) sortLocked(now time.Time) {
	type rank struct {
		est   time.Duration
		known bool
	}
	r := make(map[*Task[K, V]]rank, len(s.queue))
	for _, t := range s.queue {
		est, known := s.estimateLocked(t, now)
		r[t] = rank{est, known}
	}
	sort.SliceStable(s.queue, func(i, j int) bool {
		a, b := s.queue[i], s.queue[j]
		ra, rb := r[a], r[b]
		switch {
		case ra.known != rb.known:
			return !ra.known
		case ra.est != rb.est:
			return ra.est > rb.est
		case a.Round != b.Round:
			return a.Round < b.Round
		}
		return a.Index < b.Index
	})
}

func median(xs []time.Duration) time.Duration {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
