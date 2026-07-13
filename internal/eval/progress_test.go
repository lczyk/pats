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
	assert.Equal(t, s.HasCost, true)
	assert.Equal(t, s.Tools["Bash"], 2) // per-tool-name counts
	assert.Equal(t, s.Tools["Read"], 1)

	assert.Nil(t, summarize("codex-cli", p)) // no parser for the kind -> nil
}

func TestOpencodeSummary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "stdout.log")
	// two steps (usage summed), three tool calls, a reasoning + text part.
	log := `{"type":"step_start","timestamp":1,"sessionID":"s","part":{"type":"step-start"}}
{"type":"tool_use","timestamp":2,"sessionID":"s","part":{"type":"tool","tool":"bash","state":{"status":"completed"}}}
{"type":"tool_use","timestamp":3,"sessionID":"s","part":{"type":"tool","tool":"bash","state":{"status":"error"}}}
{"type":"reasoning","timestamp":4,"sessionID":"s","part":{"type":"reasoning","text":"hmm"}}
{"type":"step_finish","timestamp":5,"sessionID":"s","part":{"type":"step-finish","cost":0.01,"tokens":{"input":100,"output":50,"reasoning":25,"cache":{"read":1000,"write":10}}}}
{"type":"tool_use","timestamp":6,"sessionID":"s","part":{"type":"tool","tool":"read","state":{"status":"completed"}}}
{"type":"text","timestamp":7,"sessionID":"s","part":{"type":"text","text":"done"}}
{"type":"step_finish","timestamp":8,"sessionID":"s","part":{"type":"step-finish","cost":0.02,"tokens":{"input":200,"output":150,"reasoning":0,"cache":{"read":2000,"write":20}}}}
`
	require.NoError(t, os.WriteFile(p, []byte(log), 0o644))

	s := opencodeSummary(p)
	require.NotNil(t, s)
	assert.Equal(t, s.NumTurns, 2) // one per step_finish
	assert.NearlyEqual(t, s.CostUSD, 0.03, 1e-9)
	assert.Equal(t, s.HasCost, true)
	assert.Equal(t, s.InputTokens, 300)
	assert.Equal(t, s.OutputTokens, 200)
	assert.Equal(t, s.ReasoningTokens, 25)
	assert.Equal(t, s.CacheReadTokens, 3000)
	assert.Equal(t, s.CacheCreationTokens, 30)
	assert.Equal(t, s.Tools["bash"], 2)
	assert.Equal(t, s.Tools["read"], 1)
	assert.Equal(t, s.IsError, false)

	// error-only log (e.g. bad model / policy refusal) -> summary with IsError.
	pe := filepath.Join(dir, "err.log")
	require.NoError(t, os.WriteFile(pe, []byte(`{"type":"error","timestamp":1,"sessionID":"s","error":{"name":"APIError"}}`+"\n"), 0o644))
	se := opencodeSummary(pe)
	require.NotNil(t, se)
	assert.Equal(t, se.IsError, true)

	// prose/garbage -> nil.
	pg := filepath.Join(dir, "garbage.log")
	require.NoError(t, os.WriteFile(pg, []byte("just some prose\n"), 0o644))
	assert.Nil(t, opencodeSummary(pg))
}

func TestScanOpencode(t *testing.T) {
	var s pairStat
	tap := &statTap{stat: &s, scan: scanOpencode}
	tap.Write([]byte(`{"type":"tool_use","part":{"type":"tool","tool":"bash"}}` + "\n"))
	tap.Write([]byte(`{"type":"text","part":{"type":"text","text":"hi"}}` + "\n"))
	tap.Write([]byte(`{"type":"tool_use","part":{"type":"tool","tool":"read"}}` + "\n"))
	assert.Equal(t, atomic.LoadInt64(&s.out), int64(3))
	assert.Equal(t, atomic.LoadInt64(&s.tools), int64(2))
}

func TestCodexSummary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "stdout.log")
	log := `{"type":"thread.started","thread_id":"t"}
{"type":"turn.started"}
{"type":"item.started","item":{"type":"command_execution"}}
{"type":"item.completed","item":{"type":"command_execution"}}
{"type":"item.completed","item":{"type":"file_change"}}
{"type":"item.completed","item":{"type":"mcp_tool_call","server":"github","tool":"get_issue"}}
{"type":"item.completed","item":{"type":"agent_message","text":"done"}}
{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":75,"output_tokens":20,"reasoning_output_tokens":5}}
`
	require.NoError(t, os.WriteFile(p, []byte(log), 0o644))

	s := codexSummary(p)
	require.NotNil(t, s)
	assert.Equal(t, s.NumTurns, 0)     // `codex exec` is one turn; not a round-trip count
	assert.Equal(t, s.InputTokens, 25) // 100 total - 75 cached, matching the other kinds
	assert.Equal(t, s.CacheReadTokens, 75)
	assert.Equal(t, s.OutputTokens, 20)
	assert.Equal(t, s.ReasoningTokens, 5)
	assert.Equal(t, s.CostUSD, 0.0)
	assert.Equal(t, s.HasCost, false) // keyless: billed to the subscription, never priced
	assert.Equal(t, s.Tools["command_execution"], 1)
	assert.Equal(t, s.Tools["file_change"], 1)
	assert.Equal(t, s.Tools["mcp:github/get_issue"], 1)

	failed := filepath.Join(dir, "failed.log")
	require.NoError(t, os.WriteFile(failed, []byte(`{"type":"turn.failed"}`+"\n"), 0o644))
	assert.Equal(t, codexSummary(failed).IsError, true)

	garbage := filepath.Join(dir, "garbage.log")
	require.NoError(t, os.WriteFile(garbage, []byte("not json\n"), 0o644))
	assert.Nil(t, codexSummary(garbage))
}

