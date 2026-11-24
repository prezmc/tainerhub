// Package agent runs on each Docker host. It enrolls with the hub once using
// a join token, then keeps one outbound mTLS stream open to the hub.
package agent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prezmc/tainerhub/internal/pki"
	"github.com/prezmc/tainerhub/internal/regcred"
	"github.com/prezmc/tainerhub/internal/wire"
)

type Config struct {
	HubAddr   string // host:port of the hub's agent gateway, e.g. tainerhub.example.com:7443
	JoinToken string // only needed until the agent has enrolled
	// JoinTokenFile is where the hub leaves a join token for its own server's
	// agent (see cmd/tainerhub/localagent.go). The agent waits for it.
	JoinTokenFile string
	DataDir       string // stores agent.crt, agent.key, ca.crt
	Version       string
	Docker        Docker
	Stacks        *StackRunner // nil: compose stacks are not available on this agent
	Log           *slog.Logger

	// Listing containers is costly for Docker with the containerd image store
	// (it looks up every container's image), so the agent re-lists only on
	// Docker events and on this timer, which refreshes texts like "Up 5 minutes".
	SnapshotEvery     time.Duration // default 30s while someone has TainerHub open
	IdleSnapshotEvery time.Duration // default 5m while nobody does
	StatsEvery        time.Duration // default 10s while someone has TainerHub open; negative disables stats
	IdleStatsEvery    time.Duration // default 60s while nobody does
	PingEvery         time.Duration // default 20s
	DeadAfter         time.Duration // default 90s

	// The hub's client certificates expire (after a year by default), so the
	// agent renews its own once it is within RenewBefore of expiry: before
	// each connection attempt and every RenewCheckEvery while connected.
	RenewBefore     time.Duration // default 30 days
	RenewCheckEvery time.Duration // default 12h
}

// HealthFile (in DataDir) is refreshed while the agent is connected to the hub.
const HealthFile = "connected"

// Healthy reports whether the agent has heard from the hub recently.
func Healthy(dataDir string, maxAge time.Duration) bool {
	st, err := os.Stat(filepath.Join(dataDir, HealthFile))
	return err == nil && time.Since(st.ModTime()) < maxAge
}

type Agent struct {
	cfg  Config
	log  *slog.Logger
	root context.Context // lives for the whole Run, across reconnects
	// creds is the client certificate in use. Renewal replaces it; only one
	// renewal runs at a time (before a session, or inside one).
	creds atomic.Pointer[tls.Certificate]
	// hostStats measures the whole host for the hub's placement suggestions.
	hostStats *hostSampler
}

func New(cfg Config) (*Agent, error) {
	if cfg.HubAddr == "" {
		return nil, errors.New("hub address is required")
	}
	if _, _, err := net.SplitHostPort(cfg.HubAddr); err != nil {
		return nil, fmt.Errorf("hub address must be host:port: %w", err)
	}
	if cfg.SnapshotEvery == 0 {
		cfg.SnapshotEvery = 30 * time.Second
	}
	if cfg.IdleSnapshotEvery == 0 {
		cfg.IdleSnapshotEvery = 5 * time.Minute
	}
	if cfg.StatsEvery == 0 {
		cfg.StatsEvery = 10 * time.Second
	}
	if cfg.IdleStatsEvery == 0 {
		cfg.IdleStatsEvery = 60 * time.Second
	}
	if cfg.PingEvery == 0 {
		cfg.PingEvery = 20 * time.Second
	}
	if cfg.DeadAfter == 0 {
		cfg.DeadAfter = 90 * time.Second
	}
	if cfg.RenewBefore == 0 {
		cfg.RenewBefore = 30 * 24 * time.Hour
	}
	if cfg.RenewCheckEvery == 0 {
		cfg.RenewCheckEvery = 12 * time.Hour
	}
	l := cfg.Log
	if l == nil {
		l = slog.Default()
	}
	disk := ""
	if cfg.Stacks != nil {
		disk = cfg.Stacks.Dir
	}
	return &Agent{cfg: cfg, log: l, hostStats: newHostSampler(disk)}, nil
}

func (a *Agent) serverName() string {
	h, _, _ := net.SplitHostPort(a.cfg.HubAddr)
	return h
}

func (a *Agent) path(name string) string { return filepath.Join(a.cfg.DataDir, name) }

