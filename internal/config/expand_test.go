package config

import (
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

func TestTaskTimeout(t *testing.T) {
	assert.Equal(t, Task{}.TimeoutDuration(), DefaultTimeout)                 // unset -> 5m default
	assert.Equal(t, Task{Timeout: "10m"}.TimeoutDuration(), 10*time.Minute)  // explicit
	assert.Equal(t, Task{Timeout: "0"}.TimeoutDuration(), time.Duration(0))  // 0 -> never timeout
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
