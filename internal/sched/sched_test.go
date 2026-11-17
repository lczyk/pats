package sched

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// clock is a fake clock the tests move by hand.
type clock struct{ t time.Time }

func (c *clock) now() time.Time           { return c.t }
func (c *clock) add(d time.Duration)      { c.t = c.t.Add(d) }
func newClock() *clock                    { return &clock{time.Unix(1000, 0)} }
func name(t *Task[string, string]) string { return fmt.Sprintf("%s%d", t.Key, t.Round) }

// tasks is keys x rounds, the keys in index order.
func tasks(rounds int, limit time.Duration, keys ...string) []*Task[string, string] {
	var out []*Task[string, string]
	for r := 1; r <= rounds; r++ {
		for i, k := range keys {
			out = append(out, &Task[string, string]{Key: k, Round: r, Index: i, Limit: limit})
		}
	}
	return out
}

// order drops the queue to see its order.
func order(s *Scheduler[string, string], c *clock) []string {
	var out []string
	for _, t := range s.Drop(c.now()) {
		out = append(out, name(t))
	}
	return out
}

func newSched(slots int) *Scheduler[string, string] {
	return New[string, string](Options{Slots: slots})
}

func TestEmpty(t *testing.T) {
	c := newClock()
	s := newSched(2)
	assert.That(t, s.Peek(c.now()) == nil, "peeked a task from nothing")
	assert.Equal(t, s.Queued(), 0)
	assert.Equal(t, s.Running(), 0)
	assert.Len(t, s.Drop(c.now()), 0)
}

// unknown first, then the longest, then the round, then the index.
func TestOrder(t *testing.T) {
	c := newClock()
	s := newSched(1)
	s.Prime("x", 10*time.Minute)
	s.Prime("y", time.Minute, 50*time.Second, 70*time.Second)
	s.Add(tasks(2, 0, "y", "x", "z")...)
	assert.EqualArrays(t, order(s, c), []string{"z1", "z2", "x1", "x2", "y1", "y2"})
	assert.That(t, s.Primed("x") && !s.Primed("z"), "primed")
}

// with no estimate anywhere, rounds then indexes.
func TestOrderWithNothingKnown(t *testing.T) {
	c := newClock()
	s := newSched(1)
	s.Add(tasks(2, 0, "b", "a")...)
	assert.EqualArrays(t, order(s, c), []string{"b1", "a1", "b2", "a2"})
}

// a key's median, of an even count too.
func TestMedian(t *testing.T) {
	c := newClock()
	s := newSched(1)
	s.Prime("x", 1*time.Second, 9*time.Second, 3*time.Second, 5*time.Second)
	tk := &Task[string, string]{Key: "x"}
	est, ok := s.Estimate(tk, c.now())
	assert.That(t, ok, "no estimate")
	assert.Equal(t, est, 4*time.Second)
}

// a duration reported here replaces what the key was primed with.
func TestDoneReplacesPrime(t *testing.T) {
	c := newClock()
	s := newSched(1)
	s.Prime("x", 10*time.Minute)
	s.Prime("y", time.Minute)
	s.Add(tasks(2, 0, "x", "y")...)
	x1 := s.Peek(c.now())
	assert.Equal(t, name(x1), "x1")
	s.Start(x1, c.now())
	s.Done(x1, 10*time.Second, true)
	assert.EqualArrays(t, order(s, c), []string{"y1", "y2", "x2"})
}

// a task ended without a duration is no sample.
func TestDoneNotOK(t *testing.T) {
	c := newClock()
	s := newSched(1)
	s.Prime("x", 10*time.Minute)
	s.Prime("y", time.Minute)
	s.Add(tasks(2, 0, "x", "y")...)
	x1 := s.Peek(c.now())
	s.Start(x1, c.now())
	s.Done(x1, time.Second, false)
	assert.EqualArrays(t, order(s, c), []string{"x2", "y1", "y2"})
}

// a running task raises its key to how long it has run; a key whose only
// task runs is no longer unknown.
func TestRunningFloor(t *testing.T) {
	c := newClock()
	s := newSched(3)
	s.Prime("x", 10*time.Minute)
	s.Add(tasks(2, 0, "y", "z")...)
	s.Add(&Task[string, string]{Key: "x", Round: 1, Index: 2})
	y1 := s.Peek(c.now())
	assert.Equal(t, name(y1), "y1")
	s.Start(y1, c.now())
	z1 := s.Peek(c.now())
	assert.Equal(t, name(z1), "z1")
	s.Start(z1, c.now())
	c.add(20 * time.Minute)
	// y2 and z2 are no longer unknown: their tasks have run 20m, past x's 10m.
	assert.EqualArrays(t, order(s, c), []string{"y2", "z2", "x1"})

	est, ok := s.Estimate(&Task[string, string]{Key: "y"}, c.now())
	assert.That(t, ok, "no estimate")
	assert.Equal(t, est, 20*time.Minute)
	// of two running, the longer counts.
	s.Add(&Task[string, string]{Key: "y", Round: 2})
	y2 := s.Peek(c.now())
	s.Start(y2, c.now())
	c.add(time.Minute)
	est, _ = s.Estimate(&Task[string, string]{Key: "y"}, c.now())
	assert.Equal(t, est, 21*time.Minute)
}