// Run enrolls if needed, then connects and reconnects until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	a.root = ctx
	creds, err := a.loadCreds()
	if err != nil {
		switch {
		case a.cfg.JoinToken != "":
			creds, err = a.enroll(ctx, a.cfg.JoinToken)
			if err != nil {
				return fmt.Errorf("enroll: %w", err)
			}
		case a.cfg.JoinTokenFile != "":
			if creds, err = a.enrollFromFile(ctx); err != nil {
				return err
			}
		default:
			return errors.New("agent is not enrolled and no join token was given")
		}
	}
	a.creds.Store(&creds)
	backoff := time.Second
	for {
		a.renewIfDue(ctx)
		started := time.Now()
		err := a.session(ctx, *a.creds.Load())
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errCertRenewed) {
			a.log.Info("reconnecting with the renewed certificate")
			backoff = time.Second
			continue
		}
		// The hub's clock says the certificate has expired even though ours
		// did not (or it expired during the session): renew it right away.
		if errors.Is(err, errCertExpired) {
			a.log.Warn("the hub refused the connection because the client certificate has expired; renewing it", "expired", a.creds.Load().Leaf.NotAfter)
			if a.renewNow(ctx) {
				backoff = time.Second
				continue
			}
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		a.log.Warn("disconnected from hub", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// ---- enrollment ----

func (a *Agent) loadCreds() (tls.Certificate, error) {
	a.finishRenewal()
	cert, err := tls.LoadX509KeyPair(a.path("agent.crt"), a.path("agent.key"))
	if err != nil {
		return cert, err
	}
	if cert.Leaf == nil { // renewal reads the expiry and host ID from it
		if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return cert, err
		}
	}
	if _, err := pki.ReadCertFile(a.path("ca.crt")); err != nil {
		return cert, err
	}
	return cert, nil
}

func (a *Agent) caPool() (*x509.CertPool, error) {
	ca, err := pki.ReadCertFile(a.path("ca.crt"))
	if err != nil {
		return nil, err
	}
	p := x509.NewCertPool()
	p.AddCert(ca)
	return p, nil
}

// enrollFromFile waits for the hub to leave a join token in JoinTokenFile and
// enrolls with it, retrying (with whatever token is there by then) until it
// works: the hub keeps the file fresh until this agent has enrolled.
func (a *Agent) enrollFromFile(ctx context.Context) (tls.Certificate, error) {
	waiting := false
	for {
		b, err := os.ReadFile(a.cfg.JoinTokenFile)
		tok := strings.TrimSpace(string(b))
		if err == nil && tok != "" {
			creds, err := a.enroll(ctx, tok)
			if err == nil {
				return creds, nil
			}
			a.log.Warn("enrollment with the hub's token failed; retrying", "err", err)
		} else if !waiting {
			a.log.Info("waiting for the hub to provide a join token", "file", a.cfg.JoinTokenFile)
			waiting = true
		}
		select {
		case <-ctx.Done():
			return tls.Certificate{}, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func (a *Agent) enroll(ctx context.Context, joinToken string) (tls.Certificate, error) {
	tok, err := wire.ParseJoinToken(joinToken)
	if err != nil {
		return tls.Certificate{}, err
	}
	hostname, _ := os.Hostname()
	if h, err := a.cfg.Docker.Info(ctx); err == nil && h.Hostname != "" {
		hostname = h.Hostname
	}
	key, csr, err := pki.NewKeyAndCSR(hostname)
	if err != nil {
		return tls.Certificate{}, err
	}
	// No CA yet: pin the CA fingerprint carried in the join token instead.
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // replaced by VerifyConnection below
		VerifyConnection: func(cs tls.ConnectionState) error {
			return verifyPinned(cs, tok.CAFingerprint)
		},
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg, ForceAttemptHTTP2: true},
	}
	body, _ := json.Marshal(wire.EnrollRequest{Token: joinToken, Hostname: hostname, CSR: csr})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+a.cfg.HubAddr+wire.PathEnroll, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return tls.Certificate{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return tls.Certificate{}, fmt.Errorf("hub refused enrollment: %s", strings.TrimSpace(string(msg)))
	}
	var er wire.EnrollResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&er); err != nil {
		return tls.Certificate{}, err
	}
	if pki.Fingerprint(er.CA) != tok.CAFingerprint {
		return tls.Certificate{}, errors.New("hub returned an unexpected CA")
	}
	if err := os.MkdirAll(a.cfg.DataDir, 0o700); err != nil {
		return tls.Certificate{}, err
	}
	if err := pki.WriteKeyFile(a.path("agent.key"), key); err != nil {
		return tls.Certificate{}, err
	}
	if err := pki.WriteCertFile(a.path("agent.crt"), er.Cert); err != nil {
		return tls.Certificate{}, err
	}
	if err := pki.WriteCertFile(a.path("ca.crt"), er.CA); err != nil {
		return tls.Certificate{}, err
	}
	a.log.Info("enrolled with hub", "host_id", er.HostID)
	return a.loadCreds()
}

// verifyPinned accepts the hub if its chain includes the CA whose fingerprint
// the join token carries.
func verifyPinned(cs tls.ConnectionState, fingerprint string) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("hub sent no certificate")
	}
	var ca *x509.Certificate
	for _, c := range cs.PeerCertificates {
		if pki.Fingerprint(c.Raw) == fingerprint {
			ca = c
		}
	}
	if ca == nil {
		return errors.New("hub CA does not match the join token (possible man-in-the-middle)")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return verifyHub(cs, roots)
}

