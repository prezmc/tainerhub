// Package api is the hub's browser-facing HTTP server (default :8585, behind
// Nginx Proxy Manager). It serves the React UI and a JSON API under /api.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prezmc/tainerhub/internal/auth"
	"github.com/prezmc/tainerhub/internal/gateway"
	"github.com/prezmc/tainerhub/internal/pki"
	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/version"
	"github.com/prezmc/tainerhub/internal/wire"
)

type Server struct {
	Store *store.Store
	Auth  *auth.Service
	Reg   *registry.Registry
	CA    *pki.CA
	Log   *slog.Logger
	// AgentAddr is what agents dial, e.g. 192.168.1.10:7443. Empty: use the
	// host the admin's browser reached the hub on, port 7443 (see agentAddrFor).
	AgentAddr string
	Web       fs.FS // built React app (index.html at root)

	DataDir string      // hub's data folder: knows which host is the hub's own server
	HTTPS   HTTPSConfig // how HTTPS was configured at startup (see https.go)

	jobs         jobList           // running and recently finished stack operations
	hub          hubCache          // Docker Hub search and tag results
	latest       latestCache       // newest published version
	dockerLatest dockerLatestCache // newest Docker Engine release
	dockerPlans  dockerPlanCache   // what each host's package repository offers
	osStatus     osStatusCache     // each host's pending OS package updates
	execs        execList          // open terminals
	https        httpsState        // HTTPS setup tests
	statsMu      sync.Mutex        // serializes stats source changes (one settings row)
	fleetOnce    sync.Once
	fleet        *fleetFeed      // shared /api/events producer (see fleet.go)
	logStreams   logStreamCounts // open log streams per user (see streams.go)

	regKey regKey // registry login encryption, loaded on first use (see registries.go)

	// Test hooks; nil uses s.Auth.
	whoami     func(*http.Request) (store.User, error)
	authn      func(*http.Request) (store.User, *auth.TokenInfo, error)
	freshCheck func(*http.Request) error
	// More test hooks; nil uses s.Store.
	regCreds registryCredStore
	audited  func(store.AuditEntry)
	autoDB   autoUpdateStore // automatic updates and stack jobs (see autoupdate.go)

	autoUp autoUpdater // automatic updates: window schedule and runs
}

var roleRank = auth.RoleRank

type ctxKey struct{}

func userFrom(r *http.Request) store.User { return r.Context().Value(ctxKey{}).(store.User) }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// public
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/setup/begin", s.setupBegin)
	mux.HandleFunc("POST /api/setup/finish", s.setupFinish)
	mux.HandleFunc("POST /api/login/begin", s.loginBegin)
	mux.HandleFunc("POST /api/login/finish", s.loginFinish)
	mux.HandleFunc("POST /api/login/recovery", s.loginRecovery)
	mux.HandleFunc("POST /api/login/password", s.loginPassword)
	mux.HandleFunc("POST /api/setup/password", s.setupPassword)
	mux.HandleFunc("GET /join/{token}", s.joinScript)        // one-line agent install (see join.go)
	mux.HandleFunc("GET /api/https-check", s.httpsCheckEcho) // answers the hub's own HTTPS test
	mux.HandleFunc("POST /api/invite/check", s.checkInvite)
	mux.HandleFunc("POST /api/invite/accept", s.acceptInvite)
	mux.HandleFunc("POST /api/logout", s.logout)

	for _, rt := range s.routes() {
		h := s.need(rt.role, rt.scope, rt.fn)
		if rt.stream {
			h = s.stream(rt.role, rt.scope, rt.fn)
		}
		mux.Handle(rt.pattern, h)
	}

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { jsonErr(w, http.StatusNotFound, "not found") })
	mux.Handle("/", s.spa())

	return s.secure(mux)
}

