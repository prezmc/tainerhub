// Package pki manages the hub's private certificate authority, which issues
// the gateway's server certificate and one client certificate per agent.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
}

// LoadOrCreateCA reads ca.crt / ca.key from dir, creating a new CA on first run.
func LoadOrCreateCA(dir string) (*CA, error) {
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	if _, err := os.Stat(certPath); err == nil {
		cert, err := ReadCertFile(certPath)
		if err != nil {
			return nil, err
		}
		key, err := ReadKeyFile(keyPath)
		if err != nil {
			return nil, err
		}
		return &CA{Cert: cert, Key: key}, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          randSerial(),
		Subject:               pkix.Name{CommonName: "TainerHub CA", Organization: []string{"TainerHub"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	if err := WriteKeyFile(keyPath, key); err != nil {
		return nil, err
	}
	if err := WriteCertFile(certPath, der); err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key}, nil
}

// Fingerprint is the hex SHA-256 of the CA certificate, used to pin it.
func (ca *CA) Fingerprint() string { return Fingerprint(ca.Cert.Raw) }

func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Cert)
	return p
}

// ServerCert issues a TLS server certificate for the agent gateway, valid
// for the given DNS names / IPs. The chain includes the CA so agents can
// check the pinned fingerprint during enrollment.
func (ca *CA) ServerCert(hosts []string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: randSerial(),
		Subject:      pkix.Name{CommonName: "tainerhub-gateway"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der, ca.Cert.Raw}, PrivateKey: key, Leaf: leaf}, nil
}

// SignClientCSR issues an agent client certificate. The CSR's own subject is
// ignored: the CommonName is always the host ID chosen by the hub.
func (ca *CA) SignClientCSR(csrDER []byte, hostID string, validity time.Duration) (*x509.Certificate, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("parse csr: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("csr signature: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: randSerial(),
		Subject:      pkix.Name{CommonName: hostID, Organization: []string{"TainerHub agents"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// NewKeyAndCSR is used by agents to create their key pair.
func NewKeyAndCSR(commonName string) (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: commonName},
	}, crypto.Signer(key))
	return key, csr, err
}

func SerialString(c *x509.Certificate) string { return c.SerialNumber.Text(16) }

func randSerial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err)
	}
	return n
}

// ---- file helpers ----

func WriteCertFile(path string, der []byte) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func WriteKeyFile(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600)
}

func ReadCertFile(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New(path + ": not a PEM certificate")
	}
	return x509.ParseCertificate(blk.Bytes)
}

func ReadKeyFile(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New(path + ": not a PEM key")
	}
	return x509.ParseECPrivateKey(blk.Bytes)
}

// RenewingServerCert serves first and reissues the gateway's server
// certificate (from the same CA, for the same names) once it has less than
// renewBefore left, so a hub that runs for over a year without a restart
// keeps accepting agents. Use it as tls.Config.GetCertificate.
func (ca *CA) RenewingServerCert(first tls.Certificate, hosts []string, renewBefore time.Duration) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	var mu sync.Mutex
	cur := first
	return func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		mu.Lock()
		defer mu.Unlock()
		if leaf, err := x509.ParseCertificate(cur.Certificate[0]); err == nil && time.Until(leaf.NotAfter) > renewBefore {
			return &cur, nil
		}
		next, err := ca.ServerCert(hosts)
		if err != nil {
			return &cur, nil // keep serving the old one; try again on the next handshake
		}
		cur = next
		return &cur, nil
	}
}
