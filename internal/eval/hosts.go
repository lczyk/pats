package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// imageHost is where a sandbox's images live, as init and run see it.
type imageHost interface {
	// target names the host in the pin file, resolved now (a later change
	// of default must not move a pin); daemon identifies the daemon that
	// answers, "" where the target says it all.
	target(ctx context.Context) (target, daemon string, err error)
	// present says whether the image ref is on the host.
	present(ctx context.Context, ref string) bool
}

// puller is a container host init pulls into.
type puller interface {
	imageHost
	// inspect is an image's id and, when it came from a registry, its digest.
	inspect(ctx context.Context, ref string) (id, digest string, err error)
	pull(ctx context.Context, ref string) error
}

// hostFor is the image host of a sandbox's driver; a var so tests fake it.
var hostFor = func(sb config.Sandbox) imageHost {
	if sb.Driver == "lxd" {
		return lxdHost{o: lxdOptions(sb)}
	}
	return containerHost{driver: sb.Driver}
}

// lxdOptions are the driver options an lxd sandbox's images are made with.
func lxdOptions(sb config.Sandbox) sandbox.Options {
	return sandbox.Options{Driver: "lxd", Remote: sb.Remote, VM: sb.VM}.WithEnv()
}

// containerHost is a docker or podman daemon, through its cli.
type containerHost struct{ driver string }

func (h containerHost) cli(ctx context.Context, args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, h.driver, args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w (%s)", h.driver, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// target is docker:<context> with the daemon's id, or podman:<connection>
// with the connection's uri (podman has no daemon id), podman:local when
// it has no connection configured.
func (h containerHost) target(ctx context.Context) (string, string, error) {
	if h.driver == "podman" {
		out, err := h.cli(ctx, "system", "connection", "list", "--format", "json")
		if err != nil {
			return "", "", err
		}
		var conns []struct {
			Name    string
			URI     string
			Default bool
		}
		_ = json.Unmarshal([]byte(out), &conns)
		for _, c := range conns {
			if c.Default {
				return "podman:" + c.Name, c.URI, nil
			}
		}
		return "podman:local", "local", nil
	}
	name, err := h.cli(ctx, "context", "show")
	if err != nil {
		return "", "", err
	}
	id, err := h.cli(ctx, "info", "--format", "{{.ID}}")
	if err != nil {
		return "", "", err
	}
	return h.driver + ":" + name, id, nil
}

func (h containerHost) present(ctx context.Context, ref string) bool {
	_, err := h.cli(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
	return err == nil
}

func (h containerHost) inspect(ctx context.Context, ref string) (id, digest string, err error) {
	out, err := h.cli(ctx, "image", "inspect", "--format", "{{.Id}}|{{range .RepoDigests}}{{.}} {{end}}", ref)
	if err != nil {
		return "", "", err
	}
	id, digests, _ := strings.Cut(out, "|")
	if f := strings.Fields(digests); len(f) > 0 {
		_, digest, _ = strings.Cut(f[0], "@")
	}
	return id, digest, nil
}

func (h containerHost) pull(ctx context.Context, ref string) error {
	_, err := h.cli(ctx, "pull", ref)
	return err
}

// lxdHost is an lxd server, through the lxc cli.
type lxdHost struct{ o sandbox.Options }

func (h lxdHost) target(ctx context.Context) (string, string, error) {
	remote, err := sandbox.LXDRemote(ctx, h.o)
	if err != nil {
		return "", "", err
	}
	return "lxd:" + remote, "", nil
}

func (h lxdHost) present(ctx context.Context, ref string) bool {
	_, err := sandbox.LXDFingerprint(ctx, h.o, ref)
	return err == nil
}
