package auth

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/store"
)

// fakeStore records the calls the sign-in and session code makes.
type fakeStore struct {
	mu            sync.Mutex
	lookups       atomic.Int32
	accepts       atomic.Int32
	inviteOK      bool
	session       store.Session
	revokedKeep   []byte
	revokeCalls   int
	passwordSetTo string
	recoveryIDs   []string            // user IDs UseRecoveryCode was called with
	tokens        map[string]tokenRow // API tokens by hash (see tokens_test.go)
	touches       int
}

func (f *fakeStore) UserByUsername(ctx context.Context, username string) (store.User, error) {
	f.lookups.Add(1)
	return store.User{}, store.ErrNotFound
}

func (f *fakeStore) InviteRole(ctx context.Context, tokenHash []byte) (string, error) {
	if !f.inviteOK {
		return "", store.ErrNotFound
	}
	return "viewer", nil
}

func (f *fakeStore) AcceptInvite(ctx context.Context, tokenHash []byte, u store.User, recoveryHashes [][]byte) (store.User, error) {
	f.accepts.Add(1)
	return u, nil
}

func (f *fakeStore) CreateSession(ctx context.Context, tokenHash []byte, userID string, expires time.Time, ip, ua string) error {
	return nil
}

func (f *fakeStore) GetSession(ctx context.Context, tokenHash []byte) (store.Session, error) {
	return f.session, nil
}

func (f *fakeStore) DeleteOtherSessions(ctx context.Context, userID string, keep []byte) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokeCalls++
	f.revokedKeep = keep
	return 2, nil
}

func (f *fakeStore) SetPasswordAndRevoke(ctx context.Context, userID, hash string, keep []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokeCalls++
	f.revokedKeep = keep
	f.passwordSetTo = hash
	return nil
}

func testService(f *fakeStore, max int) *Service {
	return &Service{db: f, Limiter: NewLimiter(max, time.Minute), TokenLimiter: NewLimiter(max, time.Minute), ceremonies: map[string]*ceremony{}}
}

func signedInRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.RemoteAddr = "192.0.2.10:1234"
	r.AddCookie(&http.Cookie{Name: "th_session", Value: "tok"})
	return r
}

// fillArgon takes every argon2 slot, so any hash attempt fails fast with ErrBusy.
func fillArgon(t *testing.T) {
	t.Helper()
	old := argonWait
	argonWait = 20 * time.Millisecond
	n := cap(argonSlots)
	for i := 0; i < n; i++ {
		argonSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for i := 0; i < n; i++ {
			<-argonSlots
		}
		argonWait = old
	})
}

func TestLimiterParallelBurst(t *testing.T) {
	l := NewLimiter(5, time.Minute)
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Reserve("192.0.2.1") {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 5 {
		t.Fatalf("burst let %d attempts through, want 5", ok.Load())
	}
	l.Release("192.0.2.1")
	if !l.Reserve("192.0.2.1") || l.Reserve("192.0.2.1") {
		t.Fatal("Release should give back exactly one attempt")
	}
}

func TestLoginBurstOnlyAllowedThrough(t *testing.T) {
	f := &fakeStore{}
	s := testService(f, 3)
	var limited atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			r.RemoteAddr = "192.0.2.7:5555"
			_, err := s.LoginWithPassword(httptest.NewRecorder(), r, "nobody", "some password")
			if errors.Is(err, ErrRateLimited) {
				limited.Add(1)
			} else if !errors.Is(err, ErrBadPassword) {
				t.Errorf("unexpected error %v", err)
			}
		}()
	}
	wg.Wait()
	if n := f.lookups.Load(); n != 3 {
		t.Fatalf("%d logins reached the password check, want 3", n)
	}
	if limited.Load() != 27 {
		t.Fatalf("%d logins rate-limited, want 27", limited.Load())
	}
}

func TestLoginBusyWhenArgonFull(t *testing.T) {
	f := &fakeStore{}
	s := testService(f, 3)
	fillArgon(t)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	_, err := s.LoginWithPassword(httptest.NewRecorder(), r, "nobody", "some password")
	if !errors.Is(err, ErrBusy) || !errors.Is(err, ErrRateLimited) {
		t.Fatalf("got %v, want ErrBusy (as ErrRateLimited)", err)
	}
	if !s.Limiter.Allow(s.ClientIP(r)) || len(s.Limiter.fails) != 0 {
		t.Fatal("a busy server shouldn't count against the client")
	}
}

