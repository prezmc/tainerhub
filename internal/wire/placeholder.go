package wire

import (
	"regexp"
	"strings"
)

// Project READMEs show compose files with values meant to be replaced:
// "/path/to/data:/config", "/PATH_TO_YOUR_CONFIG", "PASSWORD=changeme",
// "<your-domain>". Deployed as they are, Docker creates the placeholder
// folder empty on the host, or the app runs with the sample password.
// web/src/placeholder.ts has the same rules for the browser.

var (
	placeholderAngle = regexp.MustCompile(`<[^<>]{1,60}>`)
	placeholderPath  = regexp.MustCompile(`(?i)(^|[^a-z])path[ _/-]?to([^a-z]|$)`)
	placeholderYour  = regexp.MustCompile(`(?i)(^|[^a-z])your([^a-z]|$)|(^|[^a-z])your(password|pass|domain|email|mail|token|key|secret|user|username|name|host|server|ip|path|dir|folder|config|data|media|timezone|api)`)
	placeholderWords = regexp.MustCompile(`(?i)(^|[^a-z])(change[_-]?me|change[_-]?this|replace[_-]?me|replace[_-]?with|replace[_-]?this|fill[_-]?me|insert[_-]?here|todo|xxxx+)([^a-z]|$)`)
)

// LooksLikePlaceholder says whether s looks like a sample value from
// documentation rather than a real one.
func LooksLikePlaceholder(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 500 {
		return false
	}
	return placeholderAngle.MatchString(s) || placeholderPath.MatchString(s) || placeholderYour.MatchString(s) || placeholderWords.MatchString(s)
}
