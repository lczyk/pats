package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lczyk/pats/internal/version"
)

// ProvisionMount is where provisioning material lives in a builder while the
// script runs: the script and the pats root's bin/.
const ProvisionMount = "/pats-provision"

// provisionerVersion is in every image key: bump it when what pats itself
// does to a provisioned image changes, so cached images are remade.
const provisionerVersion = "2"

// lxdImageSpec is one provisioning step: argv runs as root in an instance
// launched from Base, with Material (a host dir) at ProvisionMount and its
// bin/ first on PATH.
type lxdImageSpec struct {
	Base     string // an image ref the cli resolves (ubuntu:26.04), or a pats-img- alias
	Material string
	Argv     []string
	// Stamp is the clis whose versions the image's properties record. it is
	// no part of the image key: an image that lacks one asked for later is
	// asked itself (imageStamps).
	Stamp []string
	// Harness is the clis placed in the image, after Argv: this layer's own
	// step is pats's tools, and the project's provision is the layer above.
	Harness []Placed
	// Progress and Warn, when set, get each build step and warning as they
	// happen; the log gets them too.
	Progress func(msg string)
	Warn     func(msg string)
	// Role (sandbox | project | proxy) and Sandbox are recorded on the
	// image as pats.role and pats.sandbox, for people listing images.
	Role, Sandbox string
	// Force builds even when an image of the key exists, and points the
	// key's alias at the new one.
	Force bool
	// Key, when set, stands for Argv and Material in the image key: what
	// the image is, when its material holds bytes that do not matter (the
	// proxy of a release, keyed on the version, not the binary).
	Key string
}

func (s lxdImageSpec) step(log io.Writer, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	fmt.Fprintf(log, "==> %s\n", msg)
	if s.Progress != nil {
		s.Progress(msg)
	}
}

func (s lxdImageSpec) warn(log io.Writer, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	fmt.Fprintf(log, "==> warning: %s\n", msg)
	if s.Warn != nil {
		s.Warn(msg)
	}
}

// lowPoolSpace is the free space under which a build warns: a builder and
// the image it publishes take a few GiB.
const lowPoolSpace = 5 << 30

// image finds or builds the image spec provisions on l's server: its
// alias and fingerprint, and the fingerprint of the base it is keyed on.
// the alias is keyed on the base image's fingerprint, the material, argv,
// the instance type and what pats adds itself, so a refreshed base or an
// edited script makes a new image. log gets the build's output. built
// reports whether one ran.
func (l *lxd) image(ctx context.Context, spec lxdImageSpec, log io.Writer) (alias, fp, base string, built bool, err error) {
	if err := l.prepareServer(ctx); err != nil {
		return "", "", "", false, err
	}
	base, err = l.fingerprint(ctx, spec.Base)
	if err != nil {
		return "", "", "", false, fmt.Errorf("base %s: %w", spec.Base, err)
	}
	key, err := imageKey(base, spec, l.o.VM)
	if err != nil {
		return "", "", "", false, err
	}
	alias = "pats-img-" + key
	if fp, err := l.fingerprint(ctx, alias); err == nil && !spec.Force {
		return alias, fp, base, false, nil
	}
	spec.step(log, "building lxd image %s on %s from %s", alias, l.o.Remote, spec.Base)
	if free, total, ok := l.poolSpace(ctx); ok && free < lowPoolSpace {
		spec.warn(log, "storage pool %s on %s has %.1f GiB free of %.1f GiB; a build takes a few", l.pool, l.o.Remote, float64(free)/(1<<30), float64(total)/(1<<30))
	}
	fp, err = l.build(ctx, spec, base, alias, log)
	if err != nil {
		return "", "", "", false, err
	}
	return alias, fp, base, true, nil
}

// poolSpace is the pats pool's free and total bytes, when the server says.
func (l *lxd) poolSpace(ctx context.Context) (free, total uint64, ok bool) {
	var res struct {
		Space struct {
			Used  uint64 `json:"used"`
			Total uint64 `json:"total"`
		} `json:"space"`
	}
	if l.pool == "" || l.query(ctx, "/1.0/storage-pools/"+l.pool+"/resources", &res) != nil || res.Space.Total == 0 {
		return 0, 0, false
	}
	if res.Space.Used > res.Space.Total {
		return 0, res.Space.Total, true
	}
	return res.Space.Total - res.Space.Used, res.Space.Total, true
}

