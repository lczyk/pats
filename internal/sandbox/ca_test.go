package sandbox

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"
)

func TestGenCA(t *testing.T) {
	certPEM, keyPEM, err := genCA()
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("cert pem malformed")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !cert.IsCA {
		t.Error("not a CA cert")
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("missing cert-sign key usage")
	}
	// key matches cert (this is what the proxy's tls.LoadX509KeyPair needs)
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Errorf("cert/key pair mismatch: %v", err)
	}
}
