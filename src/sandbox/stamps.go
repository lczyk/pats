package sandbox

import (
	"bytes"
	"context"
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

// ContainerStamps runs stampScript in image id under a container driver.
// an image with no shell, or one that fails, has no stamps.
func ContainerStamps(ctx context.Context, driver, id string) map[string]string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var out bytes.Buffer
	if err := cliRunner(driver).Run(ctx, nil, &out, &bytes.Buffer{}, "run", "--rm", "--network", "none", "--entrypoint", "sh", id, "-c", stampScript); err != nil {
		return map[string]string{}
	}
	return parseStamps(out.String())
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