// verifyHub checks the hub's certificate against the hub's own private CA.
// It deliberately skips the hostname check: that CA belongs to this one hub
// and issues server certificates only to it (agent certificates are
// client-auth only), so a valid chain identifies the hub whatever address
// the agent used to reach it, an IP, a LAN name, or a VPN address.
func verifyHub(cs tls.ConnectionState, roots *x509.CertPool) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("hub sent no certificate")
	}
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: inter,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return err
}

// ---- certificate renewal ----

// errCertRenewed ends a session so the agent reconnects with its new cert.
var errCertRenewed = errors.New("client certificate renewed")

// certDue reports whether a certificate is close enough to expiry to renew.
func (a *Agent) certDue(cert *tls.Certificate) bool {
	return cert.Leaf != nil && time.Until(cert.Leaf.NotAfter) < a.cfg.RenewBefore
}

// errCertExpired means the hub refused to connect because our certificate
// has expired; the hub still renews it for wire.RenewGrace after expiry.
var errCertExpired = errors.New(wire.CertExpiredText)

// renewIfDue renews the client certificate if it is near expiry (or past
// it) and reports whether it did. Failures only log a warning: the current
// certificate keeps working until it expires, and the next check (on the
// next connection attempt, with backoff) tries again.
func (a *Agent) renewIfDue(ctx context.Context) bool {
	cur := a.creds.Load()
	if cur == nil || !a.certDue(cur) {
		return false
	}
	return a.renewNow(ctx)
}

// renewNow renews the client certificate regardless of its expiry date.
func (a *Agent) renewNow(ctx context.Context) bool {
	cur := a.creds.Load()
	if cur == nil || cur.Leaf == nil {
		return false
	}
	if since := time.Since(cur.Leaf.NotAfter); since > 0 {
		if since > wire.RenewGrace {
			a.log.Error("the client certificate expired too long ago for the hub to renew it; re-enroll this agent with a new join token from the Hosts page",
				"expired", cur.Leaf.NotAfter, "renewable_for", wire.RenewGrace)
		} else {
			a.log.Warn("the client certificate has expired; asking the hub to renew it", "expired", cur.Leaf.NotAfter,
				"renewable_until", cur.Leaf.NotAfter.Add(wire.RenewGrace))
		}
	}
	next, err := a.renew(ctx, *cur)
	if err != nil {
		if ctx.Err() == nil {
			a.log.Warn("could not renew the client certificate; will retry", "err", err, "expires", cur.Leaf.NotAfter)
		}
		return false
	}
	a.creds.Store(&next)
	a.log.Info("renewed the client certificate", "expires", next.Leaf.NotAfter)
	return true
}

