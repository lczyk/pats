package eval

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// drive the progress display against a buffer (the bar is normally tty-gated).
// asserts the bar advances, per-pair counters render (with tools gated by
// showTools, net gated by netSeen), and finished pairs' logs land above the bar.
// run under -race it also checks the ticker goroutine doesn't race foreground writes.
func TestProgress(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf, 2, len("a x t1"))

	s1 := &pairStat{}
	p.start("a x t1", s1, "", true) // showTools -> tool column
	atomic.StoreInt64(&s1.out, 5)
	atomic.StoreInt64(&s1.tools, 2)
	p.start("a x t2", &pairStat{}, "", false) // no tool column
	p.finish("a x t1", "  [a x t1] ok (exit 0, 1.0s)\n")
	p.finish("a x t2", "  [a x t2] ok (exit 0, 1.0s)\n")
	p.close()

	out := buf.String()
	assert.That(t, strings.Contains(out, "0/2"), "shows initial total", out)
	assert.That(t, strings.Contains(out, "2/2"), "reaches full total", out)
	assert.That(t, strings.Contains(out, "5 out"), "shows out count", out)
	assert.That(t, strings.Contains(out, "2 tool"), "shows tool count when showTools", out)
	assert.That(t, strings.Contains(out, "[a x t1] ok (exit 0, 1.0s)"), "logs completion above bar", out)
}

func TestStatTap(t *testing.T) {
	var s pairStat
	tap := &statTap{stat: &s, scan: scanClaude}
	// an assistant turn with two tool calls (claude-code stream-json shape).
	tap.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use"},{"type":"text"},{"type":"tool_use"}]}}` + "\n"))
	tap.Write([]byte(`{"type":"stream_event","event":{"type":"content_block_delta"}}` + "\n")) // envelope -> not a tool
	tap.Write([]byte("parti"))                                                                 // no newline -> not counted until completed
	tap.Write([]byte("al\n"))
	assert.Equal(t, atomic.LoadInt64(&s.out), int64(3))
	assert.Equal(t, atomic.LoadInt64(&s.tools), int64(2))
}

func TestCountLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "egress.log")
	require.NoError(t, os.WriteFile(p, []byte("a\nb\nc\n"), 0o644))
	n, err := countLines(p)
	require.NoError(t, err)
	assert.Equal(t, n, 3)
	_, err = countLines(filepath.Join(dir, "missing"))
	assert.Error(t, err, assert.AnyError) // missing file (non-proxy) -> not readable
}

func TestClaudeSummary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "stdout.log")
	log := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"},{"type":"tool_use","name":"Read"}]}}
{"type":"stream_event","event":{"type":"content_block_delta"}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":57,"total_cost_usd":0.42,"ttft_ms":3947,"duration_api_ms":159141,"usage":{"input_tokens":412,"output_tokens":9213,"cache_read_input_tokens":2743475,"cache_creation_input_tokens":50434}}
`
	require.NoError(t, os.WriteFile(p, []byte(log), 0o644))

	s := claudeSummary(p)
	require.NotNil(t, s)
	assert.Equal(t, s.NumTurns, 57)
	assert.Equal(t, s.OutputTokens, 9213)
	assert.Equal(t, s.CacheReadTokens, 2743475)
	assert.Equal(t, s.CostUSD, 0.42)
	assert.Equal(t, s.Tools["Bash"], 2) // per-tool-name counts
	assert.Equal(t, s.Tools["Read"], 1)

	assert.Nil(t, summarize("opencode-openrouter", p)) // no parser for the kind -> nil
}

func TestIsProgressTTY(t *testing.T) {
	assert.Equal(t, isProgressTTY(&bytes.Buffer{}), false) // a buffer is never a tty
}
