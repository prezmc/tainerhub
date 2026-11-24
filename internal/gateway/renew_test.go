package gateway_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/agent"
	"github.com/prezmc/tainerhub/internal/gateway"
	"github.com/prezmc/tainerhub/internal/pki"
	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/wire"
)

const renewHostID = "99999999-8888-7777-6666-555555555555"

type renewHarness struct {
	ca    *pki.CA
	store *memStore
	reg   *registry.Registry
	srv   *httptest.Server
	gw    *gateway.Gateway

	dropRenewals atomic.Int32 // renewal replies still to lose on the way
}

func newRenewHarness(t *testing.T) *renewHarness {
	t.Helper()
	// The CA is backdated so certificates that expired long ago can be
	// issued from it, as on a hub that has been running for years.
	caDir := t.TempDir()
	writeBackdatedCA(t, caDir)
	ca, err := pki.LoadOrCreateCA(caDir)
	if err != nil {
		t.Fatal(err)
	}
	h := &renewHarness{
		ca:    ca,
		store: &memStore{tokens: map[string]*gateway.JoinTokenRecord{}, hosts: map[string]*gateway.HostCert{renewHostID: {Name: "server-r"}}},
		reg:   registry.New(),
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	gw := &gateway.Gateway{CA: ca, Store: h.store, Reg: h.reg, Log: log, PingEvery: 200 * time.Millisecond, DeadAfter: 2 * time.Second}
	srvCert, err := ca.ServerCert([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	h.gw = gw
	inner := gw.Handler()
	h.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == wire.PathRenew && h.dropRenewals.Add(-1) >= 0 {
			// The hub does the renewal, but the reply never arrives.
			inner.ServeHTTP(httptest.NewRecorder(), r)
			panic(http.ErrAbortHandler)
		}
		inner.ServeHTTP(w, r)
	}))
	h.srv.TLS = gw.TLSConfig(srvCert)
	h.srv.EnableHTTP2 = true
	h.srv.StartTLS()
	t.Cleanup(h.srv.Close)
	return h
}

// issue gives the host a certificate as enrollment would, valid for validity.
func (h *renewHarness) issue(t *testing.T, validity time.Duration) tls.Certificate {
	t.Helper()
	key, csr, err := pki.NewKeyAndCSR("ignored")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := h.ca.SignClientCSR(csr, renewHostID, validity)
	if err != nil {
		t.Fatal(err)
	}
	h.store.mu.Lock()
	h.store.hosts[renewHostID].CertSerial = pki.SerialString(leaf)
	h.store.mu.Unlock()
	return tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
}

func (h *renewHarness) client(cert tls.Certificate) *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: h.ca.Pool(), Certificates: []tls.Certificate{cert}},
		ForceAttemptHTTP2: true,
	}}
}

// renew calls the renewal endpoint and returns the status and, on success,
// the new certificate paired with the key it was requested for.
func (h *renewHarness) renew(t *testing.T, cert tls.Certificate) (int, tls.Certificate) {
	var none tls.Certificate
	key, csr, err := pki.NewKeyAndCSR("evil-other-host") // subject must be ignored
	if err != nil {
		t.Error(err)
		return 0, none
	}
	body, _ := json.Marshal(wire.RenewRequest{CSR: csr})
	resp, err := h.client(cert).Post(h.srv.URL+wire.PathRenew, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Error(err)
		return 0, none
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, none
	}
	var rr wire.RenewResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		t.Error(err)
		return 0, none
	}
	leaf, err := x509.ParseCertificate(rr.Cert)
	if err != nil {
		t.Error(err)
		return 0, none
	}
	if !bytes.Equal(rr.CA, h.ca.Cert.Raw) {
		t.Error("renewal did not return the hub CA")
	}
	return resp.StatusCode, tls.Certificate{Certificate: [][]byte{rr.Cert}, PrivateKey: key, Leaf: leaf}
}

// connects counts the agent's connections from the activity feed.
func (h *renewHarness) connects() int {
	n := 0
	for _, a := range h.reg.Activity(registry.MaxActivity) {
		if a.Action == "connected" {
			n++
		}
	}
	return n
}