// fingerprint is an image's fingerprint: a pats-img- alias in the pats
// project, anything else as the cli resolves it.
func (l *lxd) fingerprint(ctx context.Context, ref string) (string, error) {
	// json, never lxc image info's text: an image on another remote through
	// image list, one on the sandbox's own server (where instances are made
	// from it, in the pats project) through the rest api.
	if strings.Contains(ref, ":") {
		return l.remoteFingerprint(ctx, ref)
	}
	if isFingerprint(ref) {
		var im struct {
			Fingerprint string `json:"fingerprint"`
		}
		if err := l.query(ctx, "/1.0/images/"+ref+"?project="+lxdProject, &im); err != nil {
			return "", err
		}
		return im.Fingerprint, nil
	}
	var al struct {
		Target string `json:"target"`
	}
	aerr := l.query(ctx, "/1.0/images/aliases/"+ref+"?project="+lxdProject, &al)
	if aerr == nil && al.Target != "" {
		return al.Target, nil
	}
	// not an alias: a unique prefix of a fingerprint, as lxc takes one.
	if isHex(ref) {
		var ims []struct {
			Fingerprint string `json:"fingerprint"`
		}
		if err := l.query(ctx, "/1.0/images?project="+lxdProject+"&recursion=1", &ims); err != nil {
			return "", err
		}
		var hits []string
		for _, im := range ims {
			if strings.HasPrefix(im.Fingerprint, ref) {
				hits = append(hits, im.Fingerprint)
			}
		}
		if len(hits) == 1 {
			return hits[0], nil
		}
	}
	if aerr == nil {
		aerr = fmt.Errorf("lxd image alias %s names no image", ref)
	}
	return "", aerr
}

// fingerprintRe is an lxd image fingerprint: sha256 hex.
var fingerprintRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// isFingerprint says whether ref names an image by fingerprint.
func isFingerprint(ref string) bool { return fingerprintRe.MatchString(ref) }

// copyImage brings ref, an image on another remote (ubuntu:26.04), onto
// l's server in the pats project, so instances start from it there without
// a download; present already, it is left alone unless force. it returns
// the fingerprint, and whether this call copied it. l is ready.
func (l *lxd) copyImage(ctx context.Context, ref string, force bool) (string, bool, error) {
	fp, err := l.fingerprint(ctx, ref)
	if err != nil {
		return "", false, err
	}
	if _, err := l.fingerprint(ctx, fp); err == nil && !force {
		return fp, false, nil
	}
	if _, err := l.lxc(ctx, "image", "copy", ref, l.o.Remote+":", "--target-project", lxdProject); err != nil {
		// a forced copy of what is already there: lxd says it exists.
		if _, ferr := l.fingerprint(ctx, fp); ferr != nil {
			return "", false, err
		}
	}
	return fp, true, nil
}

