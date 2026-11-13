package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

// bundleCache keeps what was read of each key. a read that failed is not
// kept: the next asker reads again.
type bundleCache struct {
	mu sync.Mutex
	m  map[string]*bundleEntry
}

type bundleEntry struct {
	turn chan struct{} // held by the one reading or looking
	data []byte
	ok   bool
}

// get is key's bundle, read when nobody has yet. a caller waiting on
// another's read stops waiting when its own ctx ends.
func (b *bundleCache) get(ctx context.Context, key string, read func() ([]byte, error)) ([]byte, error) {
	b.mu.Lock()
	if b.m == nil {
		b.m = map[string]*bundleEntry{}
	}
	e := b.m[key]
	if e == nil {
		e = &bundleEntry{turn: make(chan struct{}, 1)}
		b.m[key] = e
	}
	b.mu.Unlock()
	select {
	case e.turn <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-e.turn }()
	if !e.ok {
		data, err := read()
		if err != nil {
			return nil, err
		}
		e.data, e.ok = data, true
	}
	return append([]byte(nil), e.data...), nil
}

// material is the driver-neutral half of a proxy-mode setup: what the proxy
// is told and what the sandboxed process is told, before any driver decides
// how the two meet. CA key material is held in memory only; the cert and the
// merged trust bundle go to caDir for the driver to mount.
type material struct {
	policyYAML string
	caCert     string // pem, "" without mitm
	caKey      string // pem, never written to disk
	caDir      string // "" without a bundle
	bundlePath string // merged image roots + run CA + machine trust, under caDir
	certPath   string // the certificates pats adds (run CA, machine trust), under caDir
}

// prepare renders the policy and, when it terminates tls anywhere, mints the
// run CA. with a CA or trust (the machine's SSL_CERT_FILE, behind an
// inspecting upstream) it merges both into the image's trust bundle
// (readBundle fetches that bundle from the driver). the caller removes
// caDir.
func (p *process) prepare(eg Egress, trust []byte, readBundle func() ([]byte, error)) (_ *material, err error) {
	y, err := yaml.Marshal(eg.Policy)
	if err != nil {
		return nil, err
	}
	m := &material{policyYAML: string(y)}
	defer func() {
		if err != nil {
			m.cleanup()
		}
	}()
	if !eg.Policy.NeedsCA() && trust == nil {
		return m, nil
	}
	var added []byte
	if eg.Policy.NeedsCA() {
		certPEM, keyPEM, err := GenCA()
		if err != nil {
			return nil, fmt.Errorf("egress: gen mitm ca: %w", err)
		}
		m.caCert, m.caKey = string(certPEM), string(keyPEM)
		added = append(added, certPEM...)
	}
	if trust != nil {
		added = append(append(added, '\n'), trust...)
	}
	if m.caDir, err = p.mkTemp(namePrefix + "mitm-ca-"); err != nil {
		return nil, fmt.Errorf("egress: ca dir: %w", err)
	}
	m.certPath = filepath.Join(m.caDir, "ca.pem")
	if err := os.WriteFile(m.certPath, added, 0o644); err != nil {
		return nil, err
	}
	// merged bundle: the image's system roots + what pats adds. tunneled
	// (non-mitm) hosts still present real certs, so the system roots must
	// stay in the file.
	bundle, err := readBundle()
	if err != nil {
		return nil, fmt.Errorf("egress: read image trust bundle %s: %w", caBundlePath, err)
	}
	m.bundlePath = filepath.Join(m.caDir, "bundle.pem")
	if err := os.WriteFile(m.bundlePath, append(append(bundle, '\n'), added...), 0o644); err != nil {
		return nil, err
	}
	return m, nil
}

// cleanup removes the CA dir, if any.
func (m *material) cleanup() {
	if m != nil && m.caDir != "" {
		_ = RemoveTemp(m.caDir)
	}
}

// proxyEnv is what the proxy process reads: the policy, and with mitm the CA
// (inline, so the key never touches disk) and where the trust bundle is.
func (m *material) proxyEnv(rootsPath string) map[string]string {
	env := map[string]string{"PATS_EGRESS_POLICY": m.policyYAML}
	if m.caCert != "" {
		env["PATS_EGRESS_CA_CERT"] = m.caCert
		env["PATS_EGRESS_CA_KEY"] = m.caKey
	}
	if m.bundlePath != "" {
		env["PATS_EGRESS_ROOTS"] = rootsPath
	}
	return env
}

// caBundlePath is where ubuntu/debian images keep the system trust bundle --
// read out of the image to build the merged bundle. NOTE: the merged bundle is
// NOT mounted back over this path: docker desktop injects its own mount there
// (host-CA sharing), so a -v to it fails with "duplicate mount point". the
// bundle lives at a neutral path and env points the tools at it instead.
const caBundlePath = "/etc/ssl/certs/ca-certificates.crt"

// agentBundlePath is where the merged trust bundle is mounted in the agent.
const agentBundlePath = "/sbx-ca/bundle.pem"

// agentCertPath is where the run CA cert alone is mounted in the agent.
const agentCertPath = "/sbx-ca/ca.pem"

// agentEnv is the sandboxed process's side of the contract: the proxy url,
// and with mitm the trust env for every tls stack a tool may use. openssl
// consumers (curl, git) read SSL_CERT_FILE; python requests bundles certifi
// so it needs its own var; node's var is additive so the run CA alone
// suffices there; cargo ignores all of those. tools honouring none of these
// fail their tls handshake on mitm'd hosts -- fail closed, not bypassed.
//
// NO_PROXY and ALL_PROXY are set empty: an engine that adds the host's proxy
// env (docker's config.json proxies) adds only what a container lacks, and
// neither may route around pats's proxy.
func (m *material) agentEnv(proxyURL string) map[string]string {
	env := map[string]string{
		"HTTP_PROXY": proxyURL, "HTTPS_PROXY": proxyURL,
		"http_proxy": proxyURL, "https_proxy": proxyURL,
		"NO_PROXY": "", "no_proxy": "", "ALL_PROXY": "", "all_proxy": "",
	}
	return mergeEnv(env, m.trustEnv())
}

// trustEnv points every tls stack at the merged bundle; nil without one.
func (m *material) trustEnv() map[string]string {
	if m.bundlePath == "" {
		return nil
	}
	return map[string]string{
		"SSL_CERT_FILE":       agentBundlePath,
		"CURL_CA_BUNDLE":      agentBundlePath,
		"GIT_SSL_CAINFO":      agentBundlePath,
		"REQUESTS_CA_BUNDLE":  agentBundlePath,
		"CARGO_HTTP_CAINFO":   agentBundlePath,
		"NODE_EXTRA_CA_CERTS": agentCertPath,
	}
}

// trustMounts binds the merged bundle and the added certificates where
// trustEnv points; nil without a bundle.
func (m *material) trustMounts() []string {
	if m.bundlePath == "" {
		return nil
	}
	return []string{"-v", m.bundlePath + ":" + agentBundlePath + ":ro", "-v", m.certPath + ":" + agentCertPath + ":ro"}
}

// proxyVars are the proxy env names a tool may read, all of them.
var proxyVars = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "no_proxy", "all_proxy"}