// renew asks the hub for a new certificate over mTLS with the current one,
// then saves the new key and certificate to the data directory.
func (a *Agent) renew(ctx context.Context, cur tls.Certificate) (tls.Certificate, error) {
	roots, err := a.caPool()
	if err != nil {
		return tls.Certificate{}, err
	}
	key, csr, err := pki.NewKeyAndCSR(cur.Leaf.Subject.CommonName)
	if err != nil {
		return tls.Certificate{}, err
	}
	tr := &http.Transport{TLSClientConfig: a.hubTLS(cur, roots), ForceAttemptHTTP2: true}
	defer tr.CloseIdleConnections()
	body, _ := json.Marshal(wire.RenewRequest{CSR: csr})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+a.cfg.HubAddr+wire.PathRenew, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: tr, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return tls.Certificate{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return tls.Certificate{}, fmt.Errorf("hub refused renewal: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var rr wire.RenewResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&rr); err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(rr.Cert)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("renewed certificate: %w", err)
	}
	// Sanity-check before replacing anything on disk: the new certificate
	// must be for our new key, for the same host, and chain to our CA.
	if !key.PublicKey.Equal(leaf.PublicKey) {
		return tls.Certificate{}, errors.New("renewed certificate is not for our key")
	}
	if leaf.Subject.CommonName != cur.Leaf.Subject.CommonName {
		return tls.Certificate{}, errors.New("renewed certificate is for a different host")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return tls.Certificate{}, fmt.Errorf("renewed certificate: %w", err)
	}
	next := tls.Certificate{Certificate: [][]byte{rr.Cert}, PrivateKey: key, Leaf: leaf}
	if err := a.saveCreds(key, rr.Cert); err != nil {
		// The hub keeps accepting the old certificate until we connect
		// with the new one, so carrying on with the old one is safe; using
		// a new one we couldn't save would leave the agent locked out
		// after a restart.
		return tls.Certificate{}, fmt.Errorf("save the renewed certificate: %w", err)
	}
	return next, nil
}

// saveCreds replaces agent.key and agent.crt without ever leaving the data
// directory with no usable pair: both new files are written and synced
// under temporary names first, then renamed over the old ones (key first).
// finishRenewal completes the renames if the agent stops part way through.
func (a *Agent) saveCreds(key *ecdsa.PrivateKey, certDER []byte) error {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := writeSynced(a.path("agent.key.new"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	if err := writeSynced(a.path("agent.crt.new"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), 0o644); err != nil {
		return err
	}
	if err := os.Rename(a.path("agent.key.new"), a.path("agent.key")); err != nil {
		return err
	}
	if err := os.Rename(a.path("agent.crt.new"), a.path("agent.crt")); err != nil {
		return err
	}
	syncDir(a.cfg.DataDir)
	return nil
}

// finishRenewal recovers from an agent stopped in the middle of saveCreds.
// The hub may already have switched to the new certificate, so a complete
// new pair left behind is preferred over the old one.
func (a *Agent) finishRenewal() {
	newCrt, newKey := a.path("agent.crt.new"), a.path("agent.key.new")
	if _, err := os.Stat(newCrt); err != nil {
		return
	}
	switch {
	case loads(newCrt, newKey): // stopped before either rename
		if os.Rename(newKey, a.path("agent.key")) != nil {
			return
		}
	case loads(newCrt, a.path("agent.key")): // stopped between the renames
	default: // half-written leftovers; the old pair is still in place
		return
	}
	if err := os.Rename(newCrt, a.path("agent.crt")); err == nil {
		a.log.Info("completed an interrupted certificate renewal")
		syncDir(a.cfg.DataDir)
	}
}

func loads(certFile, keyFile string) bool {
	_, err := tls.LoadX509KeyPair(certFile, keyFile)
	return err == nil
}

func writeSynced(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if err := f.Chmod(perm); err != nil { // in case a leftover had other modes
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// syncDir makes completed renames durable; best effort.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// renewLoop checks the certificate's expiry while connected, for agents
// that stay connected for weeks. After renewing it ends the session so the
// agent reconnects with the new certificate.
func (s *session) renewLoop() {
	t := time.NewTicker(s.a.cfg.RenewCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			if s.a.renewIfDue(s.ctx) {
				s.renewed.Store(true)
				s.cancel()
				return
			}
		}
	}
}

// ---- connected session ----

type session struct {
	a      *Agent
	ctx    context.Context
	cancel context.CancelFunc
	out    chan wire.AgentMessage
	snap   chan struct{}
	// watching: someone has TainerHub open (the hub says so). Starts true so
	// an older hub, which never says, keeps the frequent sampling.
	watching atomic.Bool
	watchCh  chan struct{} // wakes statsLoop when someone starts watching
	renewed  atomic.Bool   // the session ended to reconnect with a renewed certificate

	mu    sync.Mutex
	logs  map[string]context.CancelFunc // log streams, stack ops and terminals, by command ID
	usage map[string]Usage              // container ID -> latest sample
	list  []wire.Container              // last container list from Docker; nil until the first
	execs map[string]*execState         // open terminals, by command ID
	host  *wire.HostStats               // latest whole-host sample, sent with each snapshot
}

type execState struct {
	sess *ExecSession
	in   chan []byte
}

func (s *session) send(m wire.AgentMessage) bool {
	select {
	case s.out <- m:
		return true
	case <-s.ctx.Done():
		return false
	}
}

func (s *session) requestSnapshot() {
	select {
	case s.snap <- struct{}{}:
	default:
	}
}

// hubTLS is the mTLS client config for talking to the hub once enrolled.
func (a *Agent) hubTLS(cert tls.Certificate, roots *x509.CertPool) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{cert},
		ServerName:         a.serverName(),
		InsecureSkipVerify: true, // replaced by VerifyConnection: CA check without a hostname check
		VerifyConnection:   func(cs tls.ConnectionState) error { return verifyHub(cs, roots) },
	}
}

func (a *Agent) session(parent context.Context, cert tls.Certificate) error {
	roots, err := a.caPool()
	if err != nil {
		return err
	}
	tr := &http.Transport{TLSClientConfig: a.hubTLS(cert, roots), ForceAttemptHTTP2: true}
	defer tr.CloseIdleConnections()

	ctx, cancel := context.WithCancel(parent)
	var wg sync.WaitGroup
	defer wg.Wait() // runs last: after cancel below has stopped every goroutine
	defer cancel()
	s := &session{a: a, ctx: ctx, cancel: cancel, out: make(chan wire.AgentMessage, 128), snap: make(chan struct{}, 1), watchCh: make(chan struct{}, 1), logs: map[string]context.CancelFunc{}, usage: map[string]Usage{}, execs: map[string]*execState{}}
	s.watching.Store(true)

	pr, pw := io.Pipe()
	defer pw.Close()

	wg.Add(1)
	go func() { // writer
		defer wg.Done()
		defer cancel()
		enc := json.NewEncoder(pw)
		ping := time.NewTicker(a.cfg.PingEvery)
		defer ping.Stop()
		for {
			var m wire.AgentMessage
			select {
			case <-ctx.Done():
				_ = pw.CloseWithError(ctx.Err())
				return
			case m = <-s.out:
			case <-ping.C:
				m = wire.AgentMessage{Ping: true}
			}
			if err := enc.Encode(m); err != nil {
				return
			}
		}
	}()

	hello, err := a.cfg.Docker.Info(ctx)
	if err != nil {
		return fmt.Errorf("docker info: %w", err)
	}
	hello.Protocol = wire.ProtocolVersion
	hello.AgentVersion = a.cfg.Version
	_, hello.Transfer = a.cfg.Docker.(Transferer)
	_, hello.ContainerMove = a.cfg.Docker.(ContainerMover)
	if a.cfg.Stacks != nil {
		sup := a.cfg.Stacks.Supported(ctx)
		hello.Stacks = &sup
		if sup.Error != "" {
			a.log.Warn("compose stacks unavailable", "err", sup.Error)
		}
	}
	s.send(wire.AgentMessage{Hello: &hello})

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+a.cfg.HubAddr+wire.PathConnect, pr)
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		text := strings.TrimSpace(string(msg))
		if resp.StatusCode == http.StatusUnauthorized && strings.Contains(text, wire.CertExpiredText) {
			return fmt.Errorf("hub: %s: %w", resp.Status, errCertExpired)
		}
		return fmt.Errorf("hub: %s: %s", resp.Status, text)
	}
	a.log.Info("connected to hub", "hub", a.cfg.HubAddr, "proto", resp.Proto)

	wg.Add(4)
	go func() { defer wg.Done(); s.snapshotLoop() }()
	go func() { defer wg.Done(); s.eventLoop() }()
	go func() { defer wg.Done(); s.statsLoop() }()
	go func() { defer wg.Done(); s.renewLoop() }()
	s.requestSnapshot()

	var lastRead time.Time
	var lrMu sync.Mutex
	touch := func() { lrMu.Lock(); lastRead = time.Now(); lrMu.Unlock() }
	touch()
	wg.Add(1)
	go func() { // watchdog
		defer wg.Done()
		t := time.NewTicker(a.cfg.PingEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				lrMu.Lock()
				silent := time.Since(lastRead)
				lrMu.Unlock()
				if silent > a.cfg.DeadAfter {
					a.log.Warn("hub silent too long, reconnecting")
					cancel()
					return
				}
			}
		}
	}()
	go func() { <-ctx.Done(); resp.Body.Close() }()

	dec := wire.NewDecoder(resp.Body)
	var healthWritten time.Time
	for {
		var m wire.HubMessage
		if err := dec.Decode(&m); err != nil {
			cancel()
			if parent.Err() != nil {
				return nil
			}
			if s.renewed.Load() {
				return errCertRenewed
			}
			return err
		}
		touch()
		if m.Watching != nil && s.watching.Swap(*m.Watching) != *m.Watching && *m.Watching {
			select { // sample now rather than up to a minute from now
			case s.watchCh <- struct{}{}:
			default:
			}
			s.requestSnapshot() // and refresh texts like "Up 5 minutes"
		}
		if m.StatsFromDocker != nil {
			if d, ok := s.a.cfg.Docker.(interface{ SetStatsFromDocker(bool) bool }); ok && d.SetStatsFromDocker(*m.StatsFromDocker) {
				s.a.log.Info("container stats source changed by the hub", "from_docker", *m.StatsFromDocker)
			}
		}
		if time.Since(healthWritten) > 30*time.Second {
			healthWritten = time.Now()
			_ = os.WriteFile(a.path(HealthFile), []byte(healthWritten.UTC().Format(time.RFC3339)), 0o600)
		}
		if m.Command != nil {
			s.handle(*m.Command)
		}
	}
}

