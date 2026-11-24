package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"github.com/prezmc/tainerhub/internal/store"
)

// Passwords are the sign-in that works everywhere, including a fresh install
// reached over plain HTTP. Passkeys need HTTPS and are added later from the
// Account page once TainerHub sits behind a reverse proxy.

const (
	minPassword = 10
	maxPassword = 256

	// argon2id parameters (OWASP's minimum recommendation: 19 MiB, 2 passes).
	argonTime    = 2
	argonMemory  = 19 * 1024
	argonThreads = 1
	argonKeyLen  = 32
)

var ErrBadPassword = errors.New("username or password is wrong")

func checkPasswordPolicy(p string) error {
	n := utf8.RuneCountInString(p)
	if n < minPassword {
		return fmt.Errorf("use at least %d characters", minPassword)
	}
	if len(p) > maxPassword {
		return fmt.Errorf("use at most %d characters", maxPassword)
	}
	return nil
}

// HashPassword returns an encoded argon2id hash: $argon2id$v=19$m=..,t=..,p=..$salt$key
func HashPassword(p string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	key := argon2.IDKey([]byte(p), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads, enc.EncodeToString(salt), enc.EncodeToString(key))
}

// CheckPassword reports whether p matches an encoded hash from HashPassword.
func CheckPassword(encoded, p string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var v int
	var m uint32
	var t uint32
	var th uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &th); err != nil || m == 0 || m > 1<<20 || t == 0 || t > 10 || th == 0 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[4])
	want, err2 := enc.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(p), salt, t, m, th, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// argonSlots caps how many argon2 hashes run at once. Each needs ~19 MiB, so
// an unbounded flood of sign-in requests could run the hub out of memory.
var argonSlots = make(chan struct{}, max(2, runtime.GOMAXPROCS(0)))

// argonWait is how long a request waits for a free slot before giving up.
var argonWait = 2 * time.Second

// ErrBusy is returned when too many passwords are being checked at once. It
// counts as ErrRateLimited, so handlers answer 429.
var ErrBusy error = busyError{}

type busyError struct{}

func (busyError) Error() string        { return "too many sign-ins at once; try again in a moment" }
func (busyError) Is(target error) bool { return target == ErrRateLimited }

// withArgon runs fn once an argon2 slot is free, or returns ErrBusy.
func withArgon(ctx context.Context, fn func()) error {
	t := time.NewTimer(argonWait)
	defer t.Stop()
	select {
	case argonSlots <- struct{}{}:
	case <-t.C:
		return ErrBusy
	case <-ctx.Done():
		return ErrBusy
	}
	defer func() { <-argonSlots }()
	fn()
	return nil
}

// hashPassword is HashPassword within the argon2 cap.
func hashPassword(ctx context.Context, p string) (string, error) {
	var h string
	err := withArgon(ctx, func() { h = HashPassword(p) })
	return h, err
}

// checkPassword is CheckPassword within the argon2 cap.
func checkPassword(ctx context.Context, encoded, p string) (bool, error) {
	var ok bool
	err := withArgon(ctx, func() { ok = CheckPassword(encoded, p) })
	return ok, err
}

// dummyHash is checked against when the username doesn't exist, so a
// failed sign-in takes the same time either way.
var dummyHash = HashPassword("tainerhub-timing-equaliser")

