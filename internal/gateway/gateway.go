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
	TouchHost(ctx context.Context, hostID string, hello *wire.Hello, at time.Time) error
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
	validity := g.CertValidity
	if validity == 0 {
		validity = 365 * 24 * time.Hour
	}
	cert, err := g.CA.SignClientCSR(req.CSR, rec.HostID, validity)
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

func (g *Gateway) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "client certificate required", http.StatusUnauthorized)
		return
	}
	// VerifyClientCertIfGiven has already checked the chain against our CA.
	leaf := r.TLS.PeerCertificates[0]
	hostID := leaf.Subject.CommonName
	hc, err := g.Store.GetHostCert(r.Context(), hostID)
	if err != nil || hc.Revoked || hc.CertSerial != pki.SerialString(leaf) {
		g.log().Warn("agent connection refused", "host_id", hostID, "remote", r.RemoteAddr)
		http.Error(w, "certificate revoked or unknown", http.StatusForbidden)
		return
	}

	rc := http.NewResponseController(w)
	_ = rc.EnableFullDuplex() // needed for HTTP/1.1; HTTP/2 is duplex already
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	remoteIP := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		remoteIP = h
	}
	agent := g.Reg.AttachFrom(hostID, hc.Name, remoteIP, cancel)
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