// routes is the signed-in API: each route's minimum role and which hosts it
// concerns (see scope.go). Users limited to some hosts are kept to them by
// need(); TestRouteScopes checks every route has a fitting scope.
func (s *Server) routes() []route {
	rt := func(pattern, role string, sc scope, fn http.HandlerFunc) route {
		return route{pattern: pattern, role: role, scope: sc, fn: fn}
	}
	stream := func(pattern, role string, sc scope, fn http.HandlerFunc) route {
		return route{pattern: pattern, role: role, scope: sc, fn: fn, stream: true}
	}
	out := []route{
		// any signed-in user: their own account
		rt("GET /api/me", "viewer", scopeAny, s.me),
		rt("POST /api/me/passkeys/begin", "viewer", scopeAny, s.addPasskeyBegin),
		rt("POST /api/me/passkeys/finish", "viewer", scopeAny, s.addPasskeyFinish),
		rt("DELETE /api/me/passkeys/{id}", "viewer", scopeAny, s.deletePasskey),
		rt("POST /api/me/recovery-codes", "viewer", scopeAny, s.regenRecovery),
		rt("POST /api/me/password", "viewer", scopeAny, s.changePassword),
		rt("POST /api/me/sessions/sign-out-others", "viewer", scopeAny, s.signOutOthers),
		rt("GET /api/me/tokens", "viewer", scopeAny, s.listMyTokens),
		rt("POST /api/me/tokens", "viewer", scopeAny, s.createToken), // tokens carry the user's hosts
		rt("DELETE /api/me/tokens/{tid}", "viewer", scopeAny, s.revokeMyToken),

		// viewers
		rt("GET /api/containers", "viewer", scopeFiltered, s.containers),
		rt("GET /api/events", "viewer", scopeFiltered, s.events),
		rt("GET /api/hosts", "viewer", scopeFiltered, s.hosts),
		rt("GET /api/hosts/agent-addr", "admin", scopeHub, s.agentAddrDefault),
		rt("GET /api/placement", "viewer", scopeFiltered, s.placement),
		stream("GET /api/hosts/{id}/containers/{cid}/logs", "viewer", scopeHost, s.logs),
		rt("GET /api/hosts/{id}/containers/{cid}", "viewer", scopeHost, s.inspect),
		rt("GET /api/hosts/{id}/images", "viewer", scopeHost, s.listResources("images")),
		rt("GET /api/hosts/{id}/volumes", "viewer", scopeHost, s.listResources("volumes")),
		rt("GET /api/hosts/{id}/networks", "viewer", scopeHost, s.listResources("networks")),
		rt("GET /api/stacks", "viewer", scopeFiltered, s.listStacks),
		rt("GET /api/stacks/{sid}", "viewer", scopeStack, s.getStack),
		stream("GET /api/jobs/{jid}", "viewer", scopeFiltered, s.jobStream),

		// operators
		rt("POST /api/hosts/{id}/containers/{cid}/{action}", "operator", scopeHost, s.containerAction),
		rt("POST /api/hosts/{id}/images/pull", "operator", scopeHost, s.pullImage),
		// image updates: check now, and pull & recreate a standalone container
		rt("POST /api/hosts/{id}/check-updates", "operator", scopeHost, func(w http.ResponseWriter, r *http.Request) {
			s.runOp(w, r, wire.Operation{Kind: wire.OpImagesCheckUpdates}, "images")
		}),
		rt("POST /api/hosts/{id}/containers/{cid}/recreate", "operator", scopeHost, func(w http.ResponseWriter, r *http.Request) {
			cid := r.PathValue("cid")
			s.runOp(w, r, wire.Operation{Kind: wire.OpContainerRecreate, Target: cid}, s.containerName(r.PathValue("id"), cid))
		}),
		rt("DELETE /api/hosts/{id}/images/{rid}", "operator", scopeHost, s.removeResource(wire.OpImageRemove)),
		rt("DELETE /api/hosts/{id}/networks/{rid}", "operator", scopeHost, s.removeResource(wire.OpNetworkRemove)),
		rt("POST /api/hosts/{id}/images/prune", "operator", scopeHost, s.prune(wire.OpImagesPrune)),
		rt("POST /api/hosts/{id}/networks/prune", "operator", scopeHost, s.prune(wire.OpNetworksPrune)),
		// volumes hold data: removing them is admin-only
		rt("DELETE /api/hosts/{id}/volumes/{rid}", "admin", scopeHost, s.removeResource(wire.OpVolumeRemove)),
		rt("POST /api/hosts/{id}/volumes/prune", "admin", scopeHost, s.prune(wire.OpVolumesPrune)),
		// network share volumes carry the share's login (see sharevolume.go)
		rt("POST /api/hosts/{id}/volumes/share", "admin", scopeHost, s.createShareVolume),
		rt("POST /api/hosts/{id}/containers/{cid}/remove-with-volumes", "admin", scopeHost, s.removeWithVolumes),
		// stack lifecycle on definitions an admin has saved
		rt("POST /api/stacks/{sid}/{action}", "operator", scopeStack, s.stackAction),
		rt("GET /api/stacks/{sid}/rollback", "viewer", scopeStack, s.getRollback),
		rt("POST /api/stacks/{sid}/rollback", "operator", scopeStack, s.rollbackStack),
		rt("POST /api/stacks/takeover-check", "admin", scopeFiltered, s.checkTakeover),
		rt("POST /api/stacks/preflight", "admin", scopeFiltered, s.checkPreflight),
		rt("GET /api/hosts/{id}/folders", "admin", scopeHost, s.hostFolders),
		rt("GET /api/stacks/takeover-source", "admin", scopeFiltered, s.takeoverSource),
		// moves need both hosts: the path names the source, the handler checks the target
		rt("GET /api/stacks/{sid}/move", "admin", scopeStack, s.moveCheck),
		rt("POST /api/stacks/{sid}/move", "admin", scopeStack, s.startMove),
		rt("DELETE /api/moves/{mid}/leftovers", "admin", scopeFiltered, s.removeLeftovers),
		rt("GET /api/hosts/{id}/containers/{cid}/move", "admin", scopeHost, s.containerMoveCheck),
		rt("POST /api/hosts/{id}/containers/{cid}/move", "admin", scopeHost, s.startContainerMove),
		rt("GET /api/container-moves", "admin", scopeFiltered, s.containerMoves),
		// writing a compose file is root on the host: admins only (see stacks.go)
		rt("POST /api/stacks", "admin", scopeFiltered, s.createStack),
		// Docker Hub lookups for the "New container" page (which creates a stack)
		rt("GET /api/dockerhub/search", "admin", scopeAny, s.hubSearch),
		rt("GET /api/dockerhub/tags", "admin", scopeAny, s.hubTags),
		rt("PUT /api/stacks/{sid}", "admin", scopeStack, s.updateStack),
		rt("DELETE /api/stacks/{sid}", "admin", scopeStack, s.deleteStack),

		// admins
		rt("POST /api/hosts", "admin", scopeHub, s.createHost),
		rt("POST /api/hosts/{id}/token", "admin", scopeHub, s.newHostToken),
		rt("DELETE /api/hosts/{id}", "admin", scopeHub, s.deleteHost),
		rt("PUT /api/hosts/{id}", "admin", scopeHub, s.renameHost),
		rt("GET /api/audit", "admin", scopeFiltered, s.audit),
		rt("GET /api/users", "admin", scopeHub, s.listUsers),
		rt("POST /api/invites", "admin", scopeHub, s.createInvite),
		rt("DELETE /api/invites/{iid}", "admin", scopeHub, s.deleteInvite),
		rt("PUT /api/users/{uid}/role", "admin", scopeHub, s.setRole),
		rt("PUT /api/users/{uid}/hosts", "admin", scopeHub, s.setHosts),
		rt("DELETE /api/users/{uid}", "admin", scopeHub, s.deleteUser),
		rt("GET /api/tokens", "admin", scopeHub, s.listAllTokens),
		rt("DELETE /api/tokens/{tid}", "admin", scopeHub, s.revokeAnyToken),
		rt("GET /api/updates", "admin", scopeFiltered, s.updates),
		rt("POST /api/updates/check", "admin", scopeFiltered, s.checkUpdates),
		rt("GET /api/settings/https", "admin", scopeHub, s.httpsSettings),
		rt("POST /api/settings/https/test", "admin", scopeHub, s.httpsTest),
		rt("PUT /api/settings/https", "admin", scopeHub, s.httpsEnable),
		rt("DELETE /api/settings/https", "admin", scopeHub, s.httpsDisable),
		rt("PUT /api/settings/sign-in", "admin", scopeHub, s.putSignInSettings),
		// private registry logins (see registries.go); changes need a fresh sign-in
		rt("GET /api/registries", "admin", scopeHub, s.listRegistries),
		rt("POST /api/registries", "admin", scopeHub, s.addRegistry),
		rt("PUT /api/registries/{rid}", "admin", scopeHub, s.updateRegistry),
		rt("DELETE /api/registries/{rid}", "admin", scopeHub, s.deleteRegistry),
		rt("POST /api/registries/{rid}/test", "admin", scopeHub, s.testRegistry),
		// terminals: admin-only (a shell sees everything in the container).
		// A terminal belongs to the user who opened it (on a host they reach).
		rt("POST /api/hosts/{id}/containers/{cid}/exec", "admin", scopeHost, s.execOpen),
		stream("GET /api/exec/{eid}", "admin", scopeAny, s.execStream), // ends if no longer admin, or the host goes out of reach
		rt("POST /api/exec/{eid}/input", "admin", scopeAny, s.execInput),
		rt("POST /api/exec/{eid}/resize", "admin", scopeAny, s.execResize),
		rt("DELETE /api/exec/{eid}", "admin", scopeAny, s.execClose),
		// automatic updates (see autoupdate.go); changes need a fresh sign-in
		rt("GET /api/auto-updates", "admin", scopeFiltered, s.listAutoUpdates),
		rt("GET /api/auto-updates/settings", "admin", scopeHub, s.getAutoSettings),
		rt("PUT /api/auto-updates/settings", "admin", scopeHub, s.putAutoSettings),
		rt("POST /api/auto-updates/run", "admin", scopeHub, s.runAutoUpdatesNow),
		rt("GET /api/auto-updates/runs/latest", "admin", scopeHub, s.latestAutoRunHandler),
		rt("PUT /api/stacks/{sid}/auto-update", "admin", scopeStack, s.stackAutoUpdate),
		rt("PUT /api/hosts/{id}/containers/{name}/auto-update", "admin", scopeHost, s.containerAutoUpdate),
		rt("POST /api/hosts/{id}/update", "admin", scopeHost, s.updateHost),
		rt("POST /api/hosts/{id}/lighter-stats", "admin", scopeHost, s.lighterStats),
		rt("PUT /api/hosts/{id}/stats-source", "admin", scopeHost, s.statsSource),
		// Docker Engine upgrades restart every container: admins, with explicit confirmation
		rt("POST /api/hosts/{id}/docker/plan", "admin", scopeHost, s.dockerPlan),
		rt("POST /api/hosts/{id}/os/check", "admin", scopeHost, s.checkOS),
		rt("POST /api/hosts/{id}/os/upgrade", "admin", scopeHost, s.upgradeOS),
		rt("POST /api/hosts/{id}/docker/upgrade", "admin", scopeHost, s.dockerUpgrade),
	}
	return out
}