func (s *session) snapshotLoop() {
	t := time.NewTimer(s.a.cfg.SnapshotEvery)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		case <-s.snap:
			// coalesce bursts of events (e.g. compose up) into one snapshot
			select {
			case <-time.After(300 * time.Millisecond):
			case <-s.ctx.Done():
				return
			}
		}
		t.Stop()
		if s.watching.Load() {
			t.Reset(s.a.cfg.SnapshotEvery)
		} else {
			t.Reset(s.a.cfg.IdleSnapshotEvery)
		}
		s.sendSnapshot()
	}
}

// sendSnapshot lists containers from Docker and sends them with the latest stats.
func (s *session) sendSnapshot() {
	cs, err := s.a.cfg.Docker.List(s.ctx)
	if err != nil {
		s.a.log.Warn("list containers", "err", err)
		return
	}
	s.mu.Lock()
	s.list = cs
	s.mu.Unlock()
	s.publish()
}

// publish sends the last container list with the latest stats, without
// asking Docker again.
func (s *session) publish() {
	s.mu.Lock()
	cs := slices.Clone(s.list)
	for i := range cs {
		if u, ok := s.usage[cs[i].ID]; ok && cs[i].State == "running" {
			cs[i].CPU = math.Round(u.CPU*10) / 10
			cs[i].Mem = u.Mem
			cs[i].MemLimit = u.MemLimit
		}
	}
	host := s.host
	s.mu.Unlock()
	s.send(wire.AgentMessage{Snapshot: &wire.Snapshot{At: time.Now().Unix(), Containers: cs, Host: host}})
}

