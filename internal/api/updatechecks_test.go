package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

// The hub's scheduled image update checks are sent like "Check for
// updates": the hook attaches the logins for the registries the host's
// containers use, Docker Hub's included (its pull rate limit is per account).
func TestScheduledCheckCarriesLogins(t *testing.T) {
	x := newRegHarness(t)
	for _, b := range []string{
		`{"registry":"ghcr.io","username":"octo","password":"` + ghcrPAT + `"}`,
		`{"registry":"docker.io","username":"me","password":"dckr_pat_1"}`,
		`{"registry":"quay.io","username":"q","password":"quay_1"}`,
	} {
		if w := x.do("POST", "/api/registries", b); w.Code != http.StatusCreated {
			t.Fatalf("add: %d %s", w.Code, w.Body)
		}
	}
	x.s.Reg.AddAuth = x.s.AddRegistryAuth
	a := x.s.Reg.Attach("h1", "host1", func() {})
	a.Handle(wire.AgentMessage{Snapshot: &wire.Snapshot{Containers: []wire.Container{
		{ID: "1", Name: "web", Image: "nginx:1.27"},
		{ID: "2", Name: "app", Image: "ghcr.io/org/app:1"},
	}}}, nil)

	got := make(chan *wire.Operation, 1)
	go func() {
		for m := range a.Out() {
			if c := m.Command; c != nil && c.Op != nil {
				got <- c.Op
				a.Handle(wire.AgentMessage{Result: &wire.CommandResult{CommandID: c.ID, OK: true}}, nil)
				return
			}
		}
	}()
	// What the scheduler sends (registry.UpdateChecker).
	if _, err := a.Operate(context.Background(), wire.Operation{Kind: wire.OpImagesCheckUpdates}); err != nil {
		t.Fatal(err)
	}
	op := <-got
	var regs []string
	for _, au := range op.Auth {
		regs = append(regs, au.Registry)
	}
	if strings.Join(regs, ",") != "docker.io,ghcr.io" {
		t.Fatalf("check carried logins for %v, want docker.io and ghcr.io", regs)
	}

	// The host list shows when it was checked.
	vs := x.s.hostViewsFrom([]store.Host{{ID: "h1", Name: "host1"}})
	if vs[0].ImagesCheckedAt == nil || time.Since(*vs[0].ImagesCheckedAt) > time.Minute {
		t.Fatalf("imagesCheckedAt = %v", vs[0].ImagesCheckedAt)
	}
}
