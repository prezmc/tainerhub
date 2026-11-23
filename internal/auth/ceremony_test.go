package auth

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func beginReq(ip string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/login/begin", nil)
	r.RemoteAddr = ip + ":1234"
	return r
}

func TestBeginIsRateLimitedPerClient(t *testing.T) {
	s := &Service{ceremonies: map[string]*ceremony{}, beginLimiter: NewLimiter(3, time.Minute)}
	for i := 0; i < 3; i++ {
		if err := s.putCeremony(httptest.NewRecorder(), beginReq("192.0.2.1"), &ceremony{kind: "login"}); err != nil {
			t.Fatalf("begin %d: %v", i, err)
		}
	}
	if err := s.putCeremony(httptest.NewRecorder(), beginReq("192.0.2.1"), &ceremony{kind: "login"}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("4th begin from one client: got %v, want rate limited", err)
	}
	if err := s.putCeremony(httptest.NewRecorder(), beginReq("192.0.2.2"), &ceremony{kind: "login"}); err != nil {
		t.Fatalf("another client should be unaffected: %v", err)
	}
}

func TestCeremonyTableFullRefusesInsteadOfEvicting(t *testing.T) {
	s := &Service{ceremonies: map[string]*ceremony{}}
	live := time.Now().Add(time.Minute)
	for i := 0; i < maxCeremonies; i++ {
		id := randToken(8)
		s.ceremonies[id] = &ceremony{kind: "login", expires: live}
		s.order = append(s.order, ceremonyRef{id: id, expires: live})
	}
	err := s.putCeremony(httptest.NewRecorder(), beginReq("192.0.2.1"), &ceremony{kind: "login"})
	if !errors.Is(err, ErrTooManyCeremonies) || !errors.Is(err, ErrRateLimited) {
		t.Fatalf("got %v, want ErrTooManyCeremonies (as ErrRateLimited)", err)
	}
	if len(s.ceremonies) != maxCeremonies {
		t.Fatalf("a live ceremony was evicted: %d left", len(s.ceremonies))
	}
}

func TestExpiredCeremoniesAreDropped(t *testing.T) {
	s := &Service{ceremonies: map[string]*ceremony{}}
	past := time.Now().Add(-time.Second)
	for i := 0; i < 5; i++ {
		id := randToken(8)
		s.ceremonies[id] = &ceremony{kind: "login", expires: past}
		s.order = append(s.order, ceremonyRef{id: id, expires: past})
	}
	w := httptest.NewRecorder()
	if err := s.putCeremony(w, beginReq("192.0.2.1"), &ceremony{kind: "login"}); err != nil {
		t.Fatal(err)
	}
	if len(s.ceremonies) != 1 || len(s.order) != 1 {
		t.Fatalf("expired ceremonies kept: map %d, queue %d", len(s.ceremonies), len(s.order))
	}
	// The stored ceremony can be taken back with its cookie, once.
	r := beginReq("192.0.2.1")
	for _, c := range w.Result().Cookies() {
		r.AddCookie(c)
	}
	if _, err := s.takeCeremony(httptest.NewRecorder(), r, "login"); err != nil {
		t.Fatalf("take: %v", err)
	}
	if _, err := s.takeCeremony(httptest.NewRecorder(), r, "login"); err == nil {
		t.Fatal("a ceremony must work only once")
	}
}

func TestValidUsernameASCIIOnly(t *testing.T) {
	for _, ok := range []string{"bob", "Bob.Smith", "a_b-c", "x1"} {
		if !validUsername(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	// "аdmin" starts with a Cyrillic а; "bób" has an accented letter.
	for _, bad := range []string{"а" + "dmin", "bób", "a", "has space", "名前名前", strings.Repeat("a", 33)} {
		if validUsername(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestSetupTokenNotLogged(t *testing.T) {
	var buf bytes.Buffer
	s := &Service{Log: slog.New(slog.NewTextHandler(&buf, nil))}
	tok := s.RotateSetupToken()
	if strings.Contains(buf.String(), tok) {
		t.Fatal("the setup token must not appear in the logs")
	}
	if !strings.Contains(buf.String(), "setup-link") {
		t.Fatalf("log should say how to get the link, got %q", buf.String())
	}
}
