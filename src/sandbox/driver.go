package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Driver is one place sandboxes run -- a docker or podman daemon, an lxd
// server -- and every concern pats has of it: its identity, its images, the
// sandboxes it makes. a value is bound to its place: two lxd remotes are two
// drivers of one kind. see docs/spec/sandbox-seams.md, "drivers".
type Driver interface {
	// Target names the place in a pin file, resolved now, with the daemon
	// that answers and its version where the place says.
	Target(ctx context.Context) (Target, error)
	// CPUs is the place's cpu count, for -j auto; 0 when unknown.
	CPUs(ctx context.Context) int

	// Resolve is the image ref names now; an error wrapping ErrNoImage
	// when the place has none.
	Resolve(ctx context.Context, ref string) (Image, error)
	// Images is every image id the place holds, tagged or not.
	Images(ctx context.Context) (map[string]bool, error)
	// Pull makes ref present: fetched when absent or forced, else left.
	// fetched says whether this call brought it.
	Pull(ctx context.Context, ref string, force bool, log io.Writer) (img Image, fetched bool, err error)
	// Build builds one layer, or finds the one built from identical
	// inputs, and returns the base it built on as resolved for its key.
	Build(ctx context.Context, spec BuildSpec, log io.Writer) (img Image, base string, built bool, err error)
	// ProxyImage makes the egress proxy image this place's sandboxes use
	// present: pulled from ref (containers), or built (lxd).
	ProxyImage(ctx context.Context, ref string, force bool, log io.Writer) (img Image, fetched bool, err error)
	// Stamps are the harness versions inside image id, those it has.
	Stamps(ctx context.Context, id string) (map[string]string, error)
	// RemoveImage removes image id; one in use stays and says so, one
	// already gone is no error.
	RemoveImage(ctx context.Context, id string) error
	// Upstream is the id or digest ref's source names now, for staleness.
	Upstream(ctx context.Context, ref string) (string, error)

	// Sandbox makes sandboxes of one image here.
	Sandbox(o SandboxOptions) (Sandbox, error)

	// Runs is what the place holds of each run, unjudged (see Clean).
	Runs(ctx context.Context) ([]RunRecord, error)
	// Remove removes one thing a run left; one already gone is no error.
	Remove(ctx context.Context, r Resource) error
}

// ImageUsage is what lxd's server records of an image's use; a container
// daemon records none, and init judges container images by the projects'
// pins instead.
type ImageUsage interface {
	Usage(ctx context.Context, id string) (ImageUse, error)
}

// ImageUse is what decides whether an lxd image may go: when an instance
// was last made from it (zero: never), when it was uploaded, and whether a
// run holds it under a run-private alias (pats-img-<key>-<token>) between
// publish and claim.
type ImageUse struct {
	LastUsed, Uploaded time.Time
	RunPrivate         bool
}

// Target is where a driver's images live, as a pin file names it.
type Target struct {
	Key     string // docker:<context>, podman:<connection>, lxd:<remote>
	Daemon  string // the daemon answering, "" where Key says it all
	Version string // the server's version, where it says
}

// Image is one image on a driver's place.
type Image struct {
	ID     string // a container image id, an lxd fingerprint
	Name   string // the ref it was asked for, "" when none
	Digest string // the registry digest of Name's repository, containers only
	Repo   string // the repository Digest belongs to
}

// ErrNoImage is what Resolve and Usage wrap when the image is not there, as
// opposed to a failure to ask.
var ErrNoImage = errors.New("no such image")

// BuildSpec is one layer init builds: a dockerfile and its context
// (containers), or provision material and argv run as root on Base.
type BuildSpec struct {
	// Base is the image the layer is provisioned on: an image ref (lxd:
	// ubuntu:26.04 or a pats-img- alias). "" on a container driver is the
	// pats sandbox, through SandboxImage.
	Base     string
	Material string   // a host dir at ProvisionMount while Argv runs, its bin/ first on PATH
	Argv     []string // the provision step
	// Dir and Dockerfile are a container build's context, instead of the
	// above.
	Dir, Dockerfile string
	// SandboxImage is the pats sandbox image, asked for only when the
	// build uses it (PATS_SANDBOX).
	SandboxImage func() string
	// Role (sandbox | project | proxy) and Sandbox are recorded on an lxd
	// image as pats.role and pats.sandbox, for people listing images.
	Role, Sandbox string
	// Force builds even when an image of the key exists, and points the
	// key at the new one.
	Force bool
	// Progress and Warn, when set, get each build step and warning as they
	// happen; the log gets them too.
	Progress func(msg string)
	Warn     func(msg string)
}

// SandboxOptions are what one sandbox needs on top of its place.
type SandboxOptions struct {
	Image  string // a container image, or an lxd image alias or fingerprint
	Limits Limits
	// ProxyImage is the egress proxy image a run uses as is (lxd): a
	// fingerprint or alias in the pats project. "" -> built on demand.
	ProxyImage string
	Profiles   []string          // lxd: in the pats project
	Config     map[string]string // lxd: instance config, passed through
}

// Open is the driver o names, on this process.
func Open(o Options) (Driver, error) {
	return newDriver(defaultProcess, nil, o)
}

// newDriver is the driver o names on p, running its cli through r (nil:
// the driver's own cli).
func newDriver(p *process, r runner, o Options) (Driver, error) {
	switch {
	case IsEngine(o.Driver):
		if r == nil {
			r = engineRunner(o)
		}
		return newContainer(p, r, o), nil
	case o.Driver == "lxd":
		if r == nil {
			r = lxcCLI
		}
		return newLXD(p, r, o), nil
	case o.Driver == HostDriver:
		return newHost(p, o), nil
	case o.Driver == "bwrap":
		return nil, errors.New("sandbox driver bwrap not implemented yet")
	case o.Driver == "":
		return nil, errors.New("sandbox: empty driver")
	default:
		return nil, fmt.Errorf("sandbox: unknown driver %q", o.Driver)
	}
}

// engineRunner runs o's container cli, aimed where o says: podman's
// overrides go to the cli as the environment it reads them from.
func engineRunner(o Options) runner {
	r := execRunner{bin: o.Driver, waitDelay: 10 * time.Second}
	if o.Driver == "podman" {
		if o.ContainerHost != "" {
			r.env = append(r.env, "CONTAINER_HOST="+o.ContainerHost)
		}
		if o.ContainerConnection != "" {
			r.env = append(r.env, "CONTAINER_CONNECTION="+o.ContainerConnection)
		}
	}
	return r
}

// Shutdown ends this process's hold on every place it used: lxd's lease
// renewal stops and its markers go. what a run made is already gone (each
// session closes its own); a crash skips this, and `pats clean` sees the
// lease go stale.
func Shutdown() { defaultProcess.shutdown() }

// notFound says whether a cli's error says the thing asked for is not
// there: docker's, podman's and lxd's words for it.
func notFound(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "no such image") || strings.Contains(m, "image not known") ||
		strings.Contains(m, "not found")
}

// noImage wraps err as ErrNoImage when it says the image is not there.
func noImage(err error) error {
	if err != nil && notFound(err) && !errors.Is(err, ErrNoImage) {
		return fmt.Errorf("%w: %w", ErrNoImage, err)
	}
	return err
}
