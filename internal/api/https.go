package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// HTTPS setup from the UI. The admin puts TainerHub behind a reverse proxy
// such as Nginx Proxy Manager, then:
//  1. Test: the hub requests https://<their domain>/api/https-check with a
//     one-time nonce. If the proxy, DNS and certificate work, that request
//     comes back to this hub, which records the proxy's IP and whether it
//     forwarded X-Forwarded-Proto: https.
//  2. Turn on: saves the address and that proxy IP and applies them live:
//     passkeys switch on, and browsers on the https:// address get Secure
//     cookies. The LAN address keeps working as before.

const (
	settingPublicURL       = "public_url"
	settingTrusted         = "trusted_proxies"
	settingPasskeyRequired = "passkey_required"
)

// HTTPSConfig is what the hub was started with (environment variables win
// over UI settings; when set they lock the UI).
type HTTPSConfig struct {
	EnvPublicURL *url.URL
	EnvTrusted   []netip.Prefix
	HTTPPort     string // e.g. 8585
}

type httpsCheck struct {
	at       time.Time
	seen     bool
	remoteIP string
	proto    string
	host     string
}

type httpsState struct {
	mu       sync.Mutex
	pending  map[string]*httpsCheck // nonce -> what the check request saw
	instance string                 // proves the check reached this hub
	passed   map[string]httpsResult // url -> last successful test

	// untrusted is the latest address that sent forwarding headers without
	// being a trusted proxy (see noteUntrustedProxy); warned holds when each
	// was last logged.
	untrusted untrustedSeen
	warned    map[string]time.Time
}

type untrustedSeen struct {
	IP string    `json:"ip"`
	At time.Time `json:"at"`
}

type httpsResult struct {
	At      time.Time `json:"at"`
	OK      bool      `json:"ok"`
	ProxyIP string    `json:"proxyIp,omitempty"`
	Proto   string    `json:"forwardedProto,omitempty"`
	Host    string    `json:"host,omitempty"`
	Error   string    `json:"error,omitempty"`
	TLS     string    `json:"tlsError,omitempty"`
}

func (h *httpsState) init() {
	if h.pending == nil {
		h.pending = map[string]*httpsCheck{}
		h.passed = map[string]httpsResult{}
		h.warned = map[string]time.Time{}
		h.instance = randID()
	}
}

// LoadHTTPSSettings applies UI-saved settings at startup (unless the
// environment sets the public URL).
func (s *Server) LoadHTTPSSettings(ctx context.Context) error {
	if v, err := s.Store.Setting(ctx, settingPasskeyRequired); err == nil {
		s.Auth.SetPasskeyRequired(v == "1")
	}
	if s.HTTPS.EnvPublicURL != nil {
		return nil
	}
	raw, err := s.Store.Setting(ctx, settingPublicURL)
	if err != nil || raw == "" {
		return err
	}
	u, err := parseHTTPSURL(raw)
	if err != nil {
		return nil // ignore a bad stored value rather than refusing to start
	}
	tr, _ := s.Store.Setting(ctx, settingTrusted)
	saved := parsePrefixes(tr)
	for _, p := range saved {
		// Saved before ranges were checked: keep it (dropping it could break
		// sign-in through the proxy) but say so.
		if TrustedRangeTooBroad(p) {
			s.Log.Warn("a saved trusted proxy range is very broad; set your proxy's own IP under Settings, HTTPS", "range", p.String())
		}
	}
	return s.Auth.Reconfigure(u, append(append([]netip.Prefix{}, s.HTTPS.EnvTrusted...), saved...))
}

func parseHTTPSURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(strings.TrimSuffix(raw, "/"))
	if err != nil || u.Host == "" || u.Scheme != "https" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return nil, errors.New("enter an https:// address like https://tainerhub.example.com")
	}
	u.Host = strings.ToLower(u.Host)
	return u, nil
}

func parsePrefixes(s string) []netip.Prefix {
	var out []netip.Prefix
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
		} else if a, err := netip.ParseAddr(f); err == nil {
			out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
		}
	}
	return out
}

// TrustedRangeTooBroad reports whether a trusted-proxy range covers so many
// addresses that trusting it is almost certainly a mistake: anything in it
// could claim any client IP (dodging the sign-in rate limit) and say the
// browser used HTTPS. /16 still covers a Docker bridge network, whose
// addresses can change between restarts.
func TrustedRangeTooBroad(p netip.Prefix) bool {
	switch {
	case p.Addr().Is4():
		return p.Bits() < 16
	case p.Addr().Is4In6(): // ::ffff:a.b.c.d/n: the IPv4 part starts at bit 96
		return p.Bits() < 96+16
	}
	return p.Bits() < 48
}

