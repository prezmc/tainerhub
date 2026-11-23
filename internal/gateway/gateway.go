// Package gateway is the hub's agent-facing TLS endpoint (default :7443).
// It enrolls new agents with one-time join tokens and holds one full-duplex
// HTTP/2 stream per connected agent, authenticated with client certificates.
//
// This listener must NOT sit behind a TLS-terminating proxy such as Nginx
// Proxy Manager: the agents' client certificates have to reach the hub.
package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prezmc/tainerhub/internal/pki"
	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/wire"
)

var ErrNotFound = errors.New("not found")

// ErrSerialChanged is returned by RenewHostCert when the host's stored serial
// is no longer the one the caller presented (another renewal won, or the
// host was re-enrolled or revoked in the meantime).
var ErrSerialChanged = errors.New("certificate serial changed")

type JoinTokenRecord struct {
	ID         string
	HostID     string
	SecretHash []byte // SHA-256 of the secret
	ExpiresAt  time.Time
	Used       bool
}

type HostCert struct {
	Name       string
	CertSerial string
	Revoked    bool
}

// Store is the persistence the gateway needs. The Postgres store implements it.
type Store interface {
	GetJoinToken(ctx context.Context, id string) (JoinTokenRecord, error)
	// CompleteEnrollment must atomically mark the token used (failing if it
	// already was) and record the host's certificate serial.
	CompleteEnrollment(ctx context.Context, tokenID, hostID, certSerial string) error
	GetHostCert(ctx context.Context, hostID string) (HostCert, error)
	// RenewHostCert must replace the host's serial only if it still equals
	// oldSerial (compare-and-swap), returning ErrSerialChanged otherwise, so
	// two concurrent renewals with the same certificate cannot both win.
	RenewHostCert(ctx context.Context, hostID, oldSerial, newSerial string) error
	TouchHost(ctx context.Context, hostID string, hello *wire.Hello, at time.Time) error
}

// CertRenewalRecorder is optionally implemented by the Store to keep a
// durable record (the audit log) of agent certificate renewals.
type CertRenewalRecorder interface {
	RecordCertRenewal(ctx context.Context, hostID, hostName, remoteIP string) error
}

type Gateway struct {
	CA           *pki.CA
	Store        Store
	Reg          *registry.Registry
	Log          *slog.Logger
	CertValidity time.Duration // agent cert lifetime, default 1 year
	PingEvery    time.Duration // default 20s
	DeadAfter    time.Duration // drop silent agents, default 90s
}

func HashSecret(secret string) []byte {
	s := sha256.Sum256([]byte(secret))
	return s[:]
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+wire.PathEnroll, g.handleEnroll)
	mux.HandleFunc("POST "+wire.PathConnect, g.handleConnect)
	mux.HandleFunc("POST "+wire.PathRenew, g.handleRenew)
	return mux
}

func (g *Gateway) TLSConfig(serverCert tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.VerifyClientCertIfGiven, // enroll has no cert yet; connect checks below
		ClientCAs:    g.CA.Pool(),
		NextProtos:   []string{"h2", "http/1.1"},
	}
}

// Server returns an *http.Server ready for ListenAndServeTLS("", "").
func (g *Gateway) Server(addr string, serverCert tls.Certificate) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           g.Handler(),
		TLSConfig:         g.TLSConfig(serverCert),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(g.log().Handler(), slog.LevelDebug),
	}
}

func (g *Gateway) log() *slog.Logger {
	if g.Log != nil {
		return g.Log
	}
	return slog.Default()
}

func (g *Gateway) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req wire.EnrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tok, err := wire.ParseJoinToken(req.Token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	deny := func(reason string) {
		g.log().Warn("agent enrollment refused", "reason", reason, "remote", r.RemoteAddr, "hostname", req.Hostname)
		http.Error(w, "join token invalid, used or expired", http.StatusForbidden)
	}
	if tok.CAFingerprint != g.CA.Fingerprint() {
		deny("token was issued by a different hub CA")
		return
	}
	rec, err := g.Store.GetJoinToken(r.Context(), tok.ID)
	if err != nil {
		deny("unknown token")
		return
	}
	if subtle.ConstantTimeCompare(HashSecret(tok.Secret), rec.SecretHash) != 1 {
		deny("wrong secret")
		return
	}
	if rec.Used || time.Now().After(rec.ExpiresAt) {
		deny("token used or expired")
		return
	}
	cert, err := g.CA.SignClientCSR(req.CSR, rec.HostID, g.validity())
	if err != nil {
		http.Error(w, "bad csr", http.StatusBadRequest)
		return
	}
	if err := g.Store.CompleteEnrollment(r.Context(), rec.ID, rec.HostID, pki.SerialString(cert)); err != nil {
		deny("token already used")
		return
	}
	g.log().Info("agent enrolled", "host_id", rec.HostID, "hostname", req.Hostname, "remote", r.RemoteAddr)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(wire.EnrollResponse{HostID: rec.HostID, Cert: cert.Raw, CA: g.CA.Cert.Raw})
}

func (g *Gateway) validity() time.Duration {
	if g.CertValidity > 0 {
		return g.CertValidity
	}
	return 365 * 24 * time.Hour
}

func remoteIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// authAgent identifies an agent by its client certificate, for connect and
// renew alike. It writes the error response itself and returns ok=false
// when the agent is refused.
func (g *Gateway) authAgent(w http.ResponseWriter, r *http.Request, what string) (leaf *x509.Certificate, hc HostCert, ok bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "client certificate required", http.StatusUnauthorized)
		return nil, hc, false
	}
	// VerifyClientCertIfGiven has already checked the chain (and expiry)
	// against our CA. The serial check makes only the host's latest
	// certificate count, so renewed or re-enrolled hosts' old ones stop working.
	leaf = r.TLS.PeerCertificates[0]
	hostID := leaf.Subject.CommonName
	hc, err := g.Store.GetHostCert(r.Context(), hostID)
	if err != nil || hc.Revoked || hc.CertSerial != pki.SerialString(leaf) {
		g.log().Warn("agent "+what+" refused", "host_id", hostID, "remote", r.RemoteAddr)
		http.Error(w, "certificate revoked or unknown", http.StatusForbidden)
		return nil, hc, false
	}
	return leaf, hc, true
}

// handleRenew issues a new client certificate to an agent that still holds a
// valid one, so agents never need re-enrolling by hand as theirs nears expiry.
func (g *Gateway) handleRenew(w http.ResponseWriter, r *http.Request) {
	leaf, hc, ok := g.authAgent(w, r, "certificate renewal")
	if !ok {
		return
	}
	hostID := leaf.Subject.CommonName
	var req wire.RenewRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// As at enrollment the CSR's subject is ignored: the host ID comes from
	// the certificate the agent authenticated with.
	cert, err := g.CA.SignClientCSR(req.CSR, hostID, g.validity())
	if err != nil {
		http.Error(w, "bad csr", http.StatusBadRequest)
		return
	}
	oldSerial, newSerial := pki.SerialString(leaf), pki.SerialString(cert)
	if err := g.Store.RenewHostCert(r.Context(), hostID, oldSerial, newSerial); err != nil {
		g.log().Warn("agent certificate renewal refused", "host_id", hostID, "remote", r.RemoteAddr, "err", err)
		if errors.Is(err, ErrSerialChanged) {
			http.Error(w, "certificate revoked or unknown", http.StatusForbidden)
		} else {
			http.Error(w, "renewal failed", http.StatusInternalServerError)
		}
		return
	}
	g.log().Info("agent certificate renewed", "host", hc.Name, "host_id", hostID, "remote", r.RemoteAddr,
		"old_serial", oldSerial, "new_serial", newSerial, "expires", cert.NotAfter)
	if g.Reg != nil {
		g.Reg.AddActivity(registry.Activity{Kind: "host", Action: "certificate renewed", HostID: hostID, Host: hc.Name})
	}
	if rec, ok := g.Store.(CertRenewalRecorder); ok {
		if err := rec.RecordCertRenewal(context.WithoutCancel(r.Context()), hostID, hc.Name, remoteIP(r)); err != nil {
			g.log().Warn("audit certificate renewal", "err", err)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(wire.RenewResponse{Cert: cert.Raw, CA: g.CA.Cert.Raw})
}

func (g *Gateway) handleConnect(w http.ResponseWriter, r *http.Request) {
	leaf, hc, ok := g.authAgent(w, r, "connection")
	if !ok {
		return
	}
	hostID := leaf.Subject.CommonName

	rc := http.NewResponseController(w)
	_ = rc.EnableFullDuplex() // needed for HTTP/1.1; HTTP/2 is duplex already
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	agent := g.Reg.AttachFrom(hostID, hc.Name, remoteIP(r), cancel)
	defer g.Reg.Detach(agent)
	log := g.log().With("host", hc.Name, "host_id", hostID)
	log.Info("agent connected", "remote", r.RemoteAddr, "proto", r.Proto)
	defer log.Info("agent disconnected")

	pingEvery, deadAfter := g.PingEvery, g.DeadAfter
	if pingEvery == 0 {
		pingEvery = 20 * time.Second
	}
	if deadAfter == 0 {
		deadAfter = 90 * time.Second
	}
	var lastRead atomic.Int64
	lastRead.Store(time.Now().UnixNano())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // writer: the only goroutine that touches w
		defer wg.Done()
		defer cancel()
		enc := json.NewEncoder(w)
		tick := time.NewTicker(pingEvery)
		defer tick.Stop()
		for {
			var m wire.HubMessage
			select {
			case <-ctx.Done():
				return
			case m = <-agent.Out():
			case <-tick.C:
				if time.Since(time.Unix(0, lastRead.Load())) > deadAfter {
					log.Warn("agent silent too long, dropping")
					return
				}
				m = wire.HubMessage{Ping: true}
			}
			if err := enc.Encode(m); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}()
	go func() { <-ctx.Done(); _ = r.Body.Close() }() // unblocks the reader on revoke

	dec := wire.NewDecoder(r.Body)
	for {
		var m wire.AgentMessage
		if err := dec.Decode(&m); err != nil {
			break
		}
		lastRead.Store(time.Now().UnixNano())
		if m.Hello != nil {
			if err := g.Store.TouchHost(ctx, hostID, m.Hello, time.Now()); err != nil {
				log.Warn("touch host", "err", err)
			}
			log.Info("agent hello", "hostname", m.Hello.Hostname, "docker", m.Hello.DockerVersion, "agent", m.Hello.AgentVersion)
		}
		agent.Handle(m, g.Reg.OnEvent)
	}
	cancel()
	wg.Wait()
	_ = g.Store.TouchHost(context.Background(), hostID, nil, time.Now())
}
