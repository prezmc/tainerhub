package auth

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"unicode"

	"github.com/prezmc/tainerhub/internal/store"
)

// Guessing limits per account. The per-IP limit (Service.Limiter) stops one
// source; this one stops many sources (a botnet) guessing one account's
// password or recovery codes. Passkeys aren't counted or blocked, so
// someone flooding an account's name can't lock its owner out of a passkey
// sign-in.
//
// Attempts through the trusted reverse proxy (the internet, usually) and
// direct ones (the LAN address) are counted apart: guesses from outside
// can't lock anyone out of the LAN address.
const accountMaxFails = 10

// errAccountLimited is the refusal once an account has too many recent
// failures. It reads the same for every name, known or not.
var errAccountLimited error = rateLimitedError{"too many failed sign-ins for this account; wait 15 minutes, or sign in with a passkey"}

// ErrPasskeyRequired: on the secure address, an account with a passkey must
// use it (see SetPasskeyRequired).
var ErrPasskeyRequired = errors.New("this account signs in with its passkey on this address. Use your passkey or a recovery code (passwords still work on the LAN address)")

// FromTrustedProxy reports whether the request's direct peer is a trusted
// reverse proxy.
func (s *Service) FromTrustedProxy(r *http.Request) bool {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	return err == nil && s.isTrusted(ap.Addr().Unmap())
}

// accountKey is the account limiter's key for a sign-in attempt.
func (s *Service) accountKey(r *http.Request, username string) string {
	via := "direct"
	if s.FromTrustedProxy(r) {
		via = "proxy"
	}
	return "acct:" + via + ":" + strings.ToLower(strings.TrimSpace(username))
}

// reserveSignIn takes one attempt from the client's IP and one from the
// named account. On success the caller must call release if the attempt
// turns out not to be a failure.
func (s *Service) reserveSignIn(r *http.Request, username string) (release func(), err error) {
	ip := s.ClientIP(r)
	if !s.Limiter.Reserve(ip) {
		return nil, ErrRateLimited
	}
	if s.AccountLimiter == nil {
		return func() { s.Limiter.Release(ip) }, nil
	}
	key := s.accountKey(r, username)
	if !s.AccountLimiter.Reserve(key) {
		s.Limiter.Release(ip) // no guess was made
		return nil, errAccountLimited
	}
	return func() {
		s.Limiter.Release(ip)
		s.AccountLimiter.Release(key)
	}, nil
}

// SetPasskeyRequired turns on or off the rule that, on the secure address,
// accounts with a passkey can't sign in with a password.
func (s *Service) SetPasskeyRequired(on bool) { s.passkeyRequired.Store(on) }

// PasskeyRequired reports whether that rule is on.
func (s *Service) PasskeyRequired() bool { return s.passkeyRequired.Load() }

// passwordRefused reports whether u may not use a password for this request:
// the rule is on, passkeys work, the request came through the proxy over
// HTTPS (the secure address), and u has a passkey. Users without a passkey
// keep their password, so they can sign in and add one.
func (s *Service) passwordRefused(ctx context.Context, r *http.Request, u store.User) (bool, error) {
	if !s.PasskeyRequired() || !s.PasskeysEnabled() || !s.FromTrustedProxy(r) || !s.IsHTTPS(r) {
		return false, nil
	}
	return s.st().HasPasskey(ctx, u.ID)
}

// commonPasswords are frequently used passwords of the minimum length or
// more, from public breach lists. Compared case-insensitively.
var commonPasswords = func() map[string]bool {
	m := map[string]bool{}
	for _, p := range strings.Fields(`
		1234567890 0123456789 0987654321 1111111111 0000000000 1234512345
		12345678910 123456789a a123456789 123456789q q123456789 1234567890a
		1q2w3e4r5t 1q2w3e4r5t6y 1qaz2wsx3edc zaq12wsxcde zaq1zaq1zaq1 qazwsxedcrfv
		qwertyuiop qwerty1234 qwerty12345 qwerty123456 qwertyuiop1 asdfghjkl1
		asdfghjkl; zxcvbnm123 abcdefghij abcd123456 abc1234567 abcdefg123
		password12 password123 password1234 password12345 password123! password!1
		passw0rd123 p@ssword123 p@ssw0rd123 passwordpassword mypassword1 mypassword123
		iloveyou123 iloveyou12 iloveyou1234 letmein123 letmein1234 welcome123
		welcome1234 welcome2024 welcome2025 welcome2026 changeme123 changeme1234
		administrator admin12345 admin123456 adminadmin admin@1234 administrator1
		superman123 batman1234 football123 baseball123 basketball soccer1234
		princess123 sunshine123 starwars123 pokemon123 dragon1234 monkey1234
		trustno1234 whatever123 computer123 internet123 michael123 jennifer123
		homelab123 docker1234 docker12345 dockerdocker portainer1 portainer123
		tainerhub1 tainerhub123 tainerhub1234 raspberry1 raspberrypi
		summer2024 summer2025 summer2026 winter2024 winter2025 winter2026
		spring2025 spring2026 autumn2025 autumn2026 january2026 december2025
		1qazxsw23edc 1qaz2wsx3e 123qweasdzxc qweasdzxc123 q1w2e3r4t5 a1b2c3d4e5
	`) {
		m[p] = true
	}
	return m
}()

// weakPassword says why p is too easy to guess, or "" if it isn't.
// username may be empty.
func weakPassword(p, username string) string {
	lp := strings.ToLower(p)
	if commonPasswords[lp] || commonPasswords[strings.TrimRight(lp, "!.?")] {
		return "that password is on lists of common passwords; pick another"
	}
	if u := strings.ToLower(strings.TrimSpace(username)); len(u) >= 3 && strings.Contains(lp, u) {
		return "don't use your username in your password"
	}
	distinct := map[rune]bool{}
	for _, r := range lp {
		distinct[r] = true
	}
	if len(distinct) < 4 {
		return "use more different characters"
	}
	if sequential(lp) {
		return "that password is a simple sequence; pick another"
	}
	return ""
}

// sequential reports whether p is a run of consecutive characters (1234...,
// abcd..., or backwards), the kind of thing guessed first.
func sequential(p string) bool {
	rs := []rune(p)
	if len(rs) < 2 {
		return false
	}
	step := rs[1] - rs[0]
	if step != 1 && step != -1 {
		return false
	}
	for i := 1; i < len(rs); i++ {
		if rs[i]-rs[i-1] != step || !(unicode.IsDigit(rs[i]) || unicode.IsLetter(rs[i])) {
			return false
		}
	}
	return true
}
