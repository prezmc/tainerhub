// Package auth implements TainerHub's local identity: passwords (work on any
// install, even plain HTTP), passkeys (WebAuthn; need HTTPS and a configured
// public URL), single-use recovery codes as break-glass, and server-side
// sessions.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/prezmc/tainerhub/internal/store"
)

const (
	maxCeremonies   = 10000 // bounds memory if someone floods /api/login/begin
	SessionIdle     = 12 * time.Hour
	SessionMax      = 7 * 24 * time.Hour
	ceremonyTimeout = 5 * time.Minute
	setupTokenTTL   = 15 * time.Minute
	recoveryCount   = 10
)

var (
	ErrUnauthorized = errors.New("not signed in")
	ErrSetupDone    = errors.New("setup already completed")
	ErrBadSetup     = errors.New("setup link invalid or expired; check the hub logs for a new one")
	ErrCeremony     = errors.New("sign-in attempt expired; try again")
	ErrNoPasskeys   = errors.New("passkeys need HTTPS: put TainerHub behind your reverse proxy and set TAINERHUB_PUBLIC_URL to its https:// address")
)

type Service struct {
	Store *store.Store
	Log   *slog.Logger

	// web holds the public URL, its WebAuthn config and the trusted proxies.
	// It's swapped atomically, so HTTPS can be switched on from the UI
	// without a restart (see Reconfigure).
	web atomic.Pointer[webConfig]
	// SetupTokenFile, if set, holds the current setup token while setup is
	// pending, so `tainerhub setup-link` (and the installer) can print it.
	SetupTokenFile string

	mu         sync.Mutex
	ceremonies map[string]*ceremony
	setupToken string
	setupExp   time.Time

	Limiter *Limiter
}

type ceremony struct {
	kind    string // "setup", "login", "add-passkey"
	data    webauthn.SessionData
	user    store.User // for setup / add-passkey
	keyName string
	expires time.Time
}

type webConfig struct {
	publicURL *url.URL           // nil: plain-HTTP install, passwords only
	wa        *webauthn.WebAuthn // nil when passkeys are unavailable
	trusted   []netip.Prefix     // proxies allowed to set X-Forwarded-For/-Proto
}

// New builds the auth service. publicURL may be nil: then only passwords and
// recovery codes work.
func New(st *store.Store, publicURL *url.URL, trusted []netip.Prefix, log *slog.Logger) (*Service, error) {
	s := &Service{Store: st, Log: log, ceremonies: map[string]*ceremony{}, Limiter: NewLimiter(10, 15*time.Minute)}
	if err := s.Reconfigure(publicURL, trusted); err != nil {
		return nil, err
	}
	return s, nil
}

// Reconfigure switches the public URL (and with it passkeys) and the trusted
// proxies, live.
func (s *Service) Reconfigure(publicURL *url.URL, trusted []netip.Prefix) error {
	c := &webConfig{publicURL: publicURL, trusted: trusted}
	if publicURL != nil {
		wa, err := webauthn.New(&webauthn.Config{
			RPID:          publicURL.Hostname(), // passkeys are bound to this name: don't change it later
			RPDisplayName: "TainerHub",
			RPOrigins:     []string{publicURL.Scheme + "://" + publicURL.Host},
		})
		if err != nil {
			return err
		}
		c.wa = wa
	}
	s.web.Store(c)
	return nil
}

func (s *Service) cfg() *webConfig {
	if c := s.web.Load(); c != nil {
		return c
	}
	return &webConfig{}
}

// PublicURL is the configured https:// address, or nil.
func (s *Service) PublicURL() *url.URL { return s.cfg().publicURL }

// Trusted lists the reverse proxies whose forwarding headers are believed.
func (s *Service) Trusted() []netip.Prefix { return s.cfg().trusted }

// IsHTTPS reports whether the browser reached the hub over HTTPS: directly,
// or through a trusted reverse proxy that says so (X-Forwarded-Proto).
func (s *Service) IsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil && s.isTrusted(ap.Addr().Unmap()) {
		return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	}
	return false
}

