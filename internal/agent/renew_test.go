package agent

import (
	"crypto/ecdsa"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/pki"
)

// An agent stopped part way through saving a renewed certificate must come
// back with a matching key and cert, preferring the new pair (the hub has
// already switched to it).
func TestFinishInterruptedRenewal(t *testing.T) {
	ca, err := pki.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	issue := func() (*pkiPair, error) {
		key, csr, err := pki.NewKeyAndCSR("x")
		if err != nil {
			return nil, err
		}
		c, err := ca.SignClientCSR(csr, "host-1", time.Hour)
		return &pkiPair{key: key, der: c.Raw, serial: pki.SerialString(c)}, err
	}
	for _, stage := range []string{"before renames", "between renames", "half-written"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			a := &Agent{cfg: Config{DataDir: dir}, log: slog.New(slog.DiscardHandler)}
			oldP, err := issue()
			if err != nil {
				t.Fatal(err)
			}
			newP, err := issue()
			if err != nil {
				t.Fatal(err)
			}
			mustOK(t, pki.WriteCertFile(filepath.Join(dir, "ca.crt"), ca.Cert.Raw))
			mustOK(t, pki.WriteKeyFile(filepath.Join(dir, "agent.key"), oldP.key))
			mustOK(t, pki.WriteCertFile(filepath.Join(dir, "agent.crt"), oldP.der))
			want := newP.serial
			switch stage {
			case "before renames":
				mustOK(t, pki.WriteKeyFile(filepath.Join(dir, "agent.key.new"), newP.key))
				mustOK(t, pki.WriteCertFile(filepath.Join(dir, "agent.crt.new"), newP.der))
			case "between renames":
				mustOK(t, pki.WriteKeyFile(filepath.Join(dir, "agent.key"), newP.key))
				mustOK(t, pki.WriteCertFile(filepath.Join(dir, "agent.crt.new"), newP.der))
			case "half-written":
				mustOK(t, pki.WriteKeyFile(filepath.Join(dir, "agent.key.new"), newP.key))
				mustOK(t, os.WriteFile(filepath.Join(dir, "agent.crt.new"), []byte("-----BEGIN CERT"), 0o644))
				want = oldP.serial
			}
			creds, err := a.loadCreds()
			if err != nil {
				t.Fatal(err)
			}
			if got := pki.SerialString(creds.Leaf); got != want {
				t.Fatalf("loaded serial %s, want %s", got, want)
			}
		})
	}
}

type pkiPair struct {
	key    *ecdsa.PrivateKey
	der    []byte
	serial string
}

func mustOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
