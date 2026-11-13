package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// proxyChild is `pats egress run` as a child of this process, its audit
// (stdout) going to a file: the host and bwrap drivers run their proxy so.
type proxyChild struct {
	cmd   *exec.Cmd
	done  chan struct{} // closed once it has exited
	audit *os.File
	addr  string // what it listens on, from its listening line
	// forwarder is the static pats bwrap binds into a sandbox, beside it.
	forwarder string
	// how long stop waits between asking and killing, on whose clock.
	clock clock
	grace time.Duration
}

// startProxyChild runs exe (this pats when "") as the proxy with env over
// pats's own, the machine's proxy included, so it chains through it; the
// policy and CA ride in env, never argv. it waits for the listening line,
// bounded; a proxy that fails to listen is stopped and reaped.
func startProxyChild(ctx context.Context, p *process, exe string, env map[string]string, auditPath string) (*proxyChild, error) {
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("egress: find pats: %w", err)
		}
	}
	cmd := exec.Command(exe, "egress", "run")
	cmd.Env = append(os.Environ(), sortedEnv(env)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	dieWithParent(cmd.SysProcAttr)
	pc := &proxyChild{cmd: cmd, done: make(chan struct{}), clock: p.clock, grace: p.t.execGrace}
	if auditPath != "" {
		f, err := os.Create(auditPath)
		if err != nil {
			return nil, fmt.Errorf("egress: audit log: %w", err)
		}
		pc.audit, cmd.Stdout = f, f
	}
	stderr, err := cmd.StderrPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		if pc.audit != nil {
			pc.audit.Close()
		}
		return nil, fmt.Errorf("egress: start proxy: %w", err)
	}
	addr := make(chan string, 1)
	go func() {
		br := bufio.NewReader(stderr)
		last := ""
		for {
			line, err := br.ReadString('\n')
			if a, ok := strings.CutPrefix(strings.TrimSpace(line), "listening "); ok {
				addr <- a
				_, _ = io.Copy(io.Discard, br)
				return
			}
			if t := strings.TrimSpace(line); t != "" {
				last = t
			}
			if err != nil {
				addr <- "!" + last
				return
			}
		}
	}()
	go func() {
		_ = cmd.Wait()
		close(pc.done)
	}()
	fail := func(err error) (*proxyChild, error) {
		pc.stop()
		return nil, err
	}
	select {
	case a := <-addr:
		if strings.HasPrefix(a, "!") {
			return fail(fmt.Errorf("egress: proxy exited before listening: %s", a[1:]))
		}
		pc.addr = a
	case <-p.clock.After(p.t.childReady):
		return fail(fmt.Errorf("egress: proxy not listening after %s", p.t.childReady))
	case <-ctx.Done():
		return fail(ctx.Err())
	}
	return pc, nil
}

// stop ends the proxy's group, gracefully first, and reaps it.
func (pc *proxyChild) stop() {
	_ = syscall.Kill(-pc.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-pc.done:
	case <-pc.clock.After(pc.grace):
		_ = syscall.Kill(-pc.cmd.Process.Pid, syscall.SIGKILL)
		<-pc.done
	}
	if pc.audit != nil {
		_ = pc.audit.Close()
	}
}