// secure adds security headers and CSRF protection for state-changing calls.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := ""
		if u := s.Auth.PublicURL(); u != nil { // read per request: it can change at runtime
			origin = u.Scheme + "://" + u.Host
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		// A request with a bearer token is authenticated by it alone, never
		// by cookies (see auth.HasBearer), so there is nothing for a forged
		// cross-site request to ride on: the CSRF checks don't apply.
		s.noteUntrustedProxy(r)
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions && !auth.HasBearer(r) {
			// SameSite=Strict cookies plus a custom header (which cross-site
			// forms cannot send) plus an Origin check.
			if r.Header.Get("X-TainerHub") != "1" {
				jsonErr(w, http.StatusForbidden, "missing request header")
				return
			}
			if o := r.Header.Get("Origin"); o != "" && !sameOrigin(o, origin, r, s.AgentAddr) {
				jsonErr(w, http.StatusForbidden, "cross-origin request refused")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin checks a request's Origin header: it must be the configured
// public URL, or the very host the request was sent to (e.g. the hub's LAN
// address, which keeps working after HTTPS is set up). Either way a page on
// another site can't make the request.
//
// "Origin equals Host" alone doesn't stop DNS rebinding, where an attacker's
// name (evil.example) is made to resolve to the hub: the browser then sends
// Origin and Host both as evil.example. So once a public URL is configured,
// the Host must also be a name the hub knows (the public URL's host or one
// in known), an IP literal, or a local-only name that public DNS can't hand
// to an attacker (localhost, single-label names like "nas", and suffixes
// such as .lan, .local, .home.arpa, .internal). Without a public URL there
// is no list of the hub's names to check against, and plain-HTTP installs
// are often reached by arbitrary internal names, so only Origin == Host is
// required there; rebinding still gets no session, since cookies are bound
// to the real host name.
func sameOrigin(o, configured string, r *http.Request, known ...string) bool {
	if configured != "" && o == configured {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
		return false
	}
	if configured == "" {
		return true
	}
	host := hostOnly(r.Host)
	if c, err := url.Parse(configured); err == nil && strings.EqualFold(host, c.Hostname()) {
		return true
	}
	for _, k := range known {
		if k != "" && strings.EqualFold(host, hostOnly(k)) {
			return true
		}
	}
	return localOnlyHost(host)
}

// hostOnly strips the port (and IPv6 brackets) from a host[:port].
func hostOnly(hp string) string {
	if h, _, err := net.SplitHostPort(hp); err == nil {
		return h
	}
	return strings.Trim(hp, "[]")
}

// localSuffixes are name suffixes that public DNS doesn't answer, so an
// attacker can't point one at the hub from the internet.
var localSuffixes = []string{".local", ".lan", ".home", ".home.arpa", ".internal", ".localdomain", ".localhost", ".intranet", ".corp", ".private"}

// localOnlyHost reports whether host is an IP literal or a name only a local
// resolver could have answered (see sameOrigin).
func localOnlyHost(host string) bool {
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" || !strings.Contains(h, ".") {
		return true
	}
	for _, s := range localSuffixes {
		if strings.HasSuffix(h, s) {
			return true
		}
	}
	return false
}

// need authenticates the caller, checks their role, then the route's host
// scope (see scope.go), before running fn.
func (s *Server) need(role string, sc scope, fn http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authn := s.authn
		if authn == nil {
			authn = s.Auth.Authenticate
		}
		u, tok, err := authn(r)
		switch {
		case errors.Is(err, auth.ErrRateLimited):
			jsonErr(w, http.StatusTooManyRequests, err.Error())
			return
		case errors.Is(err, auth.ErrBadToken):
			jsonErr(w, http.StatusUnauthorized, err.Error())
			return
		case err != nil:
			jsonErr(w, http.StatusUnauthorized, "not signed in")
			return
		}
		if roleRank[u.Role] < roleRank[role] {
			jsonErr(w, http.StatusForbidden, "your role can't do that")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, u)
		ctx = context.WithValue(ctx, accessKey{}, &access{role: role})
		if tok != nil {
			ctx = auth.WithToken(ctx, tok)
		}
		r, ok := s.authorize(w, r.WithContext(ctx), sc)
		if !ok {
			return
		}
		fn(w, r)
	})
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 || n > 1000 {
		n = 200
	}
	entries, err := s.aus().ListAudit(r.Context(), n, userFrom(r)) // only their hosts' entries for a scoped admin
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(entries))
}

// writeAudit saves an entry made outside a request (background jobs,
// automatic updates).
func (s *Server) writeAudit(ctx context.Context, e store.AuditEntry) {
	if s.audited != nil {
		s.audited(e)
		return
	}
	if err := s.Store.Audit(context.WithoutCancel(ctx), e); err != nil {
		s.Log.Warn("audit write failed", "err", err)
	}
}

