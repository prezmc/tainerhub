// Package gateway is the hub's agent-facing TLS endpoint (default :7443).
// It enrolls new agents with one-time join tokens and holds one full-duplex
// HTTP/2 stream per connected agent, authenticated with client certificates.
//
// This listener must NOT sit behind a TLS-terminating proxy such as Nginx
// Proxy Manager: the agents' client certificates have to reach the hub.
package gateway

import (
	"bytes"
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

// EnrollmentRecorder is optionally implemented by the Store to keep a
// durable record (the audit log) of agent enrollments. reenrolled means the
// host already had a certificate, which has now stopped working.
type EnrollmentRecorder interface {
	RecordEnrollment(ctx context.Context, hostID, hostName, remoteIP string, reenrolled bool) error
}

type Gateway struct {
	CA           *pki.CA
	Store        Store
	Reg          *registry.Registry
	Log          *slog.Logger
	CertValidity time.Duration // agent cert lifetime, default 1 year
	PingEvery    time.Duration // default 20s
	DeadAfter    time.Duration // drop silent agents, default 90s

	initOnce    sync.Once
	enrollFails *failLimiter
	conns       *connCap
}

func (g *Gateway) init() {
	g.initOnce.Do(func() {
		g.enrollFails = newFailLimiter(enrollFailures, enrollWindow)
		g.conns = newConnCap(maxUnauthConnsPerIP)
	})
}

func HashSecret(secret string) []byte {
	s := sha256.Sum256([]byte(secret))
	return s[:]
}

func (g *Gateway) Handler() http.Handler {
	g.init()
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
		// Enrolling agents have no certificate yet, and an agent whose
		// certificate has expired may still renew it for a while, which the
		// standard check (VerifyClientCertIfGiven) would refuse outright. So
		// the handshake asks for a certificate and checks it here instead;
		// authAgent then applies the stricter rules of each endpoint.
		ClientAuth: tls.RequestClientCert,
		ClientCAs:  g.CA.Pool(), // tells agents which certificate to send
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return nil
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			_, err = g.verifyClientCert(leaf, true)
			return err
		},
		NextProtos: []string{"h2", "http/1.1"},
	}
}

// Server returns an *http.Server ready for ListenAndServeTLS("", "").
func (g *Gateway) Server(addr string, serverCert tls.Certificate) *http.Server {
	g.init()
	return &http.Server{
		Addr:              addr,
		Handler:           g.Handler(),
		TLSConfig:         g.TLSConfig(serverCert),
		ReadHeaderTimeout: 10 * time.Second, // also bounds the TLS handshake
		// Drops connections with no request in flight. A connected agent's
		// stream stays in flight, and both sides ping every 20s anyway.
		IdleTimeout:    2 * time.Minute,
		MaxHeaderBytes: 64 << 10,
		// Each agent needs one stream per connection (renewals use their
		// own), so a small cap stops one client multiplexing hundreds.
		HTTP2:     &http.HTTP2Config{MaxConcurrentStreams: 16},
		ConnState: g.conns.state,
		ErrorLog:  slog.NewLogLogger(g.log().Handler(), slog.LevelDebug),
	}
}

// verifyClientCert checks that an agent certificate was issued by our CA for
// client authentication and is within its validity period. With
// allowExpired, a certificate that expired less than wire.RenewGrace ago
// passes too (checked as of its last valid moment), so its agent can renew
// it; expired then reports that it has expired.
func (g *Gateway) verifyClientCert(leaf *x509.Certificate, allowExpired bool) (expired bool, err error) {
	now := time.Now()
	at := now
	if now.After(leaf.NotAfter) {
		expired = true
		if !allowExpired || now.Sub(leaf.NotAfter) > wire.RenewGrace {
			return true, errors.New(wire.CertExpiredText)
		}
		at = leaf.NotAfter
	}
	_, err = leaf.Verify(x509.VerifyOptions{
		Roots:       g.CA.Pool(),
		CurrentTime: at,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	return expired, err
}

func (g *Gateway) log() *slog.Logger {
	if g.Log != nil {
		return g.Log
	}
	return slog.Default()
}

// readBody decodes a small JSON request body, giving the client bodyTimeout
// to send it so a slow trickle can't hold the connection and handler open.
func readBody(w http.ResponseWriter, r *http.Request, v any) error {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyTimeout))
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v)
}

