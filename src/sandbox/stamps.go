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

// stampScript prints each harness cli's version line, name=line, in the
// image it runs in; a cli the image lacks prints an empty line.
const stampScript = `for c in claude codex copilot opencode; do printf '%s=' "$c"; ("$c" --version 2>/dev/null || true) | head -n1; echo; done`

// componentProp prefixes the lxd image properties holding the stamps.
const componentProp = "pats.component."

var versionRe = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]*[0-9A-Za-z])?`)

// parseStamps reads stampScript's output into name -> version, skipping
// the clis the image lacks.
func parseStamps(out string) map[string]string {
	stamps := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		name, rest, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || name == "" {
			continue
		}
		if v := versionRe.FindString(rest); v != "" {
			stamps[name] = v
		}
	}
	return stamps
}

// ContainerStamps runs stampScript in a throwaway container of image id
// under a container driver. an image with no shell has no stamps; any other
// failure is an error. the container is named, so one a timeout or a
// failure leaves behind is removed.
func ContainerStamps(ctx context.Context, driver, id string) (map[string]string, error) {
	return containerStamps(ctx, cliRunner(driver), id, 2*time.Minute)
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
		return parseStamps(out.String()), nil
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

// LXDImageStamps reads the component stamps an lxd image was published
// with, in the pats project.
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
	stamps := map[string]string{}
	for k, v := range im.Properties {
		if name, ok := strings.CutPrefix(k, componentProp); ok {
			stamps[name] = v
		}
	}
	return stamps, nil
}