// ---------------- helpers ----------------

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func Hash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// ClientIP honours X-Forwarded-For only when the direct peer is a trusted proxy (NPM).
func (s *Service) ClientIP(r *http.Request) string {
	host := r.RemoteAddr
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		host = ap.Addr().Unmap().String()
		if s.isTrusted(ap.Addr().Unmap()) {
			parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
			for i := len(parts) - 1; i >= 0; i-- {
				p := strings.TrimSpace(parts[i])
				a, err := netip.ParseAddr(p)
				if err != nil {
					break
				}
				if !s.isTrusted(a.Unmap()) || i == 0 {
					return a.Unmap().String()
				}
			}
		}
	}
	return host
}

func (s *Service) isTrusted(a netip.Addr) bool {
	for _, p := range s.cfg().trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Cookie names use the __Host- prefix over HTTPS: the browser then refuses
// them unless Secure, Path=/ and host-only, which blocks cookie injection
// from sibling subdomains (other apps on the same parent domain).
// The choice is per request: a browser on the https:// address (through the
// proxy) gets Secure __Host- cookies, one on http://<ip>:8585 plain ones.
func (s *Service) cookieName(r *http.Request, base string) string {
	if s.IsHTTPS(r) {
		return "__Host-" + base
	}
	return base
}

func (s *Service) sessionCookie(r *http.Request) string  { return s.cookieName(r, "th_session") }
func (s *Service) ceremonyCookie(r *http.Request) string { return s.cookieName(r, "th_ceremony") }

func (s *Service) setCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.IsHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(maxAge.Seconds()),
	})
}

func (s *Service) clearCookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", HttpOnly: true, Secure: s.IsHTTPS(r), SameSite: http.SameSiteStrictMode, MaxAge: -1})
}

func (s *Service) putCeremony(w http.ResponseWriter, r *http.Request, c *ceremony) {
	id := randToken(24)
	c.expires = time.Now().Add(ceremonyTimeout)
	s.mu.Lock()
	for k, v := range s.ceremonies { // opportunistic cleanup
		if time.Now().After(v.expires) {
			delete(s.ceremonies, k)
		}
	}
	if len(s.ceremonies) >= maxCeremonies {
		for k := range s.ceremonies { // evict an arbitrary entry
			delete(s.ceremonies, k)
			break
		}
	}
	s.ceremonies[id] = c
	s.mu.Unlock()
	s.setCookie(w, r, s.ceremonyCookie(r), id, ceremonyTimeout)
}

func (s *Service) takeCeremony(w http.ResponseWriter, r *http.Request, kind string) (*ceremony, error) {
	ck, err := r.Cookie(s.ceremonyCookie(r))
	if err != nil {
		return nil, ErrCeremony
	}
	s.clearCookie(w, r, s.ceremonyCookie(r))
	s.mu.Lock()
	c := s.ceremonies[ck.Value]
	delete(s.ceremonies, ck.Value)
	s.mu.Unlock()
	if c == nil || c.kind != kind || time.Now().After(c.expires) {
		return nil, ErrCeremony
	}
	return c, nil
}

// ---------------- webauthn user adapter ----------------

type waUser struct {
	u     store.User
	creds []webauthn.Credential
}

func (u *waUser) WebAuthnID() []byte                         { return u.u.WebAuthnID }
func (u *waUser) WebAuthnName() string                       { return u.u.Username }
func (u *waUser) WebAuthnDisplayName() string                { return u.u.DisplayName }
func (u *waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }
func (u *waUser) WebAuthnIcon() string                       { return "" } // older library versions

