package eval

import (
	"fmt"
	"io"
)

type level int

const (
	lvInfo level = iota
	lvWarn
	lvErr
)

var (
	logTags   = [...]string{"INFO ", "WARN ", "ERROR"} // padded to 5 so messages align
	logColors = [...]string{"\033[2m", "\033[33m", "\033[1;31m"}
)

const logReset = "\033[0m"

// logw writes level-tagged lines. tags are coloured only when color is set (a
// real terminal); into a pipe/file/tee it stays plain so ansi never pollutes a
// log. it only formats -- callers own routing (per-pair buffer, bar, terminal).
type logw struct {
	w     io.Writer
	color bool
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
