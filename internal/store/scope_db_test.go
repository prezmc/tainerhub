package store

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"
)

// TestHostScopeDB runs against a real, empty Postgres database when
// TAINERHUB_TEST_DATABASE_URL names one (it creates users, hosts and audit
// entries there), e.g. a throwaway postgres:17-alpine container. Without it
// the test is skipped.
func TestHostScopeDB(t *testing.T) {
	url := os.Getenv("TAINERHUB_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TAINERHUB_TEST_DATABASE_URL to an empty Postgres database to run")
	}
	ctx := context.Background()
	s, err := Open(ctx, url, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountUsers(ctx); n != 0 {
		t.Skip("database isn't empty")
	}
	owner, err := s.CreateFirstOwner(ctx, User{Username: "owner", DisplayName: "Owner", WebAuthnID: []byte("w0"), PasswordHash: "x"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if owner.Scoped || owner.Hosts != nil {
		t.Fatalf("owner: %+v", owner)
	}
	a, _ := s.CreateHost(ctx, "alpha")
	b, _ := s.CreateHost(ctx, "beta")

	// An invite limited to alpha makes a user limited to alpha.
	if _, err := s.CreateInvite(ctx, []byte("tok1"), "operator", "ops", owner.ID, time.Now().Add(time.Hour), true, []string{a.ID}); err != nil {
		t.Fatal(err)
	}
	inv, _ := s.PendingInvites(ctx)
	if len(inv) != 1 || !inv[0].Scoped || !slices.Equal(inv[0].Hosts, []string{a.ID}) {
		t.Fatalf("invite: %+v", inv)
	}
	op, err := s.AcceptInvite(ctx, []byte("tok1"), User{Username: "op", DisplayName: "Op", WebAuthnID: []byte("w1")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !op.Scoped || !slices.Equal(op.Hosts, []string{a.ID}) {
		t.Fatalf("joined: %+v", op)
	}
	// An unscoped invite makes an unscoped user.
	if _, err := s.CreateInvite(ctx, []byte("tok2"), "viewer", "", owner.ID, time.Now().Add(time.Hour), false, nil); err != nil {
		t.Fatal(err)
	}
	v, err := s.AcceptInvite(ctx, []byte("tok2"), User{Username: "v", DisplayName: "V", WebAuthnID: []byte("w2")}, nil)
	if err != nil || v.Scoped {
		t.Fatalf("unscoped invite: %+v %v", v, err)
	}

	// Sessions and API tokens load the scope.
	if err := s.CreateSession(ctx, []byte("sess"), op.ID, time.Now().Add(time.Hour), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, u, err := s.SessionUser(ctx, []byte("sess")); err != nil || !u.Scoped || !slices.Equal(u.Hosts, []string{a.ID}) {
		t.Fatalf("session user: %+v %v", u, err)
	}
	if _, err := s.CreateAPIToken(ctx, op.ID, "ci", "thp_x", "operator", []byte("apitok"), nil); err != nil {
		t.Fatal(err)
	}
	if _, u, err := s.APITokenUser(ctx, []byte("apitok")); err != nil || !u.Scoped || !slices.Equal(u.Hosts, []string{a.ID}) {
		t.Fatalf("token user: %+v %v", u, err)
	}

	// Changing the scope is conditional on role and scope as checked.
	if err := s.SetUserHosts(ctx, op.ID, "operator", false, false, nil); err != ErrChanged {
		t.Fatalf("stale scope: %v", err)
	}
	if err := s.SetUserHosts(ctx, op.ID, "operator", true, true, []string{a.ID, b.ID}); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.UserByID(ctx, op.ID); !slices.Equal(u.Hosts, []string{a.ID, b.ID}) {
		t.Fatalf("widened: %+v", u)
	}
	if err := s.SetUserHosts(ctx, op.ID, "operator", true, false, nil); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.UserByID(ctx, op.ID); u.Scoped {
		t.Fatalf("all hosts: %+v", u)
	}
	if err := s.SetUserHosts(ctx, owner.ID, "owner", false, true, []string{a.ID}); err == nil {
		t.Fatal("the owner was limited")
	}
	if err := s.SetUserRole(ctx, op.ID, "operator", true, "viewer", nil); err != ErrChanged {
		t.Fatalf("role change with stale scope: %v", err)
	}

	// The audit log, as seen by a user limited to alpha.
	uid := op.ID
	for _, e := range []AuditEntry{
		{Actor: "owner", Action: "container.restart", Target: "alpha/web", HostIDs: []string{a.ID}},
		{Actor: "owner", Action: "container.restart", Target: "beta/db", HostIDs: []string{b.ID}},
		{Actor: "owner", Action: "stack.move", Target: "app", HostIDs: []string{a.ID, b.ID}},
		{Actor: "owner", Action: "settings.changed"},
		{Actor: "op", UserID: &uid, Action: "login"},
	} {
		if err := s.Audit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	scoped := User{ID: op.ID, Scoped: true, Hosts: []string{a.ID}}
	got, err := s.ListAudit(ctx, 50, scoped)
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	for _, e := range got {
		targets = append(targets, e.Action+" "+e.Target)
	}
	slices.Sort(targets)
	if want := []string{"container.restart alpha/web", "login "}; !slices.Equal(targets, want) {
		t.Fatalf("scoped audit: %q, want %q", targets, want)
	}
	if all, _ := s.ListAudit(ctx, 50, owner); len(all) != 5 {
		t.Fatalf("full audit: %d entries", len(all))
	}
}
