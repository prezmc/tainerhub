package api

import (
	"strings"

	"github.com/prezmc/tainerhub/internal/wire"
)

// hiddenValue replaces values a non-admin may not see.
const hiddenValue = "•••"

// safeLabelPrefixes are label namespaces written by Docker Compose and image
// builders. Their values are names, paths and versions, never credentials,
// and the UI is far less useful without them.
var safeLabelPrefixes = []string{"com.docker.compose.", "org.opencontainers.image."}

// redactForViewer hides the parts of a container's details that often carry
// secrets, for users below admin: environment values, label values (reverse
// proxy labels can hold basic-auth hashes or API tokens) and command-line
// arguments (--password=..., -p secret). Names and the program being run
// stay visible, so viewers can still tell what the container is.
func redactForViewer(d *wire.ContainerDetail) {
	for i, e := range d.Env {
		if k, _, ok := strings.Cut(e, "="); ok {
			d.Env[i] = k + "="
		}
	}
	for k := range d.Labels {
		if !hasSafeLabelPrefix(k) {
			d.Labels[k] = hiddenValue
		}
	}
	// Keep only argv[0]: the entrypoint's first element, or the command's
	// when there's no entrypoint.
	keep := 1
	for i := range d.Entrypoint {
		if i >= keep {
			d.Entrypoint[i] = hiddenValue
		}
	}
	if len(d.Entrypoint) > 0 {
		keep = 0
	}
	for i := range d.Cmd {
		if i >= keep {
			d.Cmd[i] = hiddenValue
		}
	}
}

func hasSafeLabelPrefix(k string) bool {
	for _, p := range safeLabelPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}
