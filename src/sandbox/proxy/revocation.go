package proxy

import (
	"crypto/tls"
	"errors"

	"golang.org/x/crypto/ocsp"
)

// ErrRevoked is an upstream whose stapled ocsp response says its certificate
// is revoked.
var ErrRevoked = errors.New("upstream certificate is revoked (stapled ocsp)")

// checkStaple is the upstream tls.Config's VerifyConnection: it runs after
// chain verification, on resumed sessions too (VerifyPeerCertificate does
// not), and refuses a connection whose stapled ocsp response says revoked. no
// staple, or one that does not parse, passes: revocation infrastructure never
// fails a run, and the audit marks such a request unchecked.
func checkStaple(cs tls.ConnectionState) error {
	if len(cs.OCSPResponse) == 0 || len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) < 2 {
		return nil
	}
	chain := cs.VerifiedChains[0]
	r, err := ocsp.ParseResponseForCert(cs.OCSPResponse, chain[0], chain[1])
	if err != nil {
		return nil
	}
	if r.Status == ocsp.Revoked {
		return ErrRevoked
	}
	return nil
}
