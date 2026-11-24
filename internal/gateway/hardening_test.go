package gateway_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/gateway"
	"github.com/prezmc/tainerhub/internal/pki"
	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/wire"
)

func (m *memStore) RecordEnrollment(_ context.Context, _, hostName, remoteIP string, reenrolled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enrollments = append(m.enrollments, fmt.Sprintf("%s %s %v", hostName, remoteIP, reenrolled))
	return nil
}

// writeBackdatedCA puts a CA created three years ago in dir, for
// LoadOrCreateCA to load.
func writeBackdatedCA(t *testing.T, dir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "TainerHub CA"},
		NotBefore: now.AddDate(-3, 0, 0), NotAfter: now.AddDate(7, 0, 0),
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.WriteKeyFile(filepath.Join(dir, "ca.key"), key); err != nil {
		t.Fatal(err)
	}
	if err := pki.WriteCertFile(filepath.Join(dir, "ca.crt"), der); err != nil {
		t.Fatal(err)
	}
}

// issueBetween gives the host a client certificate valid from nb to na
// (possibly long past), signed by the hub CA as enrollment would.
func (h *renewHarness) issueBetween(t *testing.T, nb, na time.Time, eku x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: renewHostID},
		NotBefore: nb, NotAfter: na,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{eku},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, h.ca.Cert, &key.PublicKey, h.ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	h.store.mu.Lock()
	h.store.hosts[renewHostID].CertSerial = pki.SerialString(leaf)
	h.store.mu.Unlock()
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func (h *renewHarness) expired(t *testing.T, ago time.Duration) tls.Certificate {
	na := time.Now().Add(-ago)
	return h.issueBetween(t, na.AddDate(-1, 0, 0), na, x509.ExtKeyUsageClientAuth)
}

