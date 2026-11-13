package eval

import (
	"bytes"
	"fmt"
	"github.com/lczyk/pats/internal/machine"
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
	p := newProgress(&buf, 2, len("a/t1"))

	s1 := &pairStat{}
	p.start("a/t1", s1, "", true) // showTools -> tool column
	atomic.StoreInt64(&s1.out, 5)
	atomic.StoreInt64(&s1.tools, 2)
	p.start("a/t2", &pairStat{}, "", false) // no tool column
	p.finish("a/t1", "  a/t1 ok (exit 0, 1.0s)\n")
	p.finish("a/t2", "  a/t2 ok (exit 0, 1.0s)\n")
	p.close()

	out := buf.String()
	assert.That(t, strings.Contains(out, "0/2"), "shows initial total", out)
	assert.That(t, strings.Contains(out, "2/2"), "reaches full total", out)
	assert.That(t, strings.Contains(out, "5 out"), "shows out count", out)
	assert.That(t, strings.Contains(out, "2 tool"), "shows tool count when showTools", out)
	assert.That(t, strings.Contains(out, "a/t1 ok (exit 0, 1.0s)"), "logs completion above bar", out)
}

// the region is cut to the terminal: a line that wrapped, or more lines than
// the screen holds, would leave a redraw's cursor in the wrong row.
func TestProgressFitsTerminal(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf, 9, 0)
	defer p.close()
	cols, rows := 24, 6
	p.mu.Lock()
	p.size = func() (int, int) { return cols, rows }
	p.mu.Unlock()
	for i := range 9 {
		p.start(fmt.Sprintf("agent-with-a-long-name/task-%d", i), &pairStat{}, "", true)
	}
	region := func() []string {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.regionLines()
	}
	lines := region()
	assert.Len(t, lines, rows-1) // the row under the region stays on screen
	assert.Equal(t, lines[len(lines)-1], "  ... and 6 more")
	for _, l := range lines {
		assert.That(t, len([]rune(l)) < cols, "%q reaches the last column", l)
	}
	assert.That(t, strings.HasPrefix(lines[0], "[----"), "the bar went: %q", lines[0])

	// a side that is not known cuts nothing.
	p.mu.Lock()
	cols, rows = 0, 0
	p.mu.Unlock()
	lines = region()
	assert.Len(t, lines, 10)
	assert.That(t, strings.Contains(lines[9], "agent-with-a-long-name/task-8"), "%q", lines[9])
	// room for every line: none stands for others.
	assert.Len(t, fitRegion([]string{"bar", "a", "b"}, 80, 4), 3)
	assert.EqualArrays(t, fitRegion([]string{"bar", "a", "b", "c"}, 80, 4), []string{"bar", "a", "  ... and 2 more"})
	assert.Len(t, fitRegion([]string{"bar", "a", "b"}, 80, 2), 3) // too small to say anything in
}

func TestStatTap(t *testing.T) {
	var s pairStat
	tap := &statTap{stat: &s, scan: statScanner(hd("claude-cli-keyless"))}
	// an assistant turn with two tool calls (claude-code stream-json shape).
	tap.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use"},{"type":"text"},{"type":"tool_use"}]}}` + "\n"))
	tap.Write([]byte(`{"type":"stream_event","event":{"type":"content_block_delta"}}` + "\n")) // envelope -> not a tool
	tap.Write([]byte("parti"))                                                                 // no newline -> not counted until completed
	tap.Write([]byte("al\n"))
	assert.Equal(t, atomic.LoadInt64(&s.out), int64(3))
	assert.Equal(t, atomic.LoadInt64(&s.tools), int64(2))
}

// COVER: a harness emitting a huge newline-less blob must not grow statTap's
// buffer without bound; the eventual newline still counts as one "out" line.
// fails against the old unbounded statTap.Write.
func TestStatTapCapsUnboundedLine(t *testing.T) {
	var s pairStat
	tap := &statTap{stat: &s, scan: statScanner(hd("claude-cli-keyless"))}

	big := bytes.Repeat([]byte("x"), statTapMaxBuf+1000) // over the cap, no newline yet
	tap.Write(big)
	assert.That(t, len(tap.buf) <= statTapMaxBuf, "buf must stay capped, got %d bytes", len(tap.buf))
	assert.Equal(t, atomic.LoadInt64(&s.out), int64(0)) // no newline yet -> not counted

	tap.Write([]byte("\n"))
	assert.Equal(t, atomic.LoadInt64(&s.out), int64(1)) // newline arrives -> counted once
	assert.Equal(t, len(tap.buf), 0)
}

