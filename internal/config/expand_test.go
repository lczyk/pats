package config

import (
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

func TestTaskTimeout(t *testing.T) {
	assert.Equal(t, Task{}.TimeoutDuration(), DefaultTimeout)               // unset -> 10m default
	assert.Equal(t, Task{Timeout: "10m"}.TimeoutDuration(), 10*time.Minute) // explicit
	assert.Equal(t, Task{Timeout: "0"}.TimeoutDuration(), time.Duration(0)) // 0 -> never timeout
}

// two suites: one picks whole vectors, the other single items. pairs union +
// dedupe across suites.
func suiteFixture(t *testing.T) *Config {
	t.Helper()
	return parseT(t, `
agents: {kind: agent, defaults: {harness: opencode-openrouter, model: m}, items: {a: ~, b: ~}}
tasks: {kind: task, defaults: {prompt: p}, items: {t1: ~, t2: ~}}
more: {kind: task, defaults: {prompt: p}, items: {t3: ~}}
scorers: {kind: scorer, defaults: {score: s.py}, items: {s1: ~, s2: ~}}
extra: {kind: scorer, defaults: {score: s.py}, items: {s3: ~}}
suites:
  kind: suite
  items:
    one: {agents: agents, tasks: tasks, scorers: scorers}
    two: {agents: a, tasks: t3, scorers: s3}
`)
}

func TestExpandTestPairs(t *testing.T) {
	pairs, err := suiteFixture(t).ExpandTestPairs()
	require.NoError(t, err)
	// suite one: 2x2, suite two: 1x1.
	assert.Len(t, pairs, 5)
	assert.Equal(t, pairs[0], TestPair{Agent: "a", Task: "t1"})
	assert.Equal(t, pairs[4], TestPair{Agent: "a", Task: "t3"})
}

func TestExpandScorePairs(t *testing.T) {
	pairs, err := suiteFixture(t).ExpandScorePairs()
	require.NoError(t, err)
	// suite one: 2 tasks x 2 scorers, suite two: 1x1.
	assert.Len(t, pairs, 5)
	assert.Equal(t, pairs[0], ScorePair{Task: "t1", Scorer: "s1"})
	assert.Equal(t, pairs[4], ScorePair{Task: "t3", Scorer: "s3"})
}

// an axis mixes vector and item ids; a vector id expands in file order.
func TestExpandMixedAxis(t *testing.T) {
	c := parseT(t, `
agents: {kind: agent, defaults: {harness: opencode-openrouter, model: m}, items: {a: ~}}
tasks: {kind: task, defaults: {prompt: p}, items: {t1: ~, t2: ~}}
more: {kind: task, defaults: {prompt: p}, items: {t3: ~, t4: ~}}
suites: {kind: suite, items: {s: {agents: a, tasks: [t4, tasks]}}}
`)
	pairs, err := c.ExpandTestPairs()
	require.NoError(t, err)
	assert.Len(t, pairs, 3)
	assert.Equal(t, pairs[0].Task, "t4")
	assert.Equal(t, pairs[1].Task, "t1")
	assert.Equal(t, pairs[2].Task, "t2")
}

// overlapping suites (smoke within full) dedupe rather than error.
func TestExpandOverlapDedupes(t *testing.T) {
	c := suiteFixture(t)
	c.Suites = append(c.Suites, Suite{
		Name: "smoke", Vector: "suites", Agents: StrList{"a"}, Tasks: StrList{"t1"}, Scorers: StrList{"s1"},
	})
	pairs, err := c.ExpandTestPairs()
	require.NoError(t, err)
	assert.Len(t, pairs, 5) // a x t1 already in suite one

	score, err := c.ExpandScorePairs()
	require.NoError(t, err)
	assert.Len(t, score, 5) // t1 x s1 already in suite one
}

func TestExpandSuiteFilter(t *testing.T) {
	c := suiteFixture(t)
	pairs, err := c.ExpandTestPairs("two")
	require.NoError(t, err)
	assert.Len(t, pairs, 1)
	assert.Equal(t, pairs[0], TestPair{Agent: "a", Task: "t3"})

	score, err := c.ExpandScorePairs("two")
	require.NoError(t, err)
	assert.Len(t, score, 1)

	// the suite vector selects every suite in it.
	pairs, err = c.ExpandTestPairs("suites")
	require.NoError(t, err)
	assert.Len(t, pairs, 5)

	// unknown suite id -> error (typo guard).
	_, err = c.ExpandTestPairs("nope")
	assert.Error(t, err, "no such suite")
}

func TestExpandErrors(t *testing.T) {
	// dangling refs.
	c := suiteFixture(t)
	c.Suites[0].Tasks = StrList{"t1", "ghost"}
	_, err := c.ExpandTestPairs()
	assert.Error(t, err, "unknown task")

	// an item named twice, once via its vector.
	c = suiteFixture(t)
	c.Suites[0].Agents = StrList{"agents", "a"}
	_, err = c.ExpandTestPairs()
	assert.Error(t, err, "duplicate agent \"a\"")
}

func TestOne(t *testing.T) {
	c := suiteFixture(t)
	id, err := c.One("task", "t3")
	require.NoError(t, err)
	assert.Equal(t, id, "t3")
	id, err = c.One("task", "more") // vector of one
	require.NoError(t, err)
	assert.Equal(t, id, "t3")
	_, err = c.One("task", "tasks")
	assert.Error(t, err, "names 2 items")
	_, err = c.One("task", "ghost")
	assert.Error(t, err, "unknown task")
}

func TestFilterPairs(t *testing.T) {
	c := suiteFixture(t)
	pairs, err := c.ExpandTestPairs()
	require.NoError(t, err)
	// a x t1, a x t2, b x t1, b x t2, a x t3.

	// no filter -> all.
	got, err := c.FilterPairs(pairs, nil, nil)
	require.NoError(t, err)
	assert.Len(t, got, 5)

	// agent filter.
	got, err = c.FilterPairs(pairs, []string{"b"}, nil)
	require.NoError(t, err)
	assert.Len(t, got, 2)

	// agent + task -> single pair.
	got, err = c.FilterPairs(pairs, []string{"b"}, []string{"t2"})
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Equal(t, got[0].Agent, "b")
	assert.Equal(t, got[0].Task, "t2")

	// a vector id filters to its items.
	got, err = c.FilterPairs(pairs, nil, []string{"tasks"})
	require.NoError(t, err)
	assert.Len(t, got, 4)

	// unknown filter value -> error (typo guard), naming the flag and the kind.
	_, err = c.FilterPairs(pairs, []string{"nope"}, nil)
	assert.Error(t, err, `--agent "nope": no such agent in the selected suites`)
	_, err = c.FilterPairs(pairs, nil, []string{"nope"})
	assert.Error(t, err, `--task "nope": no such task in the selected suites`)
}

func TestFilterScorePairs(t *testing.T) {
	c := suiteFixture(t)
	pairs, err := c.ExpandScorePairs()
	require.NoError(t, err)
	// t1 x s1, t1 x s2, t2 x s1, t2 x s2, t3 x s3.

	got, err := c.FilterScorePairs(pairs, nil)
	require.NoError(t, err)
	assert.Len(t, got, 5)

	got, err = c.FilterScorePairs(pairs, []string{"s3"})
	require.NoError(t, err)
	assert.EqualArrays(t, got, []ScorePair{{Task: "t3", Scorer: "s3"}})

	// a vector id filters to its items.
	got, err = c.FilterScorePairs(pairs, []string{"scorers"})
	require.NoError(t, err)
	assert.Len(t, got, 4)

	_, err = c.FilterScorePairs(pairs, []string{"nope"})
	assert.Error(t, err, `--scorer "nope": no such scorer in the selected suites`)

	// a scorer that exists but is in no selected suite is the same typo guard.
	only, err := c.ExpandScorePairs("one")
	require.NoError(t, err)
	_, err = c.FilterScorePairs(only, []string{"s3"})
	assert.Error(t, err, `--scorer "s3": no such scorer in the selected suites`)
}

// a manual suite runs only when named: a bare run takes the others.
func TestRunSuitesManual(t *testing.T) {
	c := parseT(t, `
sxx: {kind: sandbox, items: {sxx: {image: img}}}
agents: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m}, bxx: {harness: opencode-openrouter, model: m}}}
txx: {kind: task, items: {txx: {prompt: p}}}
core: {kind: suite, items: {core: {agents: axx, tasks: txx}}}
extra: {kind: suite, defaults: {manual: true}, items: {abl: {agents: bxx, tasks: txx}}}
`)
	require.NoError(t, c.Validate())
	got, err := c.RunSuites(nil)
	require.NoError(t, err)
	assert.EqualArrays(t, got, []string{"core"})
	got, err = c.RunSuites([]string{"extra"})
	require.NoError(t, err)
	assert.EqualArrays(t, got, []string{"extra"})
	assert.EqualArrays(t, c.ManualSuitesWith([]string{"bxx"}, nil), []string{"abl"})
	assert.Len(t, c.ManualSuitesWith([]string{"axx"}, nil), 0)

	all := parseT(t, `
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m}}}
txx: {kind: task, items: {txx: {prompt: p}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx, manual: true}}}
`)
	_, err = all.RunSuites(nil)
	assert.Error(t, err, "every suite is manual")
}
