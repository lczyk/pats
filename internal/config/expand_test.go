package config

import (
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

func TestTaskTimeout(t *testing.T) {
	assert.Equal(t, Task{}.TimeoutDuration(), DefaultTimeout)               // unset -> 5m default
	assert.Equal(t, Task{Timeout: "10m"}.TimeoutDuration(), 10*time.Minute) // explicit
	assert.Equal(t, Task{Timeout: "0"}.TimeoutDuration(), time.Duration(0)) // 0 -> never timeout
}

// two suites sharing an agent list; pairs union + dedupe across suites.
func suiteFixture() *Config {
	return &Config{
		Agents:  []Agent{{ID: "a"}, {ID: "b"}},
		Tasks:   []Task{{ID: "t1"}, {ID: "t2"}, {ID: "t3"}},
		Scorers: []Scorer{{ID: "s1"}, {ID: "s2"}, {ID: "s3"}},
		Suites: []Suite{
			{ID: "one", Agents: StrList{"a", "b"}, Tasks: StrList{"t1", "t2"}, Scorers: StrList{"s1", "s2"}},
			{ID: "two", Agents: StrList{"a"}, Tasks: StrList{"t3"}, Scorers: StrList{"s3"}},
		},
	}
}

func TestExpandTestPairs(t *testing.T) {
	c := suiteFixture()
	pairs, err := c.ExpandTestPairs()
	require.NoError(t, err)
	// suite one: 2x2, suite two: 1x1.
	assert.Len(t, pairs, 5)
	assert.Equal(t, pairs[0], TestPair{Agent: "a", Task: "t1"})
	assert.Equal(t, pairs[4], TestPair{Agent: "a", Task: "t3"})
}

func TestExpandScorePairs(t *testing.T) {
	c := suiteFixture()
	pairs, err := c.ExpandScorePairs()
	require.NoError(t, err)
	// suite one: 2 tasks x 2 scorers, suite two: 1x1.
	assert.Len(t, pairs, 5)
	assert.Equal(t, pairs[0], ScorePair{Task: "t1", Scorer: "s1"})
	assert.Equal(t, pairs[4], ScorePair{Task: "t3", Scorer: "s3"})
}

// overlapping suites (smoke within full) dedupe rather than error.
func TestExpandOverlapDedupes(t *testing.T) {
	c := suiteFixture()
	c.Suites = append(c.Suites, Suite{
		ID: "smoke", Agents: StrList{"a"}, Tasks: StrList{"t1"}, Scorers: StrList{"s1"},
	})
	pairs, err := c.ExpandTestPairs()
	require.NoError(t, err)
	assert.Len(t, pairs, 5) // a x t1 already in suite one

	score, err := c.ExpandScorePairs()
	require.NoError(t, err)
	assert.Len(t, score, 5) // t1 x s1 already in suite one
}

func TestExpandSuiteFilter(t *testing.T) {
	c := suiteFixture()
	pairs, err := c.ExpandTestPairs("two")
	require.NoError(t, err)
	assert.Len(t, pairs, 1)
	assert.Equal(t, pairs[0], TestPair{Agent: "a", Task: "t3"})

	score, err := c.ExpandScorePairs("two")
	require.NoError(t, err)
	assert.Len(t, score, 1)

	// unknown suite id -> error (typo guard).
	_, err = c.ExpandTestPairs("nope")
	assert.Error(t, err, "no such suite")
}

func TestExpandErrors(t *testing.T) {
	// dangling refs.
	c := suiteFixture()
	c.Suites[0].Tasks = StrList{"t1", "ghost"}
	_, err := c.ExpandTestPairs()
	assert.Error(t, err, "unknown task")

	// duplicate id within one suite's list.
	c = suiteFixture()
	c.Suites[0].Agents = StrList{"a", "a"}
	_, err = c.ExpandTestPairs()
	assert.Error(t, err, "duplicate agent")
}

func TestFilterPairs(t *testing.T) {
	pairs := []TestPair{
		{Agent: "a", Task: "t1"}, {Agent: "a", Task: "t2"},
		{Agent: "b", Task: "t1"}, {Agent: "b", Task: "t2"},
	}
	// no filter -> all.
	got, err := FilterPairs(pairs, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, len(got), 4)

	// agent filter.
	got, err = FilterPairs(pairs, []string{"a"}, nil)
	require.NoError(t, err)
	assert.Equal(t, len(got), 2)

	// agent + task -> single pair.
	got, err = FilterPairs(pairs, []string{"b"}, []string{"t2"})
	require.NoError(t, err)
	assert.Equal(t, len(got), 1)
	assert.Equal(t, got[0].Agent, "b")
	assert.Equal(t, got[0].Task, "t2")

	// unknown filter value -> error (typo guard).
	_, err = FilterPairs(pairs, []string{"nope"}, nil)
	assert.Error(t, err, assert.AnyError)
}