func (h *renewHarness) connectStatus(t *testing.T, cert tls.Certificate) int {
	t.Helper()
	resp, err := h.client(cert).Post(h.srv.URL+wire.PathConnect, "application/x-ndjson", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func TestRenewReplacesSerial(t *testing.T) {
	h := newRenewHarness(t)
	old := h.issue(t, time.Hour)

	if code := h.connectStatus(t, old); code != http.StatusOK {
		t.Fatalf("connect with the enrolled cert = %d", code)
	}
	code, renewed := h.renew(t, old)
	leaf := renewed.Leaf
	if code != http.StatusOK {
		t.Fatalf("renew = %d", code)
	}
	if leaf.Subject.CommonName != renewHostID {
		t.Fatalf("renewed cert is for %q; the CSR subject must be ignored", leaf.Subject.CommonName)
	}
	// The new serial is pending; the old one still works until the agent
	// connects with the new certificate.
	if got, p := h.store.serial(renewHostID), h.store.pending(renewHostID); got != pki.SerialString(old.Leaf) || p != pki.SerialString(leaf) {
		t.Fatalf("stored serial %s pending %s, want old %s pending %s", got, p, pki.SerialString(old.Leaf), pki.SerialString(leaf))
	}
	acts := h.reg.Activity(10)
	if len(acts) == 0 || acts[0].Action != "certificate renewed" {
		t.Errorf("no activity entry for the renewal: %+v", acts)
	}
	if code := h.connectStatus(t, old); code != http.StatusOK {
		t.Fatalf("connect with the old cert before the new one was used = %d, want 200", code)
	}
	if code := h.connectStatus(t, renewed); code != http.StatusOK {
		t.Fatalf("connect with the renewed cert = %d", code)
	}
	if got, p := h.store.serial(renewHostID), h.store.pending(renewHostID); got != pki.SerialString(leaf) || p != "" {
		t.Fatalf("after connecting with the new cert: serial %s pending %q, want %s and none", got, p, pki.SerialString(leaf))
	}

	// The old certificate is now refused, for connecting and for renewing.
	if code := h.connectStatus(t, old); code != http.StatusForbidden {
		t.Fatalf("connect with the old cert = %d, want 403", code)
	}
	if code, _ := h.renew(t, old); code != http.StatusForbidden {
		t.Fatalf("renew with the old cert = %d, want 403", code)
	}
	if code := h.connectStatus(t, renewed); code != http.StatusOK {
		t.Fatalf("connect with the renewed cert again = %d", code)
	}

	// No client certificate at all is refused.
	resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: h.ca.Pool()}}}).Post(h.srv.URL+wire.PathRenew, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("renew without a cert = %d, want 401", resp.StatusCode)
	}
}

func TestRenewRevokedRefused(t *testing.T) {
	h := newRenewHarness(t)
	cert := h.issue(t, time.Hour)
	h.store.mu.Lock()
	h.store.hosts[renewHostID].Revoked = true
	h.store.mu.Unlock()
	if code, _ := h.renew(t, cert); code != http.StatusForbidden {
		t.Fatalf("renew on a revoked host = %d, want 403", code)
	}
}

func TestConcurrentRenewalsOnlyOneWins(t *testing.T) {
	h := newRenewHarness(t)
	cert := h.issue(t, time.Hour)
	const n = 8
	codes := make([]int, n)
	certs := make([]tls.Certificate, n)
	var start, wg sync.WaitGroup
	start.Add(1)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start.Wait()
			codes[i], certs[i] = h.renew(t, cert)
		}()
	}
	start.Done()
	wg.Wait()
	ok := 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusForbidden:
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	h.store.mu.Lock()
	stored := h.store.renewals
	h.store.mu.Unlock()
	// Each stored renewal is one the hub answered; a later one (a retry
	// that saw the earlier one's result) replaces it.
	if ok == 0 || stored != ok {
		t.Fatalf("%d renewals succeeded, %d stored: %v", ok, stored, codes)
	}
	// Whatever the interleaving, only one of the issued certificates works.
	works := 0
	for i, c := range certs {
		if codes[i] == http.StatusOK && h.connectStatus(t, c) == http.StatusOK {
			works++
		}
	}
	if works != 1 {
		t.Fatalf("%d renewed certificates work, want exactly 1", works)
	}
}