// post sends body to path with cert, returning the status and reply text,
// or an error if the hub would not even complete the request (TLS refusal).
func (h *renewHarness) post(cert tls.Certificate, path string, body []byte) (int, string, error) {
	resp, err := h.client(cert).Post(h.srv.URL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(b), nil
}

func TestRenewExpiredWithinGrace(t *testing.T) {
	h := newRenewHarness(t)
	cert := h.expired(t, 30*24*time.Hour)
	code, renewed := h.renew(t, cert)
	if code != http.StatusOK {
		t.Fatalf("renew with a cert expired 30 days ago = %d, want 200", code)
	}
	if !renewed.Leaf.NotAfter.After(time.Now().Add(300 * 24 * time.Hour)) {
		t.Fatalf("renewed cert expires %v", renewed.Leaf.NotAfter)
	}
	if acts := h.reg.Activity(1); len(acts) == 0 || acts[0].Action != "certificate renewed after it had expired" {
		t.Errorf("activity: %+v", acts)
	}
	if code := h.connectStatus(t, renewed); code != http.StatusOK {
		t.Fatalf("connect with the renewed cert = %d", code)
	}
	// The expired one is spent now.
	if code, _ := h.renew(t, cert); code != http.StatusForbidden {
		t.Fatalf("second renewal with the expired cert = %d, want 403", code)
	}
}

func TestRenewExpiredBeyondGraceRefused(t *testing.T) {
	h := newRenewHarness(t)
	cert := h.expired(t, wire.RenewGrace+24*time.Hour)
	_, csr, _ := pki.NewKeyAndCSR("x")
	body, _ := json.Marshal(wire.RenewRequest{CSR: csr})
	code, _, err := h.post(cert, wire.PathRenew, body)
	if err == nil && code == http.StatusOK {
		t.Fatal("renewed a certificate that expired beyond the grace period")
	}
	if h.store.serial(renewHostID) != pki.SerialString(cert.Leaf) {
		t.Fatal("serial changed")
	}
}

func TestConnectRefusesExpiredCert(t *testing.T) {
	h := newRenewHarness(t)
	cert := h.expired(t, time.Hour)
	code, text, err := h.post(cert, wire.PathConnect, nil)
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusUnauthorized || !strings.Contains(text, wire.CertExpiredText) {
		t.Fatalf("connect with an expired cert = %d %q, want 401 %q", code, text, wire.CertExpiredText)
	}
	if h.reg.Get(renewHostID) != nil {
		t.Fatal("expired agent attached")
	}
}

func TestRenewExpiredOldSerialRefused(t *testing.T) {
	h := newRenewHarness(t)
	old := h.expired(t, 24*time.Hour)
	h.expired(t, time.Hour) // a newer certificate for the host replaces it
	if code, _ := h.renew(t, old); code != http.StatusForbidden {
		t.Fatalf("renew with a superseded expired cert = %d, want 403", code)
	}
	h.store.mu.Lock()
	h.store.hosts[renewHostID].Revoked = true
	h.store.mu.Unlock()
	cur := h.expired(t, time.Hour)
	if code, _ := h.renew(t, cur); code != http.StatusForbidden {
		t.Fatalf("renew an expired cert of a revoked host = %d, want 403", code)
	}
}

// Certificates the CA issued for something else, or from another CA, never
// authenticate an agent, whether current or expired.
func TestWrongCertificatesRefused(t *testing.T) {
	h := newRenewHarness(t)
	now := time.Now()
	server := h.issueBetween(t, now.Add(-time.Hour), now.Add(time.Hour), x509.ExtKeyUsageServerAuth)
	if code, _, err := h.post(server, wire.PathConnect, nil); err == nil && code == http.StatusOK {
		t.Fatal("connected with a server-auth certificate")
	}
	other := newRenewHarness(t) // a different CA
	foreign := other.issue(t, time.Hour)
	h.store.mu.Lock()
	h.store.hosts[renewHostID].CertSerial = pki.SerialString(foreign.Leaf)
	h.store.mu.Unlock()
	if code, _, err := h.post(foreign, wire.PathRenew, nil); err == nil && code == http.StatusOK {
		t.Fatal("renewed a certificate from another CA")
	}
}

// An agent that was switched off until after its certificate expired renews
// it on start and connects, without re-enrolling.
func TestAgentRenewsExpiredCert(t *testing.T) {
	h := newRenewHarness(t)
	cert := h.expired(t, 10*24*time.Hour)
	oldSerial := pki.SerialString(cert.Leaf)
	dir := h.startEnrolledAgent(t, cert)
	waitFor(t, "connection", func() bool { return h.reg.Get(renewHostID) != nil })
	h.checkRenewedOnDisk(t, dir, oldSerial)
}

// enroll posts an enrollment and returns the status and certificate.
func enroll(t *testing.T, h *renewHarness, token string) (int, *x509.Certificate) {
	t.Helper()
	_, csr, _ := pki.NewKeyAndCSR("x")
	body, _ := json.Marshal(wire.EnrollRequest{Token: token, Hostname: "box", CSR: csr})
	resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: h.ca.Pool()}}}).
		Post(h.srv.URL+wire.PathEnroll, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	var er wire.EnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(er.Cert)
	return resp.StatusCode, c
}

func (h *renewHarness) addToken(id, secret string) string {
	h.store.mu.Lock()
	h.store.tokens[id] = &gateway.JoinTokenRecord{ID: id, HostID: renewHostID, SecretHash: gateway.HashSecret(secret), ExpiresAt: time.Now().Add(time.Hour)}
	h.store.mu.Unlock()
	return wire.JoinToken{ID: id, Secret: secret, CAFingerprint: h.ca.Fingerprint()}.Encode()
}

func TestEnrollmentRecorded(t *testing.T) {
	h := newRenewHarness(t)
	if code, _ := enroll(t, h, h.addToken("t1", "one")); code != http.StatusOK {
		t.Fatalf("enroll = %d", code)
	}
	if acts := h.reg.Activity(1); len(acts) == 0 || acts[0].Action != "enrolled" || acts[0].Host != "server-r" {
		t.Fatalf("activity: %+v", acts)
	}
	first := h.store.serial(renewHostID)
	if code, _ := enroll(t, h, h.addToken("t2", "two")); code != http.StatusOK {
		t.Fatalf("re-enroll = %d", code)
	}
	if acts := h.reg.Activity(1); len(acts) == 0 || !strings.HasPrefix(acts[0].Action, "re-enrolled") || !strings.Contains(acts[0].Action, "previous agent certificate stopped working") {
		t.Fatalf("activity: %+v", acts)
	}
	if h.store.serial(renewHostID) == first {
		t.Fatal("serial not replaced")
	}
	h.store.mu.Lock()
	got := h.store.enrollments
	h.store.mu.Unlock()
	want := []string{"server-r 127.0.0.1 false", "server-r 127.0.0.1 true"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("audit records %q, want %q", got, want)
	}
}