// a limit caps the estimate; no limit is no cap.
func TestLimit(t *testing.T) {
	c := newClock()
	s := newSched(1)
	s.Prime("x", 10*time.Minute)
	s.Prime("y", 5*time.Minute)
	s.Add(tasks(1, 5*time.Minute, "y", "x")...)
	assert.EqualArrays(t, order(s, c), []string{"y1", "x1"}) // a tie at 5m: the index
	s.Add(tasks(1, 0, "y", "x")...)
	assert.EqualArrays(t, order(s, c), []string{"x1", "y1"})
}

// a full set of slots starts nothing until a task ends.
func TestSlots(t *testing.T) {
	c := newClock()
	s := newSched(2)
	s.Add(tasks(1, 0, "a", "b", "c")...)
	a, b := s.Peek(c.now()), (*Task[string, string])(nil)
	s.Start(a, c.now())
	b = s.Peek(c.now())
	s.Start(b, c.now())
	assert.That(t, s.Peek(c.now()) == nil, "a third slot")
	assert.Equal(t, s.Running(), 2)
	assert.Equal(t, s.Queued(), 1)
	s.Done(a, time.Second, true)
	assert.Equal(t, name(s.Peek(c.now())), "c1")
}

// Slots below 1 is one.
func TestSlotsDefault(t *testing.T) {
	c := newClock()
	s := New[string, string](Options{})
	s.Add(tasks(1, 0, "a", "b")...)
	s.Start(s.Peek(c.now()), c.now())
	assert.That(t, s.Peek(c.now()) == nil, "a second slot")
}

// a task waiting on its group's cap lets a later one of another group
// start; the cap holds across rounds.
func TestGroupCaps(t *testing.T) {
	c := newClock()
	s := New[string, string](Options{Slots: 3, Caps: map[string]int{"g": 1}})
	for r := 1; r <= 2; r++ {
		s.Add(&Task[string, string]{Key: "x", Group: "g", Round: r, Index: 0},
			&Task[string, string]{Key: "y", Group: "g", Round: r, Index: 1},
			&Task[string, string]{Key: "z", Group: "h", Round: r, Index: 2})
	}
	var started []string
	for p := s.Peek(c.now()); p != nil; p = s.Peek(c.now()) {
		s.Start(p, c.now())
		started = append(started, name(p))
	}
	// x1 holds g; z1 and then z2 (z is running, so no longer unknown,
	// but every other key waits on g) take the other slots.
	assert.EqualArrays(t, started, []string{"x1", "z1", "z2"})
}

// every queued task waiting on its group's cap: nothing to start.
func TestGroupCapsAllWaiting(t *testing.T) {
	c := newClock()
	s := New[string, string](Options{Slots: 4, Caps: map[string]int{"g": 1}})
	s.Add(&Task[string, string]{Key: "x", Group: "g"}, &Task[string, string]{Key: "y", Group: "g", Index: 1})
	x := s.Peek(c.now())
	s.Start(x, c.now())
	assert.That(t, s.Peek(c.now()) == nil, "past the group's cap")
	s.Done(x, 0, false)
	assert.Equal(t, s.Peek(c.now()).Key, "y")
}

// Start of a task not queued, or Done of one not running, does nothing.
func TestStrayCalls(t *testing.T) {
	c := newClock()
	s := newSched(1)
	stray := &Task[string, string]{Key: "x"}
	s.Start(stray, c.now())
	assert.Equal(t, s.Running(), 0)
	s.Done(stray, time.Second, true)
	_, ok := s.Estimate(stray, c.now())
	assert.That(t, !ok, "a stray Done made a sample")
	s.Add(stray)
	s.Start(stray, c.now())
	s.Done(stray, time.Second, true)
	s.Done(stray, time.Hour, true) // twice: once counted
	est, _ := s.Estimate(stray, c.now())
	assert.Equal(t, est, time.Second)
}

// sim runs keys x rounds on slots with the fake clock, each task taking
// durs[key], and says the start order and when the last ended.
func sim(t *testing.T, s *Scheduler[string, string], c *clock, durs map[string]time.Duration) ([]string, time.Duration) {
	t.Helper()
	begin := c.now()
	type run struct {
		t   *Task[string, string]
		end time.Time
	}
	var running []run
	var started []string
	for s.Queued() > 0 || len(running) > 0 {
		for p := s.Peek(c.now()); p != nil; p = s.Peek(c.now()) {
			s.Start(p, c.now())
			started = append(started, name(p))
			running = append(running, run{p, c.now().Add(durs[p.Key])})
		}
		require.That(t, len(running) > 0, "nothing running with %d queued", s.Queued())
		// end the first to end (ties: the earlier started).
		first := 0
		for i, r := range running {
			if r.end.Before(running[first].end) {
				first = i
			}
		}
		r := running[first]
		running = append(running[:first], running[first+1:]...)
		c.t = r.end
		s.Done(r.t, durs[r.t.Key], true)
	}
	return started, c.now().Sub(begin)
}

