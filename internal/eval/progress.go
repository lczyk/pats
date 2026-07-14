package eval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// progress is a sticky multi-line terminal display for a parallel run: a total
// bar (with one run-elapsed timer) plus one live line per in-flight pair showing
// its activity counters. it is used only on a tty -- non-tty runs leave bar nil
// and print plain per-pair lines instead. every terminal write goes through it
// under one mutex so concurrent workers + the redraw ticker never interleave.
type progress struct {
	mu        sync.Mutex
	out       io.Writer
	total     int
	done      int
	labelW    int         // max pair-label width, for column alignment
	began     time.Time   // run start, for the single elapsed timer
	active    []*inflight // in start order
	drawn     int         // region lines currently on screen
	ticker    *time.Ticker
	pollEvery int // refresh the net count every N redraw ticks (it's pricier i/o)
	stop      chan struct{}
	donec     chan struct{} // closed when the ticker goroutine has exited
}

// redraw fast for a smooth spinner/timer; poll egress.log (per-pair file i/o)
// much less often.
const (
	redrawEvery  = 125 * time.Millisecond
	netPollEvery = 2 * time.Second
)

// pairStat holds the live in-run counters for one pair. all fields are atomic:
// out/tools are bumped by the stdout tap (a worker goroutine), net by the
// ticker's egress-log poll, and all are read by the renderer.
type pairStat struct {
	out     int64 // stdout lines (stream-json events)
	tools   int64 // tool_use events (per-kind parse; 0 for kinds w/o a parser)
	net     int64 // egress requests, from the proxy audit log
	netSeen int32 // 1 once egress.log was readable (i.e. proxy mode) -> show net
}

// cols renders the counter columns "out [tool] [net]" -- tool only when the kind
// has a parser (showTools), net only once the proxy audit log was seen.
func (s *pairStat) cols(showTools bool) string {
	parts := []string{fmt.Sprintf("%5d out", atomic.LoadInt64(&s.out))}
	if showTools {
		parts = append(parts, fmt.Sprintf("%3d tool", atomic.LoadInt64(&s.tools)))
	}
	if atomic.LoadInt32(&s.netSeen) == 1 {
		parts = append(parts, fmt.Sprintf("%4d net", atomic.LoadInt64(&s.net)))
	}
	return strings.Join(parts, "  ")
}

type inflight struct {
	label     string
	stat      *pairStat
	egress    string  // path to egress.log, polled for the net count ("" -> skip)
	showTools bool    // the agent kind has a tool parser -> show the tool column
	spin      float64 // spinner accumulator, advanced by spinRate each tick
	spinRate  float64 // per-pair speed (jittered) so spinners slowly desync
}

// braille dots spinner, \u-escaped to keep this source ascii-only (it's runtime
// terminal UI, not prose -- the runes render as the usual spinning dots).
var spinner = []rune("\u280b\u2819\u2839\u2838\u283c\u2834\u2826\u2827\u2807\u280f")

// newProgress draws an initial frame and starts the redraw ticker (so elapsed +
// spinner advance even while a pair is silent; the net count refreshes less
// often, every pollEvery ticks). close() stops it.
func newProgress(out io.Writer, total, labelW int) *progress {
	p := &progress{
		out: out, total: total, labelW: labelW, began: time.Now(),
		pollEvery: max(int(netPollEvery/redrawEvery), 1),
		stop:      make(chan struct{}), donec: make(chan struct{}),
		ticker: time.NewTicker(redrawEvery),
	}
	p.mu.Lock()
	p.redraw()
	p.mu.Unlock()
	go func() {
		defer close(p.donec)
		tick := 0
		for {
			select {
			case <-p.stop:
				return
			case <-p.ticker.C:
				tick++
				if tick%p.pollEvery == 0 {
					p.refreshNet() // pricier file i/o -- only every netPollEvery
				}
				p.mu.Lock()
				for _, a := range p.active {
					a.spin += a.spinRate // each at its own rate -> they slowly desync
				}
				p.redraw()
				p.mu.Unlock()
			}
		}
	}()
	return p
}

// start registers an in-flight pair. its stat is shared with the stdout tap.
func (p *progress) start(label string, stat *pairStat, egress string, showTools bool) {
	p.mu.Lock()
	p.active = append(p.active, &inflight{
		label: label, stat: stat, egress: egress, showTools: showTools,
		spinRate: 0.9 + rand.Float64()*0.2, // ~+-10% so spinners drift apart
	})
	p.redraw()
	p.mu.Unlock()
}

