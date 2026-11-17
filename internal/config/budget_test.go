package config

import (
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// a number is the cost; a mapping merges over the defaults key by key, a
// number on either side read as {cost: number}, and a null unsets a kind or
// the lot.
func TestParseBudgetMerges(t *testing.T) {
	c := parseT(t, `
vxx:
  kind: agent
  defaults:
    harness: opencode-openrouter
    model: m
    budget:
      cost: 2
      tools: 300
  items:
    keeps: ~
    over:
      budget:
        cost: 0.5
        tokens: 2M
        time: 5m
    bare:
      budget: 1
    unset:
      budget:
        tools: ~
    none:
      budget: ~
nxx:
  kind: agent
  defaults:
    harness: opencode-openrouter
    model: m
    budget: 3
  items:
    plain: ~
    adds:
      budget:
        tokens: 20k
    empty:
      budget: {}
`)
	got := map[string]Budget{}
	for _, a := range c.Agents {
		got[a.Name] = a.Budget
	}
	assert.Equal(t, got["keeps"], Budget{Cost: 2, Tools: 300})
	assert.Equal(t, got["over"], Budget{Cost: 0.5, Tokens: 2_000_000, Tools: 300, Time: 5 * time.Minute})
	assert.Equal(t, got["bare"], Budget{Cost: 1, Tools: 300})
	assert.Equal(t, got["unset"], Budget{Cost: 2})
	assert.Equal(t, got["none"], Budget{})
	assert.Equal(t, got["plain"], Budget{Cost: 3})
	assert.Equal(t, got["adds"], Budget{Cost: 3, Tokens: 20_000})
	assert.Equal(t, got["empty"], Budget{Cost: 3})
}

// a bad cap fails the load, naming its kind.
func TestParseBudgetErrors(t *testing.T) {
	const pre = "axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, budget: "
	for _, tc := range []struct{ src, want string }{
		{"0", "budget: 0 is not a cost cap"},
		{"-1", "budget: -1 is not a cost cap"},
		{".nan", "is not a cost cap"},
		{"2M", `budget: "2M" is not a cost cap`},
		{"[1]", "want a cost or a mapping"},
		{"{nope: 1}", `budget: unknown kind "nope"`},
		{"{cost: inf}", "budget: cost:"},
		{"{tokens: 1.2345k}", `budget: tokens: "1.2345k" is not a count`},
		{"{tokens: 2000000.0}", "budget: tokens:"},
		{"{tools: 0}", "budget: tools:"},
		{"{tools: 2e12}", "budget: tools:"},
		{"{time: -5m}", `budget: time: "-5m" is not a duration`},
		{"{time: 300}", "budget: time:"},
	} {
		_, err := Parse([]byte(pre + tc.src + "}}}\n"))
		assert.Error(t, err, tc.want)
	}
}

// a count is whole and above zero, a decimal suffix allowed.
func TestParseCount(t *testing.T) {
	for s, want := range map[string]int64{"300": 300, "20k": 20_000, "20K": 20_000, "1.5M": 1_500_000, "2g": 2_000_000_000, "1.234k": 1234, "1000G": 1e12} {
		n, err := ParseCount(s)
		require.NoError(t, err, s)
		assert.Equal(t, n, want, s)
	}
	for _, s := range []string{"", "0", "0k", "-5", "1.2345k", "20kb", "20Ki", "k", "1e3", "1e3k", "1001G", " 5", "1_000", "0.0001"} {
		_, err := ParseCount(s)
		assert.That(t, err != nil, "%q: no error", s)
	}
}

// --budget is a bare cost or a list of kind=value.
func TestParseBudgetFlag(t *testing.T) {
	for s, want := range map[string]Budget{
		"20":                    {Cost: 20},
		"0.5":                   {Cost: 0.5},
		"cost=20,time=2h":       {Cost: 20, Time: 2 * time.Hour},
		"tokens=50M":            {Tokens: 50_000_000},
		" cost = 5 , tools=2k ": {Cost: 5, Tools: 2000},
	} {
		b, err := ParseBudget(s)
		require.NoError(t, err, s)
		assert.Equal(t, b, want, s)
	}
	for s, want := range map[string]string{
		"":              "empty",
		"0":             "cost:",
		"1e2":           "cost:",
		"cost=1,cost=2": "cost: given twice",
		"nope=1":        `unknown kind "nope"`,
		"cost=":         "cost: no value",
		"=5":            "is not kind=value",
		"cost=5,":       "is not kind=value",
		"tokens=1.5":    "tokens:",
		"time=0s":       "time:",
	} {
		_, err := ParseBudget(s)
		assert.Error(t, err, want)
	}
}

// String gives back what ParseBudget reads.
func TestBudgetString(t *testing.T) {
	b := Budget{Cost: 2.5, Tokens: 20_000, Tools: 3, Time: 90 * time.Second}
	assert.Equal(t, b.String(), "cost=2.5,tokens=20000,tools=3,time=1m30s")
	back, err := ParseBudget(b.String())
	require.NoError(t, err)
	assert.Equal(t, back, b)
}
