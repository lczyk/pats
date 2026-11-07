package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ProvisionMount is where provisioning material lives in a builder while the
// script runs: the script, a -cmd's file, and the pats root's bin/.
const ProvisionMount = "/pats-provision"

// provisionerVersion is in every image key: bump it when what pats itself
// does to a provisioned image changes, so cached images are remade.
const provisionerVersion = "1"

// LXDImageSpec is one provisioning step: argv runs as root in an instance
// launched from Base, with Material (a host dir) at ProvisionMount and its
// bin/ first on PATH.
type LXDImageSpec struct {
	Base     string // an image ref the cli resolves (ubuntu:26.04), or a pats-img- alias
	Material string
	Argv     []string
}

// LXDImage returns the alias of the image spec provisions on l's server,
// building it on a miss. the alias is keyed on the base image's fingerprint,
// the material, argv, the instance type and what pats adds itself, so a
// refreshed base or an edited script makes a new image. log gets the build's
// output. built reports whether one ran.
func LXDImage(ctx context.Context, o Options, spec LXDImageSpec, log io.Writer) (alias string, built bool, err error) {
	l := newLXD(o)
	return l.image(ctx, spec, log)
}

func (l *lxd) image(ctx context.Context, spec LXDImageSpec, log io.Writer) (string, bool, error) {
	if err := l.prepareServer(ctx); err != nil {
		return "", false, err
	}
	fp, err := l.fingerprint(ctx, spec.Base)
	if err != nil {
		return "", false, fmt.Errorf("base %s: %w", spec.Base, err)
	}
	key, err := imageKey(fp, spec, l.o.VM)
	if err != nil {
		return "", false, err
	}
	alias := "pats-img-" + key
	if _, err := l.fingerprint(ctx, alias); err == nil {
		return alias, false, nil
	}
	if err := l.build(ctx, spec, alias, log); err != nil {
		return "", false, err
	}
	return alias, true, nil
}

// fingerprint is an image's fingerprint: a pats-img- alias in the pats
// project, anything else as the cli resolves it.
func (l *lxd) fingerprint(ctx context.Context, ref string) (string, error) {
	args := []string{"image", "info"}
	if strings.HasPrefix(ref, "pats-img-") {
		args = append(args, l.ref(ref), "--project", lxdProject)
	} else {
		args = append(args, ref)
	}
	out, err := l.lxc(ctx, args...)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Fingerprint:"); ok {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("no fingerprint in lxc image info %s", ref)
}

