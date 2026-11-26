package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

// Host-scoped access (see scope.go). The tests below go over the whole
// route table, so a new route can't skip the check: it needs a scope, and a
// route about a host or stack must check it.

const (
	scopeHostC = "0c000000-0000-4000-8000-00000000000c" // known to the hub, never connects
	stackOnA   = "5d7c8f0e-0000-4000-8000-0000000000a1"
	stackOnB   = "5d7c8f0e-0000-4000-8000-0000000000b1"
	someUUID   = "1f000000-0000-4000-8000-000000000001"
)

// ---- the fake store's lists ----

func (f *fakeAutoStore) ListHosts(context.Context) ([]store.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.Host{}
	for _, h := range f.hosts {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeAutoStore) ListStacks(context.Context) ([]store.Stack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.Stack{}
	for _, st := range f.stacks {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeAutoStore) GetMove(_ context.Context, id string) (store.StackMove, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.moves {
		if m.ID == id {
			return m, nil
		}
	}
	return store.StackMove{}, store.ErrNotFound
}

func (f *fakeAutoStore) ContainerMoves(context.Context, int) ([]store.StackMove, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.StackMove
	for _, m := range f.moves {
		if m.Kind == "container" {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeAutoStore) ListAudit(_ context.Context, _ int, viewer store.User) ([]store.AuditEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auditFor = &viewer
	return nil, nil
}

// ---- the rig ----

// newScopeRig: hosts alpha (A) and beta (B) connected, gamma (C) known but
// offline; a stack on A and one on B; the caller an admin limited to A and C.
func newScopeRig(t *testing.T) *autoRig {
	t.Helper()
	x := newAutoRig(t)
	x.host(autoHostA, "alpha",
		wire.Container{ID: "c1", Name: "web", Image: "nginx:1", State: "running"},
		wire.Container{ID: "c3", Name: "site-web-1", Image: "nginx:1", Project: "site", State: "running"})
	x.host(autoHostB, "beta",
		wire.Container{ID: "c2", Name: "db", Image: "postgres:16", State: "running"},
		wire.Container{ID: "c4", Name: "hidden-app-1", Image: "app:1", Project: "hidden", State: "running"})
	x.db.hosts[scopeHostC] = store.Host{ID: scopeHostC, Name: "gamma"}
	a := deployedStack("app", "services: {}\n")
	a.ID, a.HostID = stackOnA, autoHostA
	b := deployedStack("secret", "services: {}\n")
	b.ID, b.HostID = stackOnB, autoHostB
	x.db.stacks[a.ID], x.db.stacks[b.ID] = a, b
	x.user = store.User{ID: "u1", Username: "ann", Role: "admin", Scoped: true, Hosts: []string{autoHostA, scopeHostC}}
	return x
}

// pathFor turns a route pattern into a request for the given host and
// stack, with harmless values for the other path parts.
func pathFor(pattern, host, stack string) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	path = strings.NewReplacer(
		"{id}", host, "{sid}", stack, "{cid}", "c1", "{action}", "start", "{rid}", "r1",
		"{name}", "web", "{eid}", "e1", "{uid}", someUUID, "{tid}", someUUID, "{iid}", someUUID,
		"{mid}", someUUID, "{jid}", "j1",
	).Replace(path)
	return method, path
}

func (x *autoRig) req(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("X-TainerHub", "1")
	return r
}

// ---- the route table ----

// Routes the hub answers for any signed-in user whatever their hosts, and why.
var scopeAnyRoutes = map[string]string{
	"GET /api/me":                           "the caller's own account",
	"POST /api/me/passkeys/begin":           "the caller's own account",
	"POST /api/me/passkeys/finish":          "the caller's own account",
	"DELETE /api/me/passkeys/{id}":          "the caller's own passkey ({id} is the passkey)",
	"POST /api/me/recovery-codes":           "the caller's own account",
	"POST /api/me/password":                 "the caller's own account",
	"POST /api/me/sessions/sign-out-others": "the caller's own account",
	"GET /api/me/tokens":                    "the caller's own tokens",
	"POST /api/me/tokens":                   "a token acts with its owner's hosts",
	"DELETE /api/me/tokens/{tid}":           "the caller's own tokens",
	"GET /api/dockerhub/search":             "Docker Hub, not a host",
	"GET /api/dockerhub/tags":               "Docker Hub, not a host",
	"GET /api/exec/{eid}":                   "a terminal belongs to whoever opened it on a host they reach; the stream ends if the host goes out of reach",
	"POST /api/exec/{eid}/input":            "the caller's own terminal",
	"POST /api/exec/{eid}/resize":           "the caller's own terminal",
	"DELETE /api/exec/{eid}":                "the caller's own terminal",
}

func TestRouteScopes(t *testing.T) {
	s := &Server{}
	seen := map[string]bool{}
	for _, rt := range s.routes() {
		_, path, _ := strings.Cut(rt.pattern, " ")
		seen[rt.pattern] = true
		switch {
		case rt.scope == scopeUnset:
			t.Errorf("%s has no scope", rt.pattern)
		case strings.HasPrefix(path, "/api/hosts/{id}") && rt.scope != scopeHost && rt.scope != scopeHub:
			t.Errorf("%s names a host: want scopeHost (or scopeHub), got %v", rt.pattern, rt.scope)
		case strings.HasPrefix(path, "/api/stacks/{sid}") && rt.scope != scopeStack:
			t.Errorf("%s names a stack: want scopeStack, got %v", rt.pattern, rt.scope)
		case rt.scope == scopeHost && !strings.HasPrefix(path, "/api/hosts/{id}"):
			t.Errorf("%s is scopeHost but has no host in its path", rt.pattern)
		case rt.scope == scopeStack && !strings.HasPrefix(path, "/api/stacks/{sid}"):
			t.Errorf("%s is scopeStack but has no stack in its path", rt.pattern)
		case rt.scope == scopeHub && rt.role != "admin":
			t.Errorf("%s is hub-wide but open to %ss", rt.pattern, rt.role)
		case rt.scope == scopeAny && scopeAnyRoutes[rt.pattern] == "":
			t.Errorf("%s is scopeAny: add it to scopeAnyRoutes with the reason it concerns no host", rt.pattern)
		case rt.scope == scopeFiltered && filteredChecks[rt.pattern] == nil:
			t.Errorf("%s filters or checks hosts itself: add a check to filteredChecks", rt.pattern)
		}
		if strings.Contains(path, "{id}") && !strings.HasPrefix(path, "/api/hosts/{id}") && rt.scope != scopeAny {
			t.Errorf("%s: {id} means a host only under /api/hosts/{id}", rt.pattern)
		}
	}
	for p := range scopeAnyRoutes {
		if !seen[p] {
			t.Errorf("scopeAnyRoutes lists %s, which isn't a route", p)
		}
	}
	for p := range filteredChecks {
		if !seen[p] {
			t.Errorf("filteredChecks lists %s, which isn't a route", p)
		}
	}
}

// Nothing may register a signed-in route outside the table, where it would
// skip TestRouteScopes: need() and stream() are called only in Handler.
func TestRoutesOnlyFromTable(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	call := regexp.MustCompile(`s\.(need|stream)\(`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		n := len(call.FindAll(b, -1))
		want := 0
		switch f {
		case "api.go":
			want = 2 // the loop in Handler
		case "streams.go":
			want = 1 // stream() is need() plus re-checks
		}
		if n != want {
			t.Errorf("%s calls need/stream %d times, want %d: add routes to the table in routes()", f, n, want)
		}
	}
}

// Every route about a host or stack refuses one outside the caller's hosts,
// through the real handler, before doing anything; hub-wide routes refuse
// scoped admins outright.
func TestScopedUserRefusedOtherHosts(t *testing.T) {
	x := newScopeRig(t)
	for _, rt := range x.s.routes() {
		if rt.scope != scopeHost && rt.scope != scopeStack && rt.scope != scopeHub {
			continue
		}
		method, path := pathFor(rt.pattern, autoHostB, stackOnB)
		w := httptest.NewRecorder()
		x.h.ServeHTTP(w, x.req(method, path, "{}"))
		want := http.StatusNotFound
		if rt.scope == scopeHub {
			want = http.StatusForbidden
		}
		if w.Code != want {
			t.Errorf("%s %s: %d %s, want %d", method, path, w.Code, strings.TrimSpace(w.Body.String()), want)
		}
	}
	// Hub-wide routes refuse even with the caller's own hosts in the path.
	for _, rt := range x.s.routes() {
		if rt.scope != scopeHub {
			continue
		}
		method, path := pathFor(rt.pattern, autoHostA, stackOnA)
		w := httptest.NewRecorder()
		x.h.ServeHTTP(w, x.req(method, path, "{}"))
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "every host") {
			t.Errorf("%s %s: %d %s, want 403", method, path, w.Code, w.Body)
		}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.audit) != 0 {
		t.Fatalf("refused requests were audited: %+v", x.audit)
	}
}

// The same routes let the caller through on their own hosts (and let an
// admin with every host through anywhere).
func TestScopedUserReachesOwnHosts(t *testing.T) {
	x := newScopeRig(t)
	scoped := x.user
	all := store.User{ID: "u2", Username: "bo", Role: "admin"}
	for _, rt := range x.s.routes() {
		if rt.scope != scopeHost && rt.scope != scopeStack && rt.scope != scopeHub {
			continue
		}
		for _, tc := range []struct {
			who         store.User
			host, stack string
			ok          bool
		}{
			{scoped, autoHostA, stackOnA, rt.scope != scopeHub},
			{all, autoHostB, stackOnB, true},
		} {
			x.user = tc.who
			ran := false
			mux := http.NewServeMux()
			mux.Handle(rt.pattern, x.s.need(rt.role, rt.scope, func(w http.ResponseWriter, r *http.Request) {
				ran = true
				w.WriteHeader(http.StatusTeapot)
			}))
			method, path := pathFor(rt.pattern, tc.host, tc.stack)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, x.req(method, path, "{}"))
			if ran != tc.ok {
				t.Errorf("%s as %s: ran=%v (%d %s), want %v", rt.pattern, tc.who.Username, ran, w.Code, w.Body, tc.ok)
			}
		}
	}
}

// Moves need both hosts: the path checks the source, the handler the target.
func TestMovesNeedBothHosts(t *testing.T) {
	x := newScopeRig(t)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/stacks/" + stackOnA + "/move?host=" + autoHostB, ""},
		{"POST", "/api/stacks/" + stackOnA + "/move", `{"hostId":"` + autoHostB + `"}`},
		{"GET", "/api/hosts/" + autoHostA + "/containers/c1/move?host=" + autoHostB, ""},
		{"POST", "/api/hosts/" + autoHostA + "/containers/c1/move", `{"hostId":"` + autoHostB + `"}`},
	} {
		w := httptest.NewRecorder()
		x.h.ServeHTTP(w, x.req(c.method, c.path, c.body))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s to beta: %d %s, want 404", c.method, c.path, w.Code, w.Body)
		}
	}
}