// parseTrustedStrict parses the trusted proxies an admin typed. Unlike
// parsePrefixes (used for values already saved) it refuses anything it
// can't read and ranges that are too broad, with a message for the UI.
func parseTrustedStrict(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		var p netip.Prefix
		if pp, err := netip.ParsePrefix(f); err == nil {
			// Store ::ffff:a.b.c.d/n as the plain IPv4 range, as client
			// addresses are compared unmapped.
			p = netip.PrefixFrom(pp.Addr().Unmap(), pp.Bits()-(pp.Addr().BitLen()-pp.Addr().Unmap().BitLen())).Masked()
		} else if a, err := netip.ParseAddr(f); err == nil {
			p = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
		} else {
			return nil, fmt.Errorf("trusted proxy: %q isn't an IP address or range; enter addresses like 192.168.1.20, separated by commas", f)
		}
		if !p.IsValid() || TrustedRangeTooBroad(p) {
			return nil, fmt.Errorf("trusted proxy: %s is too broad. Anything in it could pretend to be any visitor; enter your proxy's own IP (ranges no wider than /16 for IPv4 or /48 for IPv6)", f)
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *Server) httpsSettings(w http.ResponseWriter, r *http.Request) {
	var trusted []string
	for _, p := range s.Auth.Trusted() {
		trusted = append(trusted, strings.TrimSuffix(strings.TrimSuffix(p.String(), "/32"), "/128"))
	}
	out := map[string]any{
		"publicURL":      "",
		"trustedProxies": nonNil(trusted),
		"lockedByEnv":    s.HTTPS.EnvPublicURL != nil,
		"passkeys":       s.Auth.PasskeysEnabled(),
		"httpPort":       s.HTTPS.HTTPPort,
		"viaHTTPS":       s.Auth.IsHTTPS(r),
		// Sign-in rule for the secure address (see auth.SetPasskeyRequired).
		"passkeyRequired": s.Auth.PasskeyRequired(),
	}
	s.https.mu.Lock()
	if u := s.https.untrusted; u.IP != "" && time.Since(u.At) < untrustedShowFor {
		out["untrustedProxy"] = u
	}
	s.https.mu.Unlock()
	if u := s.Auth.PublicURL(); u != nil {
		out["publicURL"] = u.String()
	}
	writeJSON(w, http.StatusOK, out)
}

// httpsCheckEcho is the public end of the test: it only answers for a nonce
// the hub itself just issued.
func (s *Server) httpsCheckEcho(w http.ResponseWriter, r *http.Request) {
	nonce := r.URL.Query().Get("nonce")
	s.https.mu.Lock()
	s.https.init()
	c := s.https.pending[nonce]
	if c != nil && !c.seen {
		c.seen = true
		c.remoteIP = r.RemoteAddr
		if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			c.remoteIP = h
		}
		c.proto = r.Header.Get("X-Forwarded-Proto")
		c.host = r.Host
	}
	inst := s.https.instance
	s.https.mu.Unlock()
	if c == nil {
		jsonErr(w, http.StatusNotFound, "unknown check")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"instance": inst})
}

func (s *Server) httpsTest(w http.ResponseWriter, r *http.Request) {
	var in struct{ URL string }
	if !readJSON(w, r, &in) {
		return
	}
	u, err := parseHTTPSURL(in.URL)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	nonce := randID()
	s.https.mu.Lock()
	s.https.init()
	for k, c := range s.https.pending {
		if time.Since(c.at) > time.Minute {
			delete(s.https.pending, k)
		}
	}
	check := &httpsCheck{at: time.Now()}
	s.https.pending[nonce] = check
	inst := s.https.instance
	s.https.mu.Unlock()

	res := httpsResult{At: time.Now()}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	target := u.String() + "/api/https-check?nonce=" + nonce
	body, err := fetch(ctx, target, false)
	if err != nil && isTLSError(err) {
		res.TLS = err.Error()
		if i := strings.LastIndex(res.TLS, ": tls: "); i >= 0 {
			res.TLS = res.TLS[i+2:]
		}
		body, err = fetch(ctx, target, true) // see whether everything else works
	}
	if err != nil {
		res.Error = describeFetchError(u.Host, s.HTTPS.HTTPPort, err)
	} else {
		var got struct{ Instance string }
		_ = json.Unmarshal(body, &got)
		if got.Instance != inst {
			res.Error = fmt.Sprintf("%s answered, but not as this TainerHub. Check the proxy forwards to this hub's address and port %s.", u.Host, s.HTTPS.HTTPPort)
		}
	}
	s.https.mu.Lock()
	res.ProxyIP, res.Proto, res.Host = check.remoteIP, check.proto, check.host
	delete(s.https.pending, nonce)
	if res.Error == "" && res.TLS == "" {
		res.OK = true
		s.https.passed[u.String()] = res
	}
	s.https.mu.Unlock()
	writeJSON(w, http.StatusOK, res)
}