// finish removes a pair, prints its accumulated log above the bar, and bumps the
// completed count.
func (p *progress) finish(label, log string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, a := range p.active {
		if a.label == label {
			p.active = append(p.active[:i], p.active[i+1:]...)
			break
		}
	}
	p.done++
	p.emitLocked(log)
}

// log prints a one-off message above the bar.
func (p *progress) log(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.emitLocked(s)
}

func (p *progress) close() {
	p.ticker.Stop()
	close(p.stop)
	<-p.donec // wait for the ticker goroutine so no write races the caller
	p.mu.Lock()
	p.clearLocked() // leave the scrolled logs, remove the live region
	p.mu.Unlock()
}

// refreshNet re-counts each active pair's egress.log (proxy audit, one line per
// request). file i/o is done outside the lock; only a snapshot is taken under it.
func (p *progress) refreshNet() {
	type item struct {
		st   *pairStat
		path string
	}
	p.mu.Lock()
	var items []item
	for _, a := range p.active {
		if a.egress != "" {
			items = append(items, item{a.stat, a.egress})
		}
	}
	p.mu.Unlock()
	for _, it := range items {
		if n, err := countLines(it.path); err == nil {
			atomic.StoreInt64(&it.st.net, int64(n))
			atomic.StoreInt32(&it.st.netSeen, 1)
		}
	}
}

// emitLocked prints s above the region, then redraws the region -- in a single
// write so the terminal never paints a half-cleared frame. caller holds mu.
func (p *progress) emitLocked(s string) {
	var b strings.Builder
	if p.drawn > 0 {
		fmt.Fprintf(&b, "\r\033[%dA\033[J", p.drawn) // to region top, erase it (s scrolls in its place)
	}
	if s != "" {
		b.WriteString(s)
		if !strings.HasSuffix(s, "\n") {
			b.WriteByte('\n')
		}
	}
	p.drawn = p.writeRegion(&b)
}

func (p *progress) clearLocked() {
	if p.drawn > 0 {
		fmt.Fprintf(p.out, "\r\033[%dA\033[J", p.drawn) // up N lines, clear to end of screen
		p.drawn = 0
	}
}

// redraw repaints the region in place: one write, overwriting each line (\033[K
// clears any trailing leftovers) rather than erasing first -- so there's no
// blank intermediate frame to flash.
func (p *progress) redraw() {
	var b strings.Builder
	if p.drawn > 0 {
		fmt.Fprintf(&b, "\r\033[%dA", p.drawn) // back to region top, no erase
	}
	p.drawn = p.writeRegion(&b)
}

// writeRegion appends the region lines to b (each cleared to EOL), wipes any
// lines left over from a taller previous frame, flushes b in one write, and
// returns the new line count.
func (p *progress) writeRegion(b *strings.Builder) int {
	lines := p.regionLines()
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\033[K\n") // overwrite content + clear rest of line
	}
	if p.drawn > len(lines) {
		b.WriteString("\033[J") // region shrank: wipe the now-stale lines below
	}
	io.WriteString(p.out, b.String()) // single write per frame -> no mid-frame paint
	return len(lines)
}

func (p *progress) regionLines() []string {
	const width = 20
	filled := 0
	if p.total > 0 {
		filled = p.done * width / p.total
	}
	bar := strings.Repeat("#", filled) + strings.Repeat("-", width-filled)
	lines := []string{fmt.Sprintf("[%s] %d/%d  %d running  %s",
		bar, p.done, p.total, len(p.active), mmss(time.Since(p.began)))}
	w := 0
	for _, a := range p.active {
		if len(a.label) > w {
			w = len(a.label)
		}
	}
	for _, a := range p.active {
		sp := spinner[int(a.spin)%len(spinner)]
		lines = append(lines, fmt.Sprintf("  %c [%-*s]  %s", sp, w, a.label, a.stat.cols(a.showTools)))
	}
	return lines
}

func mmss(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

func countLines(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return bytes.Count(b, []byte{'\n'}), nil
}

// isProgressTTY reports whether w is a terminal a bar can be drawn on (a char
// device). a *bytes.Buffer (tests) or a pipe/file (ci, tee) is not.
func isProgressTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// useColor reports whether to colour log tags: only on a tty and only when
// NO_COLOR is unset (https://no-color.org -- present + non-empty disables).
func useColor(w io.Writer) bool {
	return isProgressTTY(w) && os.Getenv("NO_COLOR") == ""
}

// statTap taps a pair's stdout: it splits the stream into lines (buffering across
// writes), counts each as an "out" line, and runs the per-kind scanner on it. it
// does not disturb the stdout.log written alongside via a MultiWriter.
type statTap struct {
	stat *pairStat
	scan func([]byte, *pairStat) // per-kind line parser; nil -> count lines only
	buf  []byte
}

func (t *statTap) Write(b []byte) (int, error) {
	t.buf = append(t.buf, b...)
	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		atomic.AddInt64(&t.stat.out, 1)
		if t.scan != nil {
			t.scan(t.buf[:i], t.stat)
		}
		t.buf = t.buf[i+1:]
	}
	return len(b), nil
}

