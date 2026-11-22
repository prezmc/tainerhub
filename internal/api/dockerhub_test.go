package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHubRepoPath(t *testing.T) {
	for in, want := range map[string]string{"nginx": "library/nginx", "linuxserver/plex": "linuxserver/plex", "a.b-c/d_e": "a.b-c/d_e"} {
		if got, ok := hubRepoPath(in); !ok || got != want {
			t.Errorf("%q: got %q %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "../x", "a/b/c", "UPPER", "a/../b", "x?y=1", "a//b"} {
		if _, ok := hubRepoPath(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestHubSearchAndTags(t *testing.T) {
	calls := 0
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/v2/search/repositories/":
			_, _ = w.Write([]byte(`{"results":[{"repo_name":"library/nginx","short_description":"web","star_count":5,"pull_count":9,"is_official":true},{"repo_name":"bad name","short_description":"x"}]}`))
		case "/v2/repositories/library/nginx/tags":
			_, _ = w.Write([]byte(`{"results":[{"name":"1.27","last_updated":"2026-11-01T00:00:00Z","full_size":123}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	old := dockerHubBase
	dockerHubBase = fake.URL
	defer func() { dockerHubBase = old }()

	s := &Server{}
	rec := httptest.NewRecorder()
	s.hubSearch(rec, httptest.NewRequest("GET", "/api/dockerhub/search?q=nginx", nil))
	var repos []hubRepo
	_ = json.Unmarshal(rec.Body.Bytes(), &repos)
	if rec.Code != 200 || len(repos) != 1 || repos[0].Name != "nginx" || !repos[0].Official {
		t.Fatalf("search: %d %s", rec.Code, rec.Body)
	}
	s.hubSearch(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/dockerhub/search?q=NGINX", nil))
	if calls != 1 {
		t.Fatalf("expected a cache hit, Docker Hub called %d times", calls)
	}

	rec = httptest.NewRecorder()
	s.hubTags(rec, httptest.NewRequest("GET", "/api/dockerhub/tags?repo=nginx", nil))
	var tags []hubTag
	_ = json.Unmarshal(rec.Body.Bytes(), &tags)
	if rec.Code != 200 || len(tags) != 1 || tags[0].Name != "1.27" {
		t.Fatalf("tags: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	s.hubTags(rec, httptest.NewRequest("GET", "/api/dockerhub/tags?repo=nobody/nothing", nil))
	if rec.Code != 404 {
		t.Fatalf("missing repo: %d", rec.Code)
	}
}
