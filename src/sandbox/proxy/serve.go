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
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Policy is the `egress:` block of a sandbox as the proxy consumes it. the
// yaml tags are the config's; config.Egress embeds this type so there is one
// field set.
type Policy struct {
	Default      string     `yaml:"default" json:"default,omitempty"`             // deny (the default) | allow
	Methods      []string   `yaml:"methods,omitempty" json:"methods,omitempty"`   // the methods posture; nil: every method, nothing extra inspected
	AllowPrivate bool       `yaml:"allow-private" json:"allow_private,omitempty"` // let entries name private targets
	Allow        []RawEntry `yaml:"allow" json:"allow,omitempty"`
	Deny         []RawEntry `yaml:"deny" json:"deny,omitempty"`
	DenyResponse string     `yaml:"deny-response" json:"deny_response,omitempty"` // 403 (the default) | 404 | reset | timeout
	// DenyMessage is the body of a 403 refusal: nil is DefaultDenyMessage,
	// "" the bare refusal of pats before 1.3. Resolved fills a nil one in.
	DenyMessage *string `yaml:"deny-message,omitempty" json:"deny_message,omitempty"`
	// Harness lists the hosts pats merged in for the agent's harness. the
	// methods posture never inspects them. pats sets it; pats.yaml cannot.
	Harness []string `yaml:"harness,omitempty" json:"harness,omitempty"`
	// Keys are the keys the proxy holds for the pair (keys.go), their real
	// values in Options.Keys. pats sets them; pats.yaml cannot.
	Keys []KeyBinding `yaml:"keys,omitempty" json:"keys,omitempty"`
}

// ParsePolicy reads a policy from yaml, refusing unknown keys: a typo in a
// filter's config must not quietly mean "no rule". what the policy leaves to
// a default that an agent reads is filled in (Resolved).
func ParsePolicy(data []byte) (Policy, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return Policy{}, err
	}
	if len(doc.Content) > 0 {
		if err := CheckPolicyNode(doc.Content[0]); err != nil {
			return Policy{}, err
		}
	}
	var p Policy
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		return Policy{}, err
	}
	return p.Resolved(), nil
}

// CheckPolicyNode refuses what decoding a policy mapping would lose: a null
// deny-message, which decodes as an absent one and means another thing. the
// null may be written through an alias, or come in with a merge.
func CheckPolicyNode(node *yaml.Node) error {
	node = aliased(node)
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, v := node.Content[i], aliased(node.Content[i+1])
		switch {
		case k.Value == "deny-message" && v.Tag == "!!null":
			return fmt.Errorf("line %d: deny-message: null is neither a message nor none -- write the text, or \"\" for the bare refusal", k.Line)
		case k.Tag == "!!merge" && v.Kind == yaml.SequenceNode:
			for _, m := range v.Content {
				if err := CheckPolicyNode(m); err != nil {
					return err
				}
			}
		case k.Tag == "!!merge":
			if err := CheckPolicyNode(v); err != nil {
				return err
			}
		}
	}
	return nil
}

// aliased is the node an alias stands for, or node itself.
func aliased(node *yaml.Node) *yaml.Node {
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	return node
}

// DefaultDenyMessage is what a 403 refusal says when the policy sets no
// deny-message (docs/spec/refusal-message.md).
const DefaultDenyMessage = `this request was refused by the network policy of this environment.
it is a decision, not a fault: retrying will not change it.
do not look for another route to the same content.
carry on without it, and say so if the task cannot be done without it.
`

// maxDenyMessage bounds a deny-message, in bytes.
const maxDenyMessage = 2048

// Resolved is the policy with the message a 403 refusal carries spelled out,
// so that what is handed to a proxy and recorded for a run is the text an
// agent was shown. a policy that refuses another way has none.
func (p Policy) Resolved() Policy {
	if p.DenyMessage == nil && (p.DenyResponse == "" || p.DenyResponse == "403") {
		msg := DefaultDenyMessage
		p.DenyMessage = &msg
	}
	return p
}

