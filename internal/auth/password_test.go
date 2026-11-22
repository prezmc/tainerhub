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
	if checkPasswordPolicy("short") == nil {
		t.Fatal("short password accepted")
	}
	if checkPasswordPolicy(strings.Repeat("x", maxPassword+1)) == nil {
		t.Fatal("overlong password accepted")
	}
	if err := checkPasswordPolicy("long enough pw"); err != nil {
		t.Fatal(err)
	}
}
