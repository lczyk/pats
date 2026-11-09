package sandbox

import (
	"crypto/x509"
	"fmt"
	"os"
	"sync"

	"github.com/lczyk/pats/src/sandbox/proxy"
)

// MachineProxy is the machine's own proxy as pats found it in its
// environment at start: the upstream its egress proxies chain through and
// open sandboxes are handed, and the trust (SSL_CERT_FILE) an upstream that
// terminates tls needs.
type MachineProxy struct {
	Chain *proxy.Chain
	Trust []byte // pem, nil without SSL_CERT_FILE
}

// ReadMachineProxy reads the machine's proxy env (proxy.ChainFromEnv) and,
// behind a proxy, the certificates SSL_CERT_FILE names. nil, nil: no proxy.
func ReadMachineProxy(getenv func(string) string) (*MachineProxy, error) {
	c, err := proxy.ChainFromEnv(getenv)
	if err != nil || c == nil {
		return nil, err
	}
	m := &MachineProxy{Chain: c}
	if p := getenv("SSL_CERT_FILE"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("SSL_CERT_FILE: %w", err)
		}
		if !x509.NewCertPool().AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("SSL_CERT_FILE %s: no certificates", p)
		}
		m.Trust = b
	}
	return m, nil
}

// machineProxy is read once per process: pats's environment does not change
// under it.
var machineProxy = sync.OnceValues(func() (*MachineProxy, error) { return ReadMachineProxy(os.Getenv) })

// Machine is this process's MachineProxy.
func Machine() (*MachineProxy, error) { return machineProxy() }

// chain is m's upstream, nil without one.
func (m *MachineProxy) chain() *proxy.Chain {
	if m == nil {
		return nil
	}
	return m.Chain
}

// trust is m's certificates, nil without any.
func (m *MachineProxy) trust() []byte {
	if m == nil {
		return nil
	}
	return m.Trust
}

// loopbackError refuses an upstream on the host's loopback under a driver
// whose sandboxes and proxies dial from a network of their own.
func (m *MachineProxy) loopbackError(driver string) error {
	u := m.chain().Loopback()
	if u == nil {
		return nil
	}
	return fmt.Errorf("the upstream %s is on this host's loopback, which a %s sandbox cannot reach; "+
		"bind the proxy (cntlm, px) to an address the sandbox can reach, or run without it: "+
		"env -u HTTPS_PROXY -u https_proxy -u HTTP_PROXY -u http_proxy pats ...", proxy.Redact(u), driver)
}

// machineCheck is the start's verdict on the machine proxy for a sandbox
// that has a network: an env WithEnv could not use, or an upstream on the
// loopback, which driver's sandboxes cannot reach.
func (o Options) machineCheck(driver string) error {
	if o.machineErr != nil {
		return o.machineErr
	}
	return o.Machine.loopbackError(driver)
}
