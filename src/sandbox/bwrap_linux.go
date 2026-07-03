//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"github.com/lczyk/pats/src/sandbox/proxy"
)

func newBwrap() (Sandbox, error) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return nil, fmt.Errorf("sandbox driver bwrap: bwrap not found in PATH: %w", err)
	}
	return &bwrapSandbox{}, nil
}

func (b *bwrapSandbox) Run(ctx context.Context, spec Spec, stdout, stderr io.Writer) (int, error) {
	abs, err := filepath.Abs(spec.Workdir)
	if err != nil {
		return -1, fmt.Errorf("resolve workdir: %w", err)
	}

	var extra []string
	var sock string // non-empty -> run via the netns helper
	var teardown func()
	switch spec.Egress.Mode {
	case "", "open":
	case "none":
		extra = []string{"--unshare-net"}
	case "proxy", "mitm-proxy":
		if extra, sock, teardown, err = b.startProxy(spec); err != nil {
			return -1, err
		}
	default:
		return -1, fmt.Errorf("egress: unknown mode %q", spec.Egress.Mode)
	}
	if teardown != nil {
		defer teardown()
	}

	args, err := bwrapArgs(spec, abs, extra)
	if err != nil {
		return -1, err
	}
	args = append(args, spec.Argv...)

	var cmd *exec.Cmd
	if sock != "" {
		// proxy modes: re-exec ourselves as the netns helper (see NetnsMain),
		// which forwards 127.0.0.1:8080 to the proxy's unix socket and runs
		// bwrap. the helper is cloned into fresh user+net namespaces; the
		// identity uid map plus an ambient CAP_NET_ADMIN (valid within the new
		// userns only) lets it bring lo up w/out being root anywhere.
		self, err := os.Executable()
		if err != nil {
			return -1, err
		}
		hargs := append([]string{"__sbx-net", sock, "--", "bwrap"}, args...)
		cmd = exec.CommandContext(ctx, self, hargs...)
		uid, gid := os.Getuid(), os.Getgid()
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
			UidMappings:                []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
			GidMappings:                []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
			GidMappingsEnableSetgroups: false,
			AmbientCaps:                []uintptr{capNetAdmin},
			Pdeathsig:                  syscall.SIGKILL,
		}
	} else {
		cmd = exec.CommandContext(ctx, "bwrap", args...)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second

	switch err := cmd.Run(); {
	case err == nil:
		return 0, nil
	case isExit(err):
		return err.(*exec.ExitError).ExitCode(), nil // ran; non-zero exit is the agent's
	default:
		return -1, fmt.Errorf("bwrap run failed: %w", err)
	}
}

// startProxy runs the proxy engine in-process on a unix socket and returns
// the bwrap args wiring the sandbox to it (proxy env + mitm CA when
// applicable), the socket path for the netns helper, and a teardown.
func (b *bwrapSandbox) startProxy(spec Spec) (extra []string, sock string, teardown func(), err error) {
	dir, err := MkTemp(namePrefix + "bwrap-")
	if err != nil {
		return nil, "", nil, fmt.Errorf("egress: tmp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	var signer *proxy.Signer
	extra = proxyEnvArgs()
	if spec.Egress.Mode == "mitm-proxy" && len(spec.Egress.DenyURLs)+len(spec.Egress.AllowURLs) > 0 {
		if signer, err = setupBwrapMitm(dir); err != nil {
			cleanup()
			return nil, "", nil, err
		}
		extra = append(extra, mitmTLSArgs(dir)...)
	}

	var aw io.Writer = io.Discard
	var auditF *os.File
	if spec.Egress.AuditPath != "" {
		if auditF, err = os.Create(spec.Egress.AuditPath); err != nil {
			cleanup()
			return nil, "", nil, fmt.Errorf("egress: audit log: %w", err)
		}
		aw = auditF
	}

	sock = filepath.Join(dir, "proxy.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		cleanup()
		return nil, "", nil, fmt.Errorf("egress: proxy socket: %w", err)
	}
	srv := &http.Server{Handler: proxy.Handler(egressRule(spec.Egress), signer, http.DefaultTransport, aw)}
	go func() { _ = srv.Serve(ln) }()

	teardown = func() {
		_ = srv.Close()
		if auditF != nil {
			_ = auditF.Close()
		}
		cleanup()
	}
	return extra, sock, teardown, nil
}

const (
	capNetAdmin          = 12 // CAP_NET_ADMIN
	prCapAmbient         = 47 // PR_CAP_AMBIENT
	prCapAmbientClearAll = 4  // PR_CAP_AMBIENT_CLEAR_ALL
)

// NetnsMain is the `pats __sbx-net <sock> -- <cmd...>` helper: it runs inside
// the fresh user+net namespaces set up by Run, brings loopback up, bridges
// tcp 127.0.0.1:8080 to the host-side proxy's unix socket (reachable because
// the mount namespace is shared), then runs bwrap and returns its exit code.
func NetnsMain(args []string) int {
	if len(args) < 3 || args[1] != "--" {
		fmt.Fprintln(os.Stderr, "usage: pats __sbx-net <sock> -- <cmd...> (internal)")
		return 2
	}
	sock, argv := args[0], args[2:]

	if err := upLoopback(); err != nil {
		fmt.Fprintln(os.Stderr, "__sbx-net: loopback up:", err)
		return 1
	}
	// the ambient CAP_NET_ADMIN was only for the ioctl above; drop it before
	// spawning bwrap, which refuses to start a non-root process that carries
	// unexpected capabilities ("Unexpected capabilities but not setuid").
	if _, _, e := syscall.Syscall(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientClearAll, 0); e != 0 {
		fmt.Fprintln(os.Stderr, "__sbx-net: drop ambient caps:", e)
		return 1
	}
	ln, err := net.Listen("tcp", proxyAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "__sbx-net: listen:", err)
		return 1
	}
	go forward(ln, sock)

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "__sbx-net: start:", err)
		return 1
	}
	// pass SIGTERM/SIGINT through to bwrap so ctx cancel in the parent lands.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for s := range sig {
			_ = cmd.Process.Signal(s)
		}
	}()
	err = cmd.Wait()
	if err == nil {
		return 0
	}
	if isExit(err) {
		return err.(*exec.ExitError).ExitCode()
	}
	fmt.Fprintln(os.Stderr, "__sbx-net:", err)
	return 1
}

// upLoopback sets lo up in the current netns (a fresh netns has it down) via
// SIOCSIFFLAGS -- an ioctl needing CAP_NET_ADMIN over the netns, which the
// ambient cap grants. raw ifreq to avoid a netlink dependency.
func upLoopback() error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	var ifr [40]byte // struct ifreq: 16-byte name, then the union (flags first)
	copy(ifr[:], "lo")
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&ifr))); e != 0 {
		return e
	}
	flags := (*uint16)(unsafe.Pointer(&ifr[16]))
	*flags |= syscall.IFF_UP | syscall.IFF_RUNNING
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&ifr))); e != 0 {
		return e
	}
	return nil
}