// streamSeeds are short harness streams the stdout fuzz targets start from:
// one per harness, then lines that are not events.
var streamSeeds = []string{
	`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"},{"type":"text"},{"type":"tool_use","name":"Read"}]}}` + "\n" +
		`{"type":"stream_event","event":{"type":"content_block_delta"}}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"num_turns":3,"total_cost_usd":0.42,"usage":{"input_tokens":4,"output_tokens":9}}` + "\n",
	`{"type":"item.started","item":{"type":"command_execution"}}` + "\n" +
		`{"type":"item.completed","item":{"type":"command_execution"}}` + "\n" +
		`{"type":"item.completed","item":{"type":"mcp_tool_call","server":"github","tool":"get_issue"}}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":75,"output_tokens":20}}` + "\n",
	`{"type":"tool.execution_start","data":{"toolCallId":"t1","toolName":"bash"}}` + "\n" +
		`{"type":"assistant.message","data":{"content":"done","outputTokens":3}}` + "\n" +
		`{"type":"assistant.turn_end","data":{"turnId":"0"}}` + "\n" +
		`{"type":"result","exitCode":1,"usage":{"premiumRequests":1}}` + "\n",
	`{"type":"tool_use","part":{"type":"tool","tool":"bash"}}` + "\n" +
		`{"type":"step_finish","part":{"cost":0.01,"tokens":{"input":100,"output":50,"reasoning":25,"cache":{"read":1000,"write":10}}}}` + "\n" +
		`{"type":"error","error":{"name":"APIError"}}` + "\n",
	"", "\n\n", "just some prose\n", "null\n[]\n1\n", `{"type":"assistant","message":{"content":[{"type":"tool_use"}]}}`,
	`{"type":"tool_use"}` + "\r\n" + `{"type":"tool.execution_start"}` + "\r\n", `{"type":"result","num_turns":1.5}` + "\n",
}

// FuzzStatTap: stdout reaches the tap in whatever chunks the pipe hands over.
// the same bytes in one Write or split in two leave the same counters,
// whichever harness parses them; out is the newline count; and the tap holds
// the line in progress, never more than statTapMaxBuf of it, also when it
// starts a few bytes short of that cap.
func FuzzStatTap(f *testing.F) {
	for _, s := range streamSeeds {
		f.Add([]byte(s), uint16(len(s)/2), uint8(0), false)
		f.Add([]byte(s), uint16(7), uint8(40), true)
	}
	f.Add([]byte("0123456789"), uint16(5), uint8(5), true) // the split is where the cap is
	f.Add([]byte("0123\n456789"), uint16(2), uint8(0), true)

	// a scanner is one pass of a script: each tap gets its own.
	scans := map[string]*machine.Harness{"no parser": nil}
	for _, h := range harnessNames() {
		scans[h] = hd(h)
	}

	// a line in progress as long as the cap, one per tap: every input starts
	// from a prefix of it, so none allocates 8 MiB of its own.
	var held [2][]byte
	var dirty [2]int // bytes at the front an earlier input overwrote
	for i := range held {
		held[i] = bytes.Repeat([]byte("x"), statTapMaxBuf)
	}

	f.Fuzz(func(t *testing.T, data []byte, cut uint16, room uint8, nearCap bool) {
		k := int(cut) % (len(data) + 1)
		pre := 0
		if nearCap {
			pre = statTapMaxBuf - int(room)
		}
		start := func(i int) []byte {
			if !nearCap {
				return nil
			}
			for j := range dirty[i] {
				held[i][j] = 'x'
			}
			dirty[i] = min(len(data), statTapMaxBuf)
			return held[i][:pre]
		}
		// what the tap must hold after data: the bytes since the last newline,
		// on top of what it started with when there was none, cut at the cap.
		last := bytes.LastIndexByte(data, '\n')
		wantLen := min(len(data)-last-1, statTapMaxBuf)
		tail := data[last+1 : last+1+wantLen]
		if last < 0 {
			wantLen = min(pre+len(data), statTapMaxBuf)
			tail = data[:wantLen-pre]
		}

		for name, entry := range scans {
			var one, two pairStat
			scan := statScanner(entry)
			whole := &statTap{stat: &one, scan: scan, buf: start(0)}
			split := &statTap{stat: &two, scan: statScanner(entry), buf: start(1)}

			n, err := whole.Write(data)
			assert.That(t, n == len(data) && err == nil, "%s: Write = (%d, %v) for %d bytes", name, n, err, len(data))
			for _, part := range [][]byte{data[:k], data[k:]} {
				n, err := split.Write(part)
				assert.That(t, n == len(part) && err == nil, "%s: Write = (%d, %v) for %d bytes", name, n, err, len(part))
			}

			assert.Equal(t, one.out, int64(bytes.Count(data, []byte("\n"))), name)
			if scan == nil {
				assert.Equal(t, one.tools, int64(0), name)
			}
			assert.Equal(t, two.out, one.out, "%s: out differs split at %d", name, k)
			assert.Equal(t, two.tools, one.tools, "%s: tools differ split at %d", name, k)
			assert.Equal(t, two.liveCost.Load(), one.liveCost.Load(), "%s: cost differs split at %d", name, k)

			for _, tap := range []*statTap{whole, split} {
				assert.That(t, len(tap.buf) <= statTapMaxBuf, "%s: holds %d bytes, over the cap", name, len(tap.buf))
				assert.Equal(t, len(tap.buf), wantLen, "%s: held length, split at %d", name, k)
				if len(tap.buf) == wantLen {
					assert.That(t, bytes.Equal(tap.buf[wantLen-len(tail):], tail), "%s: held line differs, split at %d", name, k)
				}
			}
		}
	})
}