func (s *Service) loadWAUser(ctx context.Context, u store.User) (*waUser, error) {
	pks, err := s.Store.ListPasskeys(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	wu := &waUser{u: u}
	for _, pk := range pks {
		var c webauthn.Credential
		if err := json.Unmarshal(pk.Credential, &c); err != nil {
			s.Log.Warn("unreadable passkey", "user", u.Username, "err", err)
			continue
		}
		wu.creds = append(wu.creds, c)
	}
	return wu, nil
}

func registrationOptions(existing []webauthn.Credential) []webauthn.RegistrationOption {
	opts := []webauthn.RegistrationOption{
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired, // discoverable: username-less sign-in
			UserVerification: protocol.VerificationRequired,
		}),
	}
	if len(existing) > 0 {
		excl := make([]protocol.CredentialDescriptor, 0, len(existing))
		for _, c := range existing {
			excl = append(excl, c.Descriptor())
		}
		opts = append(opts, webauthn.WithExclusions(excl))
	}
	return opts
}

// ---------------- first-run setup ----------------

// SetupRequired reports whether no user exists yet.
func (s *Service) SetupRequired(ctx context.Context) (bool, error) {
	n, err := s.Store.CountUsers(ctx)
	return n == 0, err
}

// RotateSetupToken creates a new one-time setup token and logs the setup URL.
func (s *Service) RotateSetupToken() string {
	tok := randToken(24)
	s.mu.Lock()
	s.setupToken = tok
	s.setupExp = time.Now().Add(setupTokenTTL)
	s.mu.Unlock()
	if s.SetupTokenFile != "" {
		tmp := s.SetupTokenFile + ".tmp"
		if err := os.WriteFile(tmp, []byte(tok+"\n"), 0o600); err == nil {
			_ = os.Rename(tmp, s.SetupTokenFile)
		} else {
			s.Log.Warn("could not save the setup token for `tainerhub setup-link`", "err", err)
		}
	}
	base := "http://<this server's IP>:8585"
	if u := s.PublicURL(); u != nil {
		base = strings.TrimSuffix(u.String(), "/")
	}
	s.Log.Warn("No users yet. Open this link within 15 minutes to create the owner account", "url", base+"/setup?token="+tok)
	return tok
}

// RunSetupTokenLoop keeps a valid setup link in the logs until setup completes.
func (s *Service) RunSetupTokenLoop(ctx context.Context) {
	for {
		need, err := s.SetupRequired(ctx)
		if err == nil && !need {
			s.clearSetupToken()
			return
		}
		if err == nil {
			s.RotateSetupToken()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(setupTokenTTL):
		}
	}
}

// clearSetupToken ends setup: the token stops working and its file goes.
func (s *Service) clearSetupToken() {
	s.mu.Lock()
	s.setupToken = ""
	s.mu.Unlock()
	if s.SetupTokenFile != "" {
		_ = os.Remove(s.SetupTokenFile)
	}
}

func (s *Service) checkSetupToken(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setupToken != "" && time.Now().Before(s.setupExp) &&
		len(tok) == len(s.setupToken) && subtleEq(tok, s.setupToken)
}