// imageKey digests every input of a provisioned image.
func imageKey(fp string, spec lxdImageSpec, vm bool) (string, error) {
	h := sha256.New()
	// the pats version too: a layer on a custom base inherits no other.
	fmt.Fprintf(h, "provisioner=%s\nbase=%s\nvm=%v\npats=%s\n", provisionerVersion, fp, vm, version.Info.Version)
	if spec.Key != "" {
		fmt.Fprintf(h, "key=%s\n", spec.Key)
		return hex.EncodeToString(h.Sum(nil))[:24], nil
	}
	for _, a := range spec.Argv {
		fmt.Fprintf(h, "arg=%q\n", a)
	}
	// a placed cli by what decides its tree, never the tree's bytes.
	for _, p := range spec.Harness {
		fmt.Fprintf(h, "harness=%s\n", p.Artefact.Line())
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

// placeStage is where a cli's tree is pushed before it is copied into place.
const placeStage = "/pats-harness"

// place puts one cli's tree in the builder: unpacked on this machine, pushed,
// and copied over the root as root's.
func (l *lxd) place(ctx context.Context, s *lxdSession, p Placed, log io.Writer) error {
	dir, err := l.p.mkTemp("pats-harness-")
	if err != nil {
		return err
	}
	defer RemoveTemp(dir)
	if err := BuildTree(p.Artefact, p.File, dir); err != nil {
		return err
	}
	if err := s.Push(ctx, dir, placeStage); err != nil {
		return err
	}
	cli := p.Artefact.CLI // a cli's name: letters, digits and - (BuildTree refuses another)
	script := `set -e
cp -a ` + placeStage + `/. /
chown -R 0:0 /` + HarnessTreeDir + `/` + cli + `
chown -h 0:0 /` + HarnessBinDir + `/` + cli + `
rm -rf ` + placeStage
	code, err := s.exec(ctx, Cmd{Argv: []string{"/bin/sh", "-c", script}, Root: true, Dir: "/"}, log, log)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("copying its tree into place exited %d", code)
	}
	return nil
}

// uplink is the bridge builders and proxies reach the internet through: the
// one Options.Uplink names, or lxdUplink, made on first use. it is shared by
// every run, so it carries no run token and `pats clean` leaves it.
func (l *lxd) uplink(ctx context.Context) (string, error) {
	if l.o.Uplink != "" {
		return l.o.Uplink, nil
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
func (l *lxd) build(ctx context.Context, spec lxdImageSpec, baseFP, alias string, log io.Writer) (fp string, err error) {
	net, err := l.uplink(ctx)
	if err != nil {
		return "", err
	}
	name := "pats-build-" + l.p.token + "-" + strings.TrimPrefix(alias, "pats-img-")[:8]
	s := &lxdSession{l: l, name: name}
	defer func() {
		derr := l.deleteInstance(name)
		if derr == nil {
			return
		}
		msg := fmt.Sprintf("builder %s was left behind (%v); pats clean removes it once this run has ended", name, derr)
		spec.warn(log, "%s", msg)
		if err != nil {
			err = fmt.Errorf("%w (%s)", err, msg)
		}
	}()
	// from the fingerprint the key was made of, not the base's name again:
	// an alias another init re-points meanwhile must not change the base.
	from := l.ref(baseFP)
	if remote, _, ok := strings.Cut(spec.Base, ":"); ok {
		from = remote + ":" + baseFP
	}
	args := []string{"launch", from, s.inst(), "--project", lxdProject, "--storage", l.pool, "-n", net}
	if l.o.VM {
		args = append(args, "--vm")
	}
	args = append(args, l.metaFlags()...)
	spec.step(log, "launching builder %s", name)
	if _, err := l.lxc(ctx, args...); err != nil {
		return "", err
	}
	if err := s.waitReady(ctx, true); err != nil {
		return "", fmt.Errorf("%w (does the uplink bridge %s reach the internet? on a host running docker, a new bridge needs an admin rule; or set %s to a bridge that works)", err, net, LXDUplinkEnv)
	}
	if !s.online(ctx) {
		return "", fmt.Errorf("lxd: the image builder on bridge %s does not reach the internet. %s. or set %s to a bridge that does (e.g. lxdbr0)", net, dockerBridgeHint, LXDUplinkEnv)
	}
	// cloud-init may still be configuring the base on first boot.
	_, _ = s.exec(ctx, Cmd{Argv: []string{"sh", "-c", "command -v cloud-init >/dev/null && cloud-init status --wait >/dev/null 2>&1; true"}, Root: true, Dir: "/"}, io.Discard, io.Discard)
	if spec.Material != "" {
		if err := s.Push(ctx, spec.Material, ProvisionMount); err != nil {
			return "", err
		}
	}
	spec.step(log, "provisioning: %s", strings.Join(spec.Argv, " "))
	if len(spec.Argv) > 0 {
		wrapped := append([]string{"/bin/sh", "-c", `PATH="` + ProvisionMount + `/bin:$PATH"; export PATH; exec "$@"`, "provision"}, spec.Argv...)
		code, err := s.exec(ctx, Cmd{Argv: wrapped, Root: true, Dir: "/"}, log, log)
		if err != nil {
			return "", err
		}
		if code != 0 {
			return "", fmt.Errorf("provisioning exited %d", code)
		}
	}
	for _, p := range spec.Harness {
		spec.step(log, "placing %s %s", p.Artefact.CLI, p.Artefact.Version)
		if err := l.place(ctx, s, p, log); err != nil {
			return "", fmt.Errorf("placing %s: %w", p.Artefact.CLI, err)
		}
	}
	// what pats needs of every image: the agent user, and no cloud-init to
	// rerun first-boot setup on each launch.
	epilogue := `rm -rf ` + ProvisionMount + `; id -u ` + lxdAgentUser + ` >/dev/null 2>&1 || useradd -m -s /bin/bash ` + lxdAgentUser + `; mkdir -p /etc/cloud && touch /etc/cloud/cloud-init.disabled`
	var out bytes.Buffer
	if code, err := s.exec(ctx, Cmd{Argv: []string{"sh", "-c", epilogue}, Root: true, Dir: "/"}, &out, &out); err != nil || code != 0 {
		return "", fmt.Errorf("provisioning epilogue: %v %s", err, strings.TrimSpace(out.String()))
	}
	// the harness versions inside, as image properties init pins and runs check.
	var stampOut, stampErr bytes.Buffer
	sctx, scancel := context.WithTimeout(ctx, l.p.t.stamp)
	code, err := s.exec(sctx, Cmd{Argv: []string{"sh", "-c", stampScript(spec.Stamp)}, Root: true, Dir: "/"}, &stampOut, &stampErr)
	timedOut := errors.Is(sctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	scancel()
	if timedOut {
		return "", fmt.Errorf("stamping the harness versions took over %s", l.p.t.stamp)
	}
	if err != nil {
		return "", err
	} else if code != 0 {
		return "", fmt.Errorf("stamping the harness versions exited %d: %s", code, strings.TrimSpace(stampErr.String()))
	}
	stamps, err := parseStamps(stampOut.String())
	if err != nil {
		return "", err
	}
	if _, err := l.lxc(ctx, "stop", s.inst(), "--project", lxdProject); err != nil {
		return "", err
	}
	private := alias + "-" + l.p.token
	pub := []string{"publish", s.inst(), l.o.Remote + ":", "--project", lxdProject, "--alias", private}
	pub = append(pub, l.meta()...)
	pub = append(pub, stampedProp+"=1", "pats.version="+version.Info.Version, "pats.inputs="+strings.TrimPrefix(alias, "pats-img-"))
	if spec.Role != "" {
		pub = append(pub, "pats.role="+spec.Role)
	}
	if spec.Sandbox != "" {
		pub = append(pub, "pats.sandbox="+spec.Sandbox)
	}
	for _, name := range sortedKeys(stamps) {
		pub = append(pub, componentProp+name+"="+stamps[name])
	}
	spec.step(log, "publishing %s", alias)
	// a publish the client saw fail may still have made the image, so from
	// here a failed build removes it under the private alias, if it is there,
	// unless a claim that seemed to fail (a cancelled one) made it the
	// alias's. when that cannot be read, the image stays: pats clean takes a
	// run-private alias, and its image once nothing else names it.
	var mine string
	defer func() {
		if err == nil {
			return
		}
		bg := context.Background()
		if mine != "" {
			theirs, ferr := l.fingerprint(bg, alias)
			if ferr != nil && !notFound(ferr) {
				return
			}
			if theirs == mine {
				_, _ = l.lxc(bg, "image", "alias", "delete", l.ref(private), "--project", lxdProject)
				return
			}
		}
		_, _ = l.lxc(bg, "image", "delete", l.ref(private), "--project", lxdProject)
	}()
	if _, err := l.lxc(ctx, pub...); err != nil {
		return "", err
	}
	fp, err = l.fingerprint(ctx, private)
	if err != nil {
		return "", err
	}
	mine = fp
	// a claim the client saw fail may have landed: what the alias names
	// then says whose it is, and an image it names is never deleted.
	if _, err := l.lxc(ctx, "image", "alias", "create", l.ref(alias), fp, "--project", lxdProject); err != nil {
		theirs, ferr := l.fingerprint(ctx, alias)
		switch {
		case ferr != nil:
			return "", err
		case theirs == fp:
		case spec.Force:
			// a forced build replaces what the alias named, in one call: the
			// alias is never missing. pins elsewhere keep the old image.
			body, _ := json.Marshal(map[string]string{"target": fp, "description": ""})
			if _, err := l.lxc(ctx, "query", "-X", "PUT", "-d", string(body),
				l.o.Remote+":/1.0/images/aliases/"+alias+"?project="+lxdProject); err != nil {
				if now, ferr := l.fingerprint(ctx, alias); ferr != nil || now != fp {
					return "", err
				}
			}
		default:
			// another run built it first: use theirs.
			_, _ = l.lxc(context.Background(), "image", "delete", l.ref(private), "--project", lxdProject)
			return theirs, nil
		}
	}
	_, _ = l.lxc(context.Background(), "image", "alias", "delete", l.ref(private), "--project", lxdProject)
	return fp, nil
}
