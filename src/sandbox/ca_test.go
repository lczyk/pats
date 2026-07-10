package sandbox

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

func TestGenCA(t *testing.T) {
	certPEM, keyPEM, err := genCA()
	require.NoError(t, err)
	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block)
	assert.Equal(t, block.Type, "CERTIFICATE")
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	assert.Equal(t, cert.IsCA, true)
	assert.Equal(t, cert.KeyUsage&x509.KeyUsageCertSign != 0, true)
	// key matches cert (this is what the proxy's tls.LoadX509KeyPair needs)
	_, err = tls.X509KeyPair(certPEM, keyPEM)
	assert.NoError(t, err)
}
