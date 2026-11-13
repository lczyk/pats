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
	if stapleStatus(cs) == "revoked" {
		return ErrRevoked
	}
	return nil
}

// stapleStatus reads the stapled ocsp response for the verified chain: good,
// revoked, or unchecked (no staple, no chain, malformed, or status unknown).
func stapleStatus(cs tls.ConnectionState) string {
	if len(cs.OCSPResponse) == 0 || len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) < 2 {
		return "unchecked"
	}
	chain := cs.VerifiedChains[0]
	r, err := ocsp.ParseResponseForCert(cs.OCSPResponse, chain[0], chain[1])
	switch {
	case err != nil:
		return "unchecked"
	case r.Status == ocsp.Revoked:
		return "revoked"
	case r.Status == ocsp.Good:
		return "good"
	}
	return "unchecked"
}
