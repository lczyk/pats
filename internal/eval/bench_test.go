package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/lczyk/pats/internal/machine"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox/proxy"
)

// claudeStream is a claude stream-json stdout as the claude harness's flags
// make it (partial messages on), of about n lines: an init line, then turns
// of a streamed text block, a streamed tool call and the tool's result, and
// the closing result line. it returns the stream and its tool calls.
func claudeStream(n int) (stream []byte, tools int) {
	const tail = `,"session_id":"3f6c2a1e-8b7d-4c55-9e0a-1d2b3c4d5e6f","parent_tool_use_id":null,"uuid":"0b1c2d3e-4f50-4617-8293-a4b5c6d7e8f9"}` + "\n"
	var b bytes.Buffer
	ev := func(format string, args ...any) {
		fmt.Fprintf(&b, format, args...)
		b.WriteString(tail)
	}
	delta := func(index int, kind, field, text string) {
		ev(`{"type":"stream_event","event":{"type":"content_block_delta","index":%d,"delta":{"type":%q,%q:%q}}`, index, kind, field, text)
	}
	assistant := func(turn int, block string) {
		ev(`{"type":"assistant","message":{"id":"msg_%024d","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[%s],`+
			`"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":3,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,`+
			`"output_tokens":%d,"service_tier":"standard"}}`, turn, block, 400+turn, 20000+900*turn, 60+turn%40)
	}
	names := []string{"Bash", "Read", "Edit", "Grep", "Write"}
	const listing = "-rw-r--r-- 1 agent agent  4096 Nov  9 10:21 internal/eval/progress.go\n"

	ev(`{"type":"system","subtype":"init","cwd":"/work","tools":["Task","Bash","Glob","Grep","Read","Edit","Write","WebFetch","TodoWrite"],` +
		`"mcp_servers":[],"model":"claude-sonnet-5-5","permissionMode":"bypassPermissions","apiKeySource":"none"`)
	for turn := 0; bytes.Count(b.Bytes(), []byte("\n")) < n-30; turn++ {
		id := fmt.Sprintf("toolu_%022d", turn)
		ev(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_%024d","type":"message","role":"assistant",`+
			`"model":"claude-sonnet-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":1}}}`, turn)

		ev(`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		text := fmt.Sprintf("the listing for step %d shows the file is there, so next i read it and check the cap before i change anything. ", turn)
		for i := 0; i < 12; i++ {
			delta(0, "text_delta", "text", text[i*len(text)/12:(i+1)*len(text)/12])
		}
		ev(`{"type":"stream_event","event":{"type":"content_block_stop","index":0}`)
		assistant(turn, fmt.Sprintf(`{"type":"text","text":%q}`, text))

		input := fmt.Sprintf(`{"command":"ls -l internal/eval | sed -n %d,%dp","description":"list the files of step %d"}`, turn, turn+40, turn)
		ev(`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`,
			id, names[turn%len(names)])
		for i := 0; i < 8; i++ {
			delta(1, "input_json_delta", "partial_json", input[i*len(input)/8:(i+1)*len(input)/8])
		}
		ev(`{"type":"stream_event","event":{"type":"content_block_stop","index":1}`)
		assistant(turn, fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":%s}`, id, names[turn%len(names)], input))
		tools++

		ev(`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":%d}}`, 60+turn%40)
		ev(`{"type":"stream_event","event":{"type":"message_stop"}`)
		ev(`{"type":"user","message":{"role":"user","content":[{"tool_use_id":%q,"type":"tool_result","content":%q,"is_error":false}]}`,
			id, strings.Repeat(listing, 4+turn%24))
	}
	ev(`{"type":"result","subtype":"success","is_error":false,"duration_ms":412345,"duration_api_ms":159141,"num_turns":%d,"result":"done",`+
		`"total_cost_usd":0.42,"usage":{"input_tokens":412,"cache_creation_input_tokens":50434,"cache_read_input_tokens":2743475,"output_tokens":9213}`, tools)
	return b.Bytes(), tools
}

// what the stdout tap costs a pair: a claude stream written through it in the
// 4 KiB chunks a pipe hands over, parsed for tool calls and only split in
// lines (a harness with no parser).
func BenchmarkStatTap(b *testing.B) {
	stream, tools := claudeStream(2000)
	lines := int64(bytes.Count(stream, []byte("\n")))
	for name, entry := range map[string]*machine.Harness{"claude": hd("claude-cli-keyless"), "no-parser": nil} {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(stream)))
			b.ReportAllocs()
			var gotLines, gotTools int64
			for b.Loop() {
				var s pairStat
				scan := statScanner(entry) // one pass of the script a pair
				tap := &statTap{stat: &s, scan: scan}
				for rest := stream; len(rest) > 0; {
					n := min(len(rest), 4096)
					tap.Write(rest[:n])
					rest = rest[n:]
				}
				gotLines, gotTools = s.out, s.tools
			}
			if gotLines != lines || (entry != nil && gotTools != int64(tools)) {
				b.Fatalf("counted %d lines and %d tools, the stream has %d and %d", gotLines, gotTools, lines, tools)
			}
		})
	}
}