// statScanner returns the per-kind stdout line parser, or nil for kinds whose
// stream format pats can't read (those get the out count only).
//
// NOTE: this is the one harness-coupled spot -- a stream-format change breaks
// here, not the run. add a case per new kind.
func statScanner(kind string) func([]byte, *pairStat) {
	switch kind {
	case "claude-cli-keyless":
		return scanClaude
	case "codex-cli-keyless":
		return scanCodex
	case "copilot-cli-keyless":
		return scanCopilot
	case "opencode-openrouter":
		return scanOpencode
	}
	return nil
}

// claudeEvent is one line of claude-code's stream-json -- the single place the
// format is spelled out; scanClaude (live counters) and claudeSummary (post-run
// digest) both decode into it. each assistant turn arrives as a top-level
// {"type":"assistant","message":{"content":[...]}} object; a tool call is a
// content block with type "tool_use". (the raw API content_block_start events
// are nested under a "stream_event" envelope when --include-partial-messages is
// set -- the assistant message-level events are present either way, so we key
// off those.) the totals arrive on the final {"type":"result"} event.
type claudeEvent struct {
	Type    string `json:"type"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
	NumTurns int     `json:"num_turns"`
	CostUSD  float64 `json:"total_cost_usd"`
	TTFTms   int     `json:"ttft_ms"`
	APIms    int     `json:"duration_api_ms"`
	IsError  bool    `json:"is_error"`
	Usage    struct {
		Input         int `json:"input_tokens"`
		Output        int `json:"output_tokens"`
		CacheRead     int `json:"cache_read_input_tokens"`
		CacheCreation int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// scanClaude counts tool calls in claude-code's stream-json (see claudeEvent).
func scanClaude(line []byte, s *pairStat) {
	var ev claudeEvent
	if json.Unmarshal(line, &ev) != nil || ev.Type != "assistant" {
		return
	}
	for _, c := range ev.Message.Content {
		if c.Type == "tool_use" {
			atomic.AddInt64(&s.tools, 1)
		}
	}
}

// opencodeEvent is one line of `opencode run --format json` output -- the
// single place that format is spelled out; scanOpencode (live counters) and
// opencodeSummary (post-run digest) both decode into it. emitted types:
// tool_use (a completed/errored tool part, name in part.tool), step_start,
// step_finish (per-step usage: cost + tokens), text, reasoning (only with
// --thinking), error. usage is per step, so totals are summed across
// step_finish events.
type opencodeEvent struct {
	Type string `json:"type"`
	Part struct {
		Tool   string  `json:"tool"`
		Cost   float64 `json:"cost"`
		Tokens struct {
			Input     int `json:"input"`
			Output    int `json:"output"`
			Reasoning int `json:"reasoning"`
			Cache     struct {
				Read  int `json:"read"`
				Write int `json:"write"`
			} `json:"cache"`
		} `json:"tokens"`
	} `json:"part"`
}

// scanOpencode counts tool calls in opencode's json event stream (see
// opencodeEvent).
func scanOpencode(line []byte, s *pairStat) {
	var ev opencodeEvent
	if json.Unmarshal(line, &ev) == nil && ev.Type == "tool_use" {
		atomic.AddInt64(&s.tools, 1)
	}
}

// codexEvent is one line of `codex exec --json`. tool-like work is emitted as
// completed items; final token totals arrive on turn.completed. a chatgpt-account
// run does not expose a per-run dollar cost.
//
// the usage counters nest: cached_input_tokens is a slice of input_tokens, and
// reasoning_output_tokens a slice of output_tokens (claude's usage nests the
// same way for reasoning, but keeps its cache reads out of input_tokens).
type codexEvent struct {
	Type string `json:"type"`
	Item struct {
		Type   string `json:"type"`
		Server string `json:"server"`
		Tool   string `json:"tool"`
	} `json:"item"`
	Usage struct {
		Input     int `json:"input_tokens"`
		Cached    int `json:"cached_input_tokens"`
		Output    int `json:"output_tokens"`
		Reasoning int `json:"reasoning_output_tokens"`
	} `json:"usage"`
}

func codexToolName(ev codexEvent) string {
	if ev.Type != "item.completed" {
		return ""
	}
	switch ev.Item.Type {
	case "command_execution", "file_change", "web_search":
		return ev.Item.Type
	case "mcp_tool_call":
		if ev.Item.Server != "" && ev.Item.Tool != "" {
			return "mcp:" + ev.Item.Server + "/" + ev.Item.Tool
		}
		return ev.Item.Type
	default:
		return ""
	}
}

// scanCodex counts completed tool-like items, avoiding double-counting their
// matching item.started events.
func scanCodex(line []byte, s *pairStat) {
	var ev codexEvent
	if json.Unmarshal(line, &ev) == nil && codexToolName(ev) != "" {
		atomic.AddInt64(&s.tools, 1)
	}
}

// copilotEvent is one line of `copilot -p --output-format json` -- the single
// place that format is spelled out; scanCopilot (live counters) and
// copilotSummary (post-run digest) both decode into it. tool calls appear as
// tool.execution_start (the only tool event carrying the name; the matching
// tool.execution_complete doesn't). assistant.turn_end marks a model
// round-trip, assistant.message carries per-message outputTokens (the stream
// reports no input or cache counts), and the final "result" event carries the
// exit code plus session usage. streaming/delta noise is ephemeral and decodes
// to types we ignore.
type copilotEvent struct {
	Type string `json:"type"`
	Data struct {
		ToolName     string `json:"toolName"`
		OutputTokens int    `json:"outputTokens"`
	} `json:"data"`
	ExitCode *int `json:"exitCode"` // "result" only; pointer so 0 counts as reported
	Usage    struct {
		PremiumRequests    int `json:"premiumRequests"`
		TotalAPIDurationMs int `json:"totalApiDurationMs"`
	} `json:"usage"`
}

// scanCopilot counts tool executions in copilot's json event stream (see
// copilotEvent).
func scanCopilot(line []byte, s *pairStat) {
	var ev copilotEvent
	if json.Unmarshal(line, &ev) == nil && ev.Type == "tool.execution_start" {
		atomic.AddInt64(&s.tools, 1)
	}
}

// runSummary is the post-run digest extracted from a harness's log, stored in
// metadata.json. fields are per-kind best-effort -- zero/empty when absent, so
// they carry the same meaning whichever harness produced them: InputTokens
// never includes the cache-read half, NumTurns counts model round-trips (0 when
// the stream doesn't say), and HasCost tells the caller a zero CostUSD is a
// real zero rather than an unreported one.
type runSummary struct {
	NumTurns            int            `json:"num_turns,omitempty"`
	CostUSD             float64        `json:"cost_usd,omitempty"`
	HasCost             bool           `json:"has_cost"` // false when the harness reports no dollar cost at all
	InputTokens         int            `json:"input_tokens,omitempty"`
	OutputTokens        int            `json:"output_tokens,omitempty"`
	CacheReadTokens     int            `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int            `json:"cache_creation_tokens,omitempty"`
	ReasoningTokens     int            `json:"reasoning_tokens,omitempty"` // opencode splits these out; claude + codex fold them into output
	PremiumRequests     int            `json:"premium_requests,omitempty"` // copilot's billing unit -- its cost analog
	TTFTms              int            `json:"ttft_ms,omitempty"`
	APIDurationMs       int            `json:"api_duration_ms,omitempty"`
	IsError             bool           `json:"is_error,omitempty"`
	Tools               map[string]int `json:"tools,omitempty"` // tool_use counts by name
}