// FuzzSummarize: stdout.log is the harness's stream with whatever the agent
// put in it. the post-run digest reads it without a panic for every harness,
// lists no tool it did not count, never prices a keyless run, and agrees with
// the live counter the tap kept from the same bytes: both decode one event
// type, so the tool total is one number.
func FuzzSummarize(f *testing.F) {
	for _, s := range streamSeeds {
		f.Add([]byte(s))
	}
	p := filepath.Join(f.TempDir(), "stdout.log") // one file for every input
	f.Fuzz(func(t *testing.T, data []byte) {
		require.NoError(t, os.WriteFile(p, data, 0o644))

		for _, h := range harnessNames() {
			var live pairStat
			tap := &statTap{stat: &live, scan: statScanner(hd(h))}
			tap.Write(data)
			tap.Write([]byte("\n")) // the tap counts a line at its newline, the digest at eof too

			sum := summarize(hd(h), p)
			total := 0
			if sum != nil {
				for name, n := range sum.Tools {
					assert.That(t, n > 0, "%s: tool %q listed with count %d", h, name, n)
					total += n
				}
				if h == "codex-cli-keyless" || h == "copilot-cli-keyless" {
					assert.That(t, !sum.HasCost && sum.CostUSD == 0, "%s: a keyless run priced at %v", h, sum.CostUSD)
				}
			}
			assert.Equal(t, int64(total), live.tools, "%s: the digest and the live counter disagree", h)
			assert.That(t, live.spent() >= 0, "%s: live spend %v", h, live.spent())
		}
		assert.Nil(t, summarize(hd("no-such-harness"), p))
	})
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

	s := summarize(hd("claude-cli-keyless"), p)
	require.NotNil(t, s)
	assert.Equal(t, s.Turns, 57)
	assert.Equal(t, s.OutputTokens, 9213)
	assert.Equal(t, s.CacheReadTokens, 2743475)
	assert.Equal(t, s.CostUSD, 0.42)
	assert.Equal(t, s.HasCost, true)
	assert.Equal(t, s.Tools["Bash"], 2) // per-tool-name counts
	assert.Equal(t, s.Tools["Read"], 1)

	assert.Nil(t, summarize(hd("codex-cli"), p)) // no parser for the kind -> nil

	// a failed result says what went wrong where its answer would be.
	pf := filepath.Join(dir, "failed.log")
	require.NoError(t, os.WriteFile(pf, []byte(`{"type":"result","subtype":"success","is_error":true,"num_turns":1,"result":"Failed to authenticate: OAuth session expired and could not be refreshed"}`+"\n"), 0o644))
	sf := summarize(hd("claude-cli-keyless"), pf)
	assert.Equal(t, sf.IsError, true)
	assert.Equal(t, sf.Error, "Failed to authenticate: OAuth session expired and could not be refreshed")
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

	s := summarize(hd("opencode-openrouter"), p)
	require.NotNil(t, s)
	assert.Equal(t, s.Turns, 2) // one per step_finish
	assert.NearlyEqual(t, s.CostUSD, 0.03, 1e-9)
	assert.Equal(t, s.HasCost, true)
	assert.Equal(t, s.InputTokens, 300)
	assert.Equal(t, s.OutputTokens, 225) // reasoning counted in output, as the other harnesses do
	assert.Equal(t, s.ReasoningTokens, 25)
	assert.Equal(t, s.CacheReadTokens, 3000)
	assert.Equal(t, s.CacheWriteTokens, 30)
	assert.Equal(t, s.Tools["bash"], 2)
	assert.Equal(t, s.Tools["read"], 1)
	assert.Equal(t, s.IsError, false)

	// error-only log (e.g. bad model / policy refusal) -> summary with IsError.
	pe := filepath.Join(dir, "err.log")
	require.NoError(t, os.WriteFile(pe, []byte(`{"type":"error","timestamp":1,"sessionID":"s","error":{"name":"APIError"}}`+"\n"), 0o644))
	se := summarize(hd("opencode-openrouter"), pe)
	require.NotNil(t, se)
	assert.Equal(t, se.IsError, true)
	assert.Equal(t, se.Error, "APIError") // the name, when the event has no message
	require.NoError(t, os.WriteFile(pe, []byte(`{"type":"error","error":{"name":"APIError","data":{"message":"User not found.","statusCode":401}}}`+"\n"), 0o644))
	assert.Equal(t, summarize(hd("opencode-openrouter"), pe).Error, "User not found.")

	// prose/garbage -> nil, and so is json that is no object: no line of
	// either is a failed one, for any default.
	pg := filepath.Join(dir, "garbage.log")
	require.NoError(t, os.WriteFile(pg, []byte("just some prose\n42\n\"text\"\nnull\n[1]\ntrue\n"), 0o644))
	for _, name := range harnessNames() {
		assert.Nil(t, summarize(hd(name), pg), name)
	}
}

