package wire

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// StackFile is one of a stack's files besides compose.yaml and .env: a
// Dockerfile, a script, a config file. Content is plain UTF-8 text; it's
// []byte so it travels base64-encoded, whose size is predictable (JSON
// string escaping can grow some text six-fold).
type StackFile struct {
	Path       string `json:"path"` // relative, '/'-separated
	Content    []byte `json:"content"`
	Executable bool   `json:"executable,omitempty"`
}

// Limits for a stack's extra files.
const (
	MaxStackFiles       = 30
	MaxStackFileBytes   = 256 << 10
	MaxStackFilesTotal  = 2 << 20
	MaxStackFilePath    = 200
	MaxStackFileDepth   = 4
	StackManifestPrefix = ".tainerhub" // reserved: staging folders, the manifest
)

// reservedStackFiles have their own fields (or are where compose looks for
// them), so they can't also be extra files.
var reservedStackFiles = map[string]bool{
	"compose.yaml": true, "compose.yml": true,
	"docker-compose.yaml": true, "docker-compose.yml": true,
	".env": true,
}

// ValidStackFilePath checks a stack file's path. The agent joins it onto
// the stack folder, so anything that could step outside it, or onto
// TainerHub's own files there, is refused.
func ValidStackFilePath(p string) error {
	switch {
	case p == "":
		return errors.New("the name is empty")
	case len(p) > MaxStackFilePath:
		return fmt.Errorf("the path is longer than %d characters", MaxStackFilePath)
	case strings.HasPrefix(p, "/"):
		return errors.New("use a path relative to the stack's folder (no leading '/')")
	case reservedStackFiles[p]:
		return errors.New("this file has its own tab: edit it there")
	}
	segs := strings.Split(p, "/")
	if len(segs) > MaxStackFileDepth {
		return fmt.Errorf("folders go at most %d levels deep", MaxStackFileDepth-1)
	}
	for _, s := range segs {
		switch {
		case s == "":
			return errors.New("the path has an empty part ('//' or a trailing '/')")
		case s == "." || s == "..":
			return errors.New("'.' and '..' aren't allowed in paths")
		case strings.HasPrefix(s, StackManifestPrefix):
			return errors.New("names starting with .tainerhub are reserved for TainerHub")
		}
		for i := 0; i < len(s); i++ {
			c := s[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
				return errors.New("use only letters, digits, '.', '_' and '-' in names, with '/' between folders")
			}
		}
	}
	return nil
}

// ValidStackFiles checks a stack's extra files: paths, count, sizes, and
// that each is text. Errors name the file.
func ValidStackFiles(files []StackFile) error {
	if len(files) > MaxStackFiles {
		return fmt.Errorf("a stack can have at most %d extra files (has %d)", MaxStackFiles, len(files))
	}
	total := 0
	seen := map[string]bool{}
	for _, f := range files {
		if err := ValidStackFilePath(f.Path); err != nil {
			return fmt.Errorf("file %q: %w", f.Path, err)
		}
		if seen[f.Path] {
			return fmt.Errorf("file %q is listed twice", f.Path)
		}
		seen[f.Path] = true
		if len(f.Content) > MaxStackFileBytes {
			return fmt.Errorf("file %q is too large (max %d KB)", f.Path, MaxStackFileBytes>>10)
		}
		if !utf8.Valid(f.Content) || bytes.IndexByte(f.Content, 0) >= 0 {
			return fmt.Errorf("file %q isn't text: only UTF-8 text files can be added to a stack", f.Path)
		}
		total += len(f.Content)
	}
	if total > MaxStackFilesTotal {
		return fmt.Errorf("the extra files are too large together (max %d MB)", MaxStackFilesTotal>>20)
	}
	// A file can't also be a folder of another one ("conf" and "conf/a.ini").
	for p := range seen {
		for i := strings.IndexByte(p, '/'); i >= 0; i = nextSlash(p, i) {
			if seen[p[:i]] {
				return fmt.Errorf("file %q is inside %q, which is a file", p, p[:i])
			}
		}
	}
	return nil
}

func nextSlash(p string, i int) int {
	j := strings.IndexByte(p[i+1:], '/')
	if j < 0 {
		return -1
	}
	return i + 1 + j
}