func TestInviteBadTokenDoesNotHash(t *testing.T) {
	f := &fakeStore{}
	s := testService(f, 10)
	fillArgon(t) // hashing would now fail with ErrBusy
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	_, _, err := s.AcceptInvite(httptest.NewRecorder(), r, AcceptInvite{Token: "bogus", Username: "newbie", Password: "long enough password"})
	if !errors.Is(err, ErrBadInvite) {
		t.Fatalf("got %v, want ErrBadInvite", err)
	}
	if f.accepts.Load() != 0 {
		t.Fatal("bad invite reached the store")
	}
	// A valid token does go on to hash (and here finds the server busy).
	f.inviteOK = true
	_, _, err = s.AcceptInvite(httptest.NewRecorder(), r, AcceptInvite{Token: "good", Username: "newbie", Password: "long enough password"})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v, want ErrBusy", err)
	}
}

func TestLimiterIPv6Per64(t *testing.T) {
	l := NewLimiter(2, time.Minute)
	l.Reserve("2001:db8:1:2::1")
	l.Reserve("2001:db8:1:2:aaaa:bbbb:cccc:dddd")
	if l.Reserve("2001:db8:1:2::ffff") {
		t.Fatal("another address in the same /64 should share the limit")
	}
	if !l.Reserve("2001:db8:1:3::1") {
		t.Fatal("a different /64 should have its own limit")
	}
	if k := limitKey("::ffff:192.0.2.1"); k != "192.0.2.1" {
		t.Fatalf("IPv4-mapped key %q", k)
	}
	if k := limitKey("192.0.2.1"); k != "192.0.2.1" {
		t.Fatalf("IPv4 key %q", k)
	}
	if k := limitKey("2001:db8:1:2::1"); k != "2001:db8:1:2::/64" {
		t.Fatalf("IPv6 key %q", k)
	}
}

func TestFreshSignInRule(t *testing.T) {
	now := time.Now()
	if !freshSignIn(now.Add(-time.Minute), now) || !freshSignIn(now.Add(-9*time.Minute), now) {
		t.Fatal("recent sign-in should be fresh")
	}
	if freshSignIn(now.Add(-10*time.Minute), now) || freshSignIn(now.Add(-2*time.Hour), now) {
		t.Fatal("old sign-in should not be fresh")
	}

	f := &fakeStore{session: store.Session{CreatedAt: now.Add(-2 * time.Minute)}}
	s := testService(f, 10)
	if err := s.RequireFreshSignIn(signedInRequest("")); err != nil {
		t.Fatalf("fresh session refused: %v", err)
	}
	f.session.CreatedAt = now.Add(-11 * time.Minute)
	if err := s.RequireFreshSignIn(signedInRequest("")); !errors.Is(err, ErrReauth) {
		t.Fatalf("stale session: got %v, want ErrReauth", err)
	}
	if err := s.RequireFreshSignIn(httptest.NewRequest(http.MethodPost, "/", nil)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("no session: got %v", err)
	}
}

func TestSetPasswordNeedsFreshSignIn(t *testing.T) {
	f := &fakeStore{session: store.Session{CreatedAt: time.Now().Add(-time.Hour)}}
	s := testService(f, 10)
	u := store.User{ID: "u1"} // passkey-only: no password yet
	if err := s.ChangePassword(signedInRequest(""), u, "", "a brand new password"); !errors.Is(err, ErrReauth) {
		t.Fatalf("got %v, want ErrReauth", err)
	}
	if f.revokeCalls != 0 {
		t.Fatal("password set without a fresh sign-in")
	}
	f.session.CreatedAt = time.Now()
	if err := s.ChangePassword(signedInRequest(""), u, "", "a brand new password"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.revokedKeep, Hash("tok")) {
		t.Fatal("setting a password should keep only the current session")
	}
}

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	// A stale session is fine when the current password is given.
	f := &fakeStore{session: store.Session{CreatedAt: time.Now().Add(-time.Hour)}}
	s := testService(f, 10)
	u := store.User{ID: "u1", PasswordHash: HashPassword("the old password")}
	if err := s.ChangePassword(signedInRequest(""), u, "wrong password!", "a brand new password"); err == nil || f.revokeCalls != 0 {
		t.Fatalf("wrong current password: err %v, revokes %d", err, f.revokeCalls)
	}
	if err := s.ChangePassword(signedInRequest(""), u, "the old password", "a brand new password"); err != nil {
		t.Fatal(err)
	}
	if f.revokeCalls != 1 || !bytes.Equal(f.revokedKeep, Hash("tok")) {
		t.Fatal("changing the password should sign out the other sessions, keeping this one")
	}
	if !CheckPassword(f.passwordSetTo, "a brand new password") {
		t.Fatal("new password not stored")
	}
}

func TestSignOutOtherSessions(t *testing.T) {
	f := &fakeStore{}
	s := testService(f, 10)
	n, err := s.SignOutOtherSessions(signedInRequest(""), store.User{ID: "u1"})
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !bytes.Equal(f.revokedKeep, Hash("tok")) {
		t.Fatal("the current session must be kept")
	}
	if _, err := s.SignOutOtherSessions(httptest.NewRequest(http.MethodPost, "/", nil), store.User{ID: "u1"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("no session: got %v", err)
	}
}
