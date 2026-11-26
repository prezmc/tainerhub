package auth

import (
	"strings"
	"testing"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	h := HashPassword("correct horse battery")
	if !strings.HasPrefix(h, "$argon2id$v=19$") {
		t.Fatalf("unexpected encoding %q", h)
	}
	if !CheckPassword(h, "correct horse battery") {
		t.Fatal("right password rejected")
	}
	if CheckPassword(h, "correct horse battery!") || CheckPassword(h, "") {
		t.Fatal("wrong password accepted")
	}
	if HashPassword("same") == HashPassword("same") {
		t.Fatal("hashes must be salted")
	}
	for _, bad := range []string{"", "plain", "$argon2id$v=19$m=0,t=2,p=1$aa$bb", "$argon2i$v=19$m=19456,t=2,p=1$aa$bb", "$argon2id$v=19$m=99999999,t=2,p=1$aa$bb"} {
		if CheckPassword(bad, "x") {
			t.Fatalf("malformed hash %q accepted", bad)
		}
	}
}

func TestPasswordPolicy(t *testing.T) {
	if checkPasswordPolicy("short", "") == nil {
		t.Fatal("short password accepted")
	}
	if checkPasswordPolicy(strings.Repeat("x", maxPassword+1), "") == nil {
		t.Fatal("overlong password accepted")
	}
	if err := checkPasswordPolicy("long enough pw", "alice"); err != nil {
		t.Fatal(err)
	}
	for _, weak := range []string{"Password123", "password123!", "QWERTYUIOP", "1234567890", "abcdefghijk", "9876543210", "aaaaaaaaaaaa", "abababababab", "alice-secret-99"} {
		if checkPasswordPolicy(weak, "Alice") == nil {
			t.Errorf("weak password %q accepted", weak)
		}
	}
	for _, ok := range []string{"correct horse battery", "Tr0ub4dor&3x", "my cat eats 3 socks"} {
		if err := checkPasswordPolicy(ok, "alice"); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
}