// one repaint of the live region, as the ticker makes eight times a second
// under the progress mutex: the spinners step, then the frame is built and
// written.
func BenchmarkProgressRedraw(b *testing.B) {
	for _, pairs := range []int{8, 64} {
		b.Run(fmt.Sprintf("pairs=%d", pairs), func(b *testing.B) {
			// built by hand: newProgress starts the ticker, which would repaint too.
			p := &progress{out: io.Discard, total: 4 * pairs, done: pairs, began: time.Now()}
			for i := range pairs {
				st := &pairStat{out: int64(1200 + 37*i), tools: int64(11 + i), net: int64(3 * i), netSeen: 1}
				st.addCost(0.01 * float64(i+1))
				p.active = append(p.active, &inflight{
					label: fmt.Sprintf("agent-%02d/task-%02d", i%8, i), stat: st, showTools: true, spinRate: 0.9 + float64(i%20)/100,
				})
			}
			b.ReportAllocs()
			for b.Loop() {
				p.mu.Lock()
				for _, a := range p.active {
					a.spin += a.spinRate
				}
				p.redraw()
				p.mu.Unlock()
			}
			if p.drawn != pairs+1 {
				b.Fatalf("drew %d lines for %d pairs", p.drawn, pairs)
			}
		})
	}
}

// egressLog is an audit log of n events as the proxy writes them: tunnels
// opened and closed, inspected requests, and now and then a refusal of each
// kind. it returns the log and the open and req lines in it.
func egressLog(n int) (log []byte, requests int) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	began := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	for i := range n {
		e := proxy.Event{
			TS: began.Add(time.Duration(i) * 40 * time.Millisecond).Format(time.RFC3339), Port: "443",
			Host: fmt.Sprintf("cdn-%02d.example.com", i%40), Allowed: true,
		}
		switch i % 20 {
		case 0, 7:
			e.Ev, e.Kind, e.Rule, e.IP = "open", "tunnel", ".example.com", "203.0.113.7"
		case 1, 8:
			e.Ev, e.Kind, e.In, e.Out, e.MS = "close", "tunnel", int64(40_000+97*i), int64(900+i%512), int64(120+i%900)
		case 13:
			e.Ev, e.Kind, e.Rule, e.Allowed, e.Status = "open", "tunnel", "default", false, 403
			e.Host = fmt.Sprintf("paste-%02d.example.net", i/20%25)
		case 17:
			e.Ev, e.Kind, e.Rule, e.Reason, e.Allowed, e.Status = "req", "mitm", "github.com/*/answers*", "url-not-allowed", false, 403
			e.Host, e.Method, e.UA = "github.com", "GET", "git/2.47.1"
			e.URL = fmt.Sprintf("github.com/canonical/answers-%02d/info/refs?service=git-upload-pack", i/20%25)
		case 19:
			e.Ev, e.Kind, e.Rule, e.Reason, e.Allowed, e.Status = "open", "tunnel", e.Host, "dial", false, 502
		default:
			e.Ev, e.Kind, e.Rule, e.Method, e.Status = "req", "mitm", ".example.com", "GET", 200
			e.URL = fmt.Sprintf("%s/ubuntu/pool/main/p/pkg-%d/pkg-%d_1.%d-1ubuntu1_amd64.deb", e.Host, i%977, i%977, i%13)
			e.UA, e.In, e.Out, e.MS, e.Revocation = "Debian APT-HTTP/1.3 (3.1.8)", int64(18_000+211*i%90_000), 310, int64(15+i%300), "good"
		}
		if e.Ev != "close" {
			requests++
		}
		if err := enc.Encode(e); err != nil {
			panic(err)
		}
	}
	return b.Bytes(), requests
}

// what folding a pair's audit log costs when the pair ends, as the log
// grows: the file is read whole, once.
func BenchmarkEgressAudit(b *testing.B) {
	for _, events := range []int{100, 10_000, 100_000} {
		b.Run(fmt.Sprintf("events=%d", events), func(b *testing.B) {
			log, requests := egressLog(events)
			path := filepath.Join(b.TempDir(), "egress.log")
			require.NoError(b, os.WriteFile(path, log, 0o644))
			b.SetBytes(int64(len(log)))
			b.ReportAllocs()
			var a egressAudit
			for b.Loop() {
				a = readEgressAudit(path)
			}
			if a.Requests != requests {
				b.Fatalf("folded %d requests, the log has %d", a.Requests, requests)
			}
		})
	}
}