func (g *Gateway) handleEnroll(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	// Join tokens are long random secrets, so this is not about guessing
	// them; it stops one address making the hub do unbounded work.
	if g.enrollFails.blocked(ip, time.Now()) {
		g.log().Warn("agent enrollment refused", "reason", "too many failed attempts from this address", "remote", r.RemoteAddr)
		http.Error(w, "too many failed enrollment attempts; try again later", http.StatusTooManyRequests)
		return
	}
	var req wire.EnrollRequest
	if err := readBody(w, r, &req); err != nil {
		g.enrollFails.fail(ip, time.Now())
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tok, err := wire.ParseJoinToken(req.Token)
	if err != nil {
		g.enrollFails.fail(ip, time.Now())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	deny := func(reason string) {
		g.enrollFails.fail(ip, time.Now())
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
		g.enrollFails.fail(ip, time.Now())
		http.Error(w, "bad csr", http.StatusBadRequest)
		return
	}
	// A host that already has a certificate is being re-enrolled: its old
	// certificate stops working, which is worth telling the admins about.
	prev, err := g.Store.GetHostCert(r.Context(), rec.HostID)
	if err != nil {
		g.log().Warn("agent enrollment: look up host", "host_id", rec.HostID, "err", err)
	}
	reenrolled := prev.CertSerial != ""
	if err := g.Store.CompleteEnrollment(r.Context(), rec.ID, rec.HostID, pki.SerialString(cert)); err != nil {
		deny("token already used")
		return
	}
	g.log().Info("agent enrolled", "host", prev.Name, "host_id", rec.HostID, "hostname", req.Hostname, "remote", r.RemoteAddr, "reenrolled", reenrolled)
	g.recordEnrollment(r, rec.HostID, prev.Name, reenrolled)
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

// recordEnrollment puts the enrollment in the activity feed and, if the
// store keeps one, the audit log.
func (g *Gateway) recordEnrollment(r *http.Request, hostID, hostName string, reenrolled bool) {
	action := "enrolled"
	if reenrolled {
		action = "re-enrolled — the previous agent certificate stopped working"
	}
	if g.Reg != nil {
		g.Reg.AddActivity(registry.Activity{Kind: "host", Action: action, HostID: hostID, Host: hostName})
	}
	if rec, ok := g.Store.(EnrollmentRecorder); ok {
		if err := rec.RecordEnrollment(context.WithoutCancel(r.Context()), hostID, hostName, remoteIP(r), reenrolled); err != nil {
			g.log().Warn("audit enrollment", "err", err)
		}
	}
}

// authAgent identifies an agent by its client certificate, for connect and
// renew alike. It writes the error response itself and returns ok=false
// when the agent is refused. Only renewal accepts a recently expired
// certificate (allowExpired); expired reports that it was one.
func (g *Gateway) authAgent(w http.ResponseWriter, r *http.Request, what string, allowExpired bool) (leaf *x509.Certificate, hc HostCert, expired, ok bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "client certificate required", http.StatusUnauthorized)
		return nil, hc, false, false
	}
	leaf = r.TLS.PeerCertificates[0]
	hostID := leaf.Subject.CommonName
	// The handshake only asked for a certificate, so check it fully here:
	// issued by our CA for client auth, and unexpired unless allowed. The
	// handshake has already proved the agent holds its private key.
	expired, err := g.verifyClientCert(leaf, allowExpired)
	if err != nil {
		g.log().Warn("agent "+what+" refused", "host_id", hostID, "remote", r.RemoteAddr, "err", err, "expired", leaf.NotAfter)
		if expired {
			http.Error(w, wire.CertExpiredText, http.StatusUnauthorized)
		} else {
			http.Error(w, "certificate not accepted", http.StatusUnauthorized)
		}
		return nil, hc, expired, false
	}
	// The serial check makes only the host's latest certificate count, so
	// renewed or re-enrolled hosts' old ones stop working.
	hc, err = g.Store.GetHostCert(r.Context(), hostID)
	if err != nil || hc.Revoked || hc.CertSerial != pki.SerialString(leaf) {
		g.log().Warn("agent "+what+" refused", "host_id", hostID, "remote", r.RemoteAddr)
		http.Error(w, "certificate revoked or unknown", http.StatusForbidden)
		return nil, hc, expired, false
	}
	return leaf, hc, expired, true
}

// handleRenew issues a new client certificate to an agent that holds a
// valid one, or one that expired within wire.RenewGrace, so agents never
// need re-enrolling by hand as theirs nears expiry, nor after a host was
// switched off for a while.
func (g *Gateway) handleRenew(w http.ResponseWriter, r *http.Request) {
	leaf, hc, expired, ok := g.authAgent(w, r, "certificate renewal", true)
	if !ok {
		return
	}
	hostID := leaf.Subject.CommonName
	var req wire.RenewRequest
	if err := readBody(w, r, &req); err != nil {
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
		"old_serial", oldSerial, "new_serial", newSerial, "expires", cert.NotAfter, "had_expired", expired)
	if g.Reg != nil {
		action := "certificate renewed"
		if expired {
			action = "certificate renewed after it had expired"
		}
		g.Reg.AddActivity(registry.Activity{Kind: "host", Action: action, HostID: hostID, Host: hc.Name})
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
	leaf, hc, _, ok := g.authAgent(w, r, "connection", false)
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

	// Snapshots are applied (and decoded) by their own goroutine at a
	// limited rate; see snapshotGate.
	snaps := newSnapshotGate()
	wg.Add(1)
	go func() { defer wg.Done(); snaps.run(ctx, snapshotEvery, agent.HandleSnapshot) }()

	// An enrolled agent is trusted with its own host, not with the hub's
	// resources: what it sends is rate-limited so that a compromised or
	// buggy one can't load the database or wake every browser endlessly.
	events := newTokenBucket(eventBurst, eventsPerSec, time.Now())
	var dropped int
	var lastDropWarn time.Time
	helloSeen := false

	dec := wire.NewDecoder(r.Body)
	for {
		raw, err := dec.Next()
		if err != nil {
			break
		}
		lastRead.Store(time.Now().UnixNano())
		if bytes.HasPrefix(raw, snapshotPrefix) {
			snaps.putRaw(bytes.Clone(raw))
			continue
		}
		var m wire.AgentMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			log.Warn("unreadable message from agent; disconnecting", "err", err)
			break
		}
		switch {
		case m.Hello != nil:
			// Agents say hello once per connection; each one costs a
			// database write, so later ones are ignored.
			if helloSeen {
				log.Debug("ignoring repeated hello from agent")
				continue
			}
			helloSeen = true
			if err := g.Store.TouchHost(ctx, hostID, m.Hello, time.Now()); err != nil {
				log.Warn("touch host", "err", err)
			}
			log.Info("agent hello", "hostname", m.Hello.Hostname, "docker", m.Hello.DockerVersion, "agent", m.Hello.AgentVersion)
		case m.Snapshot != nil: // encoded unusually, so already decoded
			snaps.put(m.Snapshot, [32]byte{})
			continue
		case m.Event != nil:
			if now := time.Now(); !events.allow(now) {
				// The next snapshot still shows the containers' real state;
				// only some entries in the activity feed are lost.
				dropped++
				if now.Sub(lastDropWarn) > time.Minute {
					log.Warn("agent is sending Docker events faster than the hub accepts them; dropping some", "dropped", dropped)
					lastDropWarn, dropped = now, 0
				}
				continue
			}
		}
		agent.Handle(m, g.Reg.OnEvent)
	}
	cancel()
	wg.Wait()
	_ = g.Store.TouchHost(context.Background(), hostID, nil, time.Now())
}
