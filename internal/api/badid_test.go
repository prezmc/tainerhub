package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Malformed host ids must be answered 404 before reaching the database,
// where the ::uuid cast would fail and show as a 500. The Server has no
// Store here, so any database call would panic.
func TestHostHandlersRejectMalformedIDs(t *testing.T) {
	s := &Server{}
	for name, h := range map[string]http.HandlerFunc{
		"deleteHost":   s.deleteHost,
		"newHostToken": s.newHostToken,
		"renameHost":   s.renameHost,
		"statsSource":  s.statsSource,
	} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"x","source":"docker"}`))
		r.SetPathValue("id", "not-a-uuid")
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, w.Code)
		}
	}
}
