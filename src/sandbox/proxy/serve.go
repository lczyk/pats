package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Policy is the `egress:` block of a sandbox as the proxy consumes it. the
// yaml tags are the config's; config.Egress embeds this type so there is one
// field set.
type Policy struct {
	Default      string   `yaml:"default" json:"default,omitempty"`             // deny (the default) | allow
	Methods      []string `yaml:"methods,omitempty" json:"methods,omitempty"`   // the methods posture; nil: every method, nothing extra inspected
	AllowPrivate bool     `yaml:"allow-private" json:"allow_private,omitempty"` // let entries name private targets
	Allow        []string `yaml:"allow" json:"allow,omitempty"`
	Deny         []string `yaml:"deny" json:"deny,omitempty"`
	DenyResponse string   `yaml:"deny-response" json:"deny_response,omitempty"` // 403 (the default) | 404 | reset | timeout
	// Harness lists the hosts pats merged in for the agent's harness. the
	// methods posture never inspects them. pats sets it; pats.yaml cannot.
	Harness []string `yaml:"harness,omitempty" json:"harness,omitempty"`
}

// ParsePolicy reads a policy from yaml, refusing unknown keys: a typo in a
// filter's config must not quietly mean "no rule".
func ParsePolicy(data []byte) (Policy, error) {
	var p Policy
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		return Policy{}, err
	}
	return p, nil
}

// Rule parses the policy's lists.
func (p Policy) Rule() (Rule, error) {
	switch p.Default {
	case "", "deny", "allow":
	default:
		return Rule{}, fmt.Errorf("default %q: want deny or allow", p.Default)
	}
	switch p.DenyResponse {
	case "", "403", "404", "reset", "timeout":
	default:
		return Rule{}, fmt.Errorf("deny-response %q: want 403, 404, reset or timeout", p.DenyResponse)
	}
	r, err := ParseRule(p.Default == "allow", p.Allow, p.Deny)
	if err != nil {
		return r, err
	}
	if p.Methods != nil {
		if len(p.Methods) == 0 {
			return Rule{}, errors.New("methods: an empty list would refuse every request -- list the methods to allow")
		}
		r.Methods = map[string]bool{}
		for _, m := range p.Methods {
			if !methodsRe.MatchString(m) || strings.Contains(m, ",") {
				return Rule{}, fmt.Errorf("methods: %q is not an upper-case http method", m)
			}
			r.Methods[m] = true
		}
	}
	for _, line := range p.Harness {
		e, err := ParseEntry(line, false)
		if err != nil {
			return Rule{}, fmt.Errorf("harness %q: %w", line, err)
		}
		r.Exempt = append(r.Exempt, e)
	}
	return r, nil
}

// NeedsCA reports whether the policy terminates tls anywhere.
func (p Policy) NeedsCA() bool {
	r, err := p.Rule()
	if err != nil {
		return false
	}
	if r.Methods != nil {
		return true
	}
	for _, list := range [][]Entry{r.Allow, r.Deny} {
		for _, e := range list {
			if e.URL() {
				return true
			}
		}
	}
	return false
}

// Options configures Serve.
type Options struct {
	Listen string // address to listen on, default :8080
	Policy Policy
	// CACert and CAKey are the run CA, each a pem file path or the pem itself.
	// required when the policy has url entries.
	CACert, CAKey string
	// Roots is the pem bundle upstream certificates verify against; nil means
	// the process's system roots.
	Roots []byte
	// Refuse lists ranges never dialled on top of the private-range guard.
	Refuse []netip.Prefix
	Audit  io.Writer // one json line per request; nil: discard
	Ready  io.Writer // where "listening <addr>" goes once the port is open; nil: nowhere
}

// New builds the Proxy for o without listening.
func New(o Options) (*Proxy, error) {
	rule, err := o.Policy.Rule()
	if err != nil {
		return nil, err
	}
	p := &Proxy{Rule: rule, DenyResponse: o.Policy.DenyResponse, Audit: o.Audit}
	if o.Policy.NeedsCA() {
		if o.CACert == "" || o.CAKey == "" {
			return nil, errors.New("url entries need a CA (cert and key)")
		}
		if p.Signer, err = NewSigner(o.CACert, o.CAKey); err != nil {
			return nil, fmt.Errorf("ca: %w", err)
		}
	}
	guard := &Guard{AllowPrivate: o.Policy.AllowPrivate, Refuse: o.Refuse}
	p.Dial = Dialer(guard).DialContext
	tlsConf := &tls.Config{VerifyConnection: checkStaple}
	if o.Roots != nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(o.Roots) {
			return nil, errors.New("roots: no certificates in bundle")
		}
		tlsConf.RootCAs = pool
	}
	p.Upstream = &http.Transport{
		DialContext:           p.Dial,
		TLSClientConfig:       tlsConf,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
	}
	return p, nil
}

// Serve runs the proxy until ctx is done.
func Serve(ctx context.Context, o Options) error {
	p, err := New(o)
	if err != nil {
		return err
	}
	addr := o.Listen
	if addr == "" {
		addr = ":8080"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	if o.Ready != nil {
		fmt.Fprintf(o.Ready, "listening %s\n", ln.Addr())
	}
	srv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