// A renewal whose reply is lost leaves the agent with its old certificate,
// which keeps working: it can connect, and renewing again succeeds. Once
// the new certificate has connected, the old one stops working.
func TestRenewSurvivesLostReply(t *testing.T) {
	h := newRenewHarness(t)
	old := h.issue(t, time.Hour)
	_, csr, _ := pki.NewKeyAndCSR("x")
	body, _ := json.Marshal(wire.RenewRequest{CSR: csr})
	h.dropRenewals.Store(1)
	if code, _, err := h.post(old, wire.PathRenew, body); err == nil && code == http.StatusOK {
		t.Fatal("the renewal reply was meant to be lost")
	}
	lost := h.store.pending(renewHostID)
	if lost == "" {
		t.Fatal("the hub did not record the lost renewal")
	}
	if code := h.connectStatus(t, old); code != http.StatusOK {
		t.Fatalf("connect with the old cert after a lost renewal = %d, want 200", code)
	}
	code, renewed := h.renew(t, old)
	if code != http.StatusOK {
		t.Fatalf("retried renewal = %d, want 200", code)
	}
	if p := h.store.pending(renewHostID); p == lost || p != pki.SerialString(renewed.Leaf) {
		t.Fatalf("pending serial %s; lost %s, retry %s", p, lost, pki.SerialString(renewed.Leaf))
	}
	if code := h.connectStatus(t, renewed); code != http.StatusOK {
		t.Fatalf("connect with the renewed cert = %d", code)
	}
	if code := h.connectStatus(t, old); code != http.StatusForbidden {
		t.Fatalf("connect with the old cert after the new one connected = %d, want 403", code)
	}
	if code, _ := h.renew(t, old); code != http.StatusForbidden {
		t.Fatalf("renew with the old cert after the new one connected = %d, want 403", code)
	}
}

// The real agent: its first renewal's reply is lost, it carries on with the
// old certificate and renews again, then switches to the new one.
func TestAgentRetriesRenewalAfterLostReply(t *testing.T) {
	h := newRenewHarness(t)
	cert := h.issue(t, 10*time.Minute) // due for renewal at once
	oldSerial := pki.SerialString(cert.Leaf)
	h.dropRenewals.Store(1)
	dir := h.startEnrolledAgent(t, cert)
	waitFor(t, "renewed certificate in use", func() bool {
		return h.reg.Get(renewHostID) != nil && h.store.serial(renewHostID) != oldSerial && h.store.pending(renewHostID) == ""
	})
	h.store.mu.Lock()
	n := h.store.renewals
	h.store.mu.Unlock()
	if n != 2 {
		t.Fatalf("%d renewals recorded, want 2 (the lost one and the retry)", n)
	}
	h.checkRenewedOnDisk(t, dir, oldSerial)
	if code := h.connectStatus(t, cert); code != http.StatusForbidden {
		t.Fatalf("connect with the old cert = %d, want 403", code)
	}
}

// Renewing with a renewed certificate that has not connected yet shows the
// agent got it: it becomes current and the one it replaced stops working.
func TestRenewWithPendingCertPromotesIt(t *testing.T) {
	h := newRenewHarness(t)
	old := h.issue(t, time.Hour)
	_, first := h.renew(t, old)
	code, second := h.renew(t, first)
	if code != http.StatusOK {
		t.Fatalf("renew with the pending cert = %d", code)
	}
	if s, p := h.store.serial(renewHostID), h.store.pending(renewHostID); s != pki.SerialString(first.Leaf) || p != pki.SerialString(second.Leaf) {
		t.Fatalf("serial %s pending %s", s, p)
	}
	if code := h.connectStatus(t, old); code != http.StatusForbidden {
		t.Fatalf("connect with the original cert = %d, want 403", code)
	}
}

// A revoked host's pending certificate is refused like its current one.
func TestPendingCertRevoked(t *testing.T) {
	h := newRenewHarness(t)
	old := h.issue(t, time.Hour)
	_, renewed := h.renew(t, old)
	h.store.mu.Lock()
	h.store.hosts[renewHostID].Revoked = true
	h.store.mu.Unlock()
	for _, c := range []tls.Certificate{old, renewed} {
		if code := h.connectStatus(t, c); code != http.StatusForbidden {
			t.Fatalf("connect on a revoked host = %d, want 403", code)
		}
		if code, _ := h.renew(t, c); code != http.StatusForbidden {
			t.Fatalf("renew on a revoked host = %d, want 403", code)
		}
	}
}

// Connecting with the renewed certificate drops a session still using the
// old one (perhaps someone else holding its key).
func TestPromotionDropsOldSession(t *testing.T) {
	h := newRenewHarness(t)
	old := h.issue(t, time.Hour)
	pr, pw := io.Pipe()
	defer pw.Close()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+wire.PathConnect, pr)
	resp, err := h.client(old).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	oldGone := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, resp.Body); close(oldGone) }()
	waitFor(t, "old session", func() bool { return h.reg.Get(renewHostID) != nil })

	_, renewed := h.renew(t, old)
	select {
	case <-oldGone:
		t.Fatal("old session dropped by the renewal alone")
	case <-time.After(100 * time.Millisecond):
	}
	if code := h.connectStatus(t, renewed); code != http.StatusOK {
		t.Fatalf("connect with the renewed cert = %d", code)
	}
	select {
	case <-oldGone:
	case <-time.After(3 * time.Second):
		t.Fatal("old session still open after the renewed certificate connected")
	}
}

