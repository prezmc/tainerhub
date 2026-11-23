// Command tainerhub is the hub: web UI + API (behind Nginx Proxy Manager) and
// the agent gateway that the agents on each Docker host connect to.
package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/prezmc/tainerhub/internal/debugsrv"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/prezmc/tainerhub/internal/api"
	"github.com/prezmc/tainerhub/internal/auth"
	"github.com/prezmc/tainerhub/internal/gateway"
	"github.com/prezmc/tainerhub/internal/pki"
	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/version"
	"github.com/prezmc/tainerhub/internal/wire"
	"github.com/prezmc/tainerhub/web"
)

type config struct {
	PublicURL   *url.URL
	HTTPAddr    string
	AgentListen string
	AgentAddr   string
	AgentSANs   []string
	DataDir     string
	DatabaseURL string
	DBPassword  string
	Trusted     []netip.Prefix
}

func loadConfig() (config, error) {
	var c config
	// Optional: the https:// address users browse to once TainerHub is behind
	// a reverse proxy. Without it the hub works over plain HTTP with password
	// sign-in; setting it turns on passkeys and Secure cookies.
	if raw := os.Getenv("TAINERHUB_PUBLIC_URL"); raw != "" {
		u, err := url.Parse(strings.TrimSuffix(raw, "/"))
		if err != nil || u.Host == "" {
			return c, fmt.Errorf("TAINERHUB_PUBLIC_URL is not a valid URL: %q", raw)
		}
		if u.Scheme != "https" && u.Hostname() != "localhost" {
			return c, errors.New("TAINERHUB_PUBLIC_URL must be https (passkeys only work over HTTPS, except on localhost); leave it unset to use plain HTTP with passwords")
		}
		c.PublicURL = u
	}
	c.HTTPAddr = envOr("TAINERHUB_HTTP_ADDR", ":8585")
	c.AgentListen = envOr("TAINERHUB_AGENT_LISTEN", ":7443")
	c.AgentAddr = os.Getenv("TAINERHUB_AGENT_ADDR") // optional: default is chosen per request
	c.DataDir = envOr("TAINERHUB_DATA_DIR", "/data")
	c.DatabaseURL = os.Getenv("TAINERHUB_DATABASE_URL")
	if c.DatabaseURL == "" {
		return c, errors.New("TAINERHUB_DATABASE_URL is required, e.g. postgres://tainerhub@postgres:5432/tainerhub")
	}
	if f := os.Getenv("TAINERHUB_DATABASE_PASSWORD_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return c, fmt.Errorf("read database password file: %w", err)
		}
		c.DBPassword = strings.TrimSpace(string(b))
	}
	// Agents verify the gateway by the hub's pinned CA, not by name, so these
	// names only matter to other TLS clients.
	c.AgentSANs = []string{"localhost", "127.0.0.1"}
	if c.AgentAddr != "" {
		agentHost, _, err := net.SplitHostPort(c.AgentAddr)
		if err != nil {
			return c, fmt.Errorf("TAINERHUB_AGENT_ADDR must be host:port: %w", err)
		}
		c.AgentSANs = append(c.AgentSANs, agentHost)
	}
	for _, s := range strings.Split(os.Getenv("TAINERHUB_AGENT_SANS"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			c.AgentSANs = append(c.AgentSANs, s)
		}
	}
	for _, s := range strings.Split(os.Getenv("TAINERHUB_TRUSTED_PROXIES"), ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, err2 := netip.ParseAddr(s)
			if err2 != nil {
				return c, fmt.Errorf("TAINERHUB_TRUSTED_PROXIES: %q is not an IP or CIDR", s)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		c.Trusted = append(c.Trusted, p.Masked())
	}
	return c, nil
}

