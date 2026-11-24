package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/prezmc/tainerhub/internal/store"
)

// API tokens let scripts call the API with "Authorization: Bearer thp_...".
// A request that carries a bearer token is authenticated by that token
// alone: cookies are ignored entirely, so the CSRF checks that exist because
// of cookies don't apply to it (a cross-site page can't set the header
// without a CORS preflight, which the hub never answers).

// TokenPrefix starts every API token, so leaked ones are easy to grep for.
const TokenPrefix = "thp_"

// RoleRank orders roles; a higher rank can do everything a lower one can.
var RoleRank = map[string]int{"viewer": 1, "operator": 2, "admin": 3, "owner": 4}

var (
	// ErrBadToken: the bearer token is unknown, revoked or expired.
	ErrBadToken = errors.New("invalid or expired API token")
	// ErrTokenAuth: the endpoint needs a real sign-in, not an API token,
	// so a leaked token can't be turned into lasting access.
	ErrTokenAuth = errors.New("not available with an API token; sign in to the web UI")
)

// TokenInfo identifies the API token a request was authenticated with.
type TokenInfo struct {
	ID   string
	Name string
}

type tokenCtxKey struct{}

// WithToken marks ctx as authenticated by the token t.
func WithToken(ctx context.Context, t *TokenInfo) context.Context {
	return context.WithValue(ctx, tokenCtxKey{}, t)
}

// TokenFrom returns the API token ctx was authenticated with, or nil for a
// browser session.
func TokenFrom(ctx context.Context) *TokenInfo {
	t, _ := ctx.Value(tokenCtxKey{}).(*TokenInfo)
	return t
}

// bearer returns the value of an "Authorization: Bearer ..." header.
func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return "", false
	}
	return strings.TrimSpace(h[7:]), true
}

// HasBearer reports whether r carries a bearer token, in which case it is
// authenticated by that token only and its cookies are ignored.
func HasBearer(r *http.Request) bool {
	_, ok := bearer(r)
	return ok
}

// NewAPIToken returns a new token, its display prefix and its hash.
func NewAPIToken() (token, prefix string, hash []byte) {
	body := randToken(32)
	token = TokenPrefix + body
	return token, body[:8], Hash(token)
}

// lowerRole is the less powerful of two roles.
func lowerRole(a, b string) string {
	if RoleRank[a] <= RoleRank[b] {
		return a
	}
	return b
}

// tokenTouchEvery limits last-used updates to one write per token a minute.
const tokenTouchEvery = time.Minute

// Authenticate identifies the caller: by API token when the request carries
// one, otherwise by session cookie. info is non-nil for token requests.
func (s *Service) Authenticate(r *http.Request) (store.User, *TokenInfo, error) {
	if tok, ok := bearer(r); ok {
		return s.userFromToken(r, tok)
	}
	u, err := s.userFromSession(r)
	return u, nil, err
}

// userFromToken checks a bearer token. A wrong token counts as a failed
// sign-in for the client's IP, so tokens can't be guessed at speed (they
// have 256 bits anyway, but this also stops a misconfigured script from
// hammering the database).
//
// The limit only applies to bad tokens: a valid token always works, even
// from an address that has been sending bad ones. Behind Docker's port
// forwarding every client can share one address, and a 256-bit token can't
// be guessed anyway, so blocking good tokens would only let anyone lock out
// everyone's scripts. Bad attempts are counted, and once over the limit they
// get 429 instead of 401.
func (s *Service) userFromToken(r *http.Request, tok string) (store.User, *TokenInfo, error) {
	ip := s.ClientIP(r)
	fail := func() (store.User, *TokenInfo, error) {
		if s.Limiter != nil && !s.Limiter.Reserve(ip) {
			return store.User{}, nil, ErrRateLimited
		}
		return store.User{}, nil, ErrBadToken
	}
	if !strings.HasPrefix(tok, TokenPrefix) || len(tok) > 128 {
		return fail()
	}
	ctx := r.Context()
	t, u, err := s.st().APITokenUser(ctx, Hash(tok))
	if errors.Is(err, store.ErrNotFound) {
		return fail()
	}
	if err != nil {
		return store.User{}, nil, err
	}
	now := time.Now()
	if t.ExpiresAt != nil && !now.Before(*t.ExpiresAt) {
		return fail()
	}
	// The token never acts above its owner's current role, so demoting a
	// user also demotes their tokens.
	u.Role = lowerRole(t.Role, u.Role)
	if t.LastUsedAt == nil || now.Sub(*t.LastUsedAt) >= tokenTouchEvery {
		if err := s.st().TouchAPIToken(ctx, t.ID, ip); err != nil && s.Log != nil {
			s.Log.Warn("record API token use", "err", err)
		}
	}
	return u, &TokenInfo{ID: t.ID, Name: t.Name}, nil
}
