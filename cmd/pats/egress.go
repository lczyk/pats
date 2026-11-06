package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/lczyk/pats/internal/eval"
	"github.com/lczyk/pats/src/sandbox"
	"github.com/lczyk/pats/src/sandbox/proxy"
)

// EgressCommand groups the egress proxy subcommands.
type EgressCommand struct {
	Check   EgressCheckCommand   `command:"check" description:"the verdict a sandbox's policy gives a url, and the entry that decides it"`
	Suggest EgressSuggestCommand `command:"suggest" description:"fold a run's egress logs into an allow block"`
	Log     EgressLogCommand     `command:"log" description:"a pair's egress log as a table"`
	Run     EgressRunCommand     `command:"run" description:"serve the egress proxy: the sidecar's entrypoint, and standalone use"`
	CA      EgressCACommand      `command:"ca" description:"mint a CA for a standalone proxy's url entries"`
}

// EgressCheckCommand evaluates one url against a sandbox's policy.
type EgressCheckCommand struct {
	Task   string `long:"task" short:"t" description:"also apply this task's egress lists"`
	Agent  string `long:"agent" short:"a" description:"also merge this agent's harness hosts (the policy a real pair runs under)"`
	Method string `long:"method" default:"GET" description:"request method"`
	Args   struct {
		Sandbox string `positional-arg-name:"sandbox"`
		URL     string `positional-arg-name:"url"`
	} `positional-args:"yes" required:"yes"`
}

func (c *EgressCheckCommand) Execute(args []string) error {
	cfg, err := load(opts.Config)
	if err != nil {
		return err
	}
	pol, err := eval.EffectivePolicy(cfg, c.Args.Sandbox, c.Task, c.Agent)
	if err != nil {
		return err
	}
	allowed, err := eval.CheckURL(pol, c.Args.URL, c.Method, c.Agent != "", os.Stdout)
	if err != nil {
		return err
	}
	if !allowed {
		os.Exit(1)
	}
	return nil
}

// EgressSuggestCommand folds a run's audit into an allow block.
type EgressSuggestCommand struct {
	Run string `long:"run" short:"r" description:"run to read: a dir, a friendly name, or a number (default: latest)"`
}

func (c *EgressSuggestCommand) Execute(args []string) error {
	return eval.SuggestEgress(filepath.Dir(opts.Config), c.Run, os.Stdout)
}

// EgressLogCommand prints one pair's audit.
type EgressLogCommand struct {
	Denied bool     `long:"denied" description:"only refusals"`
	Method []string `long:"method" description:"only requests with these methods (repeatable or comma-separated), e.g. POST,PUT,PATCH,DELETE"`
	Args   struct {
		Pair string `positional-arg-name:"pair-dir-or-egress.log"`
	} `positional-args:"yes" required:"yes"`
}

func (c *EgressLogCommand) Execute(args []string) error {
	path := c.Args.Pair
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		path = filepath.Join(path, "egress.log")
	}
	return eval.EgressLog(path, c.Denied, c.Method, os.Stdout)
}

// EgressCACommand writes a fresh CA pair for `pats egress run --ca-cert/--ca-key`.
type EgressCACommand struct {
	Out string `long:"out" required:"yes" description:"directory to write ca.pem and ca.key into"`
}

func (c *EgressCACommand) Execute(args []string) error {
	certPEM, keyPEM, err := sandbox.GenCA()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.Out, 0o755); err != nil {
		return err
	}
	certPath, keyPath := filepath.Join(c.Out, "ca.pem"), filepath.Join(c.Out, "ca.key")
	for _, p := range []string{certPath, keyPath} {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("%s exists; not overwriting a CA", p)
		}
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s and %s (valid 24h). clients trust ca.pem; the proxy gets both.\n", certPath, keyPath)
	return nil
}

// EgressRunCommand serves the proxy. every flag has an env fallback (the
// sidecar is configured over env, since `docker run -d` has no stdin); a flag
// wins over its env.
type EgressRunCommand struct {
	Policy  string   `long:"policy" description:"policy file: a sandbox's egress block, verbatim (env PATS_EGRESS_POLICY holds the yaml itself)"`
	Sandbox string   `long:"sandbox" description:"serve this sandbox's policy out of pats.yaml instead of --policy"`
	Task    string   `long:"task" description:"with --sandbox: also apply this task's egress lists"`
	Agent   string   `long:"agent" description:"with --sandbox: also merge this agent's harness hosts"`
	Listen  string   `long:"listen" description:"listen address (env PATS_EGRESS_LISTEN; default :8080)"`
	Audit   string   `long:"audit" description:"audit log path, one json line per request (env PATS_EGRESS_AUDIT; default stdout)"`
	CACert  string   `long:"ca-cert" description:"run CA cert for url entries: a pem file, or the pem itself (env PATS_EGRESS_CA_CERT)"`
	CAKey   string   `long:"ca-key" description:"run CA key (env PATS_EGRESS_CA_KEY)"`
	Roots   string   `long:"roots" description:"pem bundle upstream certificates verify against (env PATS_EGRESS_ROOTS; default: system roots)"`
	Refuse  []string `long:"refuse" description:"cidr never dialled, repeatable (env PATS_EGRESS_REFUSE, comma-separated)"`
}

func (c *EgressRunCommand) Execute(args []string) error {
	// a flag wins over its env, so an overridden env value is never read --
	// a bad one must not stop a run whose flag is fine.
	overridden := map[string]bool{
		"PATS_EGRESS_POLICY":  c.Policy != "" || c.Sandbox != "",
		"PATS_EGRESS_LISTEN":  c.Listen != "",
		"PATS_EGRESS_CA_CERT": c.CACert != "",
		"PATS_EGRESS_CA_KEY":  c.CAKey != "",
		"PATS_EGRESS_ROOTS":   c.Roots != "",
		"PATS_EGRESS_REFUSE":  len(c.Refuse) > 0,
	}
	o, err := sandbox.ProxyEnvOptions(func(k string) string {
		if overridden[k] {
			return ""
		}
		return os.Getenv(k)
	})
	if err != nil {
		return err
	}
	switch {
	case c.Policy != "" && c.Sandbox != "":
		return errors.New("--policy and --sandbox are mutually exclusive")
	case c.Sandbox == "" && (c.Task != "" || c.Agent != ""):
		return errors.New("--task and --agent need --sandbox")
	case c.Sandbox != "":
		cfg, err := load(opts.Config)
		if err != nil {
			return err
		}
		if o.Policy, err = eval.EffectivePolicy(cfg, c.Sandbox, c.Task, c.Agent); err != nil {
			return err
		}
	case c.Policy != "":
		data, err := os.ReadFile(c.Policy)
		if err != nil {
			return err
		}
		if o.Policy, err = proxy.ParsePolicy(data); err != nil {
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
	if c.Audit == "" {
		c.Audit = os.Getenv("PATS_EGRESS_AUDIT")
	}
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