func TestScanOpencode(t *testing.T) {
	var s pairStat
	tap := &statTap{stat: &s, scan: statScanner(hd("opencode-openrouter"))}
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

	s := summarize(hd("codex-cli-keyless"), p)
	require.NotNil(t, s)
	assert.Equal(t, s.Turns, 0)        // `codex exec` is one turn; not a round-trip count
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
	require.NoError(t, os.WriteFile(failed, []byte(`{"type":"error","message":"unexpected status 401 Unauthorized"}`+"\n"+
		`{"type":"turn.failed","error":{"message":"unexpected status 401 Unauthorized: Incorrect API key provided"}}`+"\n"), 0o644))
	sf := summarize(hd("codex-cli-keyless"), failed)
	assert.Equal(t, sf.IsError, true)
	assert.Equal(t, sf.Error, "unexpected status 401 Unauthorized: Incorrect API key provided")

	garbage := filepath.Join(dir, "garbage.log")
	require.NoError(t, os.WriteFile(garbage, []byte("not json\n"), 0o644))
	assert.Nil(t, summarize(hd("codex-cli-keyless"), garbage))
}

func TestScanCodex(t *testing.T) {
	var s pairStat
	tap := &statTap{stat: &s, scan: statScanner(hd("codex-cli-keyless"))}
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

	s := summarize(hd("copilot-cli-keyless"), p)
	require.NotNil(t, s)
	assert.Equal(t, s.Turns, 2) // turn_end events are real round-trips
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
	assert.Equal(t, summarize(hd("copilot-cli-keyless"), failed).IsError, true)

	garbage := filepath.Join(dir, "garbage.log")
	require.NoError(t, os.WriteFile(garbage, []byte("not json\n"), 0o644))
	assert.Nil(t, summarize(hd("copilot-cli-keyless"), garbage))
}