func TestEnrollFailuresRateLimited(t *testing.T) {
	h := newRenewHarness(t)
	good := h.addToken("good", "right")
	bad := wire.JoinToken{ID: "good", Secret: "wrong", CAFingerprint: h.ca.Fingerprint()}.Encode()
	for i := range 10 {
		if code, _ := enroll(t, h, bad); code != http.StatusForbidden {
			t.Fatalf("attempt %d = %d, want 403", i+1, code)
		}
	}
	// Now even the right token is turned away from this address for a while.
	if code, _ := enroll(t, h, good); code != http.StatusTooManyRequests {
		t.Fatalf("after 10 failures = %d, want 429", code)
	}
	h.store.mu.Lock()
	used := h.store.tokens["good"].Used
	h.store.mu.Unlock()
	if used {
		t.Fatal("token consumed while rate limited")
	}
}

func TestEnrollSuccessesNotLimited(t *testing.T) {
	h := newRenewHarness(t)
	for i := range 15 {
		if code, _ := enroll(t, h, h.addToken(fmt.Sprint("t", i), "s")); code != http.StatusOK {
			t.Fatalf("enrollment %d = %d", i+1, code)
		}
	}
}

// A misbehaving agent flooding the hub: only its first hello reaches the
// database, events beyond the allowance are dropped, and snapshots are
// coalesced with the latest one winning.
func TestAgentFloodThrottled(t *testing.T) {
	h := newRenewHarness(t)
	cert := h.issue(t, time.Hour)
	pr, pw := io.Pipe()
	defer pw.Close()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+wire.PathConnect, pr)
	resp, err := h.client(cert).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go func() { _, _ = io.Copy(io.Discard, resp.Body) }()
	enc := json.NewEncoder(pw)
	send := func(m wire.AgentMessage) {
		if err := enc.Encode(m); err != nil {
			t.Fatal(err)
		}
	}
	for range 5 {
		send(wire.AgentMessage{Hello: &wire.Hello{Hostname: "flood"}})
	}
	for i := range 500 {
		send(wire.AgentMessage{Event: &wire.DockerEvent{Type: "container", Action: "die", ID: "c", Name: fmt.Sprint("c", i)}})
	}
	const snaps = 200
	for i := range snaps {
		send(wire.AgentMessage{Snapshot: &wire.Snapshot{At: int64(i), Containers: []wire.Container{{ID: "c", Name: fmt.Sprint("n", i)}}}})
	}
	waitFor(t, "latest snapshot", func() bool {
		a := h.reg.Get(renewHostID)
		if a == nil {
			return false
		}
		cs := a.Status().Containers
		return len(cs) == 1 && cs[0].Name == fmt.Sprint("n", snaps-1)
	})
	h.store.mu.Lock()
	hellos := h.store.hellos
	h.store.mu.Unlock()
	if hellos != 1 {
		t.Errorf("%d hellos written, want 1", hellos)
	}
	dies := 0
	for _, a := range h.reg.Activity(registry.MaxActivity) {
		if a.Action == "die" {
			dies++
		}
	}
	// The burst allowance (100), plus a little refill while sending.
	if dies < 50 || dies > 150 {
		t.Errorf("%d of 500 events accepted, want about 100", dies)
	}
}

