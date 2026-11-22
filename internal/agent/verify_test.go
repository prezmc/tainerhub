package agent

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/pki"
)

// The agent trusts the hub by its pinned private CA, not by hostname, so
// it can reach the hub by IP or any name. It must still refuse certificates
// from another CA and agent (client-only) certificates from the same CA.
func TestVerifyHubByCANotHostname(t *testing.T) {
	ca, err := pki.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ca.ServerCert([]string{"some-other-name.example"})
	if err != nil {
		t.Fatal(err)
	}
	state := func(c tls.Certificate) tls.ConnectionState {
		var certs []*x509.Certificate
		for _, der := range c.Certificate {
			x, err := x509.ParseCertificate(der)
			if err != nil {
				t.Fatal(err)
			}
			certs = append(certs, x)
		}
		return tls.ConnectionState{PeerCertificates: certs}
	}
	if err := verifyHub(state(srv), ca.Pool()); err != nil {
		t.Fatalf("hub cert from the pinned CA must be accepted regardless of name: %v", err)
	}
	if err := verifyPinned(state(srv), ca.Fingerprint()); err != nil {
		t.Fatalf("enrollment pin: %v", err)
	}

	other, _ := pki.LoadOrCreateCA(t.TempDir())
	if err := verifyHub(state(srv), other.Pool()); err == nil {
		t.Fatal("cert from a different CA accepted")
	}
	if err := verifyPinned(state(srv), other.Fingerprint()); err == nil {
		t.Fatal("pin mismatch accepted")
	}

	_, csr, _ := pki.NewKeyAndCSR("agent")
	agentCert, err := ca.SignClientCSR(csr, "h1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyHub(tls.ConnectionState{PeerCertificates: []*x509.Certificate{agentCert}}, ca.Pool()); err == nil {
		t.Fatal("an agent's client certificate must not pass as the hub")
	}
}

func TestSameHostPath(t *testing.T) {
	for _, c := range []struct {
		src, dir string
		want     bool
	}{
		{"/opt/tainerhub/stacks", "/opt/tainerhub/stacks", true},
		{"/host_mnt/opt/tainerhub/stacks", "/opt/tainerhub/stacks", true},         // Docker Desktop
		{"/host_mnt/private/tmp/tainerhub/stacks", "/tmp/tainerhub/stacks", true}, // Docker Desktop on macOS
		{"/srv/stacks", "/opt/tainerhub/stacks", false},
		{"/host_mnt/srv/stacks", "/opt/tainerhub/stacks", false},
	} {
		if got := sameHostPath(c.src, c.dir); got != c.want {
			t.Errorf("sameHostPath(%q, %q) = %v", c.src, c.dir, got)
		}
	}
}
