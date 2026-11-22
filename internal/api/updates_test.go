package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLatestAndIsCurrent(t *testing.T) {
	calls := 0
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !strings.Contains(r.URL.Path, "/actions/workflows/ci.yml/runs") || r.URL.Query().Get("status") != "success" || r.URL.Query().Get("branch") != "main" {
			t.Errorf("unexpected request %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"workflow_runs":[{"head_sha":"1a2b3c4d5e6f","display_title":"Add a thing\n\nlong body","updated_at":"2026-11-22T10:00:00Z"}]}`))
	}))
	defer fake.Close()
	old := githubAPI
	githubAPI = fake.URL
	defer func() { githubAPI = old }()

	var c latestCache
	l, err := c.get(context.Background())
	if err != nil || l.Version != "0.1.0-1a2b3c4" || l.Title != "Add a thing" {
		t.Fatalf("latest = %+v, %v", l, err)
	}
	_, _ = c.get(context.Background())
	if calls != 1 {
		t.Fatalf("expected the cache to answer, GitHub called %d times", calls)
	}
	for v, want := range map[string]bool{"0.1.0-1a2b3c4": true, "0.1.0-1a2b3c4d": true, "0.1.0-9999999": false, "dev": false, "": false} {
		if got := isCurrent(v, l); got != want {
			t.Errorf("isCurrent(%q) = %v", v, got)
		}
	}
	if !isCurrent("dev", nil) {
		t.Error("without a known latest version, don't claim an update is needed")
	}
}
