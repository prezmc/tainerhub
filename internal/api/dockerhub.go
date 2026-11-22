package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Docker Hub search and tag lists, fetched by the hub so the browser never
// talks to Docker Hub (the UI's Content-Security-Policy only allows 'self').
// Results are cached briefly; nothing here needs an account.

var dockerHubBase = "https://hub.docker.com" // tests point this elsewhere

type hubRepo struct {
	Name        string `json:"name"` // e.g. "nginx" (official) or "linuxserver/plex"
	Description string `json:"description"`
	Stars       int    `json:"stars"`
	Pulls       int64  `json:"pulls"`
	Official    bool   `json:"official"`
}

type hubTag struct {
	Name    string    `json:"name"`
	Updated time.Time `json:"updated"`
	Size    int64     `json:"size"`
}

type hubCache struct {
	mu sync.Mutex
	m  map[string]hubCacheEntry
}

type hubCacheEntry struct {
	at   time.Time
	data any
}

func (c *hubCache) get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || time.Since(e.at) > 5*time.Minute {
		return nil, false
	}
	return e.data, true
}

func (c *hubCache) put(key string, data any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) > 500 {
		c.m = map[string]hubCacheEntry{}
	}
	c.m[key] = hubCacheEntry{at: time.Now(), data: data}
}

var hubClient = &http.Client{Timeout: 10 * time.Second}

func hubGet(ctx context.Context, path string, q url.Values, v any) error {
	u := dockerHubBase + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "TainerHub")
	resp, err := hubClient.Do(req)
	if err != nil {
		return errors.New("couldn't reach Docker Hub from the hub server")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errNotOnHub
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Docker Hub answered %s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(v)
}

var errNotOnHub = errors.New("not found on Docker Hub")

// repoPart matches one path component of a Docker Hub repository name.
var repoPart = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// hubRepoPath turns "nginx" into "library/nginx" and checks "owner/name".
func hubRepoPath(repo string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(repo), "/")
	if len(parts) == 1 {
		parts = []string{"library", parts[0]}
	}
	if len(parts) != 2 || !repoPart.MatchString(parts[0]) || !repoPart.MatchString(parts[1]) || len(repo) > 255 {
		return "", false
	}
	return parts[0] + "/" + parts[1], true
}

func (s *Server) hubSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || len(q) > 100 {
		jsonErr(w, http.StatusBadRequest, "enter 1-100 characters to search for")
		return
	}
	key := "s:" + strings.ToLower(q)
	if v, ok := s.hub.get(key); ok {
		writeJSON(w, http.StatusOK, v)
		return
	}
	var raw struct {
		Results []struct {
			RepoName         string `json:"repo_name"`
			ShortDescription string `json:"short_description"`
			StarCount        int    `json:"star_count"`
			PullCount        int64  `json:"pull_count"`
			IsOfficial       bool   `json:"is_official"`
		} `json:"results"`
	}
	if err := hubGet(r.Context(), "/v2/search/repositories/", url.Values{"query": {q}, "page_size": {"25"}}, &raw); err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	out := []hubRepo{}
	for _, x := range raw.Results {
		name := strings.TrimPrefix(x.RepoName, "library/")
		if _, ok := hubRepoPath(name); !ok {
			continue
		}
		out = append(out, hubRepo{Name: name, Description: x.ShortDescription, Stars: x.StarCount, Pulls: x.PullCount, Official: x.IsOfficial})
	}
	// Docker Hub orders by text relevance, which often puts repackagings above
	// the real thing. Official images first, then the most pulled.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Official != out[j].Official {
			return out[i].Official
		}
		return out[i].Pulls > out[j].Pulls
	})
	s.hub.put(key, out)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) hubTags(w http.ResponseWriter, r *http.Request) {
	path, ok := hubRepoPath(r.URL.Query().Get("repo"))
	if !ok {
		jsonErr(w, http.StatusBadRequest, "invalid repository name")
		return
	}
	key := "t:" + path
	if v, ok := s.hub.get(key); ok {
		writeJSON(w, http.StatusOK, v)
		return
	}
	var raw struct {
		Results []struct {
			Name        string    `json:"name"`
			LastUpdated time.Time `json:"last_updated"`
			FullSize    int64     `json:"full_size"`
		} `json:"results"`
	}
	err := hubGet(r.Context(), "/v2/repositories/"+path+"/tags", url.Values{"page_size": {"50"}, "ordering": {"last_updated"}}, &raw)
	if errors.Is(err, errNotOnHub) {
		jsonErr(w, http.StatusNotFound, "that image isn't on Docker Hub")
		return
	}
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	out := []hubTag{}
	for _, x := range raw.Results {
		out = append(out, hubTag{Name: x.Name, Updated: x.LastUpdated, Size: x.FullSize})
	}
	s.hub.put(key, out)
	writeJSON(w, http.StatusOK, out)
}
