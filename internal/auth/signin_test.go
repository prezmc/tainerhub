package auth

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/store"
)

func loginFrom(s *Service, ip, xff, username string) error {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = ip + ":4000"
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
		r.Header.Set("X-Forwarded-Proto", "https")
	}
	_, err := s.LoginWithPassword(httptest.NewRecorder(), r, username, "wrong password here")
	return err
}

// Many addresses guessing one account share its budget; other accounts and
// the LAN address are unaffected.
func TestAccountLimitAcrossAddresses(t *testing.T) {
	f := &fakeStore{}
	s := testService(f, 100)
	s.AccountLimiter = NewLimiter(accountMaxFails, time.Minute)
	const proxy = "192.0.2.1"
	if err := s.Reconfigure(nil, []netip.Prefix{netip.MustParsePrefix(proxy + "/32")}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < accountMaxFails; i++ {
		if err := loginFrom(s, proxy, fmt.Sprintf("198.51.100.%d", i+1), "Owner"); !errors.Is(err, ErrBadPassword) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	before := f.lookups.Load()
	err := loginFrom(s, proxy, "203.0.113.50", "owner") // fresh IP, same account (any case)
	if !errors.Is(err, ErrRateLimited) || err.Error() != errAccountLimited.Error() {
		t.Fatalf("got %v, want the account limit", err)
	}
	if f.lookups.Load() != before {
		t.Fatal("a limited attempt still reached the password check")
	}
	if s.Limiter.fails[limitKey("203.0.113.50")] != nil {
		t.Fatal("an account-limited attempt shouldn't use up the address's own attempts")
	}
	if err := loginFrom(s, proxy, "203.0.113.50", "someone-else"); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("another account was limited too: %v", err)
	}
	if err := loginFrom(s, "192.168.1.30", "", "owner"); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("the LAN address was locked out by guesses through the proxy: %v", err)
	}
}

func TestRecoveryCodesCountPerAccount(t *testing.T) {
	f := &fakeStore{}
	s := testService(f, 100)
	s.AccountLimiter = NewLimiter(2, time.Minute)
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.RemoteAddr = fmt.Sprintf("198.51.100.%d:1", i+1)
		_, err := s.LoginWithRecoveryCode(httptest.NewRecorder(), r, "owner", "aaaa-bbbb")
		if i < 2 && errors.Is(err, ErrRateLimited) || i == 2 && !errors.Is(err, ErrRateLimited) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
}

func TestPasskeyRequiredOnSecureAddress(t *testing.T) {
	u := store.User{ID: "u1", Username: "owner", PasswordHash: HashPassword("correct horse battery")}
	f := &fakeStore{user: &u, hasPasskey: true}
	s := testService(f, 100)
	s.AccountLimiter = NewLimiter(3, time.Minute)
	const proxy = "192.0.2.1"
	pub, _ := url.Parse("https://hub.example.com")
	if err := s.Reconfigure(pub, []netip.Prefix{netip.MustParsePrefix(proxy + "/32")}); err != nil {
		t.Fatal(err)
	}
	login := func(peer string, viaProxy bool) error {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.RemoteAddr = peer + ":4000"
		if viaProxy {
			r.Header.Set("X-Forwarded-For", "198.51.100.9")
			r.Header.Set("X-Forwarded-Proto", "https")
		}
		_, err := s.LoginWithPassword(httptest.NewRecorder(), r, "owner", "wrong password")
		return err
	}
	if err := login(proxy, true); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("rule off: got %v", err)
	}
	s.SetPasskeyRequired(true)
	if err := login(proxy, true); !errors.Is(err, ErrPasskeyRequired) {
		t.Fatalf("rule on, secure address: got %v", err)
	}
	if err := login("192.168.1.30", false); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("rule on, LAN address should still check the password: got %v", err)
	}
	f.hasPasskey = false
	if err := login(proxy, true); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("an account without a passkey must keep its password: got %v", err)
	}
	// Refusals aren't failures: they don't use up the account's attempts.
	f.hasPasskey = true
	for i := 0; i < 50; i++ {
		if err := login(proxy, true); !errors.Is(err, ErrPasskeyRequired) {
			t.Fatalf("refusal %d: %v", i, err)
		}
	}
}