// ---- routes that filter or check hosts themselves ----

func getJSON[T any](t *testing.T, x *autoRig, path string) T {
	t.Helper()
	w := x.do("GET", path, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
	}
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return v
}

func wantCode(t *testing.T, x *autoRig, method, path, body string, code int) {
	t.Helper()
	if w := x.do(method, path, body); w.Code != code {
		t.Errorf("%s %s: %d %s, want %d", method, path, w.Code, w.Body, code)
	}
}

func hostIDs[T any](items []T, id func(T) string) []string {
	out := []string{}
	for _, it := range items {
		if h := id(it); !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	slices.Sort(out)
	return out
}

func onlyHosts(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s: hosts %v, want %v", what, got, want)
	}
}

// filteredChecks has a check for every scopeFiltered route (TestRouteScopes
// insists), run by TestFilteredRoutes with newScopeRig's caller (alpha and
// gamma, not beta).
var filteredChecks = map[string]func(*testing.T, *autoRig){
	"GET /api/containers": func(t *testing.T, x *autoRig) {
		cs := getJSON[[]containerView](t, x, "/api/containers")
		onlyHosts(t, "containers", hostIDs(cs, func(c containerView) string { return c.HostID }), autoHostA)
	},
	"GET /api/hosts": func(t *testing.T, x *autoRig) {
		hs := getJSON[[]hostView](t, x, "/api/hosts")
		onlyHosts(t, "hosts", hostIDs(hs, func(h hostView) string { return h.ID }), autoHostA, scopeHostC)
	},
	"GET /api/events": func(t *testing.T, x *autoRig) {
		x.s.Reg.AddActivity(registry.Activity{Kind: "user", Action: "start", HostID: autoHostB, Host: "beta", Container: "db"})
		x.s.Reg.AddActivity(registry.Activity{Kind: "user", Action: "start", HostID: autoHostA, Host: "alpha", Container: "web"})
		ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
		defer cancel()
		w := httptest.NewRecorder()
		x.h.ServeHTTP(w, x.req("GET", "/api/events", "").WithContext(ctx))
		_, data, ok := strings.Cut(w.Body.String(), "event: fleet\ndata: ")
		if !ok {
			t.Fatalf("no fleet event: %q", w.Body)
		}
		data, _, _ = strings.Cut(data, "\n")
		var p fleetPayload
		if err := json.Unmarshal([]byte(data), &p); err != nil {
			t.Fatal(err)
		}
		onlyHosts(t, "event hosts", hostIDs(p.Hosts, func(h hostView) string { return h.ID }), autoHostA, scopeHostC)
		onlyHosts(t, "event containers", hostIDs(p.Containers, func(c containerView) string { return c.HostID }), autoHostA)
		onlyHosts(t, "event activity", hostIDs(p.Activity, func(a registry.Activity) string { return a.HostID }), autoHostA)
	},
	"GET /api/placement": func(t *testing.T, x *autoRig) {
		res := getJSON[placementResult](t, x, "/api/placement")
		onlyHosts(t, "placement", hostIDs(res.Hosts, func(h placementHost) string { return h.HostID }), autoHostA, scopeHostC)
		wantCode(t, x, "GET", "/api/placement?stack="+stackOnB, "", http.StatusNotFound)
		wantCode(t, x, "GET", "/api/placement?host="+autoHostB+"&container=c2", "", http.StatusNotFound)
		wantCode(t, x, "GET", "/api/placement?stack="+stackOnA, "", http.StatusOK)
	},
	"GET /api/stacks": func(t *testing.T, x *autoRig) {
		ss := getJSON[[]stackSummary](t, x, "/api/stacks")
		onlyHosts(t, "stacks", hostIDs(ss, func(s stackSummary) string { return s.HostID }), autoHostA)
		if len(ss) != 2 { // app (managed) and site (found running)
			t.Errorf("stacks: %+v", ss)
		}
	},
	"GET /api/jobs/{jid}": func(t *testing.T, x *autoRig) {
		jb, _ := x.s.jobs.start(&job{StackID: stackOnB, HostID: autoHostB, Action: "deploy"})
		ja, _ := x.s.jobs.start(&job{StackID: stackOnA, HostID: autoHostA, Action: "deploy"})
		jb.finish("")
		ja.finish("")
		wantCode(t, x, "GET", "/api/jobs/"+jb.ID, "", http.StatusNotFound)
		wantCode(t, x, "GET", "/api/jobs/"+ja.ID, "", http.StatusOK)
	},
	"POST /api/stacks/takeover-check": func(t *testing.T, x *autoRig) {
		wantCode(t, x, "POST", "/api/stacks/takeover-check", `{"hostId":"`+autoHostB+`","name":"hidden","compose":"services: {}"}`, http.StatusNotFound)
	},
	"POST /api/stacks/preflight": func(t *testing.T, x *autoRig) {
		wantCode(t, x, "POST", "/api/stacks/preflight", `{"hostId":"`+autoHostB+`","name":"web","compose":"services: {}"}`, http.StatusNotFound)
	},
	"GET /api/stacks/takeover-source": func(t *testing.T, x *autoRig) {
		wantCode(t, x, "GET", "/api/stacks/takeover-source?host="+autoHostB+"&name=hidden", "", http.StatusNotFound)
	},
	"POST /api/stacks": func(t *testing.T, x *autoRig) {
		wantCode(t, x, "POST", "/api/stacks", `{"hostId":"`+autoHostB+`","name":"web","compose":"services: {}"}`, http.StatusNotFound)
	},
	"DELETE /api/moves/{mid}/leftovers": func(t *testing.T, x *autoRig) {
		x.db.moves = append(x.db.moves, store.StackMove{ID: someUUID, Kind: "stack", FromHost: autoHostB, ToHost: autoHostA, State: "done",
			Leftovers: &store.MoveLeftovers{HostID: autoHostB, Volumes: []string{"secret_data"}}})
		wantCode(t, x, "DELETE", "/api/moves/"+someUUID+"/leftovers", "", http.StatusNotFound)
	},
	"GET /api/container-moves": func(t *testing.T, x *autoRig) {
		x.db.moves = append(x.db.moves,
			store.StackMove{ID: "m1", Kind: "container", StackName: "web", FromHost: autoHostA, ToHost: scopeHostC},
			store.StackMove{ID: "m2", Kind: "container", StackName: "db", FromHost: autoHostB, ToHost: autoHostA},
			store.StackMove{ID: "m3", Kind: "container", StackName: "web", FromHost: autoHostA, ToHost: autoHostB})
		ms := getJSON[[]store.StackMove](t, x, "/api/container-moves")
		if len(ms) != 1 || ms[0].ID != "m1" {
			t.Errorf("container moves: %+v", ms)
		}
	},
	"GET /api/audit": func(t *testing.T, x *autoRig) {
		getJSON[[]store.AuditEntry](t, x, "/api/audit")
		if v := x.db.auditFor; v == nil || !v.Scoped || !slices.Equal(v.Hosts, x.user.Hosts) {
			t.Errorf("audit asked for %+v, want the scoped caller", v)
		}
	},
	"GET /api/updates": func(t *testing.T, x *autoRig) {
		up := getJSON[struct{ Hosts []hostUpdate }](t, x, "/api/updates")
		onlyHosts(t, "updates", hostIDs(up.Hosts, func(h hostUpdate) string { return h.ID }), autoHostA, scopeHostC)
	},
	"POST /api/updates/check": func(t *testing.T, x *autoRig) {
		w := x.do("POST", "/api/updates/check", "{}")
		var up struct{ Hosts []hostUpdate }
		_ = json.Unmarshal(w.Body.Bytes(), &up)
		onlyHosts(t, "update check", hostIDs(up.Hosts, func(h hostUpdate) string { return h.ID }), autoHostA, scopeHostC)
	},
	"GET /api/auto-updates": func(t *testing.T, x *autoRig) {
		x.optIn(autoHostA, "web")
		x.optIn(autoHostB, "db")
		au := getJSON[struct{ Targets []store.AutoUpdateTarget }](t, x, "/api/auto-updates")
		onlyHosts(t, "auto-update targets", hostIDs(au.Targets, func(t store.AutoUpdateTarget) string { return t.HostID }), autoHostA)
	},
}

