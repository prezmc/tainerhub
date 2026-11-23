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
	execs        execList          // open terminals
	https        httpsState        // HTTPS setup tests
	statsMu      sync.Mutex        // serializes stats source changes (one settings row)
	fleetOnce    sync.Once
	fleet        *fleetFeed // shared /api/events producer (see fleet.go)
}

var roleRank = map[string]int{"viewer": 1, "operator": 2, "admin": 3, "owner": 4}

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

	// any signed-in user
	mux.Handle("GET /api/me", s.need("viewer", s.me))
	mux.Handle("POST /api/me/passkeys/begin", s.need("viewer", s.addPasskeyBegin))
	mux.Handle("POST /api/me/passkeys/finish", s.need("viewer", s.addPasskeyFinish))
	mux.Handle("DELETE /api/me/passkeys/{id}", s.need("viewer", s.deletePasskey))
	mux.Handle("POST /api/me/recovery-codes", s.need("viewer", s.regenRecovery))
	mux.Handle("POST /api/me/password", s.need("viewer", s.changePassword))
	mux.Handle("POST /api/me/sessions/sign-out-others", s.need("viewer", s.signOutOthers))
	mux.Handle("GET /api/containers", s.need("viewer", s.containers))
	mux.Handle("GET /api/events", s.need("viewer", s.events))
	mux.Handle("GET /api/hosts", s.need("viewer", s.hosts))
	mux.Handle("GET /api/placement", s.need("viewer", s.placement))
	mux.Handle("GET /api/hosts/{id}/containers/{cid}/logs", s.need("viewer", s.logs))

	mux.Handle("GET /api/hosts/{id}/containers/{cid}", s.need("viewer", s.inspect))
	for _, kind := range []string{"images", "volumes", "networks"} {
		mux.Handle("GET /api/hosts/{id}/"+kind, s.need("viewer", s.listResources(kind)))
	}
	mux.Handle("GET /api/stacks", s.need("viewer", s.listStacks))
	mux.Handle("GET /api/stacks/{sid}", s.need("viewer", s.getStack))
	mux.Handle("GET /api/jobs/{jid}", s.need("viewer", s.jobStream))

	// operators
	mux.Handle("POST /api/hosts/{id}/containers/{cid}/{action}", s.need("operator", s.containerAction))
	mux.Handle("POST /api/hosts/{id}/images/pull", s.need("operator", s.pullImage))
	// image updates: check now, and pull & recreate a standalone container
	mux.Handle("POST /api/hosts/{id}/check-updates", s.need("operator", func(w http.ResponseWriter, r *http.Request) {
		s.runOp(w, r, wire.Operation{Kind: wire.OpImagesCheckUpdates}, "images")
	}))
	mux.Handle("POST /api/hosts/{id}/containers/{cid}/recreate", s.need("operator", func(w http.ResponseWriter, r *http.Request) {
		cid := r.PathValue("cid")
		s.runOp(w, r, wire.Operation{Kind: wire.OpContainerRecreate, Target: cid}, s.containerName(r.PathValue("id"), cid))
	}))
	mux.Handle("DELETE /api/hosts/{id}/images/{rid}", s.need("operator", s.removeResource(wire.OpImageRemove)))
	mux.Handle("DELETE /api/hosts/{id}/networks/{rid}", s.need("operator", s.removeResource(wire.OpNetworkRemove)))
	mux.Handle("POST /api/hosts/{id}/images/prune", s.need("operator", s.prune(wire.OpImagesPrune)))
	mux.Handle("POST /api/hosts/{id}/networks/prune", s.need("operator", s.prune(wire.OpNetworksPrune)))
	// volumes hold data: removing them is admin-only
	mux.Handle("DELETE /api/hosts/{id}/volumes/{rid}", s.need("admin", s.removeResource(wire.OpVolumeRemove)))
	mux.Handle("POST /api/hosts/{id}/volumes/prune", s.need("admin", s.prune(wire.OpVolumesPrune)))
	mux.Handle("POST /api/hosts/{id}/containers/{cid}/remove-with-volumes", s.need("admin", s.removeWithVolumes))
	// stack lifecycle on definitions an admin has saved
	mux.Handle("POST /api/stacks/{sid}/{action}", s.need("operator", s.stackAction))
	mux.Handle("POST /api/stacks/takeover-check", s.need("admin", s.checkTakeover))
	mux.Handle("GET /api/stacks/takeover-source", s.need("admin", s.takeoverSource))
	mux.Handle("GET /api/stacks/{sid}/move", s.need("admin", s.moveCheck))
	mux.Handle("POST /api/stacks/{sid}/move", s.need("admin", s.startMove))
	mux.Handle("DELETE /api/moves/{mid}/leftovers", s.need("admin", s.removeLeftovers))
	mux.Handle("GET /api/hosts/{id}/containers/{cid}/move", s.need("admin", s.containerMoveCheck))
	mux.Handle("POST /api/hosts/{id}/containers/{cid}/move", s.need("admin", s.startContainerMove))
	mux.Handle("GET /api/container-moves", s.need("admin", s.containerMoves))
	// writing a compose file is root on the host: admins only (see stacks.go)
	mux.Handle("POST /api/stacks", s.need("admin", s.createStack))
	// Docker Hub lookups for the "New container" page (which creates a stack)
	mux.Handle("GET /api/dockerhub/search", s.need("admin", s.hubSearch))
	mux.Handle("GET /api/dockerhub/tags", s.need("admin", s.hubTags))
	mux.Handle("PUT /api/stacks/{sid}", s.need("admin", s.updateStack))
	mux.Handle("DELETE /api/stacks/{sid}", s.need("admin", s.deleteStack))

	// admins
	mux.Handle("POST /api/hosts", s.need("admin", s.createHost))
	mux.Handle("POST /api/hosts/{id}/token", s.need("admin", s.newHostToken))
	mux.Handle("DELETE /api/hosts/{id}", s.need("admin", s.deleteHost))
	mux.Handle("PUT /api/hosts/{id}", s.need("admin", s.renameHost))
	mux.Handle("GET /api/audit", s.need("admin", s.audit))
	mux.Handle("GET /api/users", s.need("admin", s.listUsers))
	mux.Handle("POST /api/invites", s.need("admin", s.createInvite))
	mux.Handle("DELETE /api/invites/{iid}", s.need("admin", s.deleteInvite))
	mux.Handle("PUT /api/users/{uid}/role", s.need("admin", s.setRole))
	mux.Handle("DELETE /api/users/{uid}", s.need("admin", s.deleteUser))
	mux.Handle("GET /api/updates", s.need("admin", s.updates))
	mux.Handle("POST /api/updates/check", s.need("admin", s.checkUpdates))
	mux.Handle("GET /api/settings/https", s.need("admin", s.httpsSettings))
	mux.Handle("POST /api/settings/https/test", s.need("admin", s.httpsTest))
	mux.Handle("PUT /api/settings/https", s.need("admin", s.httpsEnable))
	mux.Handle("DELETE /api/settings/https", s.need("admin", s.httpsDisable))
	// terminals: admin-only (a shell sees everything in the container)
	mux.Handle("POST /api/hosts/{id}/containers/{cid}/exec", s.need("admin", s.execOpen))
	mux.Handle("GET /api/exec/{sid}", s.need("admin", s.execStream))
	mux.Handle("POST /api/exec/{sid}/input", s.need("admin", s.execInput))
	mux.Handle("POST /api/exec/{sid}/resize", s.need("admin", s.execResize))
	mux.Handle("DELETE /api/exec/{sid}", s.need("admin", s.execClose))
	mux.Handle("POST /api/hosts/{id}/update", s.need("admin", s.updateHost))
	mux.Handle("POST /api/hosts/{id}/lighter-stats", s.need("admin", s.lighterStats))
	mux.Handle("PUT /api/hosts/{id}/stats-source", s.need("admin", s.statsSource))
	// Docker Engine upgrades restart every container: admins, with explicit confirmation
	mux.Handle("POST /api/hosts/{id}/docker/plan", s.need("admin", s.dockerPlan))
	mux.Handle("POST /api/hosts/{id}/docker/upgrade", s.need("admin", s.dockerUpgrade))

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { jsonErr(w, http.StatusNotFound, "not found") })
	mux.Handle("/", s.spa())

	return s.secure(mux)
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
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			// SameSite=Strict cookies plus a custom header (which cross-site
			// forms cannot send) plus an Origin check.
			if r.Header.Get("X-TainerHub") != "1" {
				jsonErr(w, http.StatusForbidden, "missing request header")
				return
			}
			if o := r.Header.Get("Origin"); o != "" && !sameOrigin(o, origin, r) {
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
func sameOrigin(o, configured string, r *http.Request) bool {
	if configured != "" && o == configured {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

func (s *Server) need(role string, fn http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := s.Auth.UserFromRequest(r)
		if err != nil {
			jsonErr(w, http.StatusUnauthorized, "not signed in")
			return
		}
		if roleRank[u.Role] < roleRank[role] {
			jsonErr(w, http.StatusForbidden, "your role can't do that")
			return
		}
		fn(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
	})
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 || n > 1000 {
		n = 200
	}
	entries, err := s.Store.ListAudit(r.Context(), n)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(entries))
}

func (s *Server) record(r *http.Request, u *store.User, action, target string, detail any) {
	e := store.AuditEntry{Action: action, Target: target, IP: s.Auth.ClientIP(r), Actor: "anonymous"}
	if u != nil {
		id := u.ID
		e.UserID = &id
		e.Actor = u.Username
	}
	if detail != nil {
		e.Detail, _ = json.Marshal(detail)
	}
	if err := s.Store.Audit(context.WithoutCancel(r.Context()), e); err != nil {
		s.Log.Warn("audit write failed", "err", err)
	}
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
func (s *Server) fresh(w http.ResponseWriter, r *http.Request) bool {
	err := s.Auth.RequireFreshSignIn(r)
	switch {
	case err == nil:
		return true
	case errors.Is(err, auth.ErrReauth):
		reauthErr(w)
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
}

func (s *Server) hosts(w http.ResponseWriter, r *http.Request) {
	out, err := s.hostViews(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) hostViews(ctx context.Context) ([]hostView, error) {
	hs, err := s.Store.ListHosts(ctx)
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
	addr, err := s.agentAddrFor(r, in.AgentAddr)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	if !validUUID(id) { // a malformed id would fail the ::uuid cast with a 500
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	h, err := s.Store.GetHost(r.Context(), id)
	if err != nil {
		s.notFoundOr(w, err)
		return
	}
	s.issueToken(w, r, h, addr)
}

// agentAddrFor picks the address new agents should dial: the admin's choice,
// else TAINERHUB_AGENT_ADDR, else the host this request reached the hub on
// with port 7443. That last guess is right for a plain install opened by IP,
// and wrong behind a reverse proxy on another server, so the UI shows it and
// lets the admin change it.
func (s *Server) agentAddrFor(r *http.Request, requested string) (string, error) {
	addr := strings.TrimSpace(requested)
	if addr == "" {
		addr = s.AgentAddr
	}
	if addr == "" {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		addr = net.JoinHostPort(strings.Trim(host, "[]"), "7443")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || port == "" || strings.ContainsAny(addr, " /\\\"'\n") {
		return "", errors.New("hub address for agents must look like 192.168.1.10:7443 or hub.lan:7443")
	}
	return addr, nil
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
	u := userFrom(r)
	s.record(r, &u, "host.join_token_issued", h.Name, nil)
	encoded := tok.Encode()
	writeJSON(w, http.StatusOK, map[string]any{
		"host":      h,
		"joinToken": encoded,
		"expiresAt": expires,
		"agentAddr": agentAddr,
		"compose":   agentCompose(agentAddr, encoded),
	})
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
	writeJSON(w, http.StatusOK, s.containerViews())
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
		s.runOp(w, r, wire.Operation{Kind: kind, Target: rid}, rid)
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
	defer s.Reg.Watch()() // agents sample often only while someone is watching
	// One shared producer builds each update; this stream just copies it.
	feed := s.fleetFeed()
	defer feed.subscribe()()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	// Re-check the session periodically so a revoked session stops streaming.
	recheck := time.NewTicker(time.Minute)
	defer recheck.Stop()
	for {
		frame, next := feed.current()
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
				if _, err := s.Auth.UserFromRequest(r); err != nil {
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