// startEnrolledAgent runs an agent whose data dir already holds cert, as if
// it had enrolled long ago.
func (h *renewHarness) startEnrolledAgent(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	return h.startEnrolledAgentWith(t, cert, 0)
}

// startEnrolledAgentWith is startEnrolledAgent with a RenewBefore setting
// (0 for the default).
func (h *renewHarness) startEnrolledAgentWith(t *testing.T, cert tls.Certificate, renewBefore time.Duration) string {
	t.Helper()
	dir := t.TempDir()
	if err := pki.WriteKeyFile(filepath.Join(dir, "agent.key"), cert.PrivateKey.(*ecdsa.PrivateKey)); err != nil {
		t.Fatal(err)
	}
	if err := pki.WriteCertFile(filepath.Join(dir, "agent.crt"), cert.Certificate[0]); err != nil {
		t.Fatal(err)
	}
	if err := pki.WriteCertFile(filepath.Join(dir, "ca.crt"), h.ca.Cert.Raw); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ag, err := agent.New(agent.Config{
		HubAddr: h.srv.Listener.Addr().String(), DataDir: dir, Version: "test", Log: log,
		Docker:    &fakeDocker{running: map[string]bool{}, events: make(chan wire.DockerEvent)},
		PingEvery: 200 * time.Millisecond, DeadAfter: 2 * time.Second, SnapshotEvery: time.Hour, StatsEvery: -1,
		RenewCheckEvery: 200 * time.Millisecond, RenewBefore: renewBefore,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = ag.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("agent did not stop")
		}
	})
	return dir
}

// checkRenewedOnDisk checks the agent saved the hub's current certificate,
// with a private key file only it can read and no leftovers.
func (h *renewHarness) checkRenewedOnDisk(t *testing.T, dir, oldSerial string) {
	t.Helper()
	onDisk, err := pki.ReadCertFile(filepath.Join(dir, "agent.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if s := pki.SerialString(onDisk); s == oldSerial || s != h.store.serial(renewHostID) {
		t.Fatalf("agent.crt serial %s; old %s, hub has %s", s, oldSerial, h.store.serial(renewHostID))
	}
	if _, err := tls.LoadX509KeyPair(filepath.Join(dir, "agent.crt"), filepath.Join(dir, "agent.key")); err != nil {
		t.Fatalf("saved key and cert do not match: %v", err)
	}
	if st, err := os.Stat(filepath.Join(dir, "agent.key")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("agent.key mode: %v %v", st.Mode(), err)
	}
	for _, f := range []string{"agent.key.new", "agent.crt.new"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s left behind", f)
		}
	}
}

// An agent connected with a certificate that comes within the renewal
// window renews it while connected and reconnects with the new one.
func TestAgentRenewsWhileConnected(t *testing.T) {
	h := newRenewHarness(t)
	// Due for renewal (30 days before expiry) about a second from now.
	cert := h.issue(t, 30*24*time.Hour+1500*time.Millisecond)
	oldSerial := pki.SerialString(cert.Leaf)
	dir := h.startEnrolledAgent(t, cert)

	waitFor(t, "first connection", func() bool { return h.reg.Get(renewHostID) != nil })
	if h.store.serial(renewHostID) != oldSerial {
		t.Fatal("renewed before it was due")
	}
	waitFor(t, "renewal", func() bool { return h.store.serial(renewHostID) != oldSerial })
	// The agent drops the old stream and connects again with the new cert
	// (the hub would refuse the old one now).
	waitFor(t, "reconnection", func() bool { return h.connects() == 2 && h.reg.Get(renewHostID) != nil })
	h.checkRenewedOnDisk(t, dir, oldSerial)

	// It stays connected, and does not keep renewing: the new cert has a year.
	time.Sleep(1 * time.Second)
	if h.reg.Get(renewHostID) == nil {
		t.Fatal("agent not connected after renewal")
	}
	h.store.mu.Lock()
	n := h.store.renewals
	h.store.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d renewals, want 1", n)
	}
}

// An agent starting with a certificate already near expiry renews before
// connecting.
func TestAgentRenewsBeforeConnecting(t *testing.T) {
	h := newRenewHarness(t)
	cert := h.issue(t, 10*time.Minute)
	oldSerial := pki.SerialString(cert.Leaf)
	dir := h.startEnrolledAgent(t, cert)
	waitFor(t, "connection", func() bool { return h.reg.Get(renewHostID) != nil })
	if h.store.serial(renewHostID) == oldSerial {
		t.Fatal("connected without renewing")
	}
	h.checkRenewedOnDisk(t, dir, oldSerial)
}