// statsLoop samples CPU and memory for running containers (8 at a time) and
// then publishes a fresh snapshot carrying the numbers.
func (s *session) statsLoop() {
	if s.a.cfg.StatsEvery < 0 {
		return
	}
	t := time.NewTimer(s.a.cfg.StatsEvery)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		case <-s.watchCh:
			t.Stop()
		}
		if s.watching.Load() {
			t.Reset(s.a.cfg.StatsEvery)
		} else {
			t.Reset(s.a.cfg.IdleStatsEvery)
		}
		s.mu.Lock()
		cs := s.list
		s.mu.Unlock()
		if cs == nil { // no list yet: get one
			var err error
			if cs, err = s.a.cfg.Docker.List(s.ctx); err != nil {
				continue
			}
			s.mu.Lock()
			s.list = cs
			s.mu.Unlock()
		}
		next := map[string]Usage{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		for _, c := range cs {
			if c.State != "running" {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(id string) {
				defer wg.Done()
				defer func() { <-sem }()
				ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
				defer cancel()
				if u, err := s.a.cfg.Docker.Stats(ctx, id); err == nil {
					mu.Lock()
					next[id] = u
					mu.Unlock()
				}
			}(c.ID)
		}
		wg.Wait()
		host := s.a.hostStats.Sample()
		s.mu.Lock()
		s.usage = next
		s.host = host
		s.mu.Unlock()
		s.publish()
	}
}

func (s *session) eventLoop() {
	for s.ctx.Err() == nil {
		err := s.a.cfg.Docker.Events(s.ctx, func(ev wire.DockerEvent) {
			s.send(wire.AgentMessage{Event: &ev})
			s.requestSnapshot()
		})
		if s.ctx.Err() != nil {
			return
		}
		s.a.log.Warn("docker events stream ended", "err", err)
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		s.requestSnapshot() // catch up on changes missed while the stream was down
	}
}