// checkDenyMessage holds a deny-message to text an agent can be shown in a
// response body: valid utf-8, bounded, and no control character but a newline.
func (p Policy) checkDenyMessage() error {
	if p.DenyMessage == nil {
		return nil
	}
	msg := *p.DenyMessage
	switch {
	case p.DenyResponse != "" && p.DenyResponse != "403":
		return fmt.Errorf("deny-message needs deny-response 403: a %s refusal carries no message", p.DenyResponse)
	case !utf8.ValidString(msg):
		return errors.New("deny-message is not valid utf-8")
	case len(msg) > maxDenyMessage:
		return fmt.Errorf("deny-message is %d bytes, over the %d a refusal carries", len(msg), maxDenyMessage)
	}
	if i := strings.IndexFunc(msg, func(r rune) bool { return r != '\n' && unicode.IsControl(r) }); i >= 0 {
		return fmt.Errorf("deny-message holds a control character (byte %d): only a newline is allowed", i)
	}
	return nil
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
	if err := p.checkDenyMessage(); err != nil {
		return Rule{}, err
	}
	r, err := ParseRawRule(p.Default == "allow", p.Allow, p.Deny)
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
	if r.Methods != nil || len(p.Keys) > 0 {
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
	Listen string // address to listen on, default :8080; unix:<path> for a socket
	Policy Policy
	// CACert and CAKey are the run CA, each a pem file path or the pem itself.
	// required when the policy has url entries.
	CACert, CAKey string
	// Roots is the pem bundle upstream certificates verify against; nil means
	// the process's system roots.
	Roots []byte
	// Refuse lists ranges never dialled on top of the private-range guard.
	Refuse []netip.Prefix
	// Chain is the machine's proxy every upstream connection goes through;
	// nil: direct.
	Chain *Chain
	// Keys are the real values of the policy's keys, by placeholder.
	Keys  map[string]string
	Audit io.Writer // one json line per request; nil: discard
	Ready io.Writer // where "listening <addr>" goes once the port is open; nil: nowhere
}

// New builds the Proxy for o without listening.
func New(o Options) (*Proxy, error) {
	rule, err := o.Policy.Rule()
	if err != nil {
		return nil, err
	}
	p := &Proxy{Rule: rule, DenyResponse: o.Policy.DenyResponse, DenyMessage: o.Policy.DenyMessage, Audit: o.Audit}
	if p.keys, err = holdKeys(o.Policy.Keys, o.Keys); err != nil {
		return nil, err
	}
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
	p.Chain = o.Chain
	tlsConf := &tls.Config{VerifyConnection: checkStaple}
	if o.Roots != nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(o.Roots) {
			return nil, errors.New("roots: no certificates in bundle")
		}
		tlsConf.RootCAs = pool
	}
	transport := func() *http.Transport {
		return &http.Transport{
			DialContext:           p.Dial,
			TLSClientConfig:       tlsConf,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 5 * time.Minute,
		}
	}
	if o.Chain == nil {
		p.Upstream = transport()
		return p, nil
	}
	// plain http goes direct or to the http upstream, each over its own
	// transport: only a connection to the upstream that was chosen skips
	// the guard, never a direct request that happens to name its address.
	directHTTP := transport()
	proxiedHTTP := transport()
	proxiedHTTP.Proxy = func(*http.Request) (*url.URL, error) { return o.Chain.HTTP, nil }
	proxiedHTTP.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialUpstream(ctx, o.Chain.HTTP)
	}
	p.Dial = chainedDial(o.Chain, guard, p.Dial)
	p.Upstream = chainRouter{chain: o.Chain, guard: guard, https: transport(), direct: directHTTP, proxied: proxiedHTTP}
	return p, nil
}

// readyTimeout bounds the start's dials to the upstream, under the 15s pats
// waits for the listening line.
var readyTimeout = 10 * time.Second

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
	if o.Chain != nil {
		if err := o.Chain.Reach(ctx, readyTimeout); err != nil {
			return err
		}
	}
	network := "tcp"
	if p, ok := strings.CutPrefix(addr, "unix:"); ok {
		// a socket a sandbox reaches through a bind, with no network of its
		// own: bwrap's.
		network, addr = "unix", p
		_ = os.Remove(p)
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		return err
	}
	if o.Ready != nil {
		fmt.Fprintf(o.Ready, "listening %s\n", ln.Addr())
	}
	srv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       idleWait,
		// "OPTIONS *" is a request like any other: decided and audited.
		DisableGeneralOptionsHandler: true,
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
