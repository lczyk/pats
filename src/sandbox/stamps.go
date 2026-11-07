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

// stampScript prints name=<first --version line> for each harness cli the
// image has, skipping those it lacks, and exits 3 when one it has cannot say
// its version.
const stampScript = `for c in claude codex copilot opencode; do
  command -v "$c" >/dev/null 2>&1 || continue
  v=$("$c" --version 2>&1) || { echo "$c --version failed: $v" >&2; exit 3; }
  printf '%s=%s\n' "$c" "$(printf '%s\n' "$v" | head -n1)"
done`

// stampTimeout bounds a stamp: a hung --version must not hang the init.
var stampTimeout = 2 * time.Minute

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

// ContainerStamps runs stampScript in a throwaway container of image id
// under a container driver. an image with no shell has no stamps; any other
// failure is an error. the container is named, so one a timeout or a
// failure leaves behind is removed.
func ContainerStamps(ctx context.Context, driver, id string) (map[string]string, error) {
	return containerStamps(ctx, cliRunner(driver), id, stampTimeout)
}

func containerStamps(ctx context.Context, r runner, id string, timeout time.Duration) (map[string]string, error) {
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	name := "pats-stamp-" + hex.EncodeToString(suffix[:])
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out, errb bytes.Buffer
	err := r.Run(rctx, nil, &out, &errb, "run", "--rm", "--name", name, "--network", "none", "--entrypoint", "sh", id, "-c", stampScript)
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

// LXDImageStamps is the component stamps of an lxd image in the pats
// project: the properties a pats build published it with, or, for an image
// pats did not build, what stampScript says in a throwaway instance of it.
func LXDImageStamps(ctx context.Context, o Options, fp string) (map[string]string, error) {
	l := newLXD(defaultProcess, lxcCLI, o)
	if err := l.prepareServer(ctx); err != nil {
		return nil, err
	}
	return l.imageStamps(ctx, fp)
}

func (l *lxd) imageStamps(ctx context.Context, fp string) (map[string]string, error) {
	var im struct {
		Properties map[string]string `json:"properties"`
	}
	if err := l.query(ctx, "/1.0/images/"+fp+"?project="+lxdProject, &im); err != nil {
		return nil, err
	}
	if im.Properties[stampedProp] == "" {
		return l.runStamp(ctx, fp)
	}
	stamps := map[string]string{}
	for k, v := range im.Properties {
		if name, ok := strings.CutPrefix(k, componentProp); ok {
			stamps[name] = v
		}
	}
	return stamps, nil
}

// runStamp runs stampScript in a throwaway instance of image fp, with no
// network, and removes the instance.
func (l *lxd) runStamp(ctx context.Context, fp string) (stamps map[string]string, err error) {
	ctx, cancel := context.WithTimeout(ctx, stampTimeout)
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
	code, err := s.exec(ctx, Cmd{Argv: []string{"sh", "-c", stampScript}, Root: true, Dir: "/"}, &out, &errb)
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