func (s *session) handle(c wire.Command) {
	switch {
	case c.Action != nil:
		go func() {
			ctx, cancel := context.WithTimeout(s.ctx, 50*time.Second)
			defer cancel()
			err := s.a.cfg.Docker.Action(ctx, c.Action.ContainerID, c.Action.Action)
			res := wire.CommandResult{CommandID: c.ID, OK: err == nil}
			if err != nil {
				res.Error = err.Error()
			}
			s.a.log.Info("container action", "action", c.Action.Action, "container", c.Action.ContainerID, "ok", res.OK, "err", res.Error)
			s.send(wire.AgentMessage{Result: &res})
			s.requestSnapshot()
		}()
	case c.Op != nil:
		go func() {
			timeout := time.Minute
			switch c.Op.Kind {
			case wire.OpImagePull, wire.OpImagesPrune, wire.OpContainerRecreate, wire.OpImagesCheckUpdates, wire.OpDockerPlan, wire.OpDockerUpgrade, wire.OpStackDirRemove, wire.OpContainerExport, wire.OpContainerCreate:
				timeout = 15 * time.Minute // pulls and registry round trips
			}
			ctx, cancel := context.WithTimeout(s.ctx, timeout)
			defer cancel()
			res := wire.CommandResult{CommandID: c.ID}
			if err := wire.ValidOperation(*c.Op); err != nil {
				res.Error = err.Error()
			} else if data, err := s.operate(ctx, *c.Op); err != nil {
				// Registry logins never travel back in an error (nor into the log below).
				res.Error = regcred.Scrub(err.Error(), c.Op.Auth)
			} else {
				res.OK = true
				if data != nil {
					res.Data, _ = json.Marshal(data)
				}
			}
			if wire.OpKinds[c.Op.Kind] {
				s.a.log.Info("resource operation", "kind", c.Op.Kind, "target", c.Op.Target, "arg", c.Op.Arg, "ok", res.OK, "err", res.Error)
			}
			if wire.OpKinds[c.Op.Kind] || c.Op.Kind == wire.OpImagesCheckUpdates {
				s.requestSnapshot() // e.g. fresh update badges
			}
			s.send(wire.AgentMessage{Result: &res})
		}()
	case c.Logs != nil:
		ctx, cancel := context.WithCancel(s.ctx)
		s.mu.Lock()
		s.logs[c.ID] = cancel
		s.mu.Unlock()
		go func() {
			defer func() {
				cancel()
				s.mu.Lock()
				delete(s.logs, c.ID)
				s.mu.Unlock()
			}()
			w := &chunkWriter{s: s, id: c.ID}
			err := s.a.cfg.Docker.Logs(ctx, c.Logs.ContainerID, c.Logs.Tail, c.Logs.Follow, w)
			end := wire.LogChunk{CommandID: c.ID, EOF: true}
			if err != nil && ctx.Err() == nil {
				end.Error = err.Error()
			}
			s.send(wire.AgentMessage{Log: &end})
		}()
	case c.Stack != nil:
		go s.runStack(c.ID, *c.Stack)
	case c.Transfer != nil:
		s.startTransfer(c.ID, *c.Transfer)
	case c.Exec != nil:
		s.startExec(c.ID, *c.Exec)
	case c.ExecInput != nil:
		s.mu.Lock()
		e := s.execs[c.ExecInput.Session]
		s.mu.Unlock()
		if e != nil {
			select {
			case e.in <- c.ExecInput.Data:
			default: // the process isn't reading; drop rather than stall every host command
			}
		}
	case c.ExecResize != nil:
		s.mu.Lock()
		e := s.execs[c.ExecResize.Session]
		s.mu.Unlock()
		if e != nil && wire.ValidExecResize(*c.ExecResize) {
			go func() {
				ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
				defer cancel()
				_ = e.sess.Resize(ctx, c.ExecResize.Cols, c.ExecResize.Rows)
			}()
		}
	case c.Cancel != "":
		s.mu.Lock()
		if cancel, ok := s.logs[c.Cancel]; ok {
			cancel()
		}
		s.mu.Unlock()
	}
}

// operate runs a resource operation: the stack ones here, the rest in Docker.
func (s *session) operate(ctx context.Context, op wire.Operation) (any, error) {
	switch op.Kind {
	case wire.OpStackInspect:
		return s.inspectStack(ctx, op.Target)
	case wire.OpStackDirRemove:
		return nil, s.removeStackDir(ctx, op.Target)
	case wire.OpStackPreview:
		return s.previewStack(ctx, op.Target, *op.Files)
	case wire.OpStackRead:
		return s.readProject(ctx, op.Target)
	}
	return s.a.cfg.Docker.Operate(ctx, op)
}

