// Package regcred handles logins for private container registries: naming
// registries the way Docker does, picking which logins a command needs,
// encrypting them at rest on the hub, and handing them to Docker on agents
// without leaving them behind.
package regcred

import (
	"errors"
	"regexp"
	"sort"
	"strings"

	"github.com/prezmc/tainerhub/internal/wire"
)

// DockerHub is how Docker Hub is named everywhere in TainerHub.
const DockerHub = "docker.io"

// dockerHubAliases are other names people (and Docker) use for Docker Hub.
var dockerHubAliases = map[string]bool{
	"docker.io": true, "index.docker.io": true, "registry-1.docker.io": true,
	"registry.hub.docker.com": true, "hub.docker.com": true,
}

// NormalizeHost turns what an admin typed ("https://GHCR.io/", "ghcr.io/org",
// "index.docker.io") into the registry's host as Docker matches it
// ("ghcr.io", "docker.io"), or an error if it isn't a registry host.
func NormalizeHost(s string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(s))
	for _, p := range []string{"https://", "http://"} {
		h = strings.TrimPrefix(h, p)
	}
	h, _, _ = strings.Cut(h, "/")
	if strings.Contains(h, "@") {
		return "", errors.New("enter just the registry's address, without a username")
	}
	if dockerHubAliases[strings.TrimSuffix(h, ":443")] {
		return DockerHub, nil
	}
	h = strings.TrimSuffix(h, ":443") // the default port: Docker leaves it off too
	if !wire.ValidRegistryHost(h) {
		return "", errors.New("enter a registry address like ghcr.io, registry.gitlab.com or myregistry.lan:5000")
	}
	return h, nil
}

// ImageHost is the registry an image reference pulls from, by Docker's
// rule: the first path part is a registry only if it has a '.' or a ':' or
// is "localhost"; otherwise the image is on Docker Hub ("nginx",
// "library/nginx", "bitnami/redis:7").
func ImageHost(ref string) string {
	ref = strings.TrimSpace(ref)
	ref, _, _ = strings.Cut(ref, "@") // a digest names the image, not the registry
	first, rest, ok := strings.Cut(ref, "/")
	if !ok || rest == "" || !(strings.ContainsAny(first, ".:") || first == "localhost") {
		return DockerHub
	}
	h := strings.ToLower(first)
	if dockerHubAliases[h] {
		return DockerHub
	}
	return h
}

// imageLine matches an `image:` key in a compose file, in block style.
var imageLine = regexp.MustCompile(`^\s*(?:-\s+)?image\s*:\s*(.*)$`)

// ComposeImages lists the images a compose file names on its `image:`
// lines, with ${VAR} / $VAR filled in from the stack's .env. It's a best
// effort reading (not a YAML parser): an image whose registry part still
// holds a variable is left out, since its registry can't be known.
func ComposeImages(compose, env string) []string {
	vars := parseEnv(env)
	var out []string
	for _, line := range strings.Split(compose, "\n") {
		m := imageLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v := strings.TrimSpace(m[1])
		if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		v = strings.Trim(v, `"'`)
		v = interpolate(v, vars)
		if v == "" || strings.ContainsAny(v, "${} ") {
			continue
		}
		out = append(out, v)
	}
	return out
}

// parseEnv reads KEY=value lines from a .env file.
func parseEnv(env string) map[string]string {
	vars := map[string]string{}
	for _, line := range strings.Split(env, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		vars[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return vars
}

var varRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?:(:?-)([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// interpolate fills in $VAR, ${VAR}, ${VAR:-default} and ${VAR-default}.
// Unknown variables without a default are left as they are.
func interpolate(s string, vars map[string]string) string {
	return varRef.ReplaceAllStringFunc(s, func(m string) string {
		p := varRef.FindStringSubmatch(m)
		name := p[1] + p[4]
		v, set := vars[name]
		switch {
		case p[2] == ":-" && v == "":
			return p[3]
		case p[2] == "-" && !set:
			return p[3]
		case set:
			return v
		}
		return m
	})
}

// Hosts is the sorted set of registries the images pull from.
func Hosts(images []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, img := range images {
		if img == "" || strings.HasPrefix(img, "sha256:") {
			continue // an image ID is already local: nothing to pull
		}
		if h := ImageHost(img); !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}