// errBlockedTarget: the test address resolves somewhere a reverse proxy
// can't be, such as the hub itself or a cloud metadata service.
var errBlockedTarget = errors.New("blocked address")

// blockedProbeAddr reports whether the HTTPS test may not connect to a.
// The test is for a reverse proxy, which lives on the LAN or the internet,
// so private ranges stay allowed; loopback, link-local (169.254.169.254
// cloud metadata), unspecified and multicast addresses are refused, so the
// test can't be used to probe the hub's own host.
func blockedProbeAddr(a netip.Addr) bool {
	a = a.Unmap()
	return !a.IsValid() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified()
}

// probeDialControl runs after DNS resolution, on the exact address being
// dialled, so a name that resolves differently the second time (DNS
// rebinding) can't slip past.
func probeDialControl(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return errBlockedTarget
	}
	if blockedProbeAddr(ap.Addr()) {
		return fmt.Errorf("%w %s", errBlockedTarget, ap.Addr())
	}
	return nil
}

func fetch(ctx context.Context, target string, insecure bool) ([]byte, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: probeDialControl}
	tr := &http.Transport{
		DialContext:     dialer.DialContext,
		Proxy:           nil,                                                                     // connect directly, so the address check applies
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecure}, // #nosec G402 -- only to report what else works
	}
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return b, fmt.Errorf("HTTP %s", resp.Status)
	}
	return b, nil
}

func isTLSError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "x509") || strings.Contains(s, "certificate") || strings.Contains(s, "tls:")
}

func describeFetchError(host, port string, err error) string {
	s := err.Error()
	switch {
	case errors.Is(err, errBlockedTarget):
		return fmt.Sprintf("%s points at this server itself or a reserved address, which can't be a reverse proxy. Point its DNS record at your proxy's LAN or public IP.", host)
	case strings.Contains(s, "no such host"):
		return fmt.Sprintf("%s doesn't resolve from the hub. Add a DNS record pointing it at your proxy (in your router, Pi-hole or DNS provider).", host)
	case strings.Contains(s, "connection refused"):
		return fmt.Sprintf("Nothing answered on %s port 443. Is the proxy running, and does the DNS record point at it?", host)
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline"):
		return fmt.Sprintf("%s didn't answer in time. Check DNS and that the proxy is reachable from the hub's server.", host)
	case strings.HasPrefix(s, "HTTP 502") || strings.HasPrefix(s, "HTTP 504"):
		return fmt.Sprintf("The proxy answered but couldn't reach TainerHub (%s). Check the forward host and port %s in the proxy.", s, port)
	case strings.HasPrefix(s, "HTTP 3"):
		return fmt.Sprintf("The proxy redirected the request (%s). Check the proxy host's scheme is http and its port the hub's.", s)
	case strings.HasPrefix(s, "HTTP "):
		return fmt.Sprintf("The proxy answered %s. Check the proxy host points at this hub.", s)
	}
	return "Couldn't reach " + host + ": " + s
}