func TestScanCopilot(t *testing.T) {
	var s pairStat
	tap := &statTap{stat: &s, scan: statScanner(hd("copilot-cli-keyless"))}
	tap.Write([]byte(`{"type":"tool.execution_start","data":{"toolCallId":"t1","toolName":"bash"}}` + "\n"))
	tap.Write([]byte(`{"type":"tool.execution_complete","data":{"toolCallId":"t1","success":true}}` + "\n"))
	tap.Write([]byte(`{"type":"assistant.message","data":{"content":"done","outputTokens":3}}` + "\n"))
	tap.Write([]byte(`{"type":"tool.execution_start","data":{"toolCallId":"t2","toolName":"write"}}` + "\n"))
	assert.Equal(t, atomic.LoadInt64(&s.out), int64(4))
	assert.Equal(t, atomic.LoadInt64(&s.tools), int64(2))
}

// a recorded run, cut down: a thinking turn that writes a file, one that runs
// a command, and the reply.
const piLog = `{"type":"session","version":3,"id":"s","timestamp":"2026-11-12T22:46:37.370Z","cwd":"/ws"}
{"type":"agent_start"}
{"type":"turn_start"}
{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"do the thing"}]}}
{"type":"message_update","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"assistantMessageEvent":{"type":"toolcall_start","contentIndex":1,"id":"c1","toolName":"write"}}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"thinking","thinking":"write, then cat"},{"type":"toolCall","id":"c1","name":"write","arguments":{"path":"hello.txt","content":"hi"}}],"provider":"openrouter","model":"openai/gpt-5-mini","usage":{"input":987,"output":121,"cacheRead":0,"cacheWrite":0,"reasoning":64,"totalTokens":1108,"cost":{"input":0.00024675,"output":0.000242,"cacheRead":0,"cacheWrite":0,"total":0.00048875,"totalFromProvider":true}},"stopReason":"toolUse","thinkingLevel":"low"}}
{"type":"tool_execution_start","toolCallId":"c1","toolName":"write","args":{"path":"hello.txt","content":"hi"}}
{"type":"tool_execution_end","toolCallId":"c1","toolName":"write","result":{"content":[{"type":"text","text":"Successfully wrote to hello.txt"}]},"isError":false,"durationMs":5}
{"type":"message_end","message":{"role":"toolResult","toolCallId":"c1","toolName":"write","content":[{"type":"text","text":"Successfully wrote to hello.txt"}],"isError":false}}
{"type":"turn_end","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"write","arguments":{"path":"hello.txt","content":"hi"}}],"usage":{"input":987,"output":121,"cacheRead":0,"cacheWrite":0,"reasoning":64,"totalTokens":1108,"cost":{"total":0.00048875}},"stopReason":"toolUse"},"toolResults":[]}
{"type":"turn_start"}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"toolCall","id":"c2","name":"bash","arguments":{"command":"cat hello.txt","timeout":120}}],"provider":"openrouter","model":"openai/gpt-5-mini","usage":{"input":96,"output":26,"cacheRead":1024,"cacheWrite":0,"reasoning":0,"totalTokens":1146,"cost":{"input":2.4e-05,"output":5.2e-05,"cacheRead":2.56e-05,"cacheWrite":0,"total":0.00010672,"totalFromProvider":true}},"stopReason":"toolUse","thinkingLevel":"low"}}
{"type":"tool_execution_start","toolCallId":"c2","toolName":"bash","args":{"command":"cat hello.txt","timeout":120}}
{"type":"tool_execution_update","toolCallId":"c2","toolName":"bash","args":{"command":"cat hello.txt","timeout":120},"partialResult":{"content":[]}}
{"type":"tool_execution_end","toolCallId":"c2","toolName":"bash","result":{"content":[{"type":"text","text":"hi"}]},"isError":false,"durationMs":52}
{"type":"turn_start"}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"it printed hi"}],"provider":"openrouter","model":"openai/gpt-5-mini","usage":{"input":129,"output":5,"cacheRead":1024,"cacheWrite":0,"reasoning":0,"totalTokens":1158,"cost":{"input":3.225e-05,"output":1e-05,"cacheRead":2.56e-05,"cacheWrite":0,"total":7.297e-05,"totalFromProvider":true}},"stopReason":"stop","thinkingLevel":"low"}}
{"type":"agent_end","messages":[],"willRetry":false}
{"type":"agent_settled","aborted":false}
`

