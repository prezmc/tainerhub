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

// ErrSerialChanged is returned by RenewHostCert and PromoteHostCert when the
// host's stored serials are no longer the ones the caller saw (another
// renewal won, or the host was re-enrolled or revoked in the meantime).
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
	// PendingSerial is a renewed certificate the agent has not connected
	// with yet ("" if none). Until it does, CertSerial keeps working, so a
	// renewal whose reply never reached the agent doesn't lock it out.
	PendingSerial string
	Revoked       bool
}

// accepts reports whether serial is one of the host's working certificates.
func (hc HostCert) accepts(serial string) bool {
	if hc.Revoked || serial == "" {
		return false
	}
	return serial == hc.CertSerial || serial == hc.PendingSerial
}

// Store is the persistence the gateway needs. The Postgres store implements it.
type Store interface {
	GetJoinToken(ctx context.Context, id string) (JoinTokenRecord, error)
	// CompleteEnrollment must atomically mark the token used (failing if it
	// already was) and record the host's certificate serial, clearing any
	// pending one.
	CompleteEnrollment(ctx context.Context, tokenID, hostID, certSerial string) error
	GetHostCert(ctx context.Context, hostID string) (HostCert, error)
	// RenewHostCert must set the host's pending serial to newSerial only if
	// its current and pending serials still equal seen's (compare-and-swap),
	// returning ErrSerialChanged otherwise, so two concurrent renewals
	// cannot both win. The current serial is left as it is.
	RenewHostCert(ctx context.Context, hostID string, seen HostCert, newSerial string) error
	// PromoteHostCert must make the pending serial the current one (and
	// clear pending) only if the pending serial still equals serial,
	// returning ErrSerialChanged otherwise.
	PromoteHostCert(ctx context.Context, hostID, serial string) error
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

	initOnce      sync.Once
	enrollFails   *failLimiter
	conns         *connCap
	certlessGrace time.Duration // tests shorten it
}

