package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/prezmc/tainerhub/internal/regcred"
	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

// Settings → Container registries: logins for private registries (and
// Docker Hub, to lift its anonymous pull limits). Admins manage them; the
// password is encrypted at rest and never returned by any endpoint. The hub
// hands a login to an agent only inside a command that pulls an image from
// that registry (see AddRegistryAuth).

// registryCredStore is the part of the store this file uses (a fake in tests).
type registryCredStore interface {
	ListRegistryCredentials(ctx context.Context) ([]store.RegistryCredential, error)
	RegistryCredentialsFor(ctx context.Context, registries []string) ([]store.RegistryCredential, error)
	GetRegistryCredential(ctx context.Context, id string) (store.RegistryCredential, error)
	CreateRegistryCredential(ctx context.Context, registry, username string, secret []byte, createdBy string) (store.RegistryCredential, error)
	UpdateRegistryCredential(ctx context.Context, id, username string, secret []byte) (store.RegistryCredential, error)
	DeleteRegistryCredential(ctx context.Context, id string) (string, error)
	TouchRegistryCredentials(ctx context.Context, registries []string) error
}

func (s *Server) regStore() registryCredStore {
	if s.regCreds != nil {
		return s.regCreds
	}
	return s.Store
}

// regKey loads the encryption key lazily: a hub that never saves a login
// never creates one.
type regKey struct {
	mu     sync.Mutex
	sealer *regcred.Sealer
}

func (s *Server) sealer() (*regcred.Sealer, error) {
	s.regKey.mu.Lock()
	defer s.regKey.mu.Unlock()
	if s.regKey.sealer != nil {
		return s.regKey.sealer, nil
	}
	if s.DataDir == "" {
		return nil, errors.New("the hub has no data folder to keep its registry key in")
	}
	key, err := regcred.LoadOrCreateKey(filepath.Join(s.DataDir, regcred.KeyFile))
	if err != nil {
		return nil, err
	}
	sl, err := regcred.NewSealer(key)
	if err != nil {
		return nil, err
	}
	s.regKey.sealer = sl
	return sl, nil
}

