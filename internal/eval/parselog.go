package eval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"github.com/lczyk/pats/internal/machine"
)

// ParseLog runs a harness's parse script over a log as the pass after a pair
// does, and prints what it heard: each line it failed on, the summary and
// the reply (docs/spec/harness-parse.md, "trying one").
func ParseLog(hd *machine.Harness, in io.Reader, out io.Writer) error {
	p := hd.Parser()
	if p == nil {
		return fmt.Errorf("harness %q has no parse script: no summary, and its whole stdout is its reply", hd.Name)
	}
	pass := p.NewPass()
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), statTapMaxBuf)
	for n := 1; sc.Scan(); n++ {
		if err := pass.Line(sc.Bytes()); err != nil {
			fmt.Fprintf(out, "line %d: %v\n", n, err)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	r := pass.Result()
	if sum := summaryOf(r); sum == nil {
		fmt.Fprintln(out, "summary: none")
	} else {
		b, err := json.Marshal(sum)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "summary: %s\n", b)
	}
	replies := r.Replies()
	if len(replies) == 0 {
		fmt.Fprintln(out, "reply: none")
		return nil
	}
	if len(replies) > 1 {
		fmt.Fprintf(out, "reply (the newest of %d candidates):\n", len(replies))
	} else {
		fmt.Fprintln(out, "reply:")
	}
	fmt.Fprintln(out, replies[0])
	return nil
}