// startTransfer sends or receives one volume or stack folder through the
// hub. It runs on the session's context: if the hub connection drops, the
// hub has already given up on the transfer. Cancel stops it.
func (s *session) startTransfer(id string, c wire.TransferCommand) {
	ctx, cancel := context.WithCancel(s.ctx)
	s.mu.Lock()
	s.logs[id] = cancel
	s.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			s.mu.Lock()
			delete(s.logs, id)
			s.mu.Unlock()
		}()
		res := wire.CommandResult{CommandID: id}
		td, ok := s.a.cfg.Docker.(Transferer)
		if !ok {
			res.Error = "this agent can't copy data"
		} else {
			r := &transferRunner{docker: td, hub: hubClient{a: s.a}, log: s.a.log}
			if s.a.cfg.Stacks != nil && s.a.cfg.Stacks.Supported(ctx).Error == "" {
				r.stacksDir = s.a.cfg.Stacks.Dir
			}
			start := time.Now()
			out, err := r.run(ctx, c)
			if err != nil {
				res.Error = err.Error()
			} else {
				res.OK = true
				res.Data, _ = json.Marshal(out)
			}
			s.a.log.Info("transfer", "role", c.Role, "kind", c.Item.Kind, "name", c.Item.Name,
				"bytes", out.Bytes, "files", out.Files, "took", time.Since(start).Round(time.Second), "ok", res.OK, "err", res.Error)
		}
		s.send(wire.AgentMessage{Result: &res})
		if c.Role == wire.TransferReceive {
			s.requestSnapshot()
		}
	}()
}

// runStack runs a compose operation and streams its output back. It runs on
// the agent's root context, not the session's: a dropped hub connection must
// not interrupt a half-finished `compose up`.
func (s *session) runStack(id string, c wire.StackCommand) {
	var out io.Writer = &safeWriter{w: &chunkWriter{s: s, id: id}}
	if len(c.Auth) > 0 {
		// compose output goes to the job log: keep registry logins out of it.
		out = regcred.NewScrubWriter(out, c.Auth)
	}
	var err error
	if s.a.cfg.Stacks == nil {
		err = errors.New("this agent was started without compose stack support")
	} else {
		root := s.a.root
		if root == nil {
			root = s.ctx
		}
		ctx, cancel := context.WithTimeout(root, 30*time.Minute)
		err = s.a.cfg.Stacks.Run(ctx, c, out)
		cancel()
	}
	if f, ok := out.(*regcred.ScrubWriter); ok {
		_ = f.Flush() // before the final chunk
	}
	end := wire.LogChunk{CommandID: id, EOF: true}
	if err != nil {
		end.Error = regcred.Scrub(err.Error(), c.Auth)
	}
	s.a.log.Info("stack operation", "action", c.Action, "stack", c.Name, "ok", err == nil, "err", end.Error)
	s.send(wire.AgentMessage{Log: &end})
	s.requestSnapshot()
}

// startExec opens a terminal and pumps its output back as LogChunks. It
// ends when the process exits, the hub cancels it, or the session drops.
func (s *session) startExec(id string, c wire.ExecStart) {
	ex, ok := s.a.cfg.Docker.(Execer)
	end := func(msg string) {
		s.send(wire.AgentMessage{Log: &wire.LogChunk{CommandID: id, EOF: true, Error: msg}})
	}
	if !ok {
		end("this agent can't open terminals")
		return
	}
	if err := wire.ValidExecStart(c); err != nil {
		end(err.Error())
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.mu.Lock()
	s.logs[id] = cancel
	s.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			s.mu.Lock()
			delete(s.logs, id)
			delete(s.execs, id)
			s.mu.Unlock()
		}()
		sess, err := ex.Exec(ctx, c.ContainerID, c.Cmd, c.Cols, c.Rows)
		if err != nil {
			end(err.Error())
			return
		}
		defer sess.Close()
		st := &execState{sess: sess, in: make(chan []byte, 256)}
		s.mu.Lock()
		s.execs[id] = st
		s.mu.Unlock()
		s.a.log.Info("terminal opened", "container", c.ContainerID)
		go func() { // input
			for {
				select {
				case <-ctx.Done():
					return
				case b := <-st.in:
					if _, err := sess.Write(b); err != nil {
						cancel()
						return
					}
				}
			}
		}()
		go func() { <-ctx.Done(); sess.Close() }() // unblocks the copy below
		_, _ = io.Copy(&chunkWriter{s: s, id: id}, sess)
		msg := ""
		if ctx.Err() == nil {
			ectx, ecancel := context.WithTimeout(s.ctx, 3*time.Second)
			if code, ok := sess.ExitCode(ectx); ok && code != 0 {
				msg = fmt.Sprintf("process exited with code %d", code)
			}
			ecancel()
		}
		end(msg)
		s.a.log.Info("terminal closed", "container", c.ContainerID)
	}()
}

// chunkWriter turns log bytes into LogChunk messages of at most 32 KiB.
type chunkWriter struct {
	s  *session
	id string
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		n := min(len(p), 32<<10)
		data := make([]byte, n)
		copy(data, p[:n])
		if !w.s.send(wire.AgentMessage{Log: &wire.LogChunk{CommandID: w.id, Data: data}}) {
			return total - len(p), context.Canceled
		}
		p = p[n:]
	}
	return total, nil
}