// summarize extracts a runSummary from a finished pair's stdout log, per agent
// kind. nil for kinds whose log format pats can't read.
//
// NOTE: harness-coupled, like statScanner -- a stream-format change breaks here.
func summarize(kind, stdoutPath string) *runSummary {
	switch kind {
	case "claude-cli-keyless":
		return claudeSummary(stdoutPath)
	case "codex-cli-keyless":
		return codexSummary(stdoutPath)
	case "copilot-cli-keyless":
		return copilotSummary(stdoutPath)
	case "opencode-openrouter":
		return opencodeSummary(stdoutPath)
	}
	return nil
}

// codexSummary parses codex's json event stream (see codexEvent): tool names
// from completed items, tokens from turn.completed. turn.failed and error both
// mark the summary as failed even when the process itself exited cleanly.
//
// NumTurns stays 0: `codex exec` is a single turn by construction, so counting
// turn.completed events would print a constant 1 next to claude's and opencode's
// real round-trip counts. the stream carries no round-trip marker to use instead.
// CostUSD stays 0 for the same honesty -- a keyless (chatgpt-account) run is
// billed against the subscription, never priced per run.
func codexSummary(path string) *runSummary {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	sum := &runSummary{Tools: map[string]int{}}
	hasEvent := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var ev codexEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "item.completed":
			if name := codexToolName(ev); name != "" {
				sum.Tools[name]++
				hasEvent = true
			}
		case "turn.completed":
			// cached is a slice of input, so subtract to match the other kinds.
			sum.InputTokens += ev.Usage.Input - ev.Usage.Cached
			sum.CacheReadTokens += ev.Usage.Cached
			sum.OutputTokens += ev.Usage.Output
			sum.ReasoningTokens += ev.Usage.Reasoning
			hasEvent = true
		case "turn.failed", "error":
			sum.IsError = true
			hasEvent = true
		}
	}
	if !hasEvent {
		return nil
	}
	return sum
}