func TestFilteredRoutes(t *testing.T) {
	gh := httptest.NewServer(http.NotFoundHandler()) // /api/updates asks GitHub
	defer gh.Close()
	old := githubAPI
	githubAPI = gh.URL
	defer func() { githubAPI = old }()
	for p, check := range filteredChecks {
		t.Run(p, func(t *testing.T) { check(t, newScopeRig(t)) })
	}
}

// ---- the hub's own server ----

// A scoped admin is an operator on the hub's own server: admin routes there
// are refused, and the others see the lower role (so container settings
// such as the hub's database password stay hidden).
func TestScopedAdminIsOperatorOnHubHost(t *testing.T) {
	x := newScopeRig(t)
	x.s.DataDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(x.s.DataDir, "local_host_id"), []byte(autoHostA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, rt := range x.s.routes() {
		if rt.scope != scopeHost && rt.scope != scopeStack {
			continue
		}
		var role string
		mux := http.NewServeMux()
		mux.Handle(rt.pattern, x.s.need(rt.role, rt.scope, func(w http.ResponseWriter, r *http.Request) { role = userFrom(r).Role }))
		method, path := pathFor(rt.pattern, autoHostA, stackOnA)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, x.req(method, path, "{}"))
		switch {
		case rt.role == "admin" && (w.Code != http.StatusForbidden || role != ""):
			t.Errorf("%s on the hub's server: %d, want 403", rt.pattern, w.Code)
		case rt.role != "admin" && role != "operator":
			t.Errorf("%s on the hub's server: ran as %q (%d), want operator", rt.pattern, role, w.Code)
		}
	}
	// Body-named hosts too.
	wantCode(t, x, "POST", "/api/stacks", `{"hostId":"`+autoHostA+`","name":"web","compose":"services: {}"}`, http.StatusForbidden)
	// An admin with every host is still an admin there.
	x.user = store.User{ID: "u2", Username: "bo", Role: "admin"}
	if got := x.s.hostRole(x.user, []string{autoHostA}); got != "admin" {
		t.Fatalf("unscoped admin on hub host: %s", got)
	}
}