// pi exits 0 on a reply that failed: a refused key reads so.
const piFailed = `{"type":"message_end","message":{"role":"assistant","content":[],"provider":"openrouter","model":"openai/gpt-5-mini","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"error","errorMessage":"401: {\"message\":\"User not found.\",\"code\":401}","thinkingLevel":"low"}}
`

func TestPiSummary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "stdout.log")
	require.NoError(t, os.WriteFile(p, []byte(piLog), 0o644))

	s := summarize(hd("pi-openrouter"), p)
	require.NotNil(t, s)
	assert.Equal(t, s.Turns, 3) // one per assistant message: a turn_end restates its message
	assert.NearlyEqual(t, s.CostUSD, 0.00066844, 1e-12)
	assert.Equal(t, s.HasCost, true)
	assert.Equal(t, s.InputTokens, 1212)
	assert.Equal(t, s.OutputTokens, 152) // reasoning is a part of it
	assert.Equal(t, s.ReasoningTokens, 64)
	assert.Equal(t, s.CacheReadTokens, 2048)
	assert.Equal(t, s.Tools["write"], 1)
	assert.Equal(t, s.Tools["bash"], 1)
	assert.Equal(t, s.IsError, false)

	var out bytes.Buffer
	require.NoError(t, ParseLog(hd("pi-openrouter"), strings.NewReader(piLog), &out))
	assert.ContainsString(t, out.String(), "reply:\nit printed hi\n")

	failed := filepath.Join(dir, "failed.log")
	require.NoError(t, os.WriteFile(failed, []byte(piFailed), 0o644))
	sf := summarize(hd("pi-openrouter"), failed)
	assert.Equal(t, sf.IsError, true)
	assert.Equal(t, sf.Error, `401: {"message":"User not found.","code":401}`)
	// a retry that worked is no error.
	require.NoError(t, os.WriteFile(failed, []byte(piFailed+piLog), 0o644))
	sf = summarize(hd("pi-openrouter"), failed)
	assert.Equal(t, sf.IsError, false)
	assert.Equal(t, sf.Error, "")
}

// a format that states its totals at the end reports its cost live then: the
// tap hears claude's result as it hears a step of opencode's.
func TestScanCostAtTheEnd(t *testing.T) {
	var s pairStat
	var heard []float64
	s.spend = func(c float64) { heard = append(heard, c) }
	tap := &statTap{stat: &s, scan: statScanner(hd("claude-cli-keyless"))}
	_, _ = tap.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}` + "\n"))
	assert.Equal(t, s.costSeen.Load(), false)
	_, _ = tap.Write([]byte(`{"type":"result","total_cost_usd":0.4,"num_turns":1}` + "\n"))
	_, _ = tap.Write([]byte(`{"type":"result","total_cost_usd":0.5,"num_turns":2}` + "\n")) // a later result restates the total
	assert.Equal(t, s.spent(), 0.5)
	assert.Equal(t, len(heard), 2)
	assert.Equal(t, s.costSeen.Load(), true)
	assert.Equal(t, atomic.LoadInt64(&s.tools), int64(1))
}

// the two passes frame lines alike: a carriage return before the newline is
// no part of a line in either, and a last line with no newline is the pass
// after's alone.
func TestPassesFrameLinesAlike(t *testing.T) {
	steps := `{"type":"tool_use","part":{"tool":"read"}}` + "\r\n" + `{"type":"step_finish","part":{"cost":1.5}}` + "\r\n"
	var live pairStat
	tap := &statTap{stat: &live, scan: statScanner(hd("opencode-openrouter"))}
	_, _ = tap.Write([]byte(steps))
	sum := summarize(hd("opencode-openrouter"), writeLog(t, steps))
	require.That(t, sum != nil, "no summary")
	assert.Equal(t, sum.CostUSD, 1.5)
	assert.Equal(t, live.spent(), sum.CostUSD)
	assert.Equal(t, atomic.LoadInt64(&live.tools), int64(sum.Tools["read"]))
	assert.Equal(t, sum.ParseErrors, 0)

	// one carriage return is the line ending's, a second is the line's own:
	// both passes hand the script the same line.
	// the script counts a line only when it is the line with one carriage
	// return left: either pass handing it another spelling counts nothing.
	strict := tables(t, `harnesses: {h: {run: "true", parse: 'if line == "a\r": out.tool("kept")'}}`).Harness("h")
	var cr pairStat
	tap = &statTap{stat: &cr, scan: statScanner(strict)}
	_, _ = tap.Write([]byte("a\r\r\nb\n"))
	after := summarize(strict, writeLog(t, "a\r\r\nb\n"))
	assert.Equal(t, after.Tools["kept"], 1)
	assert.Equal(t, atomic.LoadInt64(&cr.tools), int64(1))

	open := steps + `{"type":"step_finish","part":{"cost":2}}` // no newline yet
	var cut pairStat
	tap = &statTap{stat: &cut, scan: statScanner(hd("opencode-openrouter"))}
	_, _ = tap.Write([]byte(open))
	assert.Equal(t, cut.spent(), 1.5)
	assert.Equal(t, summarize(hd("opencode-openrouter"), writeLog(t, open)).CostUSD, 3.5)
}

// a line a script fails on is counted, named in the summary's warning, and
// enough for a summary by itself; the lines around it still count.
func TestSummarizeParseErrors(t *testing.T) {
	strict := tables(t, `