func (s *Server) record(r *http.Request, u *store.User, action, target string, detail any) {
	e := auditEntry(r, u, action, target, detail)
	e.IP = s.Auth.ClientIP(r)
	e.HostIDs = accessFrom(r).hosts
	if s.audited != nil {
		s.audited(e)
		return
	}
	if err := s.Store.Audit(context.WithoutCancel(r.Context()), e); err != nil {
		s.Log.Warn("audit write failed", "err", err)
	}
}

// auditEntry builds an audit entry (without the IP). The actor is always
// the username; when the request came with an API token, the detail also
// names that token ({"token": "<name>"}), so the log shows which script
// acted.
func auditEntry(r *http.Request, u *store.User, action, target string, detail any) store.AuditEntry {
	e := store.AuditEntry{Action: action, Target: target, Actor: "anonymous"}
	if u != nil {
		id := u.ID
		e.UserID = &id
		e.Actor = u.Username
	}
	if detail != nil {
		e.Detail, _ = json.Marshal(detail)
	}
	if t := auth.TokenFrom(r.Context()); t != nil {
		e.Detail = withToken(e.Detail, t.Name)
	}
	return e
}

// withToken adds "token": name to a JSON detail object. A detail that isn't
// an object (none is today) is kept under "detail".
func withToken(detail json.RawMessage, name string) json.RawMessage {
	m := map[string]any{}
	if len(detail) > 0 && string(detail) != "null" {
		if err := json.Unmarshal(detail, &m); err != nil {
			m = map[string]any{"detail": detail}
		}
	}
	m["token"] = name
	out, _ := json.Marshal(m)
	return out
}

// ---------------- auth endpoints ----------------

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	need, err := s.Auth.SetupRequired(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"setupRequired": need, "version": version.Version, "passkeys": s.Auth.PasskeysEnabled(), "publicURL": s.publicURLString()})
}