// ---- audit ----

func TestAuditRecordsHosts(t *testing.T) {
	x := newScopeRig(t)
	if w := x.do("POST", "/api/hosts/"+autoHostA+"/images/pull", `{"ref":"nginx:1"}`); w.Code >= 300 {
		t.Fatalf("pull: %d %s", w.Code, w.Body)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.audit) != 1 || !slices.Equal(x.audit[0].HostIDs, []string{autoHostA}) {
		t.Fatalf("audit: %+v", x.audit)
	}
}

// ---- helpers ----

func TestScopeHelpers(t *testing.T) {
	all := store.User{ID: "u", Role: "operator"}
	a := store.User{ID: "u", Role: "operator", Scoped: true, Hosts: []string{"A"}}
	ab := store.User{ID: "u", Role: "operator", Scoped: true, Hosts: []string{"A", "B"}}
	none := store.User{ID: "u", Role: "operator", Scoped: true}
	if !all.CanSeeHost("X") || !a.CanSeeHost("A") || a.CanSeeHost("B") || none.CanSeeHost("A") {
		t.Fatal("CanSeeHost")
	}
	for _, c := range []struct {
		me, now store.User
		ok      bool
	}{
		{all, all, true}, {a, all, true}, {a, ab, true}, // widened: fine
		{all, a, false}, {ab, a, false}, {a, none, false}, // narrowed: streams end
		{a, a, true},
	} {
		if got := sameScope(c.me, c.now); got != c.ok {
			t.Errorf("sameScope(%v, %v) = %v", c.me.Hosts, c.now.Hosts, got)
		}
	}
	// An admin with every host manages one limited to some; not the reverse.
	admin := store.User{Role: "admin"}
	hostAdmin := store.User{Role: "admin", Scoped: true, Hosts: []string{"A"}}
	if !outranks(admin, hostAdmin) || outranks(hostAdmin, admin) || outranks(admin, admin) || !outranks(store.User{Role: "owner"}, admin) {
		t.Fatal("outranks")
	}
}