func (s *Server) httpsEnable(w http.ResponseWriter, r *http.Request) {
	if s.HTTPS.EnvPublicURL != nil {
		jsonErr(w, http.StatusConflict, "the address is set by TAINERHUB_PUBLIC_URL in the hub's compose file; change it there")
		return
	}
	// The hub's address decides where passkeys and cookies are valid.
	if !s.fresh(w, r) {
		return
	}
	var in struct {
		URL     string
		Trusted string // comma-separated IPs or CIDRs
		Force   bool   // turn on without a passing test
	}
	if !readJSON(w, r, &in) {
		return
	}
	u, err := parseHTTPSURL(in.URL)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.https.mu.Lock()
	s.https.init()
	passed, ok := s.https.passed[u.String()]
	s.https.mu.Unlock()
	if !in.Force && (!ok || time.Since(passed.At) > 15*time.Minute) {
		jsonErr(w, http.StatusBadRequest, "test the address first (or turn it on anyway)")
		return
	}
	trusted, err := parseTrustedStrict(in.Trusted)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(trusted) == 0 {
		// Without it the hub can't tell visitors apart (they all look like
		// the proxy: one bad guesser locks everyone out) or know they used
		// HTTPS (cookies wouldn't be marked Secure).
		jsonErr(w, http.StatusBadRequest, "enter your proxy's IP address (the test shows it), so TainerHub can see each visitor's real IP and that they used HTTPS")
		return
	}
	var parts []string
	for _, p := range trusted {
		parts = append(parts, p.String())
	}
	if err := s.Store.SetSetting(r.Context(), settingPublicURL, u.String()); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.Store.SetSetting(r.Context(), settingTrusted, strings.Join(parts, ",")); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.Auth.Reconfigure(u, append(append([]netip.Prefix{}, s.HTTPS.EnvTrusted...), trusted...)); err != nil {
		s.fail(w, err)
		return
	}
	me := userFrom(r)
	s.record(r, &me, "settings.https_on", u.String(), map[string]any{"trusted_proxies": parts, "tested": ok})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) httpsDisable(w http.ResponseWriter, r *http.Request) {
	if s.HTTPS.EnvPublicURL != nil {
		jsonErr(w, http.StatusConflict, "the address is set by TAINERHUB_PUBLIC_URL in the hub's compose file; change it there")
		return
	}
	if !s.fresh(w, r) {
		return
	}
	_ = s.Store.DeleteSetting(r.Context(), settingPublicURL)
	_ = s.Store.DeleteSetting(r.Context(), settingTrusted)
	if err := s.Auth.Reconfigure(nil, s.HTTPS.EnvTrusted); err != nil {
		s.fail(w, err)
		return
	}
	me := userFrom(r)
	s.record(r, &me, "settings.https_off", "", nil)
	w.WriteHeader(http.StatusNoContent)
}

// publicURLString is the secure address ("" when HTTPS is off). Passkeys only
// work there, so the UI links to it from the LAN address.
func (s *Server) publicURLString() string {
	if u := s.Auth.PublicURL(); u != nil {
		return u.String()
	}
	return ""
}

// untrustedShowFor is how long Settings warns about a proxy that isn't
// trusted after it was last seen; untrustedLogEvery how often the log does.
const (
	untrustedShowFor  = 24 * time.Hour
	untrustedLogEvery = time.Hour
)

// noteUntrustedProxy spots a reverse proxy that isn't in the trusted list:
// a request carrying forwarding headers from a peer that isn't trusted,
// while a secure address is set. Its headers are ignored (as they must be),
// so every visitor through it looks like the proxy itself: the sign-in limit
// is shared by all of them and cookies aren't marked Secure. Settings shows
// it, and the log says so once an hour per address.
func (s *Server) noteUntrustedProxy(r *http.Request) {
	if r.Header.Get("X-Forwarded-For") == "" && r.Header.Get("X-Forwarded-Proto") == "" {
		return
	}
	if s.Auth.PublicURL() == nil || s.Auth.FromTrustedProxy(r) {
		return
	}
	ip := r.RemoteAddr
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		ip = ap.Addr().Unmap().String()
	}
	now := time.Now()
	s.https.mu.Lock()
	s.https.init()
	s.https.untrusted = untrustedSeen{IP: ip, At: now}
	logIt := now.Sub(s.https.warned[ip]) > untrustedLogEvery
	if logIt {
		if len(s.https.warned) > 1000 { // bounded: a flood of addresses can't grow it
			clear(s.https.warned)
		}
		s.https.warned[ip] = now
	}
	s.https.mu.Unlock()
	if logIt {
		s.Log.Warn("a reverse proxy that isn't trusted is forwarding to TainerHub, so visitors' real IPs and HTTPS are ignored. If it's your proxy, add its IP to TAINERHUB_TRUSTED_PROXIES (or turn HTTPS off and on again in Settings with it)", "proxy", ip)
	}
}

// putSignInSettings changes the sign-in rule for the secure address:
// accounts with a passkey must use it there (passwords keep working on the
// LAN address). Either way changes how people sign in, so it needs a fresh
// sign-in.
func (s *Server) putSignInSettings(w http.ResponseWriter, r *http.Request) {
	if !s.fresh(w, r) {
		return
	}
	var in struct {
		PasskeyRequired bool `json:"passkeyRequired"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	v := "0"
	if in.PasskeyRequired {
		v = "1"
	}
	if err := s.Store.SetSetting(r.Context(), settingPasskeyRequired, v); err != nil {
		s.fail(w, err)
		return
	}
	s.Auth.SetPasskeyRequired(in.PasskeyRequired)
	me := userFrom(r)
	s.record(r, &me, "settings.sign_in", "", map[string]bool{"passkey_required": in.PasskeyRequired})
	w.WriteHeader(http.StatusNoContent)
}