type SetupPassword struct {
	Token       string `json:"token"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Password    string `json:"password"`
}

// SetupWithPassword creates the owner account with a password, signs them in
// and returns their recovery codes (shown once).
func (s *Service) SetupWithPassword(w http.ResponseWriter, r *http.Request, in SetupPassword) (store.User, []string, error) {
	if need, err := s.SetupRequired(r.Context()); err != nil {
		return store.User{}, nil, err
	} else if !need {
		return store.User{}, nil, ErrSetupDone
	}
	if !s.checkSetupToken(in.Token) {
		return store.User{}, nil, ErrBadSetup
	}
	in.Username = strings.TrimSpace(in.Username)
	if !validUsername(in.Username) {
		return store.User{}, nil, errBadUsername
	}
	if err := checkPasswordPolicy(in.Password); err != nil {
		return store.User{}, nil, errors.New("password: " + err.Error())
	}
	if strings.TrimSpace(in.DisplayName) == "" {
		in.DisplayName = in.Username
	}
	hash, err := hashPassword(r.Context(), in.Password)
	if err != nil {
		return store.User{}, nil, err
	}
	codes, hashes := newRecoveryCodes()
	u := store.User{Username: in.Username, DisplayName: strings.TrimSpace(in.DisplayName), Role: "owner", WebAuthnID: []byte(randToken(32)), PasswordHash: hash}
	u, err = s.Store.CreateFirstOwner(r.Context(), u, nil, hashes)
	if errors.Is(err, store.ErrConflict) {
		return store.User{}, nil, ErrSetupDone
	}
	if err != nil {
		return store.User{}, nil, err
	}
	s.clearSetupToken()
	return u, codes, s.startSession(w, r, u)
}

// LoginWithPassword signs in with a username and password, rate-limited per
// client IP like the other sign-in methods.
func (s *Service) LoginWithPassword(w http.ResponseWriter, r *http.Request, username, password string) (store.User, error) {
	ip := s.ClientIP(r)
	// Reserve the attempt before the costly hash check, so a parallel burst
	// can't all get past the limit; a successful sign-in hands it back.
	if !s.Limiter.Reserve(ip) {
		return store.User{}, ErrRateLimited
	}
	failed := false
	defer func() {
		if !failed {
			s.Limiter.Release(ip)
		}
	}()
	if len(password) > maxPassword {
		failed = true
		return store.User{}, ErrBadPassword
	}
	u, err := s.st().UserByUsername(r.Context(), strings.TrimSpace(username))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.User{}, err
	}
	hash := u.PasswordHash
	if hash == "" {
		hash = dummyHash
	}
	ok, err := checkPassword(r.Context(), hash, password)
	if err != nil {
		return store.User{}, err
	}
	if !ok || u.PasswordHash == "" {
		failed = true
		return store.User{}, ErrBadPassword
	}
	return u, s.startSession(w, r, u)
}

// ChangePassword sets a new password and signs the user out of every other
// session. A user who already has a password must give it (that counts as
// signing in again); a user without one (passkey-only) needs a recent
// sign-in instead, so a stolen session can't add a password.
func (s *Service) ChangePassword(r *http.Request, u store.User, current, next string) error {
	if HasBearer(r) {
		return ErrTokenAuth
	}
	ctx := r.Context()
	if u.PasswordHash != "" {
		// Guessing the current password is a sign-in attempt like any other.
		ip := s.ClientIP(r)
		if !s.Limiter.Reserve(ip) {
			return ErrRateLimited
		}
		ok, err := checkPassword(ctx, u.PasswordHash, current)
		if err != nil || ok {
			s.Limiter.Release(ip)
		}
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("current password is wrong")
		}
	} else if err := s.RequireFreshSignIn(r); err != nil {
		return err
	}
	if err := checkPasswordPolicy(next); err != nil {
		return errors.New("new password: " + err.Error())
	}
	keep := s.sessionHash(r)
	if keep == nil {
		return ErrUnauthorized
	}
	hash, err := hashPassword(ctx, next)
	if err != nil {
		return err
	}
	return s.st().SetPasswordAndRevoke(ctx, u.ID, hash, keep)
}

// ---------------- invites ----------------

var ErrBadInvite = errors.New("this invite link is invalid, already used or expired; ask for a new one")

// NewInviteToken returns a link token and the hash to store for it.
func NewInviteToken() (token string, hash []byte) {
	t := randToken(24)
	return t, Hash(t)
}

// InviteRole tells the invite page which role a link is for.
func (s *Service) InviteRole(ctx context.Context, token string) (string, error) {
	role, err := s.Store.InviteRole(ctx, Hash(strings.TrimSpace(token)))
	if errors.Is(err, store.ErrNotFound) {
		return "", ErrBadInvite
	}
	return role, err
}

type AcceptInvite struct {
	Token       string `json:"token"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Password    string `json:"password"`
}

// AcceptInvite creates the invited user, signs them in and returns their
// recovery codes. Failed attempts count toward the sign-in rate limit.
func (s *Service) AcceptInvite(w http.ResponseWriter, r *http.Request, in AcceptInvite) (store.User, []string, error) {
	ip := s.ClientIP(r)
	if !s.Limiter.Reserve(ip) {
		return store.User{}, nil, ErrRateLimited
	}
	failed := false
	defer func() {
		if !failed {
			s.Limiter.Release(ip)
		}
	}()
	in.Username = strings.TrimSpace(in.Username)
	if !validUsername(in.Username) {
		return store.User{}, nil, errBadUsername
	}
	if err := checkPasswordPolicy(in.Password); err != nil {
		return store.User{}, nil, errors.New("password: " + err.Error())
	}
	if strings.TrimSpace(in.DisplayName) == "" {
		in.DisplayName = in.Username
	}
	// Check the link before hashing, so bad tokens cost only a lookup.
	tokenHash := Hash(strings.TrimSpace(in.Token))
	if _, err := s.st().InviteRole(r.Context(), tokenHash); errors.Is(err, store.ErrNotFound) {
		failed = true
		return store.User{}, nil, ErrBadInvite
	} else if err != nil {
		return store.User{}, nil, err
	}
	hash, err := hashPassword(r.Context(), in.Password)
	if err != nil {
		return store.User{}, nil, err
	}
	codes, hashes := newRecoveryCodes()
	u := store.User{Username: in.Username, DisplayName: strings.TrimSpace(in.DisplayName), WebAuthnID: []byte(randToken(32)), PasswordHash: hash}
	created, err := s.st().AcceptInvite(r.Context(), tokenHash, u, hashes)
	switch {
	case errors.Is(err, store.ErrNotFound):
		failed = true
		return store.User{}, nil, ErrBadInvite
	case errors.Is(err, store.ErrConflict):
		return store.User{}, nil, errors.New("that username is taken; pick another")
	case err != nil:
		return store.User{}, nil, err
	}
	return created, codes, s.startSession(w, r, created)
}