func TestValidateScope(t *testing.T) {
	hosts := []store.Host{{ID: "h1", Name: "beta"}, {ID: "h2", Name: "alpha"}, {ID: "hub", Name: "nas"}}
	ids, names, msg := validateScope("operator", []string{"h1", "h2", "h1", "hub"}, hosts, "hub")
	if msg != "" || !slices.Equal(ids, []string{"h2", "h1", "hub"}) || !slices.Equal(names, []string{"alpha", "beta", "nas"}) {
		t.Fatalf("operator: %v %v %q", ids, names, msg)
	}
	if _, _, msg := validateScope("admin", []string{"h1", "hub"}, hosts, "hub"); !strings.Contains(msg, "hub's own server") {
		t.Errorf("admin with the hub's server: %q", msg)
	}
	if _, _, msg := validateScope("admin", []string{"h1"}, hosts, "hub"); msg != "" {
		t.Errorf("admin elsewhere: %q", msg)
	}
	if _, _, msg := validateScope("viewer", nil, hosts, ""); msg == "" {
		t.Error("no hosts accepted")
	}
	if _, _, msg := validateScope("viewer", []string{"gone"}, hosts, ""); msg == "" {
		t.Error("unknown host accepted")
	}
}

// Streams end once the user loses a host they could reach.
func TestStreamsEndWhenScopeNarrows(t *testing.T) {
	x := newScopeRig(t)
	me := x.user
	now := me
	x.s.whoami = func(*http.Request) (store.User, error) { return now, nil }
	r := httptest.NewRequest("GET", "/", nil)
	if !x.s.stillAllowed(r, me, "viewer") {
		t.Fatal("unchanged scope ended the stream")
	}
	now.Hosts = []string{scopeHostC}
	if x.s.stillAllowed(r, me, "viewer") {
		t.Fatal("stream outlived losing a host")
	}
}

func TestScopedFleetFrameEncodesLikeTheFeed(t *testing.T) {
	p := fleetPayload{Hosts: []hostView{}, Containers: []containerView{}, Activity: []registry.Activity{}}
	all, _ := json.Marshal(p)
	frame := scopedFleetFrame(store.User{Scoped: true}, p)
	if string(frame) != "event: fleet\ndata: "+string(all)+"\n\n" {
		t.Fatalf("frame %q", frame)
	}
	if scopedFleetFrame(store.User{Scoped: true}, "not a payload") != nil {
		t.Fatal("unknown payload")
	}
}
