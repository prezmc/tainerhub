package pki

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"
)

func TestRenewingServerCert(t *testing.T) {
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := ca.ServerCert([]string{"hub.test"})
	if err != nil {
		t.Fatal(err)
	}
	serial := func(c *tls.Certificate) string {
		leaf, _ := x509.ParseCertificate(c.Certificate[0])
		return leaf.SerialNumber.String()
	}
	get := ca.RenewingServerCert(first, []string{"hub.test"}, 30*24*time.Hour)
	if c, _ := get(nil); serial(c) != serial(&first) {
		t.Fatal("reissued a fresh certificate")
	}
	// A renewal window longer than the certificate's life forces a reissue.
	get = ca.RenewingServerCert(first, []string{"hub.test"}, 400*24*time.Hour)
	c, _ := get(nil)
	if serial(c) == serial(&first) {
		t.Fatal("didn't reissue a certificate inside the renewal window")
	}
	leaf, _ := x509.ParseCertificate(c.Certificate[0])
	if leaf.VerifyHostname("hub.test") != nil {
		t.Fatal("reissued certificate lost its names")
	}
}