harnesses:
  strict:
    run: "true"
    parse: |
      ev = json.decode(line)
      out.usage(cost_usd = ev["cost"])
`).Harness("strict")
	s := summarize(strict, writeLog(t, `{"cost": 1}`+"\n"+`{"price": 2}`+"\n"+`{"cost": 3}`+"\n"))
	require.That(t, s != nil, "no summary")
	assert.Equal(t, s.CostUSD, 4)
	assert.Equal(t, s.ParseErrors, 1)
	assert.Equal(t, s.parseError, `line 2: key "cost" not in dict (1 line(s) failed)`)

	only := summarize(strict, writeLog(t, `{"price": 2}`+"\n"))
	require.That(t, only != nil, "a failed line alone left no summary")
	assert.Equal(t, only.ParseErrors, 1)
	assert.Equal(t, only.HasCost, false)

	// a negative cost is an error of its line, not spend taken back.
	neg := summarize(hd("opencode-openrouter"), writeLog(t, steps(1, 1)+`{"type":"step_finish","part":{"cost":-0.5}}`+"\n"))
	assert.Equal(t, neg.CostUSD, 1)
	assert.Equal(t, neg.ParseErrors, 1)
}

func TestIsProgressTTY(t *testing.T) {
	assert.Equal(t, isProgressTTY(&bytes.Buffer{}), false) // a buffer is never a tty
	// a character device that is no terminal: `pats run >/dev/null` draws no bar.
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	require.NoError(t, err)
	defer null.Close()
	assert.Equal(t, isProgressTTY(null), false)
	file, err := os.Create(filepath.Join(t.TempDir(), "out"))
	require.NoError(t, err)
	defer file.Close()
	assert.Equal(t, isProgressTTY(file), false)
}

// pats harness parse: each failed line, the summary and the newest reply.
func TestParseLog(t *testing.T) {
	var out bytes.Buffer
	log := `{"type":"result","result":"first","total_cost_usd":1}` + "\n" + `{"type":"result","result":"second","total_cost_usd":-2}` + "\n" + `{"type":"result","result":"third","total_cost_usd":2,"num_turns":3}` + "\n"
	require.NoError(t, ParseLog(hd("claude-cli-keyless"), strings.NewReader(log), &out))
	assert.Equal(t, out.String(), `line 2: out.usage: cost_usd: -2 is not a finite number of at least 0
summary: {"turns":3,"cost_usd":2,"has_cost":true,"parse_errors":1}
reply (the newest of 2 candidates):
third
`)

	out.Reset()
	require.NoError(t, ParseLog(hd("codex-cli-keyless"), strings.NewReader("not codex's\n"), &out))
	assert.Equal(t, out.String(), "summary: none\nreply: none\n")

	bare := tables(t, `harnesses: {bare: {run: "true"}}`).Harness("bare")
	assert.Error(t, ParseLog(bare, strings.NewReader(""), &out), "has no parse script")
}