// reportTree lays out runs scored runs under configDir, each of agents x
// tasks pairs with the metadata.json a run leaves, scored by two scripts and
// a judge whose judgement holds one sample.
func reportTree(b *testing.B, configDir string, runs, agents, tasks int) {
	b.Helper()
	write := func(path string, v any) {
		data, err := json.Marshal(v)
		require.NoError(b, err)
		require.NoError(b, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(b, os.WriteFile(path, data, 0o644))
	}
	yes := true
	for r := 1; r <= runs; r++ {
		runDir := filepath.Join(configDir, runsSubdir, fmt.Sprintf("%03d-202611%02d-%s", r, r, generateName(strconv.Itoa(r))))
		var results []ScoreResult
		var pairs []config.TestPair
		for a := range agents {
			agent := fmt.Sprintf("agent-%02d", a)
			for k := range tasks {
				task := fmt.Sprintf("task-%02d", k)
				meta := pairMeta{
					Agent: agent, Harness: "claude-cli-keyless", Model: "claude-sonnet-5-5", Effort: "high", Task: task,
					Sandbox: "sandbox", Driver: "docker", Image: "sha256:" + strings.Repeat("ab", 32),
					DurationS: 60 + float64((a*31+k*17+r)%900), Status: "ok", Synced: &yes, PatsVersion: "1.0.0",
					DeniedEgress: []string{"paste-01.example.net", "paste-02.example.net"},
					Egress: &egressRecord{
						Policy:   proxy.Policy{Allow: []string{".example.com", "github.com/canonical/*"}, Deny: []string{"github.com/*/answers*"}},
						Requests: 240 + k, BytesIn: 9_000_000, BytesOut: 120_000, Enforced: &yes,
					},
					Summary: &runSummary{
						Turns: 20 + k%30, CostUSD: 0.2 + float64(k%7)/10, HasCost: true, InputTokens: 400 + k, OutputTokens: 9000 + 13*k,
						CacheReadTokens: 2_700_000, CacheWriteTokens: 50_000, TTFTms: 3900, APIDurationMs: 159_000,
						Tools: map[string]int{"Bash": 14 + k%9, "Read": 9, "Edit": 4, "Grep": 3},
					},
					Components: map[string]string{"claude": "2.1.319", "codex": "0.48.0"},
				}
				if (a+k)%17 == 0 {
					meta.Status, meta.ExitCode, meta.Error = "nonzero", 1, "the harness exited 1"
				}
				write(filepath.Join(runDir, agent, task, "metadata.json"), meta)
				pairs = append(pairs, config.TestPair{Agent: agent, Task: task})

				score := float64((a*7+k*3+r)%11) / 10
				for _, scorer := range []string{"builds", "tests", "rubric"} {
					results = append(results, ScoreResult{Agent: agent, Task: task, Scorer: scorer, Score: score})
				}
				jd, who := judgementDir(runDir, "rubric", agent, task), judged{Agent: agent, Task: task}
				write(filepath.Join(jd, "verdict.json"), verdict{
					OK: true, Score: &score, Samples: 1, Ran: 1, Numeric: 1, FooterVersion: footerVersion,
					Judge: "judge", Scorer: "rubric", Judged: who,
				})
				write(filepath.Join(jd, "1", "metadata.json"), pairMeta{
					Agent: "judge", Harness: "opencode-openrouter", Model: "openai/gpt-4o", Task: "rubric", Sandbox: "sandbox",
					Driver: "docker", DurationS: 21, Status: "ok", Synced: &yes, PatsVersion: "1.0.0", Judged: &who, Sample: 1,
					Summary: &runSummary{Turns: 3, CostUSD: 0.01, HasCost: true, InputTokens: 5000, OutputTokens: 300},
				})
			}
		}
		require.NoError(b, writeJSON(filepath.Join(runDir, "scores.json"), aggregate(runDir, results, pairs)))
	}
}

// `pats report` over a big matrix: one run of 20 agents x 50 tasks (its
// scores, every pair's metadata.json, every judgement's verdict and sample),
// and the latest 5 such runs together. text is the pivot, json the document.
func BenchmarkReport(b *testing.B) {
	const runs, agents, tasks = 5, 20, 50
	configDir := b.TempDir()
	reportTree(b, configDir, runs, agents, tasks)

	// the tree reads whole and clean before anything is timed.
	var out, warn bytes.Buffer
	require.NoError(b, Report(configDir, "", 0, true, &out, &warn))
	var doc reportDoc
	require.NoError(b, json.Unmarshal(out.Bytes(), &doc))
	if len(doc.Pairs) != agents*tasks || len(doc.Judges) != 1 || doc.Judges[0].Samples != agents*tasks || warn.Len() > 0 {
		b.Fatalf("the tree reads as %d pairs and %d judges, warnings %q", len(doc.Pairs), len(doc.Judges), warn.String())
	}

	for _, n := range []int{1, runs} {
		for _, form := range []string{"text", "json"} {
			b.Run(fmt.Sprintf("runs=%d/%s", n, form), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if err := Report(configDir, "", n, form == "json", io.Discard, io.Discard); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