// startRealServer runs the gateway's own *http.Server (with its connection
// limits) and returns its address. grace, if set, replaces how long
// certificate-less connections may stay open.
func (h *renewHarness) startRealServer(t *testing.T, grace time.Duration) string {
	t.Helper()
	gw := &gateway.Gateway{CA: h.ca, Store: h.store, Reg: h.reg, Log: h.gw.Log}
	if grace > 0 {
		gateway.SetCertlessGrace(gw, grace)
	}
	srvCert, err := h.ca.ServerCert([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	srv := gw.Server("127.0.0.1:0", srvCert)
	if srv.IdleTimeout == 0 || srv.HTTP2 == nil || srv.HTTP2.MaxConcurrentStreams == 0 {
		t.Fatalf("server limits not set: idle %v, http2 %+v", srv.IdleTimeout, srv.HTTP2)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// tlsDial completes a TLS handshake with the hub, with cert if given, and
// keeps the connection for the test's lifetime.
func (h *renewHarness) tlsDial(t *testing.T, addr string, cert *tls.Certificate) *tls.Conn {
	t.Helper()
	cfg := &tls.Config{RootCAs: h.ca.Pool(), NextProtos: []string{"http/1.1"}}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	c, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// open reports whether the hub is keeping c open: a read waits rather than
// failing. In TLS 1.3 the server checks the client's certificate (or its
// absence) after the client's handshake has finished, so a refusal shows up
// as an alert on the first read.
func open(c net.Conn) bool {
	_ = c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	return err != nil && errors.As(err, &ne) && ne.Timeout()
}

// The real server caps connections per address that finished a handshake
// without a client certificate. Connections that never handshake, and
// agents with a certificate, don't count.
func TestServerCapsUnauthenticatedConns(t *testing.T) {
	h := newRenewHarness(t)
	addr := h.startRealServer(t, 0)

	// Bare TCP connections (no handshake yet) take no slots.
	for range 40 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
	}
	var conns []*tls.Conn
	for range 32 {
		conns = append(conns, h.tlsDial(t, addr, nil))
	}
	for i, c := range conns {
		if !open(c) {
			t.Fatalf("certificate-less connection %d refused, want up to 32 kept", i+1)
		}
	}
	if open(h.tlsDial(t, addr, nil)) {
		t.Fatal("33rd certificate-less connection kept")
	}

	// An agent from the same address still gets in and renews.
	cert := h.issue(t, time.Hour)
	if !open(h.tlsDial(t, addr, &cert)) {
		t.Fatal("connection with a client certificate refused")
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: h.ca.Pool(), Certificates: []tls.Certificate{cert}}, ForceAttemptHTTP2: true}
	defer tr.CloseIdleConnections()
	_, csr, _ := pki.NewKeyAndCSR("x")
	body, _ := json.Marshal(wire.RenewRequest{CSR: csr})
	resp, err := (&http.Client{Transport: tr}).Post("https://"+addr+wire.PathRenew, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("renew = %d", resp.StatusCode)
	}

	conns[0].Close()
	waitFor(t, "slot freed", func() bool { return open(h.tlsDial(t, addr, nil)) })
}

// Connections without a client certificate are closed after a short grace
// period, unless they are in the middle of enrolling.
func TestCertlessConnsTimedOut(t *testing.T) {
	h := newRenewHarness(t)
	const grace = 300 * time.Millisecond
	addr := h.startRealServer(t, grace)

	idle := h.tlsDial(t, addr, nil)
	cert := h.issue(t, time.Hour)
	agent := h.tlsDial(t, addr, &cert)
	_ = idle.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	if _, err := idle.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("idle certificate-less connection not closed: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("closed after %v, want about %v", d, grace)
	}
	if !open(agent) {
		t.Fatal("connection with a client certificate was closed")
	}

	// An enrollment trickling in for longer than the grace period is
	// served to the end.
	enr := h.tlsDial(t, addr, nil)
	payload, _ := json.Marshal(wire.EnrollRequest{Token: "not-a-token"})
	fmt.Fprintf(enr, "POST %s HTTP/1.1\r\nHost: hub\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", wire.PathEnroll, len(payload))
	for i, b := range payload {
		if _, err := enr.Write([]byte{b}); err != nil {
			t.Fatalf("enrollment cut off after %d bytes: %v", i, err)
		}
		time.Sleep(3 * grace / time.Duration(len(payload)))
	}
	_ = enr.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(enr), nil)
	if err != nil {
		t.Fatalf("no reply to a slow enrollment: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("enroll with a bad token = %d, want 400", resp.StatusCode)
	}
}

// When the hub refuses a connection because the certificate expired (here
// the agent does not think it is due, as with a clock running behind the
// hub's), the agent renews straight away and reconnects.
func TestAgentRenewsWhenHubSaysExpired(t *testing.T) {
	h := newRenewHarness(t)
	cert := h.expired(t, 10*24*time.Hour)
	oldSerial := pki.SerialString(cert.Leaf)
	dir := h.startEnrolledAgentWith(t, cert, -365*24*time.Hour)
	waitFor(t, "connection", func() bool { return h.reg.Get(renewHostID) != nil })
	h.checkRenewedOnDisk(t, dir, oldSerial)
}
