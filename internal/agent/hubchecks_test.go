package agent

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

func TestHubCheckCovers(t *testing.T) {
	var h hubCheckTimes
	now := time.Now()
	if h.covers(now, HubCheckCovers) {
		t.Fatal("no hub check yet: the agent's own check must run")
	}
	done := h.begin()
	if !h.covers(now, HubCheckCovers) {
		t.Fatal("a hub check is running: skip the local one")
	}
	done(false)
	if h.covers(now, HubCheckCovers) {
		t.Fatal("a failed hub check doesn't count")
	}
	h.begin()(true)
	if !h.covers(time.Now().Add(HubCheckCovers-time.Minute), HubCheckCovers) {
		t.Fatal("a hub check within 5 hours must cover the local one")
	}
	if h.covers(time.Now().Add(HubCheckCovers+time.Minute), HubCheckCovers) {
		t.Fatal("an older hub check must not stop the local one")
	}
}

func TestRunUpdateChecksSkipsWhenCovered(t *testing.T) {
	for _, skip := range []bool{true, false} {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		var n atomic.Int32
		runUpdateChecks(ctx, time.Millisecond, time.Millisecond, func() bool { return skip }, func(context.Context) { n.Add(1) })
		cancel()
		if skip && n.Load() != 0 {
			t.Fatalf("ran %d local checks while the hub's checks were recent", n.Load())
		}
		if !skip && n.Load() == 0 {
			t.Fatal("the local check must run when the hub hasn't checked")
		}
	}
}

// privateEngine is a fake Docker Engine with one container on a private
// image: its registry answers only with a login.
func privateEngine(t *testing.T) *SocketDocker {
	t.Helper()
	tmp, err := os.MkdirTemp("", "th")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	sock := filepath.Join(tmp, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/json":
			w.Write([]byte(`[{"Id":"abcdef1234567890","Names":["/app"],"Image":"ghcr.io/org/app:1","State":"running"}]`))
		case strings.HasPrefix(r.URL.Path, "/images/"):
			w.Write([]byte(`{"RepoDigests":["ghcr.io/org/app@sha256:old"]}`))
		case strings.HasPrefix(r.URL.Path, "/distribution/"):
			if r.Header.Get("X-Registry-Auth") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"message":"unauthorized: authentication required"}`))
				return
			}
			w.Write([]byte(`{"Descriptor":{"Digest":"sha256:new"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return NewSocketDocker(sock)
}

// A check the hub sends (with logins) finds the private image's update; the
// agent's own check (no logins) then can't see it, and must neither clear
// the badge nor run at all while the hub's check is recent.
func TestHubCheckResultSurvivesLocalCheck(t *testing.T) {
	d := privateEngine(t)
	ctx := context.Background()
	res, err := d.Operate(ctx, wire.Operation{Kind: wire.OpImagesCheckUpdates, Auth: []wire.RegistryAuth{ghcrLogin}})
	if err != nil || !res.(wire.ImageUpdates).Images["ghcr.io/org/app:1"] {
		t.Fatalf("hub check = %+v, %v", res, err)
	}
	if !d.hubChecks.covers(time.Now(), HubCheckCovers) {
		t.Fatal("the hub's check must be recorded")
	}
	local, err := d.checkUpdates(ctx, nil)
	if err != nil || local.Errors["ghcr.io/org/app:1"] == "" {
		t.Fatalf("without a login the registry must refuse: %+v, %v", local, err)
	}
	if err := d.refreshUpdates(ctx, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := d.List(ctx)
	if err != nil || len(cs) != 1 || !cs[0].UpdateAvailable {
		t.Fatalf("after an uncredentialed check: %+v, %v", cs, err)
	}
}
