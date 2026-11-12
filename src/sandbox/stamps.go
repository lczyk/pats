package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// stampScript is a shell script that prints name=<version line> for each of
// clis the image has, skipping those it lacks, and exits 3 when one it has
// cannot say its version. the line is the first of its --version output that
// holds an x.y.z: a cli may say something else first (one that unpacks itself
// on its first run does), and the first line when none does. a name that is
// not a cli's (letters, digits and -) is left out: it goes into a shell.
func stampScript(clis []string) string {
	var names []string
	for _, c := range clis {
		if cliNameRe.MatchString(c) {
			names = append(names, c)
		}
	}
	if len(names) == 0 {
		return ":"
	}
	return "for c in " + strings.Join(names, " ") + `; do
  command -v "$c" >/dev/null 2>&1 || continue
  v=$("$c" --version 2>&1) || { echo "$c --version failed: $v" >&2; exit 3; }
  l=$(printf '%s\n' "$v" | while IFS= read -r l; do case "$l" in (*[0-9].[0-9]*.[0-9]*) printf '%s\n' "$l"; break ;; esac; done)
  [ -n "$l" ] || l=$(printf '%s\n' "$v" | head -n1)
  printf '%s=%s\n' "$c" "$l"
done`
}

var cliNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// wanted is stamps narrowed to clis: what was asked for, and no more.
func wanted(stamps map[string]string, clis []string) map[string]string {
	out := map[string]string{}
	for _, c := range clis {
		if v, ok := stamps[c]; ok {
			out[c] = v
		}
	}
	return out
}

// covers is whether stamps kept from an earlier ask answer for every one of
// clis. a cli they lack was absent then, or not asked about: the image has
// to say which.
func covers(stamps map[string]string, clis []string) bool {
	for _, c := range clis {
		if _, ok := stamps[c]; !ok {
			return false
		}
	}
	return true
}

// componentProp prefixes the lxd image properties holding the stamps;
// stampedProp marks an image pats stamped, so one with no harness is not
// stamped again.
const (
	componentProp = "pats.component."
	stampedProp   = "pats.stamped"
)

var versionRe = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]*[0-9A-Za-z])?`)

// parseStamps reads stampScript's output into name -> version; a cli
// whose version line holds no version is an error.
func parseStamps(out string) (map[string]string, error) {
	stamps := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		name, rest, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || name == "" {
			continue
		}
		v := versionRe.FindString(rest)
		if v == "" {
			return nil, fmt.Errorf("%s --version said %q, no version in it", name, rest)
		}
		stamps[name] = v
	}
	return stamps, nil
}

// stamps runs stampScript in a throwaway container of image id. an
// image with no shell has no stamps; any other failure is an error. the
// container is named, so one a timeout or a failure leaves behind is
// removed.
func (c *container) stamps(ctx context.Context, id string, clis []string, timeout time.Duration) (map[string]string, error) {
	if len(clis) == 0 {
		return map[string]string{}, nil
	}
	if err := c.p.holdLease(); err != nil {
		return nil, err
	}
	r := c.r
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	name := "pats-stamp-" + hex.EncodeToString(suffix[:])
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out, errb bytes.Buffer
	args := append([]string{"run", "--rm", "--name", name}, c.p.runLabels()...)
	args = append(args, "--network", "none", "--entrypoint", "sh", id, "-c", stampScript(clis))
	err := r.Run(rctx, nil, &out, &errb, args...)
	if err == nil {
		return parseStamps(out.String())
	}
	cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
	_ = r.Run(cctx, nil, io.Discard, io.Discard, "rm", "-f", name)
	ccancel()
	var code interface{ ExitCode() int }
	if errors.As(err, &code) && (code.ExitCode() == 126 || code.ExitCode() == 127) && rctx.Err() == nil {
		return map[string]string{}, nil // no shell to run the stamp with
	}
	return nil, fmt.Errorf("stamping %s: %w%s", id, err, stderrTail(&errb))
}

// imageStamps is the component stamps of an lxd image in the pats project,
// for clis: the properties a pats build published it with when they answer
// for each, or what stampScript says in a throwaway instance of it. l is
// ready.
func (l *lxd) imageStamps(ctx context.Context, fp string, clis []string) (map[string]string, error) {
	if len(clis) == 0 {
		return map[string]string{}, nil
	}
	var im struct {
		Properties map[string]string `json:"properties"`
	}
	if err := l.query(ctx, "/1.0/images/"+fp+"?project="+lxdProject, &im); err != nil {
		return nil, err
	}
	stamps := map[string]string{}
	for k, v := range im.Properties {
		if name, ok := strings.CutPrefix(k, componentProp); ok {
			stamps[name] = v
		}
	}
	if im.Properties[stampedProp] == "" || !covers(stamps, clis) {
		return l.runStamp(ctx, fp, clis)
	}
	return wanted(stamps, clis), nil
}

// runStamp runs stampScript in a throwaway instance of image fp, with no
// network, and removes the instance.
func (l *lxd) runStamp(ctx context.Context, fp string, clis []string) (stamps map[string]string, err error) {
	ctx, cancel := context.WithTimeout(ctx, l.p.t.stamp)
	defer cancel()
	s := &lxdSession{l: l, name: "pats-stamp-" + l.p.token + "-" + fp[:min(12, len(fp))]}
	defer func() {
		if derr := l.deleteInstance(s.name); derr != nil && err == nil {
			err = fmt.Errorf("stamp instance %s was left behind: %w", s.name, derr)
		}
	}()
	// made, then started: no nic, as an `egress: none` sandbox is.
	args := []string{"init", l.ref(fp), s.inst(), "--project", lxdProject, "--storage", l.pool}
	if l.o.VM {
		args = append(args, "--vm")
	}
	if _, err := l.lxc(ctx, append(args, l.metaFlags()...)...); err != nil {
		return nil, err
	}
	if _, err := l.lxc(ctx, "start", s.inst(), "--project", lxdProject); err != nil {
		return nil, err
	}
	if err := s.waitReady(ctx, false); err != nil {
		return nil, err
	}
	var out, errb bytes.Buffer
	code, err := s.exec(ctx, Cmd{Argv: []string{"sh", "-c", stampScript(clis)}, Root: true, Dir: "/"}, &out, &errb)
	switch {
	case err != nil:
		return nil, fmt.Errorf("stamping %s: %w", fp, err)
	case code == 126 || code == 127:
		return map[string]string{}, nil // no shell to run the stamp with
	case code != 0:
		return nil, fmt.Errorf("stamping %s exited %d: %s", fp, code, strings.TrimSpace(errb.String()))
	}
	return parseStamps(out.String())
}
