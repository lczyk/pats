package eval

import (
	"context"
	"io"
	"strings"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// imageHost is where a sandbox's images live, as init and run see it: a
// sandbox.Driver, narrowed to what they ask of it.
type imageHost interface {
	// Target names the host in the pin file, resolved now (a later change
	// of default must not move a pin), with the daemon that answers.
	Target(ctx context.Context) (sandbox.Target, error)
	// Resolve is the image a ref names now.
	Resolve(ctx context.Context, ref string) (sandbox.Image, error)
	// Stamps are the harness versions inside image id, those it has.
	Stamps(ctx context.Context, id string) (map[string]string, error)
}

// puller is a host init pulls into.
type puller interface {
	imageHost
	// Images is every image id the host holds, tagged or not.
	Images(ctx context.Context) (map[string]bool, error)
	Pull(ctx context.Context, ref string, force bool, log io.Writer) (sandbox.Image, bool, error)
}

// builder is a host init makes a sandbox's images on.
type builder interface {
	puller
	Build(ctx context.Context, spec sandbox.BuildSpec, log io.Writer) (sandbox.Image, string, bool, error)
	ProxyImage(ctx context.Context, ref string, force bool, log io.Writer) (sandbox.Image, bool, error)
}

// host is all eval asks of a sandbox's driver.
type host interface {
	builder
	// Upstream is the id or digest ref's source names now.
	Upstream(ctx context.Context, ref string) (string, error)
	CPUs(ctx context.Context) int
}

// hostFor is the driver of a sandbox's place; a var so tests fake it.
var hostFor = func(sb config.Sandbox) (host, error) {
	d, err := sandbox.Open(placeOptions(sb))
	if err != nil {
		return nil, err
	}
	return d, nil
}

// placeOptions are the driver options of the place a sandbox runs on.
func placeOptions(sb config.Sandbox) sandbox.Options {
	return sandbox.Options{Driver: sb.Driver, Remote: sb.Remote, VM: sb.VM}.WithEnv()
}

// newSandbox makes a sandbox on a place, swappable in tests to run the
// orchestration against a fake sandbox (no docker).
var newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) {
	d, err := sandbox.Open(o)
	if err != nil {
		return nil, err
	}
	return d.Sandbox(s)
}

// stepLog is a build log that also reports its steps: each "==> " line
// to step, each "==> warning: " line to warn.
type stepLog struct {
	w          io.Writer
	step, warn func(msg string)
	partial    []byte
}

func (s *stepLog) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	s.partial = append(s.partial, p...)
	for {
		line, rest, ok := strings.Cut(string(s.partial), "\n")
		if !ok {
			break
		}
		s.partial = []byte(rest)
		if msg, ok := strings.CutPrefix(line, "==> warning: "); ok {
			s.warn(msg)
		} else if msg, ok := strings.CutPrefix(line, "==> "); ok {
			s.step(msg)
		}
	}
	return n, err
}

// warnUnboxed says, once per sandbox and before the first pair, which of
// the pairs' sandboxes run their agents on this machine itself.
func warnUnboxed(cfg *config.Config, pairs []config.TestPair, lg logw) {
	used := usedSandboxes(cfg, pairs)
	for _, sb := range cfg.Sandboxes {
		if used[sb.Name] && sb.IsHost() {
			lg.warn("sandbox %q runs agents on this machine, unsandboxed (sand-not-boxed); egress is advisory", sb.Name)
		}
	}
}