// registryView is a saved login as the UI sees it: never the password.
type registryView struct {
	ID          string     `json:"id"`
	Registry    string     `json:"registry"`
	Username    string     `json:"username"`
	PasswordSet bool       `json:"passwordSet"`
	CreatedBy   string     `json:"createdBy,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	LastUsedAt  *time.Time `json:"lastUsedAt"`
}

func viewRegistry(c store.RegistryCredential) registryView {
	return registryView{ID: c.ID, Registry: c.Registry, Username: c.Username, PasswordSet: len(c.Secret) > 0,
		CreatedBy: c.CreatedBy, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, LastUsedAt: c.LastUsedAt}
}

func (s *Server) listRegistries(w http.ResponseWriter, r *http.Request) {
	cs, err := s.regStore().ListRegistryCredentials(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]registryView, 0, len(cs))
	for _, c := range cs {
		out = append(out, viewRegistry(c))
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

type registryIn struct {
	Registry string `json:"registry"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func checkRegistryUser(u string) (string, error) {
	u = strings.TrimSpace(u)
	if u == "" || len(u) > wire.MaxRegistryUser || strings.IndexFunc(u, unicode.IsControl) >= 0 {
		return "", errors.New("enter the username (max 255 characters)")
	}
	return u, nil
}

// checkRegistryPassword accepts a password or token; surrounding spaces
// are dropped (they're always a copy-paste slip).
func checkRegistryPassword(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || len(p) > wire.MaxRegistryPassword || strings.IndexFunc(p, unicode.IsControl) >= 0 {
		return "", errors.New("enter the password or access token")
	}
	return p, nil
}

func (s *Server) addRegistry(w http.ResponseWriter, r *http.Request) {
	// fresh also refuses API tokens: saving a login is a sensitive change.
	if !s.fresh(w, r) {
		return
	}
	var in registryIn
	if !readJSON(w, r, &in) {
		return
	}
	host, err := regcred.NormalizeHost(in.Registry)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	user, err := checkRegistryUser(in.Username)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	pw, err := checkRegistryPassword(in.Password)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sl, err := s.sealer()
	if err != nil {
		s.fail(w, err)
		return
	}
	secret, err := sl.Seal([]byte(pw), host)
	if err != nil {
		s.fail(w, err)
		return
	}
	me := userFrom(r)
	c, err := s.regStore().CreateRegistryCredential(r.Context(), host, user, secret, me.ID)
	if errors.Is(err, store.ErrConflict) {
		jsonErr(w, http.StatusConflict, "a login for "+host+" is already saved: edit that one instead")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.record(r, &me, "registry.added", host, map[string]string{"username": user})
	writeJSON(w, http.StatusCreated, viewRegistry(c))
}

func (s *Server) updateRegistry(w http.ResponseWriter, r *http.Request) {
	if !s.fresh(w, r) {
		return
	}
	id := r.PathValue("rid")
	if !validUUID(id) {
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	var in registryIn
	if !readJSON(w, r, &in) {
		return
	}
	cur, err := s.regStore().GetRegistryCredential(r.Context(), id)
	if err != nil {
		s.notFoundOr(w, err)
		return
	}
	user := cur.Username
	if strings.TrimSpace(in.Username) != "" {
		if user, err = checkRegistryUser(in.Username); err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	var secret []byte // nil keeps the saved password
	if in.Password != "" {
		pw, err := checkRegistryPassword(in.Password)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		sl, err := s.sealer()
		if err != nil {
			s.fail(w, err)
			return
		}
		if secret, err = sl.Seal([]byte(pw), cur.Registry); err != nil {
			s.fail(w, err)
			return
		}
	}
	c, err := s.regStore().UpdateRegistryCredential(r.Context(), id, user, secret)
	if err != nil {
		s.notFoundOr(w, err)
		return
	}
	me := userFrom(r)
	detail := map[string]string{"username": user, "password": "kept"}
	if secret != nil {
		detail["password"] = "replaced"
	}
	s.record(r, &me, "registry.updated", c.Registry, detail)
	writeJSON(w, http.StatusOK, viewRegistry(c))
}

func (s *Server) deleteRegistry(w http.ResponseWriter, r *http.Request) {
	if !s.fresh(w, r) {
		return
	}
	id := r.PathValue("rid")
	if !validUUID(id) {
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	host, err := s.regStore().DeleteRegistryCredential(r.Context(), id)
	if err != nil {
		s.notFoundOr(w, err)
		return
	}
	me := userFrom(r)
	s.record(r, &me, "registry.removed", host, nil)
	w.WriteHeader(http.StatusNoContent)
}

// testRegistry has an online agent's Docker Engine log in with the saved
// login (POST /auth), so the check sees the registry the way pulls will
// (including registries only the LAN can reach). Nothing is saved on the host.
func (s *Server) testRegistry(w http.ResponseWriter, r *http.Request) {
	if !s.notViaToken(w, r) {
		return
	}
	id := r.PathValue("rid")
	if !validUUID(id) {
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	var in struct {
		HostID string `json:"hostId"`
	}
	if r.ContentLength != 0 && !readJSON(w, r, &in) {
		return
	}
	c, err := s.regStore().GetRegistryCredential(r.Context(), id)
	if err != nil {
		s.notFoundOr(w, err)
		return
	}
	a := s.testAgent(in.HostID)
	if a == nil {
		jsonErr(w, http.StatusServiceUnavailable, "no host is online to test from")
		return
	}
	auth, err := s.openCredential(c)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := struct {
		OK     bool   `json:"ok"`
		Host   string `json:"host"`
		Status string `json:"status,omitempty"`
		Error  string `json:"error,omitempty"`
	}{Host: a.Name}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	raw, err := a.Operate(ctx, wire.Operation{Kind: wire.OpRegistryLogin, Auth: []wire.RegistryAuth{auth}})
	if err != nil {
		out.Error = regcred.Scrub(err.Error(), []wire.RegistryAuth{auth})
		if strings.Contains(out.Error, "unknown operation") {
			out.Error = "the agent on " + a.Name + " is too old to test logins: update it from Hosts → Updates"
		}
	} else {
		var res wire.RegistryLoginResult
		_ = json.Unmarshal(raw, &res)
		out.OK, out.Status = true, regcred.Scrub(res.Status, []wire.RegistryAuth{auth})
	}
	me := userFrom(r)
	detail := map[string]any{"host": a.Name, "ok": out.OK}
	if out.Error != "" {
		detail["error"] = out.Error
	}
	s.record(r, &me, "registry.tested", c.Registry, detail)
	writeJSON(w, http.StatusOK, out)
}

// testAgent is the requested host's agent if it's online, else the first
// online one by name.
func (s *Server) testAgent(hostID string) *registry.Agent {
	if s.Reg == nil {
		return nil
	}
	if hostID != "" {
		return s.Reg.Get(hostID)
	}
	all := s.Reg.All()
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	if len(all) == 0 {
		return nil
	}
	return all[0]
}

// openCredential decrypts a saved login for sending to an agent.
func (s *Server) openCredential(c store.RegistryCredential) (wire.RegistryAuth, error) {
	sl, err := s.sealer()
	if err != nil {
		return wire.RegistryAuth{}, err
	}
	pw, err := sl.Open(c.Secret, c.Registry)
	if err != nil {
		return wire.RegistryAuth{}, err
	}
	return wire.RegistryAuth{Registry: c.Registry, Username: c.Username, Password: string(pw)}, nil
}

// ---------------- delivery to agents ----------------

// imagesToPull lists the images a command may pull, which decides the
// registry logins it carries. Commands that don't pull get none.
func imagesToPull(st registry.Status, cmd *wire.Command) []string {
	switch {
	case cmd.Op != nil:
		op := cmd.Op
		if len(op.Auth) > 0 { // set by the caller (a login check)
			return nil
		}
		switch op.Kind {
		case wire.OpImagePull:
			return []string{op.Arg}
		case wire.OpContainerRecreate:
			for _, c := range st.Containers {
				if c.ID == op.Target || c.Name == op.Target || (len(op.Target) >= 12 && strings.HasPrefix(c.ID, op.Target)) {
					return []string{c.Image}
				}
			}
		case wire.OpStackPreflight:
			if op.Files != nil {
				return regcred.ComposeImages(op.Files.Compose, op.Files.Env)
			}
		case wire.OpImagesCheckUpdates:
			var out []string
			for _, c := range st.Containers {
				out = append(out, c.Image)
			}
			return out
		case wire.OpSelfUpdate:
			if op.Arg != "" || st.Hello == nil || st.Hello.OwnProject == "" {
				return nil // adding the cgroup mount pulls nothing
			}
			var out []string
			for _, c := range st.Containers {
				if c.Project == st.Hello.OwnProject {
					out = append(out, c.Image)
				}
			}
			return out
		}
	case cmd.Stack != nil:
		if c := cmd.Stack; c.Action == wire.StackDeploy && len(c.Auth) == 0 {
			// `up` pulls missing images even without Pull set, and a build
			// pulls its Dockerfiles' base images.
			return append(regcred.ComposeImages(c.Compose, c.Env), regcred.BuildImages(c.Compose, c.Env, c.Files)...)
		}
	}
	return nil
}

// AddRegistryAuth is the registry's AddAuth hook: it attaches the saved
// logins for the registries a pulling command's images come from, and only
// those. A login that can't be loaded is left out (the pull then fails
// with the registry's own "unauthorized", which the UI explains).
func (s *Server) AddRegistryAuth(ctx context.Context, a *registry.Agent, cmd *wire.Command) {
	images := imagesToPull(a.Status(), cmd)
	if len(images) == 0 {
		return
	}
	auths := s.registryAuths(ctx, regcred.Hosts(images))
	if len(auths) == 0 {
		return
	}
	if cmd.Op != nil {
		cmd.Op.Auth = auths
	} else if cmd.Stack != nil {
		cmd.Stack.Auth = auths
	}
}

// registryAuths loads and decrypts the saved logins for these registries.
func (s *Server) registryAuths(ctx context.Context, hosts []string) []wire.RegistryAuth {
	if len(hosts) == 0 {
		return nil
	}
	cs, err := s.regStore().RegistryCredentialsFor(ctx, hosts)
	if err != nil {
		s.Log.Warn("load registry logins", "err", err)
		return nil
	}
	var out []wire.RegistryAuth
	var used []string
	for _, c := range cs {
		auth, err := s.openCredential(c)
		if err != nil {
			s.Log.Warn("a saved registry login can't be used", "registry", c.Registry, "err", err)
			continue
		}
		out = append(out, auth)
		used = append(used, c.Registry)
		if len(out) == wire.MaxRegistryAuths {
			break
		}
	}
	if len(used) > 0 {
		if err := s.regStore().TouchRegistryCredentials(context.WithoutCancel(ctx), used); err != nil {
			s.Log.Warn("record registry login use", "err", err)
		}
	}
	return out
}