// copilotSummary parses copilot's json event stream (see copilotEvent): tool
// names from tool.execution_start, turns counted as assistant.turn_end events
// (real model round-trips), output tokens summed over assistant.message.
// input/cache token counts stay 0 -- the stream doesn't report them. CostUSD
// stays 0 like codex's (a copilot run is billed in premium requests against
// the subscription, recorded in PremiumRequests instead); IsError comes from
// the final result event's exit code.
func copilotSummary(path string) *runSummary {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	sum := &runSummary{Tools: map[string]int{}}
	hasEvent := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var ev copilotEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "tool.execution_start":
			sum.Tools[ev.Data.ToolName]++
			hasEvent = true
		case "assistant.turn_end":
			sum.NumTurns++
			hasEvent = true
		case "assistant.message":
			sum.OutputTokens += ev.Data.OutputTokens
			hasEvent = true
		case "result":
			sum.PremiumRequests = ev.Usage.PremiumRequests
			sum.APIDurationMs = ev.Usage.TotalAPIDurationMs
			sum.IsError = ev.ExitCode != nil && *ev.ExitCode != 0
			hasEvent = true
		}
	}
	if !hasEvent {
		return nil // empty/garbage log -> nothing worth storing
	}
	return sum
}

// opencodeSummary parses opencode's json event stream (see opencodeEvent):
// tool names from tool_use events, cost + tokens summed over step_finish
// events (one per model round-trip, so their count doubles as num_turns).
func opencodeSummary(path string) *runSummary {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	sum := &runSummary{Tools: map[string]int{}}
	steps := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var ev opencodeEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "tool_use":
			sum.Tools[ev.Part.Tool]++
		case "step_finish":
			steps++
			sum.HasCost = true
			sum.CostUSD += ev.Part.Cost
			sum.InputTokens += ev.Part.Tokens.Input
			sum.OutputTokens += ev.Part.Tokens.Output
			sum.ReasoningTokens += ev.Part.Tokens.Reasoning
			sum.CacheReadTokens += ev.Part.Tokens.Cache.Read
			sum.CacheCreationTokens += ev.Part.Tokens.Cache.Write
		case "error":
			sum.IsError = true
		}
	}
	sum.NumTurns = steps
	if steps == 0 && len(sum.Tools) == 0 && !sum.IsError {
		return nil // empty/garbage log -> nothing worth storing
	}
	return sum
}

// claudeSummary parses claude-code stream-json (see claudeEvent): tool_use
// names from each assistant turn, and the totals (cost, tokens, turns, ttft)
// from the final "result" event.
func claudeSummary(path string) *runSummary {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	sum := &runSummary{Tools: map[string]int{}}
	hasResult := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // lines carry base64 sigs -- allow big
	for sc.Scan() {
		var ev claudeEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "assistant":
			for _, c := range ev.Message.Content {
				if c.Type == "tool_use" {
					sum.Tools[c.Name]++
				}
			}
		case "result":
			hasResult = true
			sum.NumTurns = ev.NumTurns
			sum.HasCost = true
			sum.CostUSD = ev.CostUSD
			sum.TTFTms = ev.TTFTms
			sum.APIDurationMs = ev.APIms
			sum.IsError = ev.IsError
			sum.InputTokens = ev.Usage.Input
			sum.OutputTokens = ev.Usage.Output
			sum.CacheReadTokens = ev.Usage.CacheRead
			sum.CacheCreationTokens = ev.Usage.CacheCreation
		}
	}
	if !hasResult && len(sum.Tools) == 0 {
		return nil // empty/garbage log -> nothing worth storing
	}
	return sum
}
