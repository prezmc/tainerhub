package api

import (
	"testing"

	"github.com/prezmc/tainerhub/internal/auth"
)

// http.ServeMux panics on conflicting patterns when they're registered, so
// building the handler is itself the test.
func TestRoutesRegister(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("route registration panicked: %v", r)
		}
	}()
	(&Server{Auth: &auth.Service{}}).Handler()
}
