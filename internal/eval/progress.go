package eval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"github.com/lczyk/pats/internal/machine"
	"github.com/lczyk/pats/internal/parse"
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
	// size is the terminal's columns and rows, asked for at each frame; nil
	// when out is no terminal.
	size func() (cols, rows int)
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
	tools   int64 // tool_use events (per-harness parse; 0 for harnesses w/o a parser)
	net     int64 // egress requests, from the proxy audit log
	netSeen int32 // 1 once egress.log was readable (i.e. proxy mode) -> show net

	// set once the pair ends, for the run's summary.
	status    string
	tokens    int
	cost      float64
	costKnown bool // the summary reported a cost

	liveCost atomic.Uint64 // float64 bits: spend reported so far, for the bar
	costSeen atomic.Bool   // the harness reported a cost at all, 0 included
	// spend, when set, hears each step's cost as the harness reports it.
	spend func(c float64)
}

// spent is the spend the pair's harness reported so far.
func (s *pairStat) spent() float64 { return math.Float64frombits(s.liveCost.Load()) }

// addCost records a step's cost as the harness reports it.
func (s *pairStat) addCost(c float64) {
	for {
		old := s.liveCost.Load()
		if s.liveCost.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+c)) {
			break
		}
	}
	if s.spend != nil {
		s.spend(c)
	}
}

// cols renders the counter columns "out [tool] [net]" -- tool only when the harness
// has a parser (showTools), net only once the proxy audit log was seen.
func (s *pairStat) cols(showTools bool) string {
	parts := []string{fmt.Sprintf("%5d out", atomic.LoadInt64(&s.out))}
	if showTools {
		parts = append(parts, fmt.Sprintf("%3d tool", atomic.LoadInt64(&s.tools)))
	}
	if atomic.LoadInt32(&s.netSeen) == 1 {
		parts = append(parts, fmt.Sprintf("%4d net", atomic.LoadInt64(&s.net)))
	}
	if c := s.spent(); c > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f", c))
	}
	return strings.Join(parts, "  ")
}

type inflight struct {
	label     string
	stat      *pairStat
	egress    string // path to egress.log, polled for the net count ("" -> skip)
	tail      auditTail
	showTools bool    // the harness has a tool parser -> show the tool column
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
	if f, ok := out.(*os.File); ok {
		p.size = func() (int, int) {
			cols, rows, _ := term.GetSize(int(f.Fd()))
			return cols, rows
		}
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
		tail *auditTail
	}
	p.mu.Lock()
	var items []item
	for _, a := range p.active {
		if a.egress != "" {
			items = append(items, item{a.stat, a.egress, &a.tail})
		}
	}
	p.mu.Unlock()
	for _, it := range items {
		if n, ok := it.tail.count(it.path); ok {
			atomic.StoreInt64(&it.st.net, int64(n))
			atomic.StoreInt32(&it.st.netSeen, 1)
		}
	}
}

// auditTail counts the requests in a growing egress.log, reading at each
// call only what was written since the last. only the ticker calls it.
type auditTail struct {
	off int64
	n   int
}

// count is the open and req lines so far, false while there is no file.
func (t *auditTail) count(path string) (int, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Size() < t.off {
		t.off, t.n = 0, 0 // not the file it was
	}
	if _, err := f.Seek(t.off, io.SeekStart); err != nil {
		return t.n, true
	}
	data, _ := io.ReadAll(f)
	// a line still being written is left for the next call.
	data = data[:bytes.LastIndexByte(data, '\n')+1]
	t.off += int64(len(data))
	for _, line := range bytes.Split(data, []byte("\n")) {
		var e struct {
			Ev string `json:"ev"`
		}
		if json.Unmarshal(line, &e) == nil && (e.Ev == "open" || e.Ev == "req") {
			t.n++
		}
	}
	return t.n, true
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
		lines = append(lines, fmt.Sprintf("  %c %-*s  %s", sp, w, a.label, a.stat.cols(a.showTools)))
	}
	if p.size == nil {
		return lines
	}
	cols, rows := p.size()
	return fitRegion(lines, cols, rows)
}

// fitRegion cuts the region to a terminal of cols by rows, zero for a side
// not known. a redraw moves the cursor up by the lines it drew: a line that
// wraps, or a region taller than the screen, leaves it somewhere else. so
// each line is cut short of the last column, and the lines to what leaves
// the row under them on screen, the last one saying how many it stands for.
func fitRegion(lines []string, cols, rows int) []string {
	if keep := rows - 2; keep >= 1 && len(lines) > keep+1 {
		lines = append(lines[:keep:keep], fmt.Sprintf("  ... and %d more", len(lines)-keep))
	}
	if cols > 1 {
		for i, l := range lines {
			if r := []rune(l); len(r) > cols-1 {
				lines[i] = string(r[:cols-1])
			}
		}
	}
	return lines
}

