package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
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
		return store.User{}, nil, errors.New("username must be 2-32 letters, digits, dots, dashes or underscores")
	}
	if err := checkPasswordPolicy(in.Password); err != nil {
		return store.User{}, nil, errors.New("password: " + err.Error())
	}
	if strings.TrimSpace(in.DisplayName) == "" {
		in.DisplayName = in.Username
	}
	codes, hashes := newRecoveryCodes()
	u := store.User{Username: in.Username, DisplayName: strings.TrimSpace(in.DisplayName), Role: "owner", WebAuthnID: []byte(randToken(32)), PasswordHash: HashPassword(in.Password)}
	u, err := s.Store.CreateFirstOwner(r.Context(), u, nil, hashes)
	if errors.Is(err, store.ErrConflict) {
		return store.User{}, nil, ErrSetupDone
	}
	if err != nil {
		return store.User{}, nil, err
	}
	s.mu.Lock()
	s.setupToken = ""
	s.mu.Unlock()
	return u, codes, s.startSession(w, r, u)
}

// LoginWithPassword signs in with a username and password, rate-limited per
// client IP like the other sign-in methods.
func (s *Service) LoginWithPassword(w http.ResponseWriter, r *http.Request, username, password string) (store.User, error) {
	ip := s.ClientIP(r)
	if !s.Limiter.Allow(ip) {
		return store.User{}, ErrRateLimited
	}
	if len(password) > maxPassword {
		s.Limiter.Fail(ip)
		return store.User{}, ErrBadPassword
	}
	u, err := s.Store.UserByUsername(r.Context(), strings.TrimSpace(username))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.User{}, err
	}
	hash := u.PasswordHash
	if hash == "" {
		hash = dummyHash
	}
	if !CheckPassword(hash, password) || u.PasswordHash == "" {
		s.Limiter.Fail(ip)
		return store.User{}, ErrBadPassword
	}
	return u, s.startSession(w, r, u)
}

// ChangePassword sets a new password. A user who already has one must give
// it; a user without one (passkey-only) is already signed in, which is enough.
func (s *Service) ChangePassword(ctx context.Context, u store.User, current, next string) error {
	if u.PasswordHash != "" && !CheckPassword(u.PasswordHash, current) {
		return errors.New("current password is wrong")
	}
	if err := checkPasswordPolicy(next); err != nil {
		return errors.New("new password: " + err.Error())
	}
	return s.Store.SetPasswordHash(ctx, u.ID, HashPassword(next))
}
