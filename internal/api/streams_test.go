package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/auth"
	"github.com/prezmc/tainerhub/internal/store"
)

// runStream opens a rechecked stream as me and reports whether it ended
// (its context was cancelled) within the wait.
func runStream(t *testing.T, s *Server, role string, me store.User, wait time.Duration) bool {
	t.Helper()
	old := streamRecheck
	streamRecheck = 5 * time.Millisecond
	t.Cleanup(func() { streamRecheck = old })
	ended := make(chan struct{})
	h := s.rechecked(role, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			close(ended)
		case <-time.After(wait):
		}
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx, cancel := context.WithCancel(context.WithValue(r.Context(), ctxKey{}, me))
	defer cancel()
	h(httptest.NewRecorder(), r.WithContext(ctx))
	select {
	case <-ended:
		return true
	default:
		return false
	}
}

func TestStreamEndsWhenSessionRevoked(t *testing.T) {
	me := store.User{ID: "u1", Role: "viewer"}
	var revoked atomic.Bool
	s := &Server{whoami: func(*http.Request) (store.User, error) {
		if revoked.Load() {
			return store.User{}, auth.ErrUnauthorized
		}
		return me, nil
	}}
	if runStream(t, s, "viewer", me, 50*time.Millisecond) {
		t.Fatal("a stream with a valid session was ended")
	}
	revoked.Store(true)
	if !runStream(t, s, "viewer", me, 2*time.Second) {
		t.Fatal("stream kept running after the session was revoked")
	}
}

func TestStreamEndsOnDemotion(t *testing.T) {
	admin := store.User{ID: "u1", Role: "admin"}
	for _, tc := range []struct {
		name, need, now string
		ends            bool
	}{
		{"terminal, demoted to operator", "admin", "operator", true},
		{"logs, demoted admin to viewer", "viewer", "viewer", true},
		{"logs, promoted to owner", "viewer", "owner", false},
		{"terminal, unchanged", "admin", "admin", false},
	} {
		s := &Server{whoami: func(*http.Request) (store.User, error) {
			return store.User{ID: "u1", Role: tc.now}, nil
		}}
		wait := 50 * time.Millisecond
		if tc.ends {
			wait = 2 * time.Second
		}
		if got := runStream(t, s, tc.need, admin, wait); got != tc.ends {
			t.Errorf("%s: ended=%v, want %v", tc.name, got, tc.ends)
		}
	}
}

func TestStreamEndsIfSessionBelongsToSomeoneElse(t *testing.T) {
	s := &Server{whoami: func(*http.Request) (store.User, error) {
		return store.User{ID: "other", Role: "admin"}, nil
	}}
	if !runStream(t, s, "viewer", store.User{ID: "u1", Role: "admin"}, 2*time.Second) {
		t.Fatal("stream continued under a different user")
	}
}

func TestLogStreamCaps(t *testing.T) {
	var c logStreamCounts
	for i := 0; i < maxLogStreamsPerHost; i++ {
		if !c.acquire("u1", "h1") {
			t.Fatalf("stream %d on one host refused", i+1)
		}
	}
	if c.acquire("u1", "h1") {
		t.Fatal("per-host cap not enforced")
	}
	if !c.acquire("u2", "h1") {
		t.Fatal("another user's streams must not count against this one")
	}
	for i := 0; i < maxLogStreamsPerUser-maxLogStreamsPerHost; i++ {
		if !c.acquire("u1", "h"+string(rune('2'+i))) {
			t.Fatalf("stream on another host refused before the total cap")
		}
	}
	if c.acquire("u1", "h9") {
		t.Fatal("per-user total cap not enforced")
	}
	c.release("u1", "h1")
	if !c.acquire("u1", "h1") {
		t.Fatal("a released slot should be reusable")
	}
	for i := 0; i < maxLogStreamsPerHost; i++ {
		c.release("u1", "h1")
	}
	if _, ok := c.byHost[[2]string{"u1", "h1"}]; ok {
		t.Fatal("empty counters should be dropped")
	}
}

// Changes that grant lasting access need a recent sign-in. freshCheck fails
// here and the Server has no Store, so reaching the database would panic.
func TestLastingAccessNeedsFreshSignIn(t *testing.T) {
	s := &Server{freshCheck: func(*http.Request) error { return auth.ErrReauth }}
	admin := store.User{ID: "u1", Role: "admin", Username: "ann"}
	for name, h := range map[string]http.HandlerFunc{
		"createInvite": s.createInvite,
		"setRole":      s.setRole,
		"deleteUser":   s.deleteUser,
		"httpsEnable":  s.httpsEnable,
		"httpsDisable": s.httpsDisable,
		"createHost":   s.createHost,
		"newHostToken": s.newHostToken,
	} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"role":"viewer","name":"x","url":"https://hub.example"}`))
		r.SetPathValue("id", "6f1c2f2e-6a8e-4a43-9a65-1d0f6ad7b0a1")
		r.SetPathValue("uid", "6f1c2f2e-6a8e-4a43-9a65-1d0f6ad7b0a2")
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, admin))
		w := httptest.NewRecorder()
		h(w, r)
		var body struct{ Reauth bool }
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != http.StatusForbidden || !body.Reauth {
			t.Errorf("%s: status %d body %s, want 403 with reauth", name, w.Code, w.Body.String())
		}
	}
}

func TestInvitesRevokedFor(t *testing.T) {
	for role, want := range map[string]string{
		"admin":    "",
		"operator": "admin,operator,viewer",
		"viewer":   "admin,operator,viewer",
	} {
		if got := strings.Join(invitesRevokedFor(role), ","); got != want {
			t.Errorf("invitesRevokedFor(%s) = %q, want %q", role, got, want)
		}
	}
}

func TestUserChangeConflict(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	s.userChangeFailed(w, store.ErrChanged)
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", w.Code)
	}
	w = httptest.NewRecorder()
	s.userChangeFailed(w, store.ErrNotFound)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", w.Code)
	}
	if errors.Is(store.ErrChanged, store.ErrNotFound) {
		t.Fatal("ErrChanged must be distinct from ErrNotFound")
	}
}