func mmss(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

// isProgressTTY reports whether w is a terminal a bar can be drawn on. a
// *bytes.Buffer (tests), a pipe or a file (ci, tee) is not, and neither is
// /dev/null, a character device like a terminal.
func isProgressTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// useColor reports whether to colour log tags: only on a tty and only when
// NO_COLOR is unset (https://no-color.org -- present + non-empty disables).
func useColor(w io.Writer) bool {
	return isProgressTTY(w) && os.Getenv("NO_COLOR") == ""
}

// statTapMaxBuf caps how many bytes of an unterminated line statTap will
// retain: a harness that emits one huge newline-less blob (or just corrupt
// output) must not grow buf without bound. match the 8 MiB ceiling the
// post-run summary scanners already allow a single line (claude's
// stream-json lines carry base64 signatures) rather than invent a smaller
// cap that could clip a legitimate one. past the cap, extra bytes are
// dropped -- the retained (truncated) line then fails its per-harness json
// scan and is silently skipped (same as any other malformed line), but the
// eventual newline still counts as one "out" line, so the running total
// stays accurate even though the tool/harness breakdown for that one line is lost.
const statTapMaxBuf = 8 * 1024 * 1024

// statTap taps a pair's stdout: it splits the stream into lines (buffering across
// writes), counts each as an "out" line, and runs the per-harness scanner on it. it
// does not disturb the stdout.log written alongside via a MultiWriter. buf is
// capped at statTapMaxBuf -- bytes past the cap are dropped, though the eventual
// newline still counts as one out line.
type statTap struct {
	stat *pairStat
	scan func([]byte, *pairStat) // per-harness line parser; nil -> count lines only
	buf  []byte
}

func (t *statTap) Write(b []byte) (int, error) {
	n := len(b)
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		end := len(b)
		if i >= 0 {
			end = i
		}
		if room := statTapMaxBuf - len(t.buf); room > 0 {
			take := min(end, room)
			t.buf = append(t.buf, b[:take]...)
		}
		if i < 0 {
			break // still mid-line -- wait for more writes
		}
		atomic.AddInt64(&t.stat.out, 1)
		if t.scan != nil {
			t.scan(t.buf, t.stat)
		}
		t.buf = t.buf[:0]
		b = b[i+1:]
	}
	return n, nil
}

// statScanner returns the live reader of a harness's stdout lines: a fresh
// pass of its parse script that keeps the pair's tool count and its spend
// current. nil for a harness with no script (those get the out count only).
func statScanner(hd *machine.Harness) func([]byte, *pairStat) {
	p := hd.Parser()
	if p == nil {
		return nil
	}
	pass := p.NewPass()
	var cost float64
	return func(line []byte, s *pairStat) {
		_ = pass.Line(line) // the pass after the pair reports a failed line
		r := pass.Result()
		atomic.StoreInt64(&s.tools, int64(r.ToolCalls))
		if r.HasCost {
			s.costSeen.Store(true)
		}
		if r.CostUSD != cost {
			d := r.CostUSD - cost
			cost = r.CostUSD
			s.addCost(d)
		}
	}
}

// runSummary is the post-run digest extracted from a harness's log, stored in
// metadata.json. fields are per-harness best-effort -- zero/empty when absent, so
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
	ReasoningTokens     int            `json:"reasoning_tokens,omitempty"` // the reasoning part of OutputTokens, where the log splits it out
	PremiumRequests     int            `json:"premium_requests,omitempty"` // copilot's billing unit -- its cost analog
	TTFTms              int            `json:"ttft_ms,omitempty"`
	APIDurationMs       int            `json:"api_duration_ms,omitempty"`
	IsError             bool           `json:"is_error,omitempty"`
	Tools               map[string]int `json:"tools,omitempty"` // tool_use counts by name
	// ParseErrors counts the lines the harness's parse script failed on:
	// above 0, the rest of the summary is what the other lines said.
	ParseErrors int `json:"parse_errors,omitempty"`

	parseError string // the first failed line, for the pair's warning
}

// readLog runs a fresh pass of a parse script over a finished stdout log. a
// line past the scanner's cap fails the whole read: part of a log is not
// taken for all of it.
func readLog(p *parse.Program, path string) (*parse.Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	pass := p.NewPass()
	sc := logScanner(f)
	for sc.Scan() {
		_ = pass.Line(sc.Bytes()) // counted in the result
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return pass.Result(), nil
}

// logScanner reads a stdout log line by line as the live tap frames them: a
// line ends at its newline and keeps whatever is before it (the pass drops
// one carriage return, for both), and may be as long as the tap holds.
func logScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), statTapMaxBuf+1) // lines carry base64 sigs -- allow big
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	return sc
}

// summarize extracts a runSummary from a finished pair's stdout log, by its
// harness's parse script. nil for a harness with no script, for a log that
// cannot be read whole, and for one the script heard nothing in: an empty
// log, or another format's. a failed line is enough for a summary, so its
// count always has a place.
func summarize(hd *machine.Harness, stdoutPath string) *runSummary {
	p := hd.Parser()
	if p == nil {
		return nil
	}
	r, err := readLog(p, stdoutPath)
	if err != nil {
		return nil
	}
	return summaryOf(r)
}

// summaryOf is what a pass heard as a summary, nil when it heard nothing.
func summaryOf(r *parse.Result) *runSummary {
	if !r.Said && r.Errors == 0 {
		return nil
	}
	sum := &runSummary{
		NumTurns: r.Turns, CostUSD: r.CostUSD, HasCost: r.HasCost,
		InputTokens: r.Input, OutputTokens: r.Output, CacheReadTokens: r.CacheRead, CacheCreationTokens: r.CacheWrite,
		ReasoningTokens: r.Reasoning, PremiumRequests: r.PremiumRequests,
		TTFTms: r.TTFTms, APIDurationMs: r.APIms, IsError: r.IsError,
		Tools: r.Tools, ParseErrors: r.Errors,
	}
	if r.Errors > 0 {
		sum.parseError = fmt.Sprintf("line %d: %s (%d line(s) failed)", r.FirstErrorLine, r.FirstError, r.Errors)
	}
	return sum
}