func TestScanCodex(t *testing.T) {
	var s pairStat
	tap := &statTap{stat: &s, scan: scanCodex}
	tap.Write([]byte(`{"type":"item.started","item":{"type":"command_execution"}}` + "\n"))
	tap.Write([]byte(`{"type":"item.completed","item":{"type":"command_execution"}}` + "\n"))
	tap.Write([]byte(`{"type":"item.completed","item":{"type":"agent_message"}}` + "\n"))
	tap.Write([]byte(`{"type":"item.completed","item":{"type":"web_search"}}` + "\n"))
	assert.Equal(t, atomic.LoadInt64(&s.tools), int64(2))
}

// fixture lines are trimmed copies of a real `copilot -p --output-format json`
// stream (extra event fields dropped, ids shortened).
func TestCopilotSummary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "stdout.log")
	log := `{"type":"session.mcp_servers_loaded","data":{"servers":[]},"ephemeral":true}
{"type":"user.message","data":{"content":"do the thing"}}
{"type":"assistant.turn_start","data":{"turnId":"0"}}
{"type":"assistant.tool_call_delta","data":{"toolCallId":"t1","toolName":"bash","inputDelta":"{"},"ephemeral":true}
{"type":"assistant.message","data":{"messageId":"m1","content":"","toolRequests":[{"toolCallId":"t1","name":"bash"}],"turnId":"0","outputTokens":74}}
{"type":"tool.execution_start","data":{"toolCallId":"t1","toolName":"bash","turnId":"0"}}
{"type":"tool.execution_complete","data":{"toolCallId":"t1","success":true}}
{"type":"assistant.turn_end","data":{"turnId":"0"}}
{"type":"assistant.turn_start","data":{"turnId":"1"}}
{"type":"tool.execution_start","data":{"toolCallId":"t2","toolName":"write","turnId":"1"}}
{"type":"tool.execution_complete","data":{"toolCallId":"t2","success":true}}
{"type":"assistant.message","data":{"messageId":"m2","content":"done","toolRequests":[],"turnId":"1","outputTokens":3}}
{"type":"assistant.turn_end","data":{"turnId":"1"}}
{"type":"result","sessionId":"s1","exitCode":0,"usage":{"premiumRequests":1,"totalApiDurationMs":3484,"sessionDurationMs":6273}}
`
	require.NoError(t, os.WriteFile(p, []byte(log), 0o644))

	s := copilotSummary(p)
	require.NotNil(t, s)
	assert.Equal(t, s.NumTurns, 2) // turn_end events are real round-trips
	assert.Equal(t, s.OutputTokens, 77)
	assert.Equal(t, s.InputTokens, 0) // the stream reports no input/cache counts
	assert.Equal(t, s.CostUSD, 0.0)
	assert.Equal(t, s.HasCost, false) // billed in premium requests, never dollars
	assert.Equal(t, s.PremiumRequests, 1)
	assert.Equal(t, s.APIDurationMs, 3484)
	assert.Equal(t, s.IsError, false)
	assert.Equal(t, s.Tools["bash"], 1)
	assert.Equal(t, s.Tools["write"], 1)

	failed := filepath.Join(dir, "failed.log")
	require.NoError(t, os.WriteFile(failed, []byte(`{"type":"result","exitCode":1,"usage":{"premiumRequests":1}}`+"\n"), 0o644))
	assert.Equal(t, copilotSummary(failed).IsError, true)

	garbage := filepath.Join(dir, "garbage.log")
	require.NoError(t, os.WriteFile(garbage, []byte("not json\n"), 0o644))
	assert.Nil(t, copilotSummary(garbage))
}

func TestScanCopilot(t *testing.T) {
	var s pairStat
	tap := &statTap{stat: &s, scan: scanCopilot}
	tap.Write([]byte(`{"type":"tool.execution_start","data":{"toolCallId":"t1","toolName":"bash"}}` + "\n"))
	tap.Write([]byte(`{"type":"tool.execution_complete","data":{"toolCallId":"t1","success":true}}` + "\n"))
	tap.Write([]byte(`{"type":"assistant.message","data":{"content":"done","outputTokens":3}}` + "\n"))
	tap.Write([]byte(`{"type":"tool.execution_start","data":{"toolCallId":"t2","toolName":"write"}}` + "\n"))
	assert.Equal(t, atomic.LoadInt64(&s.out), int64(4))
	assert.Equal(t, atomic.LoadInt64(&s.tools), int64(2))
}

func TestIsProgressTTY(t *testing.T) {
	assert.Equal(t, isProgressTTY(&bytes.Buffer{}), false) // a buffer is never a tty
}
