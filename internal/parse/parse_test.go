package parse

import (
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// run compiles src and feeds it lines, one pass.
func run(t *testing.T, src string, lines ...string) *Result {
	t.Helper()
	p, err := Compile("test", src)
	require.NoError(t, err)
	ps := p.NewPass()
	for _, l := range lines {
		ps.Line([]byte(l))
	}
	return ps.Result()
}

// usage adds, and total=True replaces the totals it names and no others.
func TestUsage(t *testing.T) {
	r := run(t, `
if line == "add":
    out.usage(turns = 1, cost_usd = 0.5, input_tokens = 10, output_tokens = 3, reasoning_tokens = 1, cache_read_tokens = 2, cache_write_tokens = 4)
elif line == "total":
    out.usage(total = True, cost_usd = 2, input_tokens = 7, ttft_ms = 40, api_duration_ms = 900, premium_requests = 3)
`, "add", "add", "total")
	assert.Equal(t, r.Turns, 2)   // not named by the total
	assert.Equal(t, r.Output, 6)  // nor this
	assert.Equal(t, r.Input, 7)   // replaced
	assert.Equal(t, r.CostUSD, 2) // replaced
	assert.Equal(t, r.CacheRead, 4)
	assert.Equal(t, r.CacheWrite, 8)
	assert.Equal(t, r.Reasoning, 2)
	assert.Equal(t, r.TTFTms, 40)
	assert.Equal(t, r.APIms, 900)
	assert.Equal(t, r.PremiumRequests, 3)
	assert.Equal(t, r.HasCost, true)
	assert.Equal(t, r.Said, true)
	assert.Equal(t, r.Errors, 0)
}

// a cost of 0 is a cost: reported, not missing.
func TestHasCost(t *testing.T) {
	assert.Equal(t, run(t, `out.usage(cost_usd = 0)`, "x").HasCost, true)
	assert.Equal(t, run(t, `out.usage(input_tokens = 1)`, "x").HasCost, false)
}

// a number that is negative, not finite, fractional where a count is wanted,
// or whose total would not fit is an error that changes no total.
func TestUsageNumbers(t *testing.T) {
	for name, call := range map[string]string{
		"negative cost":    `out.usage(input_tokens = 5, cost_usd = -1)`,
		"nan cost":         `out.usage(input_tokens = 5, cost_usd = float("nan"))`,
		"infinite cost":    `out.usage(input_tokens = 5, cost_usd = float("inf"))`,
		"negative count":   `out.usage(cost_usd = 1, input_tokens = 1 - 2)`,
		"fractional count": `out.usage(cost_usd = 1, output_tokens = 1.5)`,
		"count overflow":   `out.usage(cost_usd = 1, input_tokens = 1 << 62)`,
		"cost overflow":    `out.usage(input_tokens = 5, cost_usd = 1e308)`,
		"not a number":     `out.usage(input_tokens = "5")`,
		"positional":       `out.usage(5)`,
		"unknown keyword":  `out.usage(tokens = 5)`,
	} {
		r := run(t, "if line == \"ok\":\n    out.usage(input_tokens = 1 << 62, cost_usd = 1e308)\nelse:\n    "+call, "ok", "bad")
		assert.Equal(t, r.Errors, 1, name)
		assert.Equal(t, r.FirstErrorLine, 2, name)
		assert.Equal(t, r.Input, 1<<62, name)
		assert.Equal(t, r.CostUSD, 1e308, name)
		assert.Equal(t, r.Output, 0, name)
	}
	// a whole float is a count.
	assert.Equal(t, run(t, `out.usage(output_tokens = 4.0)`, "x").Output, 4)
}

func TestToolsAndError(t *testing.T) {
	r := run(t, `
if line == "t":
    out.tool("bash")
    out.tool("read")
    out.tool("bash")
elif line == "e":
    out.error()
elif line == "ok":
    out.error(False)
`, "t", "e")
	assert.Equal(t, r.ToolCalls, 3)
	assert.Equal(t, r.Tools["bash"], 2)
	assert.Equal(t, r.IsError, true)
	assert.Equal(t, run(t, `out.error(line == "e")`, "e", "ok").IsError, false) // the last call wins

	// an error may say what it is, and no error says nothing.
	said := run(t, `out.error(msg = "401: no such user")`, "x")
	assert.Equal(t, said.IsError, true)
	assert.Equal(t, said.ErrorMsg, "401: no such user")
	assert.Equal(t, run(t, `out.error(True, "why")`, "x").ErrorMsg, "why")
	assert.Equal(t, run(t, `out.error(line == "e", "why")`, "e", "ok").ErrorMsg, "")
	assert.Equal(t, run(t, `out.error(msg = 5)`, "x").Errors, 1)
}

// the reply: final replaces the candidates before it, add keeps them, newest
// first; with no final it is the text since the last clear.
func TestReplies(t *testing.T) {
	final := `
if line.startswith("f "):
    out.final(line[2:])
elif line.startswith("a "):
    out.final(line[2:], add = True)
else:
    out.text(line)
`
	assert.Equal(t, strings.Join(run(t, final, "f one", "f two").Replies(), "|"), "two")
	assert.Equal(t, strings.Join(run(t, final, "f one", "a two", "a three").Replies(), "|"), "three|two|one")
	assert.Equal(t, strings.Join(run(t, final, "text", "f one").Replies(), "|"), "one") // a final outranks text

	text := `
if line == "clear":
    out.clear()
elif line == "next":
    out.clear(next = True)
else:
    out.text(line)
`
	assert.Equal(t, strings.Join(run(t, text, "a", "clear", "b", "c").Replies(), "|"), "b\nc")
	assert.Equal(t, len(run(t, text, "a", "clear").Replies()), 0)
	// next=True drops the pieces only once another arrives.
	assert.Equal(t, strings.Join(run(t, text, "a", "next").Replies(), "|"), "a")
	assert.Equal(t, strings.Join(run(t, text, "a", "next", "b", "c").Replies(), "|"), "b\nc")
	assert.Equal(t, len(run(t, text, "a", "next", "clear").Replies()), 0)

	// reply calls alone are no summary.
	assert.Equal(t, run(t, final, "f one", "text").Said, false)
}

// a line's ending is not the script's: one carriage return before the
// newline is dropped.
func TestLineEnding(t *testing.T) {
	r := run(t, `out.tool(line)`, "a\r", "b", "c\r\r")
	assert.Equal(t, r.Tools["a"], 1)
	assert.Equal(t, r.Tools["b"], 1)
	assert.Equal(t, r.Tools["c\r"], 1)
}

// a failed line ends its own run and nothing else: what it said before
// failing stands, the lines around it count, and the first failure is kept.
func TestLineErrors(t *testing.T) {
	r := run(t, `
ev = json.decode(line, default = None)
if type(ev) == "dict":
    out.tool("seen")
    out.usage(cost_usd = ev["cost"])
`, `{"cost": 1}`, `not json`, `{"price": 2}`, `{"cost": 3}`, `{}`)
	assert.Equal(t, r.ToolCalls, 4)
	assert.Equal(t, r.CostUSD, 4)
	assert.Equal(t, r.Errors, 2)
	assert.Equal(t, r.FirstErrorLine, 3)
	assert.ContainsString(t, r.FirstError, `key "cost" not in dict`)
}

func TestStepBudget(t *testing.T) {
	r := run(t, "for i in range(10000000):\n    pass\nout.tool(\"after\")", "x")
	assert.Equal(t, r.Errors, 1)
	assert.Equal(t, r.ToolCalls, 0)
}

// nothing a script sets lasts to the next line, and it reaches nothing but
// what it is handed.
func TestHermetic(t *testing.T) {
	r := run(t, "if line == \"set\":\n    x = 1\nout.tool(str(x))", "set", "use")
	assert.Equal(t, r.Tools["1"], 1)
	assert.Equal(t, r.Errors, 1) // x is not there on the second line

	for name, src := range map[string]string{
		"syntax":         `out.tool(`,
		"undefined name": `out.tool(nope)`,
		"open":           `open("/etc/passwd")`,
		"environment":    `out.tool(env["HOME"])`,
		"load":           `load("other.star", "x")`,
		"return":         `return`,
	} {
		_, err := Compile("h", src)
		assert.That(t, err != nil, "%s: compiled", name)
	}

	// helpers, top-level control flow and a name set twice are a script's.
	_, err := Compile("h", "def f(s):\n    return s.upper()\nn = 0\nfor c in line.elems():\n    n = n + 1\nif n:\n    out.tool(f(line))\n")
	require.NoError(t, err)
}

func TestRe(t *testing.T) {
	r := run(t, `
m = re.search(r"cost: \$([0-9.]+)( usd)?", line)
if m:
    out.usage(cost_usd = float(m[1]))
    out.tool("%s|%s|%s" % (m[0], m[1], m[2]))
for w in re.findall(r"tool=(\w+)", line):
    out.tool(w[1])
`, "cost: $0.25", "nothing here", "tool=bash tool=read", "cost: $1.5 usd")
	assert.Equal(t, r.Errors, 0)
	assert.Equal(t, r.CostUSD, 1.75)
	assert.Equal(t, r.Tools["cost: $0.25|0.25|None"], 1)
	assert.Equal(t, r.Tools["cost: $1.5 usd|1.5| usd"], 1)
	assert.Equal(t, r.Tools["bash"], 1)
	assert.Equal(t, r.Tools["read"], 1)

	// what go's regexp refuses is an error on the line that uses it.
	bad := run(t, `re.search(r"(?=x)", line)`, "x")
	assert.Equal(t, bad.Errors, 1)
	assert.ContainsString(t, bad.FirstError, "error parsing regexp")
}

// two passes of one program do not share totals.
func TestPasses(t *testing.T) {
	p, err := Compile("test", `out.usage(input_tokens = 1)`)
	require.NoError(t, err)
	a, b := p.NewPass(), p.NewPass()
	a.Line([]byte("x"))
	a.Line([]byte("y"))
	b.Line([]byte("x"))
	assert.Equal(t, a.Result().Input, 2)
	assert.Equal(t, b.Result().Input, 1)
}
