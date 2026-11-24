package regcred

import (
	"path"
	"regexp"
	"strings"

	"github.com/prezmc/tainerhub/internal/wire"
)

// dockerfileLine matches a `dockerfile:` key under a compose build: section.
var dockerfileLine = regexp.MustCompile(`^\s*dockerfile\s*:\s*(.*)$`)

// isDockerfile says whether a stack file is a Dockerfile: by its name
// (Dockerfile, *.Dockerfile, Dockerfile.*), or because the compose file
// names it in a `dockerfile:` key.
func isDockerfile(p string, named []string) bool {
	base := path.Base(p)
	if base == "Dockerfile" || strings.HasSuffix(base, ".Dockerfile") || strings.HasPrefix(base, "Dockerfile.") {
		return true
	}
	for _, n := range named {
		if p == n || strings.HasSuffix(p, "/"+n) {
			return true
		}
	}
	return false
}

// BuildImages lists the base images (FROM lines) of the stack's Dockerfiles,
// so a build gets logins for private base images. Like ComposeImages it's a
// best-effort reading.
func BuildImages(compose, env string, files []wire.StackFile) []string {
	vars := parseEnv(env)
	var named []string
	for _, line := range strings.Split(compose, "\n") {
		m := dockerfileLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v := strings.TrimSpace(m[1])
		if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		v = path.Clean(strings.TrimPrefix(interpolate(strings.Trim(v, `"'`), vars), "./"))
		if v != "" && v != "." && !strings.Contains(v, "$") {
			named = append(named, v)
		}
	}
	var out []string
	for _, f := range files {
		if isDockerfile(f.Path, named) {
			out = append(out, DockerfileImages(string(f.Content))...)
		}
	}
	return out
}

// DockerfileImages lists the images a Dockerfile's FROM lines pull. ARGs
// declared before the first FROM fill in ${VAR} with their defaults; a
// stage name (FROM x AS build, then FROM build) and "scratch" pull nothing;
// an image still holding a variable is left out.
func DockerfileImages(dockerfile string) []string {
	args := map[string]string{}
	stages := map[string]bool{}
	seenFrom := false
	var out []string
	for _, line := range dockerfileInstructions(dockerfile) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "ARG":
			if seenFrom {
				continue // only ARGs before the first FROM apply to FROM lines
			}
			for _, a := range fields[1:] {
				k, v, _ := strings.Cut(a, "=")
				args[k] = strings.Trim(v, `"'`)
			}
		case "FROM":
			seenFrom = true
			rest := fields[1:]
			for len(rest) > 0 && strings.HasPrefix(rest[0], "--") {
				rest = rest[1:] // --platform=...
			}
			if len(rest) == 0 {
				continue
			}
			img := interpolate(rest[0], args)
			earlierStage := stages[strings.ToLower(img)]
			if len(rest) >= 3 && strings.EqualFold(rest[1], "AS") {
				stages[strings.ToLower(rest[2])] = true
			}
			if img == "" || earlierStage || strings.ContainsAny(img, "${}") || strings.EqualFold(img, "scratch") {
				continue
			}
			out = append(out, img)
		}
	}
	return out
}

// dockerfileInstructions splits a Dockerfile into instructions: comments
// dropped, lines ending in '\' joined with the next.
func dockerfileInstructions(s string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasSuffix(t, `\`) {
			cur.WriteString(strings.TrimSuffix(t, `\`) + " ")
			continue
		}
		cur.WriteString(t)
		out = append(out, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
