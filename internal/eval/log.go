package eval

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
)

type level int

const (
	lvInfo level = iota
	lvWarn
	lvErr
)

var (
	logTags   = [...]string{"INFO", "WARN", "ERROR"}
	logColors = [...]string{"\033[2m", "\033[33m", "\033[1;31m"}
)

const logReset = "\033[0m"

// logw writes level-tagged lines. tags are coloured only when color is set (a
// real terminal); into a pipe/file/tee it stays plain so ansi never pollutes a
// log. it only formats -- callers own routing (per-pair buffer, bar, terminal).
type logw struct {
	w       io.Writer
	color   bool
	verbose int // -v count: detail lines show from 1
}

// line renders a tagged (optionally coloured) line without the trailing newline,
// for callers that route the string themselves (e.g. the progress bar's log()).
func (l logw) line(lv level, f string, a ...any) string {
	tag := logTags[lv]
	if l.color {
		tag = logColors[lv] + tag + logReset
	}
	return tag + " " + fmt.Sprintf(f, a...)
}

func (l logw) info(f string, a ...any)  { fmt.Fprintln(l.w, l.line(lvInfo, f, a...)) }
func (l logw) warn(f string, a ...any)  { fmt.Fprintln(l.w, l.line(lvWarn, f, a...)) }
func (l logw) error(f string, a ...any) { fmt.Fprintln(l.w, l.line(lvErr, f, a...)) }

// lineStream hands each whole line written to it to emit, after prefix:
// -vv's live output of one pair or score among others, or under the bar.
// a script's stdout and stderr may write at once.
type lineStream struct {
	mu     sync.Mutex
	prefix string
	emit   func(string)
	part   []byte
}

func (l *lineStream) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.part = append(l.part, p...)
	for {
		i := bytes.IndexByte(l.part, '\n')
		if i < 0 {
			return len(p), nil
		}
		l.emit(l.prefix + strings.TrimRight(string(l.part[:i]), "\r"))
		l.part = l.part[i+1:]
	}
}

// flush emits a last line with no newline.
func (l *lineStream) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.part) > 0 {
		l.emit(l.prefix + string(l.part))
		l.part = nil
	}
}

// liveStream is where -vv streams a job's output: straight to out when the
// job writes there itself, else line by line above the bar or, among
// parallel jobs, under mu.
func liveStream(label string, bar *progress, out io.Writer, mu *sync.Mutex) *lineStream {
	return &lineStream{prefix: label + " | ", emit: func(s string) {
		if bar != nil {
			bar.log(s)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintln(out, s)
	}}
}

// detail is an info line shown only under -v.
func (l logw) detail(f string, a ...any) {
	if l.verbose >= 1 {
		l.info(f, a...)
	}
}