func (s *Server) setupBegin(w http.ResponseWriter, r *http.Request) {
	var in auth.SetupBegin
	if !readJSON(w, r, &in) {
		return
	}
	opts, err := s.Auth.BeginSetup(w, r, in)
	if errors.Is(err, auth.ErrRateLimited) {
		jsonErr(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

func (s *Server) setupFinish(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	u, codes, err := s.Auth.FinishSetup(w, r)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.record(r, &u, "setup.owner_created", u.Username, nil)
	writeJSON(w, http.StatusOK, map[string]any{"user": u, "recoveryCodes": codes})
}

func (s *Server) loginBegin(w http.ResponseWriter, r *http.Request) {
	opts, err := s.Auth.BeginLogin(w, r)
	if errors.Is(err, auth.ErrRateLimited) {
		jsonErr(w, http.StatusTooManyRequests, err.Error()) // not audited: avoids log flooding
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

func (s *Server) loginFinish(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	u, err := s.Auth.FinishLogin(w, r)
	if err != nil {
		if errors.Is(err, auth.ErrRateLimited) {
			jsonErr(w, http.StatusTooManyRequests, err.Error()) // not audited: avoids log flooding
			return
		}
		s.record(r, nil, "login.failed", "", map[string]string{"method": "passkey", "error": err.Error()})
		jsonErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	s.record(r, &u, "login", u.Username, map[string]string{"method": "passkey"})
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

func (s *Server) setupPassword(w http.ResponseWriter, r *http.Request) {
	var in auth.SetupPassword
	if !readJSON(w, r, &in) {
		return
	}
	u, codes, err := s.Auth.SetupWithPassword(w, r, in)
	if errors.Is(err, auth.ErrRateLimited) {
		jsonErr(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.record(r, &u, "setup.owner_created", u.Username, map[string]string{"method": "password"})
	writeJSON(w, http.StatusOK, map[string]any{"user": u, "recoveryCodes": codes})
}

func (s *Server) loginPassword(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Password string }
	if !readJSON(w, r, &in) {
		return
	}
	u, err := s.Auth.LoginWithPassword(w, r, in.Username, in.Password)
	if err != nil {
		if errors.Is(err, auth.ErrRateLimited) {
			jsonErr(w, http.StatusTooManyRequests, err.Error())
			return
		}
		if errors.Is(err, auth.ErrPasskeyRequired) {
			s.record(r, nil, "login.failed", truncate(in.Username, 64), map[string]string{"method": "password", "reason": "passkey_required"})
			jsonErr(w, http.StatusForbidden, err.Error())
			return
		}
		if !errors.Is(err, auth.ErrBadPassword) {
			s.fail(w, err)
			return
		}
		s.record(r, nil, "login.failed", truncate(in.Username, 64), map[string]string{"method": "password"})
		jsonErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	s.record(r, &u, "login", u.Username, map[string]string{"method": "password"})
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	if !s.notViaToken(w, r) {
		return
	}
	var in struct{ Current, New string }
	if !readJSON(w, r, &in) {
		return
	}
	u := userFrom(r)
	err := s.Auth.ChangePassword(r, u, in.Current, in.New)
	switch {
	case errors.Is(err, auth.ErrReauth):
		reauthErr(w)
		return
	case errors.Is(err, auth.ErrTokenAuth):
		jsonErr(w, http.StatusForbidden, err.Error())
		return
	case errors.Is(err, auth.ErrRateLimited):
		jsonErr(w, http.StatusTooManyRequests, err.Error())
		return
	case errors.Is(err, auth.ErrUnauthorized):
		jsonErr(w, http.StatusUnauthorized, "not signed in")
		return
	case err != nil:
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	action := "password.changed"
	if u.PasswordHash == "" {
		action = "password.set"
	}
	s.record(r, &u, action, u.Username, map[string]string{"otherSessions": "signed out"})
	w.WriteHeader(http.StatusNoContent)
}

// signOutOthers ends every session of the caller except this one.
func (s *Server) signOutOthers(w http.ResponseWriter, r *http.Request) {
	if !s.notViaToken(w, r) {
		return
	}
	u := userFrom(r)
	n, err := s.Auth.SignOutOtherSessions(r, u)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.record(r, &u, "sessions.signed_out_others", u.Username, map[string]string{"sessions": strconv.FormatInt(n, 10)})
	writeJSON(w, http.StatusOK, map[string]any{"signedOut": n})
}

// reauthErr tells the browser this change needs a recent sign-in; the
// "reauth" flag lets the UI offer to sign in again.
func reauthErr(w http.ResponseWriter) {
	writeJSON(w, http.StatusForbidden, map[string]any{"error": auth.ErrReauth.Error(), "reauth": true})
}

// fresh checks that the caller signed in recently, answering 403 if not.
// Used for changes that would give a stolen session lasting access.
// API tokens are refused outright: they are long-lived by design, and must
// not be able to mint more access (invites, roles, passkeys, more tokens).
func (s *Server) fresh(w http.ResponseWriter, r *http.Request) bool {
	if !s.notViaToken(w, r) {
		return false
	}
	check := s.freshCheck
	if check == nil {
		check = s.Auth.RequireFreshSignIn
	}
	err := check(r)
	switch {
	case err == nil:
		return true
	case errors.Is(err, auth.ErrReauth):
		reauthErr(w)
	case errors.Is(err, auth.ErrTokenAuth):
		jsonErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, auth.ErrUnauthorized):
		jsonErr(w, http.StatusUnauthorized, "not signed in")
	default:
		s.fail(w, err)
	}
	return false
}

func (s *Server) loginRecovery(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Code string }
	if !readJSON(w, r, &in) {
		return
	}
	u, err := s.Auth.LoginWithRecoveryCode(w, r, in.Username, in.Code)
	if err != nil {
		if errors.Is(err, auth.ErrRateLimited) {
			jsonErr(w, http.StatusTooManyRequests, err.Error())
			return
		}
		s.record(r, nil, "login.failed", truncate(in.Username, 64), map[string]string{"method": "recovery_code"})
		jsonErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	s.record(r, &u, "login", u.Username, map[string]string{"method": "recovery_code"})
	left, _ := s.Store.CountRecoveryCodes(r.Context(), u.ID)
	writeJSON(w, http.StatusOK, map[string]any{"user": u, "recoveryCodesLeft": left})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.Auth.Logout(w, r)
	w.WriteHeader(http.StatusNoContent)
}

type passkeyView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	pks, err := s.Store.ListPasskeys(r.Context(), u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	views := make([]passkeyView, 0, len(pks))
	for _, p := range pks {
		views = append(views, passkeyView{ID: base64.RawURLEncoding.EncodeToString(p.ID), Name: p.Name, CreatedAt: p.CreatedAt, LastUsedAt: p.LastUsedAt})
	}
	left, _ := s.Store.CountRecoveryCodes(r.Context(), u.ID)
	writeJSON(w, http.StatusOK, map[string]any{"user": u, "passkeys": views, "recoveryCodesLeft": left, "hasPassword": u.PasswordHash != "", "passkeysEnabled": s.Auth.PasskeysEnabled(), "publicURL": s.publicURLString()})
}

func (s *Server) addPasskeyBegin(w http.ResponseWriter, r *http.Request) {
	if !s.fresh(w, r) {
		return
	}
	var in struct{ Name string }
	if !readJSON(w, r, &in) {
		return
	}
	opts, err := s.Auth.BeginAddPasskey(w, r, userFrom(r), in.Name)
	if errors.Is(err, auth.ErrRateLimited) {
		jsonErr(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

func (s *Server) addPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if !s.fresh(w, r) {
		return
	}
	u := userFrom(r)
	if err := s.Auth.FinishAddPasskey(w, r, u); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.record(r, &u, "passkey.added", u.Username, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deletePasskey(w http.ResponseWriter, r *http.Request) {
	if !s.fresh(w, r) {
		return
	}
	u := userFrom(r)
	id, err := base64.RawURLEncoding.DecodeString(r.PathValue("id"))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.Store.DeletePasskey(r.Context(), u.ID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			jsonErr(w, http.StatusBadRequest, "passkey not found, or it is your only way to sign in (set a password first)")
			return
		}
		s.fail(w, err)
		return
	}
	s.record(r, &u, "passkey.removed", u.Username, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) regenRecovery(w http.ResponseWriter, r *http.Request) {
	if !s.fresh(w, r) {
		return
	}
	u := userFrom(r)
	codes, err := s.Auth.RegenerateRecoveryCodes(r.Context(), u)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.record(r, &u, "recovery_codes.regenerated", u.Username, nil)
	writeJSON(w, http.StatusOK, map[string]any{"recoveryCodes": codes})
}

// ---------------- hosts ----------------

type hostView struct {
	store.Host
	Online     bool        `json:"online"`
	Address    string      `json:"address,omitempty"` // IP its agent connects from
	HubHost    bool        `json:"hubHost,omitempty"` // also runs the hub (its agent connects internally)
	Hello      *wire.Hello `json:"hello,omitempty"`
	Containers int         `json:"containers"`
	Running    int         `json:"running"`
	Unhealthy  int         `json:"unhealthy"`
	SnapshotAt *time.Time  `json:"snapshotAt,omitempty"`
	// Load is the host's recent CPU, memory and disk use (nil from older
	// agents, and while the host is offline).
	Load *registry.HostLoad `json:"load,omitempty"`
	// ImagesCheckedAt is when the hub last had this host check its images
	// for updates (scheduled or "Check for updates"); unset until it has.
	ImagesCheckedAt *time.Time `json:"imagesCheckedAt,omitempty"`
}

func (s *Server) hosts(w http.ResponseWriter, r *http.Request) {
	out, err := s.hostViews(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, visible(userFrom(r), out, func(h hostView) string { return h.ID }))
}

func (s *Server) hostViews(ctx context.Context) ([]hostView, error) {
	hs, err := s.aus().ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	return s.hostViewsFrom(hs), nil
}

// hostViewsFrom joins host rows with their agents' live state.
func (s *Server) hostViewsFrom(hs []store.Host) []hostView {
	out := make([]hostView, 0, len(hs))
	local := s.localHostID()
	for _, h := range hs {
		v := hostView{Host: h, HubHost: h.ID == local}
		if a := s.Reg.Get(h.ID); a != nil {
			st := a.Status()
			v.Online = true
			v.Address = a.RemoteIP
			v.Hello = st.Hello
			v.Containers = len(st.Containers)
			v.Load = s.Reg.Load(h.ID)
			if t := s.Reg.ImagesCheckedAt(h.ID); !t.IsZero() {
				v.ImagesCheckedAt = &t
			}
			if !st.SnapshotAt.IsZero() {
				t := st.SnapshotAt
				v.SnapshotAt = &t
			}
			for _, c := range st.Containers {
				if c.State == "running" {
					v.Running++
				}
				if strings.Contains(c.Status, "(unhealthy)") || c.State == "restarting" {
					v.Unhealthy++
				}
			}
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) createHost(w http.ResponseWriter, r *http.Request) {
	// A join token enrolls a machine that stays trusted: needs a fresh sign-in.
	if !s.fresh(w, r) {
		return
	}
	var in struct{ Name, AgentAddr string }
	if !readJSON(w, r, &in) {
		return
	}
	addr, err := s.agentAddrFor(r, in.AgentAddr)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 64 {
		jsonErr(w, http.StatusBadRequest, "name is required (max 64 characters)")
		return
	}
	h, err := s.Store.CreateHost(r.Context(), in.Name)
	if errors.Is(err, store.ErrConflict) {
		jsonErr(w, http.StatusConflict, "a host with that name already exists")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.HostsChanged()
	s.issueToken(w, r, h, addr)
}

func (s *Server) newHostToken(w http.ResponseWriter, r *http.Request) {
	var in struct{ AgentAddr string }
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in) // body is optional
	id := r.PathValue("id")
	if !validUUID(id) { // a malformed id would fail the ::uuid cast with a 500
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	// A join token enrolls a machine that stays trusted: needs a fresh sign-in.
	if !s.fresh(w, r) {
		return
	}
	addr, err := s.agentAddrFor(r, in.AgentAddr)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	h, err := s.Store.GetHost(r.Context(), id)
	if err != nil {
		s.notFoundOr(w, err)
		return
	}
	s.issueToken(w, r, h, addr)
}

// settingAgentAddr remembers the last hub address for agents an admin used,
// so the next Add host (and New join token) suggests it.
const settingAgentAddr = "agent_addr"

// agentAddrFor picks the address new agents should dial: the admin's choice,
// else TAINERHUB_AGENT_ADDR, else the last address used, else the host this
// request reached the hub on with port 7443. That last guess is right for a
// plain install opened by IP, and wrong when the page came through a reverse
// proxy: agents connect with mutual TLS on 7443 and must reach the hub itself,
// so an address that names the proxy is refused with an explanation.
func (s *Server) agentAddrFor(r *http.Request, requested string) (string, error) {
	addr := strings.TrimSpace(requested)
	if addr == "" {
		addr = s.suggestAgentAddr(r)
	}
	if addr == "" {
		return "", errors.New("enter the hub address for agents: the hub server's LAN IP or name with port 7443, e.g. 192.168.1.10:7443. This page came through your reverse proxy, and agents must reach the hub directly")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || port == "" || strings.ContainsAny(addr, " /\\\"'\n") {
		return "", errors.New("hub address for agents must look like 192.168.1.10:7443 or hub.lan:7443")
	}
	if msg := s.proxyAddrProblem(host); msg != "" {
		return "", errors.New(msg)
	}
	return addr, nil
}

// suggestAgentAddr is the default hub address for agents, or "" when the
// only guess would be the reverse proxy.
func (s *Server) suggestAgentAddr(r *http.Request) string {
	stored := ""
	if s.AgentAddr == "" {
		stored, _ = s.Store.Setting(r.Context(), settingAgentAddr)
	}
	return s.guessAgentAddr(r.Host, stored)
}

func (s *Server) guessAgentAddr(reqHost, stored string) string {
	if s.AgentAddr != "" {
		return s.AgentAddr
	}
	if stored != "" {
		if h, _, err := net.SplitHostPort(stored); err == nil && s.proxyAddrProblem(h) == "" {
			return stored
		}
	}
	host := reqHost
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" || s.proxyAddrProblem(host) != "" {
		return ""
	}
	return net.JoinHostPort(host, "7443")
}

// proxyAddrProblem explains why host can't be the hub address for agents:
// it is the HTTPS name your reverse proxy serves, or the proxy's own IP from
// the trusted proxies. Returns "" when host may be the hub.
func (s *Server) proxyAddrProblem(host string) string {
	host = strings.Trim(host, "[]")
	if u := s.Auth.PublicURL(); u != nil && strings.EqualFold(host, u.Hostname()) {
		return fmt.Sprintf("%s is the address your reverse proxy serves (TainerHub's public URL), and agents can't connect through a proxy (they use mutual TLS on port 7443). Enter the hub server's own LAN IP or name instead, e.g. 192.168.1.10:7443", host)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		for _, p := range s.Auth.Trusted() {
			if p.IsSingleIP() && p.Addr().Unmap() == ip.Unmap() {
				return fmt.Sprintf("%s is your reverse proxy (it's in the trusted proxies), and agents can't connect through a proxy (they use mutual TLS on port 7443). Enter the hub server's own LAN IP or name instead", host)
			}
		}
	}
	return ""
}

// issueToken creates a one-time join token (valid 1 hour) and returns it with
// a ready-to-paste compose file for the agent.
func (s *Server) issueToken(w http.ResponseWriter, r *http.Request, h store.Host, agentAddr string) {
	tok := wire.JoinToken{ID: randID(), Secret: randID() + randID(), CAFingerprint: s.CA.Fingerprint()}
	expires := time.Now().Add(time.Hour)
	if err := s.Store.CreateJoinToken(r.Context(), tok.ID, h.ID, gateway.HashSecret(tok.Secret), expires); err != nil {
		s.fail(w, err)
		return
	}
	if s.AgentAddr == "" { // remember it for the next host; best effort
		_ = s.Store.SetSetting(r.Context(), settingAgentAddr, agentAddr)
	}
	u := userFrom(r)
	s.record(r, &u, "host.join_token_issued", h.Name, nil)
	encoded := tok.Encode()
	writeJSON(w, http.StatusOK, map[string]any{
		"host":      h,
		"joinToken": encoded,
		"expiresAt": expires,
		"agentAddr": agentAddr,
		"directURL": directHubURL(agentAddr, s.HTTPS.HTTPPort),
		"compose":   agentCompose(agentAddr, encoded),
	})
}

// directHubURL is the hub's own web address on the host agents dial, e.g.
// http://192.168.1.10:8585. The Add host dialog offers it for downloading
// the installer when the browser reaches the hub through a reverse proxy
// whose access list may not admit the new server (a proxy answers 403).
func directHubURL(agentAddr, httpPort string) string {
	host, _, err := net.SplitHostPort(agentAddr)
	if err != nil || host == "" {
		return ""
	}
	if httpPort == "" {
		httpPort = "8585"
	}
	return "http://" + net.JoinHostPort(host, httpPort)
}

func agentCompose(hub, token string) string {
	return fmt.Sprintf(`services:
  tainerhub-agent:
    image: ghcr.io/prezmc/tainerhub-agent:latest
    container_name: tainerhub-agent
    restart: unless-stopped
    read_only: true
    security_opt: ["no-new-privileges:true"]
    cap_drop: [ALL]
    healthcheck:
      test: ["CMD", "/tainerhub-agent", "healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - tainerhub-agent:/data
      # Compose stacks. Keep the same path on both sides (relative bind mounts).
      - /opt/tainerhub/stacks:/opt/tainerhub/stacks
      # Read-only: lets the agent read container CPU and memory from the kernel,
      # which costs the server far less than asking Docker. Optional.
      - /sys/fs/cgroup:/host/sys/fs/cgroup:ro
    environment:
      TAINERHUB_HUB: %s
      # One-time token, valid for 1 hour. It can be removed after the first start.
      TAINERHUB_JOIN_TOKEN: %s
volumes:
  tainerhub-agent:
`, hub, token)
}

func (s *Server) renameHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUUID(id) {
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	var in struct{ Name string }
	if !readJSON(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 64 {
		jsonErr(w, http.StatusBadRequest, "name is required (max 64 characters)")
		return
	}
	h, err := s.Store.GetHost(r.Context(), id)
	if err != nil {
		s.notFoundOr(w, err)
		return
	}
	if h.Name == in.Name {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.Store.RenameHost(r.Context(), id, in.Name); err != nil {
		if errors.Is(err, store.ErrConflict) {
			jsonErr(w, http.StatusConflict, "a host with that name already exists")
			return
		}
		s.notFoundOr(w, err)
		return
	}
	s.HostsChanged()
	// The live connection carries the name it connected under; dropping it
	// makes the agent reconnect (within seconds) under the new one.
	s.Reg.Disconnect(id)
	u := userFrom(r)
	s.record(r, &u, "host.renamed", in.Name, map[string]string{"from": h.Name})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUUID(id) {
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	h, err := s.Store.GetHost(r.Context(), id)
	if err != nil {
		s.notFoundOr(w, err)
		return
	}
	if err := s.Store.DeleteHost(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	s.HostsChanged()
	s.Reg.Disconnect(id) // its certificate is no longer accepted from now on
	u := userFrom(r)
	s.record(r, &u, "host.removed", h.Name, nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------- containers ----------------

type containerView struct {
	wire.Container
	HostID   string `json:"hostId"`
	HostName string `json:"hostName"`
}

func (s *Server) containers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, visible(userFrom(r), s.containerViews(), func(c containerView) string { return c.HostID }))
}

func (s *Server) containerViews() []containerView {
	out := []containerView{}
	for _, a := range s.Reg.All() {
		for _, c := range a.Status().Containers {
			c.Labels = nil // keep the fleet payload small
			out = append(out, containerView{Container: c, HostID: a.HostID, HostName: a.Name})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].HostName != out[j].HostName {
			return out[i].HostName < out[j].HostName
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ---------------- resources ----------------

// runOp sends an operation to the host's agent and writes the agent's JSON
// result. Mutating operations are audited.
func (s *Server) runOp(w http.ResponseWriter, r *http.Request, op wire.Operation, target string) {
	a := s.Reg.Get(r.PathValue("id"))
	if a == nil {
		jsonErr(w, http.StatusServiceUnavailable, "host is offline")
		return
	}
	if err := wire.ValidOperation(op); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	data, err := a.Operate(r.Context(), op)
	if wire.OpKinds[op.Kind] {
		u := userFrom(r)
		detail := map[string]string{"host": a.Name}
		if op.Arg != "" {
			detail["arg"] = op.Arg
		}
		if err != nil {
			detail["error"] = err.Error()
		}
		s.record(r, &u, op.Kind, a.Name+"/"+target, detail)
	}
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if len(data) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (s *Server) listResources(kind string) http.HandlerFunc {
	opKind := map[string]string{"images": wire.OpImagesList, "volumes": wire.OpVolumesList, "networks": wire.OpNetworksList}[kind]
	return func(w http.ResponseWriter, r *http.Request) { s.runOp(w, r, wire.Operation{Kind: opKind}, kind) }
}

func (s *Server) removeResource(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid := r.PathValue("rid")
		op := wire.Operation{Kind: kind, Target: rid}
		// An image kept for a stack's rollback is refused by the agent;
		// ?force=1 removes it anyway, and only an admin may (the stack can
		// no longer roll back to it).
		if kind == wire.OpImageRemove && r.URL.Query().Get("force") == "1" {
			if roleRank[userFrom(r).Role] < roleRank["admin"] {
				jsonErr(w, http.StatusForbidden, "only an admin can remove an image kept for a stack's rollback")
				return
			}
			op.Arg = wire.ImageRemoveForce
		}
		s.runOp(w, r, op, rid)
	}
}

func (s *Server) prune(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		arg := ""
		if r.URL.Query().Get("all") == "1" {
			arg = "all"
		}
		s.runOp(w, r, wire.Operation{Kind: kind, Arg: arg}, "unused")
	}
}

func (s *Server) pullImage(w http.ResponseWriter, r *http.Request) {
	var in struct{ Ref string }
	if !readJSON(w, r, &in) {
		return
	}
	in.Ref = strings.TrimSpace(in.Ref)
	s.runOp(w, r, wire.Operation{Kind: wire.OpImagePull, Arg: in.Ref}, in.Ref)
}

// inspect returns container details. Environment values, label values and
// command arguments can hold secrets, so only admins receive them; everyone
// else gets the names (see redactForViewer).
func (s *Server) inspect(w http.ResponseWriter, r *http.Request) {
	a := s.Reg.Get(r.PathValue("id"))
	if a == nil {
		jsonErr(w, http.StatusServiceUnavailable, "host is offline")
		return
	}
	cid := r.PathValue("cid")
	data, err := a.Operate(r.Context(), wire.Operation{Kind: wire.OpContainerInspect, Target: cid})
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	var d wire.ContainerDetail
	if err := json.Unmarshal(data, &d); err != nil {
		s.fail(w, err)
		return
	}
	reveal := roleRank[userFrom(r).Role] >= roleRank["admin"]
	if !reveal {
		redactForViewer(&d)
	}
	// envRevealed covers labels and command arguments too; the name stays for
	// older UIs.
	writeJSON(w, http.StatusOK, map[string]any{"detail": d, "envRevealed": reveal, "hostId": a.HostID, "hostName": a.Name})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.Store.Ping(ctx); err != nil {
		jsonErr(w, http.StatusServiceUnavailable, "database unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "agents": len(s.Reg.All())})
}

// events pushes the whole fleet (hosts + containers) over Server-Sent Events
// whenever anything changes, so the UI updates instantly without polling.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()
	me := userFrom(r)
	defer s.Reg.Watch()() // agents sample often only while someone is watching
	// One shared producer builds each update; this stream just copies it,
	// or, for a user limited to some hosts, sends the part about those.
	feed := s.fleetFeed()
	defer feed.subscribe()()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	// Re-check the session periodically so a revoked session stops streaming.
	recheck := time.NewTicker(time.Minute)
	defer recheck.Stop()
	for {
		frame, payload, next := feed.current()
		if frame != nil && me.Scoped {
			frame = scopedFleetFrame(me, payload)
		}
		if frame != nil {
			if _, err := w.Write(frame); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	wait:
		for {
			select {
			case <-ctx.Done():
				return
			case <-keepalive.C:
				fmt.Fprint(w, ": keepalive\n\n")
				if err := rc.Flush(); err != nil {
					return
				}
			case <-recheck.C:
				if !s.stillAllowed(r, me, "viewer") {
					return
				}
			case <-next:
				break wait
			}
		}
	}
}

func (s *Server) containerAction(w http.ResponseWriter, r *http.Request) {
	hostID, cid, action := r.PathValue("id"), r.PathValue("cid"), r.PathValue("action")
	if !wire.ValidAction(action) || !wire.ValidContainerRef(cid) {
		jsonErr(w, http.StatusBadRequest, "unknown action or container")
		return
	}
	a := s.Reg.Get(hostID)
	if a == nil {
		jsonErr(w, http.StatusServiceUnavailable, "host is offline")
		return
	}
	name := cid
	for _, c := range a.Status().Containers {
		if c.ID == cid {
			name = c.Name
		}
	}
	u := userFrom(r)
	err := a.Do(r.Context(), wire.ContainerAction{ContainerID: cid, Action: action})
	detail := map[string]string{"host": a.Name, "container": name}
	if err != nil {
		detail["error"] = err.Error()
	}
	s.record(r, &u, "container."+action, a.Name+"/"+name, detail)
	if err == nil {
		s.Reg.AddActivity(registry.Activity{Kind: "user", Action: action, HostID: a.HostID, Host: a.Name, Container: name, Actor: u.Username})
	}
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeWithVolumes removes a container plus its anonymous volumes and the
// named volumes the admin picked (deleted only if nothing else uses them).
// Volumes hold data, so this is admin-only like the other volume deletes.
func (s *Server) removeWithVolumes(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	var in struct {
		Volumes []string `json:"volumes"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	op := wire.Operation{Kind: wire.OpContainerRemoveVolumes, Target: cid, Arg: strings.Join(in.Volumes, ",")}
	if err := wire.ValidOperation(op); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a := s.Reg.Get(r.PathValue("id"))
	if a == nil {
		jsonErr(w, http.StatusServiceUnavailable, "host is offline")
		return
	}
	name := cid
	for _, c := range a.Status().Containers {
		if c.ID == cid {
			name = c.Name
		}
	}
	data, err := a.Operate(r.Context(), op)
	var res wire.RemoveVolumesResult
	if err == nil {
		_ = json.Unmarshal(data, &res)
	}
	u := userFrom(r)
	detail := map[string]any{"host": a.Name, "container": name, "volumes_removed": res.Removed, "anonymous_volumes_removed": res.Anonymous}
	if len(res.Kept) > 0 {
		detail["volumes_kept"] = res.Kept
	}
	if err != nil {
		detail["error"] = err.Error()
	}
	s.record(r, &u, "container.remove_with_volumes", a.Name+"/"+name, detail)
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.Reg.AddActivity(registry.Activity{Kind: "user", Action: "remove", HostID: a.HostID, Host: a.Name, Container: name, Actor: u.Username})
	writeJSON(w, http.StatusOK, res)
}

// containerName is a container's name on a connected host, else its ID.
func (s *Server) containerName(hostID, cid string) string {
	if a := s.Reg.Get(hostID); a != nil {
		for _, c := range a.Status().Containers {
			if c.ID == cid {
				return c.Name
			}
		}
	}
	return cid
}

// logs streams container logs as Server-Sent Events (one "log" event per chunk).
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	if !wire.ValidContainerRef(r.PathValue("cid")) {
		jsonErr(w, http.StatusBadRequest, "invalid container")
		return
	}
	a := s.Reg.Get(r.PathValue("id"))
	if a == nil {
		jsonErr(w, http.StatusServiceUnavailable, "host is offline")
		return
	}
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	if tail <= 0 || tail > 10000 {
		tail = 300
	}
	follow := r.URL.Query().Get("follow") != "0"
	u := userFrom(r)
	if !s.logStreams.acquire(u.ID, a.HostID) {
		jsonErr(w, http.StatusTooManyRequests, fmt.Sprintf("you have too many log streams open (at most %d per host, %d in all); close some and retry", maxLogStreamsPerHost, maxLogStreamsPerUser))
		return
	}
	defer s.logStreams.release(u.ID, a.HostID)
	ch, stop, err := a.Logs(r.Context(), wire.LogsRequest{ContainerID: r.PathValue("cid"), Tail: tail, Follow: follow})
	if errors.Is(err, registry.ErrTooManyLogs) {
		jsonErr(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		jsonErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer stop()

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // tell nginx (NPM) not to buffer
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
		case c, ok := <-ch:
			if !ok {
				fmt.Fprint(w, "event: end\ndata: {}\n\n")
				_ = rc.Flush()
				return
			}
			if len(c.Data) > 0 {
				b, _ := json.Marshal(string(c.Data))
				fmt.Fprintf(w, "event: log\ndata: %s\n\n", b)
			}
			if c.Error != "" {
				b, _ := json.Marshal(c.Error)
				fmt.Fprintf(w, "event: error\ndata: %s\n\n", b)
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

// ---------------- web UI ----------------

func (s *Server) spa() http.Handler {
	files := http.FileServerFS(s.Web)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" {
			if f, err := s.Web.Open(p); err == nil {
				st, _ := f.Stat()
				f.Close()
				if st != nil && !st.IsDir() {
					if strings.HasPrefix(p, "assets/") { // hashed file names
						w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
						if s.servePrecompressed(w, r, p) {
							return
						}
					}
					files.ServeHTTP(w, r)
					return
				}
			}
		}
		index, err := fs.ReadFile(s.Web, "index.html")
		if err != nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "TainerHub API is running, but the web UI was not built into this binary (run: make web).")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
}

// servePrecompressed serves the .br or .gz sibling written at build time.
func (s *Server) servePrecompressed(w http.ResponseWriter, r *http.Request, p string) bool {
	ae := r.Header.Get("Accept-Encoding")
	for _, enc := range []struct{ name, ext string }{{"br", ".br"}, {"gzip", ".gz"}} {
		if !strings.Contains(ae, enc.name) {
			continue
		}
		b, err := fs.ReadFile(s.Web, p+enc.ext)
		if err != nil {
			continue
		}
		if ct := mime.TypeByExtension(path.Ext(p)); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Content-Encoding", enc.name)
		w.Header().Add("Vary", "Accept-Encoding")
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		if r.Method != http.MethodHead {
			_, _ = w.Write(b)
		}
		return true
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---------------- helpers ----------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.Log.Error("request failed", "err", err)
	jsonErr(w, http.StatusInternalServerError, "internal error")
}

func (s *Server) notFoundOr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	s.fail(w, err)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
