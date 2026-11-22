// Package agent runs on each Docker host. It enrolls with the hub once using
// a join token, then keeps one outbound mTLS stream open to the hub.
package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/prezmc/tainerhub/internal/pki"
	"github.com/prezmc/tainerhub/internal/wire"
)

type Config struct {
	HubAddr   string // host:port of the hub's agent gateway, e.g. tainerhub.example.com:7443
	JoinToken string // only needed until the agent has enrolled
	DataDir   string // stores agent.crt, agent.key, ca.crt
	Version   string
	Docker    Docker
	Stacks    *StackRunner // nil: compose stacks are not available on this agent
	Log       *slog.Logger

	SnapshotEvery time.Duration // default 30s
	StatsEvery    time.Duration // default 10s; negative disables stats
	PingEvery     time.Duration // default 20s
	DeadAfter     time.Duration // default 90s
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
	if cfg.StatsEvery == 0 {
		cfg.StatsEvery = 10 * time.Second
	}
	if cfg.PingEvery == 0 {
		cfg.PingEvery = 20 * time.Second
	}
	if cfg.DeadAfter == 0 {
		cfg.DeadAfter = 90 * time.Second
	}
	l := cfg.Log
	if l == nil {
		l = slog.Default()
	}
	return &Agent{cfg: cfg, log: l}, nil
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
		if a.cfg.JoinToken == "" {
			return errors.New("agent is not enrolled and no join token was given")
		}
		creds, err = a.enroll(ctx)
		if err != nil {
			return fmt.Errorf("enroll: %w", err)
		}
	}
	backoff := time.Second
	for {
		started := time.Now()
		err := a.session(ctx, creds)
		if ctx.Err() != nil {
			return nil
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
	cert, err := tls.LoadX509KeyPair(a.path("agent.crt"), a.path("agent.key"))
	if err != nil {
		return cert, err
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

func (a *Agent) enroll(ctx context.Context) (tls.Certificate, error) {
	tok, err := wire.ParseJoinToken(a.cfg.JoinToken)
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
	body, _ := json.Marshal(wire.EnrollRequest{Token: a.cfg.JoinToken, Hostname: hostname, CSR: csr})
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

// ---- connected session ----

type session struct {
	a      *Agent
	ctx    context.Context
	cancel context.CancelFunc
	out    chan wire.AgentMessage
	snap   chan struct{}

	mu    sync.Mutex
	logs  map[string]context.CancelFunc
	usage map[string]Usage // container ID -> latest sample
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

func (a *Agent) session(parent context.Context, cert tls.Certificate) error {
	roots, err := a.caPool()
	if err != nil {
		return err
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS13,
			Certificates:       []tls.Certificate{cert},
			ServerName:         a.serverName(),
			InsecureSkipVerify: true, // replaced by VerifyConnection: CA check without a hostname check
			VerifyConnection:   func(cs tls.ConnectionState) error { return verifyHub(cs, roots) },
		},
		ForceAttemptHTTP2: true,
	}
	defer tr.CloseIdleConnections()

	ctx, cancel := context.WithCancel(parent)
	var wg sync.WaitGroup
	defer wg.Wait() // runs last: after cancel below has stopped every goroutine
	defer cancel()
	s := &session{a: a, ctx: ctx, cancel: cancel, out: make(chan wire.AgentMessage, 128), snap: make(chan struct{}, 1), logs: map[string]context.CancelFunc{}, usage: map[string]Usage{}}

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
		return fmt.Errorf("hub: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	a.log.Info("connected to hub", "hub", a.cfg.HubAddr, "proto", resp.Proto)

	wg.Add(3)
	go func() { defer wg.Done(); s.snapshotLoop() }()
	go func() { defer wg.Done(); s.eventLoop() }()
	go func() { defer wg.Done(); s.statsLoop() }()
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
			return err
		}
		touch()
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
	t := time.NewTicker(s.a.cfg.SnapshotEvery)
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
		s.sendSnapshot()
	}
}

func (s *session) sendSnapshot() {
	cs, err := s.a.cfg.Docker.List(s.ctx)
	if err != nil {
		s.a.log.Warn("list containers", "err", err)
		return
	}
	s.mu.Lock()
	for i := range cs {
		if u, ok := s.usage[cs[i].ID]; ok && cs[i].State == "running" {
			cs[i].CPU = math.Round(u.CPU*10) / 10
			cs[i].Mem = u.Mem
			cs[i].MemLimit = u.MemLimit
		}
	}
	s.mu.Unlock()
	s.send(wire.AgentMessage{Snapshot: &wire.Snapshot{At: time.Now().Unix(), Containers: cs}})
}

// statsLoop samples CPU and memory for running containers (8 at a time) and
// then publishes a fresh snapshot carrying the numbers.
func (s *session) statsLoop() {
	if s.a.cfg.StatsEvery < 0 {
		return
	}
	t := time.NewTicker(s.a.cfg.StatsEvery)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		cs, err := s.a.cfg.Docker.List(s.ctx)
		if err != nil {
			continue
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
		s.mu.Lock()
		s.usage = next
		s.mu.Unlock()
		s.sendSnapshot()
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
			if c.Op.Kind == wire.OpImagePull {
				timeout = 15 * time.Minute
			}
			ctx, cancel := context.WithTimeout(s.ctx, timeout)
			defer cancel()
			res := wire.CommandResult{CommandID: c.ID}
			if err := wire.ValidOperation(*c.Op); err != nil {
				res.Error = err.Error()
			} else if data, err := s.a.cfg.Docker.Operate(ctx, *c.Op); err != nil {
				res.Error = err.Error()
			} else {
				res.OK = true
				if data != nil {
					res.Data, _ = json.Marshal(data)
				}
			}
			if wire.OpKinds[c.Op.Kind] {
				s.a.log.Info("resource operation", "kind", c.Op.Kind, "target", c.Op.Target, "arg", c.Op.Arg, "ok", res.OK, "err", res.Error)
				s.requestSnapshot()
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
	case c.Cancel != "":
		s.mu.Lock()
		if cancel, ok := s.logs[c.Cancel]; ok {
			cancel()
		}
		s.mu.Unlock()
	}
}

// runStack runs a compose operation and streams its output back. It runs on
// the agent's root context, not the session's: a dropped hub connection must
// not interrupt a half-finished `compose up`.
func (s *session) runStack(id string, c wire.StackCommand) {
	out := &safeWriter{w: &chunkWriter{s: s, id: id}}
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
	end := wire.LogChunk{CommandID: id, EOF: true}
	if err != nil {
		end.Error = err.Error()
	}
	s.a.log.Info("stack operation", "action", c.Action, "stack", c.Name, "ok", err == nil, "err", end.Error)
	s.send(wire.AgentMessage{Log: &end})
	s.requestSnapshot()
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
