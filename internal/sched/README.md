# sched

a longest-first task scheduler. it knows nothing of pats: tasks are a key, a group, a round, an index and a limit,
with a value of the caller's riding along. `internal/eval` uses it to start the pairs of `pats run` (see
`docs/spec/repeats.md`, "dispatch order"), but nothing here depends on that, and it could move out as a module of its
own without change.

## what it does

it holds a queue of tasks and an estimate per key of how long that key's tasks take, and says which task to start
next. the order:

1. a key with no estimate goes first. it may be the longest, and starting it learns its length soonest.
2. then the longest estimate.
3. then the lower round, then the lower index.

a key's estimate is the median of the durations its tasks reported here, else of the durations it was primed with. a
task of the key still running raises it to at least how long that task has run, and a task's limit caps it. every task
of a key shares the estimate, so of two tasks differing only in round the lower always starts first.

it also keeps count of what runs: at most `Slots` at once, and at most `Caps[group]` of a group. a task waiting on its
group's cap does not hold back a later task of another group.

## what it does not do

it starts nothing and times nothing. it has no clock: every call that ranks takes the caller's `now`, and a task's
duration is whatever the caller reports. so a test drives it with a fake clock and made-up durations, and the caller
decides what counts as a duration (pats reports the agent's run, not its setup).

## use

```go
s := sched.New[string, job](sched.Options{Slots: 4, Caps: map[string]int{"gpu": 1}})
s.Prime("build", 9*time.Minute, 11*time.Minute) // durations from before, if any
s.Add(&sched.Task[string, job]{Key: "build", Group: "gpu", Round: 1, Index: 0, Limit: 20 * time.Minute, Value: j})

for t := s.Peek(now()); t != nil; t = s.Peek(now()) {
	s.Start(t, now())
	go run(t) // which ends with s.Done(t, took, true)
}
```

- **`Peek(now)`** is the task to start next, nil when no slot is free, nothing is queued, or every queued task waits on
  its group's cap. it stays queued, so a caller that decides not to start it (a budget said no) just stops asking.
- **`Start(t, at)`** takes it off the queue as running since `at`.
- **`Done(t, took, ok)`** ends it. with `ok`, `took` is a duration of its key, and the key's durations from here replace
  what it was primed with. without, it only frees the slot (a task that failed before doing its work).
- **`Drop(now)`** empties the queue and returns it in order, for a caller that stops.
- **`Estimate(t, now)`**, **`Primed(key)`**, **`Queued()`** and **`Running()`** say where things stand.

it is safe to call from several goroutines, though the order only means something when one goroutine drives it, as
pats's dispatcher does.

## tests

`sched_test.go` drives it on a fake clock: the order, medians, priming replaced by durations seen, the running floor,
limits, slots, group caps, stray calls, a simulator that runs tasks to their end (learning the order, and ending sooner
than round by round), a randomised check that rounds always start in order, and concurrent use under `-race`.