// imageKey digests every input of a provisioned image.
func imageKey(fp string, spec LXDImageSpec, vm bool) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "provisioner=%s\nbase=%s\nvm=%v\n", provisionerVersion, fp, vm)
	for _, a := range spec.Argv {
		fmt.Fprintf(h, "arg=%q\n", a)
	}
	if spec.Material != "" {
		err := filepath.WalkDir(spec.Material, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(spec.Material, p)
			info, err := d.Info()
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%s %o\n", filepath.ToSlash(rel), info.Mode())
			if info.Mode().IsRegular() {
				b, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				fmt.Fprintf(h, "%d\n", len(b))
				h.Write(b)
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:24], nil
}

// uplink is the bridge builders and proxies reach the internet through: the
// one lxdUplinkEnv names, or lxdUplink, made on first use. it is shared by
// every run, so it carries no run token and `pats clean` leaves it.
func (l *lxd) uplink(ctx context.Context) (string, error) {
	if name := os.Getenv(lxdUplinkEnv); name != "" {
		return name, nil
	}
	if _, err := l.lxc(ctx, "network", "show", l.ref(lxdUplink), "--project", "default"); err == nil {
		return lxdUplink, nil
	}
	if _, err := l.lxc(ctx, "network", "create", l.ref(lxdUplink), "--project", "default",
		"ipv4.address=auto", "ipv4.nat=true", "ipv6.address=none"); err != nil {
		// another pats may have made it meanwhile.
		if _, serr := l.lxc(ctx, "network", "show", l.ref(lxdUplink), "--project", "default"); serr == nil {
			return lxdUplink, nil
		}
		return "", err
	}
	return lxdUplink, nil
}

// build provisions spec on a builder instance and publishes it as alias. it
// publishes under a run-private alias first and only then claims alias, so
// two runs missing the same image never replace one in use: the loser's copy
// is deleted, the winner's is identical by construction.
func (l *lxd) build(ctx context.Context, spec LXDImageSpec, alias string, log io.Writer) (err error) {
	net, err := l.uplink(ctx)
	if err != nil {
		return err
	}
	name := "pats-build-" + lxdRunToken + "-" + strings.TrimPrefix(alias, "pats-img-")[:8]
	s := &lxdSession{l: l, name: name}
	defer func() {
		_, _ = l.lxc(context.Background(), "delete", s.inst(), "--project", lxdProject, "--force")
	}()
	args := []string{"launch", spec.Base, s.inst(), "--project", lxdProject, "--storage", l.pool, "-n", net}
	if strings.HasPrefix(spec.Base, "pats-img-") {
		args[1] = l.ref(spec.Base)
	}
	if l.o.VM {
		args = append(args, "--vm")
	}
	for _, kv := range lxdMeta() {
		args = append(args, "-c", kv)
	}
	fmt.Fprintf(log, "==> launching builder %s from %s\n", name, spec.Base)
	if _, err := l.lxc(ctx, args...); err != nil {
		return err
	}
	if err := s.waitReady(ctx, true); err != nil {
		return fmt.Errorf("%w (does the uplink bridge %s reach the internet? on a host running docker, a new bridge needs an admin rule; or set %s to a bridge that works)", err, net, lxdUplinkEnv)
	}
	// cloud-init may still be configuring the base on first boot.
	_, _ = s.exec(ctx, Cmd{Argv: []string{"sh", "-c", "command -v cloud-init >/dev/null && cloud-init status --wait >/dev/null 2>&1; true"}, Root: true, Dir: "/"}, io.Discard, io.Discard)
	if spec.Material != "" {
		if err := s.Push(ctx, spec.Material, ProvisionMount); err != nil {
			return err
		}
	}
	fmt.Fprintf(log, "==> provisioning: %s\n", strings.Join(spec.Argv, " "))
	if len(spec.Argv) > 0 {
		wrapped := append([]string{"/bin/sh", "-c", `PATH="` + ProvisionMount + `/bin:$PATH"; export PATH; exec "$@"`, "provision"}, spec.Argv...)
		code, err := s.exec(ctx, Cmd{Argv: wrapped, Root: true, Dir: "/"}, log, log)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("provisioning exited %d", code)
		}
	}
	// what pats needs of every image: the agent user, and no cloud-init to
	// rerun first-boot setup on each launch.
	epilogue := `rm -rf ` + ProvisionMount + `; id -u ` + lxdAgentUser + ` >/dev/null 2>&1 || useradd -m -s /bin/bash ` + lxdAgentUser + `; mkdir -p /etc/cloud && touch /etc/cloud/cloud-init.disabled`
	var out bytes.Buffer
	if code, err := s.exec(ctx, Cmd{Argv: []string{"sh", "-c", epilogue}, Root: true, Dir: "/"}, &out, &out); err != nil || code != 0 {
		return fmt.Errorf("provisioning epilogue: %v %s", err, strings.TrimSpace(out.String()))
	}
	if _, err := l.lxc(ctx, "stop", s.inst(), "--project", lxdProject); err != nil {
		return err
	}
	private := alias + "-" + lxdRunToken
	pub := []string{"publish", s.inst(), l.o.Remote + ":", "--project", lxdProject, "--alias", private}
	pub = append(pub, lxdMeta()...)
	fmt.Fprintf(log, "==> publishing %s\n", alias)
	if _, err := l.lxc(ctx, pub...); err != nil {
		return err
	}
	fp, err := l.fingerprint(ctx, private)
	if err != nil {
		return err
	}
	if _, err := l.lxc(ctx, "image", "alias", "create", l.ref(alias), fp, "--project", lxdProject); err != nil {
		if _, ferr := l.fingerprint(ctx, alias); ferr != nil {
			return err
		}
		// another run built it first: use theirs.
		_, _ = l.lxc(context.Background(), "image", "delete", l.ref(private), "--project", lxdProject)
		return nil
	}
	_, _ = l.lxc(context.Background(), "image", "alias", "delete", l.ref(private), "--project", lxdProject)
	return nil
}

// LXDProvenance is what a pair's metadata records of the lxd server and
// image it ran on.
type LXDProvenance struct {
	Remote           string `json:"remote"`
	InstanceType     string `json:"instance_type"` // container | virtual-machine
	ImageFingerprint string `json:"image_fingerprint"`
	ServerVersion    string `json:"server_version"`
}

// LXDInfo resolves o's remote, image fingerprint and server version.
func LXDInfo(ctx context.Context, o Options) (LXDProvenance, error) {
	l := newLXD(o)
	if err := l.prepareServer(ctx); err != nil {
		return LXDProvenance{}, err
	}
	p := LXDProvenance{Remote: l.o.Remote, InstanceType: "container"}
	if o.VM {
		p.InstanceType = "virtual-machine"
	}
	fp, err := l.fingerprint(ctx, o.Image)
	if err != nil {
		return p, err
	}
	p.ImageFingerprint = fp
	out, err := l.lxc(ctx, "query", l.o.Remote+":/1.0")
	if err != nil {
		return p, err
	}
	var srv struct {
		Environment struct {
			ServerVersion string `json:"server_version"`
		} `json:"environment"`
	}
	if err := json.Unmarshal([]byte(out), &srv); err != nil {
		return p, err
	}
	p.ServerVersion = srv.Environment.ServerVersion
	return p, nil
}
