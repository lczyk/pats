package main

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/lczyk/pats/src/sandbox"
	"github.com/lczyk/pats/src/sandbox/proxy"
	"gopkg.in/yaml.v3"
)

// EgressCommand groups the egress proxy subcommands.
type EgressCommand struct {
	Run EgressRunCommand `command:"run" description:"serve the egress proxy: the sidecar's entrypoint, and standalone use"`
}

// EgressRunCommand serves the proxy. every flag has an env fallback (the
// sidecar is configured over env, since `docker run -d` has no stdin); a flag
// wins over its env.
type EgressRunCommand struct {
	Policy string   `long:"policy" description:"policy file: a sandbox's egress block, verbatim (env PATS_EGRESS_POLICY holds the yaml itself)"`
	Listen string   `long:"listen" description:"listen address (env PATS_EGRESS_LISTEN; default :8080)"`
	Audit  string   `long:"audit" description:"audit log path, one json line per request (default stdout)"`
	CACert string   `long:"ca-cert" description:"run CA cert for url entries: a pem file, or the pem itself (env PATS_EGRESS_CA_CERT)"`
	CAKey  string   `long:"ca-key" description:"run CA key (env PATS_EGRESS_CA_KEY)"`
	Roots  string   `long:"roots" description:"pem bundle upstream certificates verify against (env PATS_EGRESS_ROOTS; default: system roots)"`
	Refuse []string `long:"refuse" description:"cidr never dialled, repeatable (env PATS_EGRESS_REFUSE, comma-separated)"`
}

func (c *EgressRunCommand) Execute(args []string) error {
	o, err := sandbox.ProxyEnvOptions(os.Getenv)
	if err != nil {
		return err
	}
	if c.Policy != "" {
		data, err := os.ReadFile(c.Policy)
		if err != nil {
			return err
		}
		o.Policy = proxy.Policy{}
		if err := yaml.Unmarshal(data, &o.Policy); err != nil {
			return fmt.Errorf("%s: %w", c.Policy, err)
		}
	}
	if c.Listen != "" {
		o.Listen = c.Listen
	}
	if c.CACert != "" {
		o.CACert = c.CACert
	}
	if c.CAKey != "" {
		o.CAKey = c.CAKey
	}
	if c.Roots != "" {
		if strings.Contains(c.Roots, "-----BEGIN") {
			o.Roots = []byte(c.Roots)
		} else if o.Roots, err = os.ReadFile(c.Roots); err != nil {
			return err
		}
	}
	if len(c.Refuse) > 0 {
		o.Refuse = nil
		for _, s := range c.Refuse {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				return fmt.Errorf("--refuse %q: %w", s, err)
			}
			o.Refuse = append(o.Refuse, p)
		}
	}
	var audit io.Writer = os.Stdout
	if c.Audit != "" {
		f, err := os.OpenFile(c.Audit, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		audit = f
	}
	o.Audit, o.Ready = audit, os.Stderr

	// SIGTERM is how the daemon stops the sidecar; serve until then.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return proxy.Serve(ctx, o)
}