// with nothing known and one slot, round 1 in index order, then what was
// learned ranks the rest.
func TestLearns(t *testing.T) {
	c := newClock()
	s := newSched(1)
	s.Add(tasks(3, 0, "x", "y")...)
	started, _ := sim(t, s, c, map[string]time.Duration{"x": 10 * time.Minute, "y": time.Minute})
	assert.EqualArrays(t, started, []string{"x1", "y1", "x2", "x3", "y2", "y3"})
}

// longest first ends sooner than round by round with a barrier between
// rounds: one long task among short ones, two slots, three rounds.
func TestShorterThanRoundByRound(t *testing.T) {
	durs := map[string]time.Duration{"long": 10 * time.Minute, "a": time.Minute, "b": time.Minute, "c": time.Minute}
	c := newClock()
	s := newSched(2)
	for k, d := range durs {
		s.Prime(k, d)
	}
	s.Add(tasks(3, 0, "a", "b", "c", "long")...)
	started, took := sim(t, s, c, durs)
	assert.EqualArrays(t, started[:2], []string{"long1", "long2"})

	// round by round, each round its own scheduler, waiting for its last.
	var barrier time.Duration
	for range 3 {
		c := newClock()
		r := newSched(2)
		r.Add(tasks(1, 0, "a", "b", "c", "long")...)
		_, d := sim(t, r, c, durs)
		barrier += d
	}
	assert.That(t, took < barrier, "longest first took %s, round by round %s", took, barrier)
	assert.Equal(t, took, 20*time.Minute)
}

// a task never starts before the task of its key a round earlier, whatever
// the durations, slots and caps: the order of first starts follows rounds.
func TestRoundsInOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	keys := []string{"a", "b", "c", "d", "e"}
	for trial := range 200 {
		c := newClock()
		caps := map[string]int{"g": 1 + rng.Intn(2)}
		s := New[string, string](Options{Slots: 1 + rng.Intn(4), Caps: caps})
		durs := map[string]time.Duration{}
		for i, k := range keys {
			durs[k] = time.Duration(1+rng.Intn(20)) * time.Minute
			if rng.Intn(2) == 0 {
				s.Prime(k, time.Duration(1+rng.Intn(20))*time.Minute)
			}
			group := ""
			if rng.Intn(2) == 0 {
				group = "g"
			}
			for r := 1; r <= 3; r++ {
				s.Add(&Task[string, string]{Key: k, Group: group, Round: r, Index: i})
			}
		}
		started, _ := sim(t, s, c, durs)
		require.Len(t, started, 15)
		last := map[string]int{}
		firstOfRound := map[int]int{}
		for i, n := range started {
			k, r := n[:1], int(n[1]-'0')
			require.That(t, r == last[k]+1, "trial %d: %s after round %d of %s: %v", trial, n, last[k], k, started)
			last[k] = r
			if _, ok := firstOfRound[r]; !ok {
				firstOfRound[r] = i
			}
		}
		require.That(t, firstOfRound[1] < firstOfRound[2] && firstOfRound[2] < firstOfRound[3], "trial %d: rounds began out of order: %v", trial, started)
	}
}

// the value rides along.
func TestValue(t *testing.T) {
	c := newClock()
	s := New[string, *int](Options{})
	v := 7
	s.Add(&Task[string, *int]{Key: "x", Value: &v})
	assert.Equal(t, *s.Peek(c.now()).Value, 7)
}

// several goroutines at once: nothing lost, no slot overrun.
func TestConcurrentUse(t *testing.T) {
	c := newClock()
	s := New[string, string](Options{Slots: 3})
	for i := range 200 {
		s.Add(&Task[string, string]{Key: fmt.Sprint(i % 7), Index: i})
	}
	var mu sync.Mutex
	most, now, done := 0, 0, 0
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for {
				mu.Lock()
				p := s.Peek(c.now())
				if p == nil {
					q := s.Queued()
					mu.Unlock()
					if q == 0 {
						return
					}
					time.Sleep(time.Microsecond)
					continue
				}
				s.Start(p, c.now())
				now++
				most = max(most, now)
				mu.Unlock()
				time.Sleep(time.Microsecond)
				mu.Lock()
				now--
				done++
				s.Done(p, time.Microsecond, true)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	assert.Equal(t, done, 200)
	assert.That(t, most <= 3, "%d ran at once", most)
}