func main() {
	// `tainerhub healthcheck` lets Docker check the hub without a shell or curl
	// in the distroless image.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(healthcheck())
		case "setup-link":
			os.Exit(printSetupLink())
		case "db-password":
			os.Exit(printDBPassword())
		}
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("tainerhub stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	debugsrv.Start(log)
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	publicURL := "(plain HTTP with passwords; turn on HTTPS and passkeys under Settings)"
	if cfg.PublicURL != nil {
		publicURL = cfg.PublicURL.String()
	}
	log.Info("tainerhub starting", "version", version.Version, "public_url", publicURL, "agent_addr", cfg.AgentAddr)

	var st *store.Store
	if cfg.DBPassword != "" { // TAINERHUB_DATABASE_PASSWORD_FILE: managed by the admin
		st, err = store.Open(ctx, cfg.DatabaseURL, cfg.DBPassword)
	} else {
		open := func(ctx context.Context, pw string) (*store.Store, error) {
			return store.Open(ctx, cfg.DatabaseURL, pw)
		}
		st, err = openManaged(ctx, open, urlPassword(cfg.DatabaseURL), cfg.DataDir, log)
	}
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	ca, err := pki.LoadOrCreateCA(filepath.Join(cfg.DataDir, "pki"))
	if err != nil {
		return fmt.Errorf("certificate authority: %w", err)
	}
	srvCert, err := ca.ServerCert(cfg.AgentSANs)
	if err != nil {
		return err
	}

	reg := registry.New()
	reg.OnEvent = func(hostID string, ev wire.DockerEvent) {
		log.Debug("docker event", "host_id", hostID, "action", ev.Action, "container", ev.Name)
	}
	gw := &gateway.Gateway{CA: ca, Store: st, Reg: reg, Log: log.With("component", "gateway")}

	authSvc, err := auth.New(st, cfg.PublicURL, cfg.Trusted, log.With("component", "auth"))
	if err != nil {
		return err
	}
	authSvc.SetupTokenFile = filepath.Join(cfg.DataDir, setupTokenFile)
	// The hub's compose file runs an agent for this server too; enroll it.
	if dir := os.Getenv("TAINERHUB_LOCAL_AGENT_DIR"); dir != "" {
		la := &localAgent{st: st, caFP: ca.Fingerprint(), dataDir: cfg.DataDir, shareDir: dir, log: log.With("component", "local-agent"), now: time.Now,
			renamed: reg.Disconnect} // the agent reconnects within seconds, under its new name
		go la.run(ctx)
	}
	go authSvc.RunSetupTokenLoop(ctx)
	go authSvc.RunSessionJanitor(ctx)

	apiSrv := &api.Server{Store: st, Auth: authSvc, Reg: reg, CA: ca, Log: log.With("component", "api"), AgentAddr: cfg.AgentAddr, Web: web.Dist(), DataDir: cfg.DataDir,
		HTTPS: api.HTTPSConfig{EnvPublicURL: cfg.PublicURL, EnvTrusted: cfg.Trusted, HTTPPort: httpPort(cfg.HTTPAddr)}}
	if err := apiSrv.LoadStatsSettings(ctx); err != nil {
		log.Warn("load stats settings", "err", err)
	}
	if err := apiSrv.LoadHTTPSSettings(ctx); err != nil {
		log.Warn("couldn't load the HTTPS settings saved in the UI", "err", err)
	} else if u := authSvc.PublicURL(); cfg.PublicURL == nil && u != nil {
		log.Info("HTTPS turned on under Settings", "public_url", u.String())
	}
	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second, // request bodies; SSE responses are not limited
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	gwSrv := gw.Server(cfg.AgentListen, srvCert)

	errc := make(chan error, 2)
	go func() {
		log.Info("web UI and API listening", "addr", cfg.HTTPAddr)
		errc <- httpSrv.ListenAndServe()
	}()
	go func() {
		log.Info("agent gateway listening (mTLS)", "addr", cfg.AgentListen, "ca_fingerprint", ca.Fingerprint())
		errc <- gwSrv.ListenAndServeTLS("", "")
	}()

	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(sctx)
	_ = gwSrv.Shutdown(sctx)
	return nil
}

func healthcheck() int {
	addr := envOr("TAINERHUB_HTTP_ADDR", ":8585")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy:", resp.Status)
		return 1
	}
	return 0
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func logLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(envOr("TAINERHUB_LOG_LEVEL", "info"))); err != nil {
		return slog.LevelInfo
	}
	return l
}

// urlPassword returns the password in a postgres:// URL, or "".
func urlPassword(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return ""
	}
	pw, _ := u.User.Password()
	return pw
}

const setupTokenFile = "setup_token"

// printSetupLink prints the path of the one-time setup link while no account
// exists yet: `docker exec tainerhub /tainerhub setup-link`.
func printSetupLink() int {
	b, err := os.ReadFile(filepath.Join(envOr("TAINERHUB_DATA_DIR", "/data"), setupTokenFile))
	tok := strings.TrimSpace(string(b))
	if err != nil || tok == "" {
		fmt.Fprintln(os.Stderr, "No setup link: the owner account already exists, or the hub hasn't finished starting (try again in a few seconds).")
		return 1
	}
	fmt.Println("/setup?token=" + tok)
	return 0
}

// printDBPassword prints the database password the hub generated on first
// start: `docker exec tainerhub /tainerhub db-password`.
func printDBPassword() int {
	b, err := os.ReadFile(filepath.Join(envOr("TAINERHUB_DATA_DIR", "/data"), dbPasswordFile))
	if err != nil {
		fmt.Fprintln(os.Stderr, "No saved database password: either the hub hasn't started yet, or you chose your own (DB_PASSWORD).")
		return 1
	}
	fmt.Println(strings.TrimSpace(string(b)))
	return 0
}

func httpPort(addr string) string {
	if _, p, err := net.SplitHostPort(addr); err == nil && p != "" {
		return p
	}
	return "8585"
}