type SetupBegin struct {
	Token       string `json:"token"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	PasskeyName string `json:"passkeyName"`
}

// PasskeysEnabled reports whether passkeys can be used (a public URL is set).
func (s *Service) PasskeysEnabled() bool { return s.cfg().wa != nil }

func (s *Service) BeginSetup(w http.ResponseWriter, r *http.Request, in SetupBegin) (*protocol.CredentialCreation, error) {
	wa := s.cfg().wa
	if wa == nil {
		return nil, ErrNoPasskeys
	}
	if need, err := s.SetupRequired(r.Context()); err != nil {
		return nil, err
	} else if !need {
		return nil, ErrSetupDone
	}
	if !s.checkSetupToken(in.Token) {
		return nil, ErrBadSetup
	}
	in.Username = strings.TrimSpace(in.Username)
	if !validUsername(in.Username) {
		return nil, errors.New("username must be 2-32 letters, digits, dots, dashes or underscores")
	}
	if strings.TrimSpace(in.DisplayName) == "" {
		in.DisplayName = in.Username
	}
	u := store.User{Username: in.Username, DisplayName: strings.TrimSpace(in.DisplayName), Role: "owner", WebAuthnID: []byte(randToken(32))}
	opts, data, err := wa.BeginRegistration(&waUser{u: u}, registrationOptions(nil)...)
	if err != nil {
		return nil, err
	}
	s.putCeremony(w, r, &ceremony{kind: "setup", data: *data, user: u, keyName: keyName(in.PasskeyName)})
	return opts, nil
}

// FinishSetup verifies the passkey, creates the owner, signs them in and
// returns their recovery codes (shown once).
func (s *Service) FinishSetup(w http.ResponseWriter, r *http.Request) (store.User, []string, error) {
	wa := s.cfg().wa
	if wa == nil {
		return store.User{}, nil, ErrNoPasskeys
	}
	c, err := s.takeCeremony(w, r, "setup")
	if err != nil {
		return store.User{}, nil, err
	}
	cred, err := wa.FinishRegistration(&waUser{u: c.user}, c.data, r)
	if err != nil {
		return store.User{}, nil, errors.New("passkey verification failed")
	}
	credJSON, _ := json.Marshal(cred)
	codes, hashes := newRecoveryCodes()
	u, err := s.Store.CreateFirstOwner(r.Context(), c.user, &store.Passkey{ID: cred.ID, Name: c.keyName, Credential: credJSON}, hashes)
	if errors.Is(err, store.ErrConflict) {
		return store.User{}, nil, ErrSetupDone
	}
	if err != nil {
		return store.User{}, nil, err
	}
	s.clearSetupToken()
	if err := s.startSession(w, r, u); err != nil {
		return store.User{}, nil, err
	}
	return u, codes, nil
}

// ---------------- sign-in ----------------

func (s *Service) BeginLogin(w http.ResponseWriter, r *http.Request) (*protocol.CredentialAssertion, error) {
	wa := s.cfg().wa
	if wa == nil {
		return nil, ErrNoPasskeys
	}
	opts, data, err := wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, err
	}
	s.putCeremony(w, r, &ceremony{kind: "login", data: *data})
	return opts, nil
}

func (s *Service) FinishLogin(w http.ResponseWriter, r *http.Request) (store.User, error) {
	wa := s.cfg().wa
	if wa == nil {
		return store.User{}, ErrNoPasskeys
	}
	ip := s.ClientIP(r)
	if !s.Limiter.Allow(ip) {
		return store.User{}, ErrRateLimited
	}
	c, err := s.takeCeremony(w, r, "login")
	if err != nil {
		return store.User{}, err
	}
	var found *waUser
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		u, err := s.Store.UserByWebAuthnID(r.Context(), userHandle)
		if err != nil {
			return nil, err
		}
		wu, err := s.loadWAUser(r.Context(), u)
		if err != nil {
			return nil, err
		}
		found = wu
		return wu, nil
	}
	cred, err := wa.FinishDiscoverableLogin(handler, c.data, r)
	if err != nil || found == nil {
		s.Limiter.Fail(ip)
		return store.User{}, errors.New("passkey not recognised")
	}
	if cred.Authenticator.CloneWarning {
		s.Log.Warn("passkey sign count went backwards; possible cloned authenticator", "user", found.u.Username)
		s.Limiter.Fail(ip)
		return store.User{}, errors.New("passkey rejected")
	}
	credJSON, _ := json.Marshal(cred)
	if err := s.Store.UpdatePasskeyAfterLogin(r.Context(), cred.ID, credJSON); err != nil {
		s.Log.Warn("update passkey", "err", err)
	}
	return found.u, s.startSession(w, r, found.u)
}

var ErrRateLimited = errors.New("too many failed attempts; wait 15 minutes")

// LoginWithRecoveryCode signs in with a single-use code. Each code works once.
func (s *Service) LoginWithRecoveryCode(w http.ResponseWriter, r *http.Request, username, code string) (store.User, error) {
	ip := s.ClientIP(r)
	if !s.Limiter.Allow(ip) {
		return store.User{}, ErrRateLimited
	}
	fail := errors.New("username or recovery code is wrong")
	u, err := s.Store.UserByUsername(r.Context(), strings.TrimSpace(username))
	if err != nil {
		s.Limiter.Fail(ip)
		return store.User{}, fail
	}
	ok, err := s.Store.UseRecoveryCode(r.Context(), u.ID, Hash(normalizeCode(code)))
	if err != nil {
		return store.User{}, err
	}
	if !ok {
		s.Limiter.Fail(ip)
		return store.User{}, fail
	}
	return u, s.startSession(w, r, u)
}

// ---------------- adding passkeys ----------------

func (s *Service) BeginAddPasskey(w http.ResponseWriter, r *http.Request, u store.User, name string) (*protocol.CredentialCreation, error) {
	wa := s.cfg().wa
	if wa == nil {
		return nil, ErrNoPasskeys
	}
	wu, err := s.loadWAUser(r.Context(), u)
	if err != nil {
		return nil, err
	}
	opts, data, err := wa.BeginRegistration(wu, registrationOptions(wu.creds)...)
	if err != nil {
		return nil, err
	}
	s.putCeremony(w, r, &ceremony{kind: "add-passkey", data: *data, user: u, keyName: keyName(name)})
	return opts, nil
}

func (s *Service) FinishAddPasskey(w http.ResponseWriter, r *http.Request, u store.User) error {
	wa := s.cfg().wa
	if wa == nil {
		return ErrNoPasskeys
	}
	c, err := s.takeCeremony(w, r, "add-passkey")
	if err != nil {
		return err
	}
	if c.user.ID != u.ID {
		return ErrCeremony
	}
	wu, err := s.loadWAUser(r.Context(), u)
	if err != nil {
		return err
	}
	cred, err := wa.FinishRegistration(wu, c.data, r)
	if err != nil {
		return errors.New("passkey verification failed")
	}
	credJSON, _ := json.Marshal(cred)
	return s.Store.AddPasskey(r.Context(), store.Passkey{ID: cred.ID, UserID: u.ID, Name: c.keyName, Credential: credJSON})
}

func (s *Service) RegenerateRecoveryCodes(ctx context.Context, u store.User) ([]string, error) {
	codes, hashes := newRecoveryCodes()
	return codes, s.Store.ReplaceRecoveryCodes(ctx, u.ID, hashes)
}

// ---------------- sessions ----------------

func (s *Service) startSession(w http.ResponseWriter, r *http.Request, u store.User) error {
	tok := randToken(32)
	if err := s.Store.CreateSession(r.Context(), Hash(tok), u.ID, time.Now().Add(SessionMax), s.ClientIP(r), truncate(r.UserAgent(), 200)); err != nil {
		return err
	}
	s.setCookie(w, r, s.sessionCookie(r), tok, SessionMax)
	return nil
}

// UserFromRequest returns the signed-in user, enforcing idle and absolute timeouts.
func (s *Service) UserFromRequest(r *http.Request) (store.User, error) {
	ck, err := r.Cookie(s.sessionCookie(r))
	if err != nil || ck.Value == "" {
		return store.User{}, ErrUnauthorized
	}
	h := Hash(ck.Value)
	ss, err := s.Store.GetSession(r.Context(), h)
	if err != nil {
		return store.User{}, ErrUnauthorized
	}
	now := time.Now()
	if now.After(ss.ExpiresAt) || now.Sub(ss.LastSeenAt) > SessionIdle {
		_ = s.Store.DeleteSession(r.Context(), h)
		return store.User{}, ErrUnauthorized
	}
	if now.Sub(ss.LastSeenAt) > time.Minute {
		_ = s.Store.TouchSession(r.Context(), h)
	}
	u, err := s.Store.UserByID(r.Context(), ss.UserID)
	if err != nil {
		return store.User{}, ErrUnauthorized
	}
	return u, nil
}

func (s *Service) Logout(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(s.sessionCookie(r)); err == nil {
		_ = s.Store.DeleteSession(r.Context(), Hash(ck.Value))
	}
	s.clearCookie(w, r, s.sessionCookie(r))
}

func (s *Service) RunSessionJanitor(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := s.Store.PurgeSessions(ctx, time.Now().Add(-SessionIdle)); err != nil && ctx.Err() == nil {
			s.Log.Warn("purge sessions", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