func (g *Gateway) init() {
	g.initOnce.Do(func() {
		g.enrollFails = newFailLimiter(enrollFailures, enrollWindow)
		grace := g.certlessGrace
		if grace == 0 {
			grace = certlessGrace
		}
		g.conns = newConnCap(maxUnauthConnsPerIP, grace)
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
	mux.HandleFunc("POST "+wire.PathTransfer+"{id}", g.handleTransferUpload)
	mux.HandleFunc("GET "+wire.PathTransfer+"{id}", g.handleTransferDownload)
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
		Addr:    addr,
		Handler: g.Handler(),
		// Connections without a client certificate are counted and timed
		// out per address once their handshake is done; see connCap.
		TLSConfig:         g.conns.tlsConfig(g.TLSConfig(serverCert)),
		ConnContext:       g.conns.connContext,
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
	// Enrolling is the one thing a connection without a client certificate
	// may stay open for.
	defer g.conns.enrolling(r.Context())()
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
	// Whatever is still connected with an earlier certificate (perhaps a
	// stolen key, the reason to re-enroll) is dropped now, not whenever it
	// next reconnects.
	if g.Reg != nil {
		g.Reg.Disconnect(rec.HostID)
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
	// The serial check makes only the host's current certificate (and a
	// renewed one not yet used) count, so re-enrolled hosts' old ones, and
	// renewed hosts' once the new one has connected, stop working.
	hc, err = g.Store.GetHostCert(r.Context(), hostID)
	if err != nil || !hc.accepts(pki.SerialString(leaf)) {
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
	seen := hc
	if oldSerial == hc.PendingSerial {
		// Renewing with the renewed certificate shows the agent got it, as
		// connecting with it would: it becomes the current one first.
		if !g.promote(r.Context(), hostID, oldSerial) {
			g.log().Warn("agent certificate renewal refused", "host_id", hostID, "remote", r.RemoteAddr, "err", ErrSerialChanged)
			http.Error(w, "certificate revoked or unknown", http.StatusForbidden)
			return
		}
		seen = HostCert{CertSerial: oldSerial}
	}
	// The new serial is only pending: the certificate presented now keeps
	// working until the agent connects with the new one, so if this reply
	// is lost the agent simply renews again.
	if err := g.Store.RenewHostCert(r.Context(), hostID, seen, newSerial); err != nil {
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
	serial := pki.SerialString(leaf)
	// The first connection with a renewed certificate retires the old one.
	if serial == hc.PendingSerial && !g.promote(r.Context(), hostID, serial) {
		g.log().Warn("agent connection refused", "host_id", hostID, "remote", r.RemoteAddr, "err", ErrSerialChanged)
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
	agent := g.Reg.AttachFrom(hostID, hc.Name, remoteIP(r), cancel)
	defer g.Reg.Detach(agent)
	log := g.log().With("host", hc.Name, "host_id", hostID)
	// The certificate may have been superseded (re-enrolled, or a renewed
	// one promoted) between the check above and attaching; whoever did that
	// disconnected the host before this connection was attached, so check
	// again now that it is.
	if now, err := g.Store.GetHostCert(ctx, hostID); err != nil || !now.accepts(serial) {
		log.Warn("agent connection dropped: its certificate was superseded while connecting", "remote", r.RemoteAddr)
		return
	}
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
			// An agent that stops reading would otherwise block this write
			// forever once flow control fills up, and with it the whole
			// teardown below. A missed deadline fails the write instead.
			_ = rc.SetWriteDeadline(time.Now().Add(deadAfter))
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
	// A writer stuck on an agent that stopped reading would hold us here
	// until its deadline; a deadline in the past fails its write at once.
	_ = rc.SetWriteDeadline(time.Now())
	wg.Wait()
	// Unregister before touching the database, so a stalled database can't
	// keep a dead agent looking connected and calls to it waiting for their
	// full timeouts.
	g.Reg.Detach(agent)
	tctx, tcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer tcancel()
	if err := g.Store.TouchHost(tctx, hostID, nil, time.Now()); err != nil {
		log.Debug("touch host on disconnect", "err", err)
	}
}

// promote makes a host's renewed certificate its current one, now that the
// agent has shown it holds it, and drops any connection still using the
// old one. It reports false if serial is no longer the host's renewed or
// current certificate.
func (g *Gateway) promote(ctx context.Context, hostID, serial string) bool {
	if err := g.Store.PromoteHostCert(ctx, hostID, serial); err != nil {
		// A concurrent connection with the same certificate may have
		// promoted it first.
		hc, gerr := g.Store.GetHostCert(ctx, hostID)
		return gerr == nil && !hc.Revoked && hc.CertSerial == serial
	}
	g.log().Info("agent is using its renewed certificate; the previous one no longer works", "host_id", hostID)
	if g.Reg != nil {
		g.Reg.Disconnect(hostID)
	}
	return true
}

// transferFor authenticates an agent and finds the transfer it names. Only
// the two hosts the hub tied the transfer to may use it; for any other the
// transfer doesn't exist.
func (g *Gateway) transferFor(w http.ResponseWriter, r *http.Request, what string) (*registry.Transfer, string, bool) {
	leaf, _, _, ok := g.authAgent(w, r, what, false)
	if !ok {
		return nil, "", false
	}
	id := r.PathValue("id")
	var t *registry.Transfer
	if wire.ValidTransferID(id) && g.Reg != nil {
		t = g.Reg.Transfer(id)
	}
	if t == nil {
		http.Error(w, "no such transfer", http.StatusNotFound)
		return nil, "", false
	}
	return t, leaf.Subject.CommonName, true
}

// handleTransferUpload receives the sending agent's tar stream and passes it
// on to the receiving agent as it arrives. It answers once all of it has
// been read by the receiver, or the transfer failed.
func (g *Gateway) handleTransferUpload(w http.ResponseWriter, r *http.Request) {
	t, hostID, ok := g.transferFor(w, r, "transfer upload")
	if !ok {
		return
	}
	n, err := t.Upload(hostID, r.Body)
	switch {
	case errors.Is(err, registry.ErrNoTransfer):
		http.Error(w, "no such transfer", http.StatusNotFound)
	case errors.Is(err, registry.ErrTransferClaimed):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, registry.ErrTransferTooBig):
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
	case err != nil:
		g.log().Warn("transfer upload failed", "transfer", t.ID, "host_id", hostID, "bytes", n, "err", err)
		http.Error(w, "transfer failed", http.StatusBadGateway)
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{"bytes": n})
	}
}

// handleTransferDownload streams a transfer to the receiving agent.
func (g *Gateway) handleTransferDownload(w http.ResponseWriter, r *http.Request) {
	t, hostID, ok := g.transferFor(w, r, "transfer download")
	if !ok {
		return
	}
	if hostID != t.Dst {
		http.Error(w, "no such transfer", http.StatusNotFound)
		return
	}
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()
	// A failure after the headers went out can only be signalled by cutting
	// the stream short; the receiver then sees a truncated archive.
	if n, err := t.Download(hostID, w, func() { _ = rc.Flush() }); err != nil {
		g.log().Warn("transfer download failed", "transfer", t.ID, "host_id", hostID, "bytes", n, "err", err)
		panic(http.ErrAbortHandler)
	}
}
