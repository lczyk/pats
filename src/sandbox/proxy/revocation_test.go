package proxy

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"golang.org/x/crypto/ocsp"
)

// chain mints a CA and a leaf it signed.
func chain(t *testing.T) (ca, leaf *x509.Certificate, caKey crypto.Signer) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &key.PublicKey, key)
	require.NoError(t, err)
	ca, err = x509.ParseCertificate(der)
	require.NoError(t, err)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "up.test"}, DNSNames: []string{"up.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err = x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, key)
	require.NoError(t, err)
	leaf, err = x509.ParseCertificate(der)
	require.NoError(t, err)
	return ca, leaf, key
}

func staple(t *testing.T, ca, leaf *x509.Certificate, key crypto.Signer, status int) []byte {
	t.Helper()
	resp, err := ocsp.CreateResponse(ca, ca, ocsp.Response{
		Status: status, SerialNumber: leaf.SerialNumber,
		ThisUpdate: time.Now().Add(-time.Minute), NextUpdate: time.Now().Add(time.Hour),
		RevokedAt: time.Now().Add(-time.Minute),
	}, key)
	require.NoError(t, err)
	return resp
}

func TestCheckStaple(t *testing.T) {
	ca, leaf, key := chain(t)
	chains := [][]*x509.Certificate{{leaf, ca}}

	revoked := tls.ConnectionState{VerifiedChains: chains, OCSPResponse: staple(t, ca, leaf, key, ocsp.Revoked)}
	assert.That(t, errors.Is(checkStaple(revoked), ErrRevoked))
	assert.Equal(t, upstreamReason(checkStaple(revoked)), "revoked")

	good := tls.ConnectionState{VerifiedChains: chains, OCSPResponse: staple(t, ca, leaf, key, ocsp.Good)}
	assert.NoError(t, checkStaple(good))

	assert.Equal(t, stapleStatus(good), "good")
	assert.Equal(t, stapleStatus(revoked), "revoked")
	unknown := tls.ConnectionState{VerifiedChains: chains, OCSPResponse: staple(t, ca, leaf, key, ocsp.Unknown)}
	assert.Equal(t, stapleStatus(unknown), "unchecked")
	assert.NoError(t, checkStaple(unknown))
	assert.Equal(t, stapleStatus(tls.ConnectionState{VerifiedChains: chains, OCSPResponse: []byte("junk")}), "unchecked")

	// no staple, a staple that does not parse, or no verified chain: pass.
	assert.NoError(t, checkStaple(tls.ConnectionState{VerifiedChains: chains}))
	assert.NoError(t, checkStaple(tls.ConnectionState{VerifiedChains: chains, OCSPResponse: []byte("junk")}))
	assert.NoError(t, checkStaple(tls.ConnectionState{OCSPResponse: revoked.OCSPResponse}))
}

// New wires the check into the upstream transport.
func TestNewWiresStapleCheck(t *testing.T) {
	p, err := New(Options{})
	require.NoError(t, err)
	tr, ok := p.Upstream.(*http.Transport)
	require.That(t, ok, "upstream is not an *http.Transport")
	require.NotNil(t, tr.TLSClientConfig.VerifyConnection)
}
