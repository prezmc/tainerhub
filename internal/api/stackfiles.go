package api

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"

	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

// maxStackBody bounds a stack create/update request: compose (256 KB),
// .env (64 KB) and up to 2 MB of extra files, with room for JSON escaping.
const maxStackBody = 6 << 20

// stackHash identifies what a deploy writes, for "changes not deployed".
// A stack without extra files hashes as it did before they existed, so
// upgrading doesn't mark every stack as changed.
func stackHash(st store.Stack) string {
	h := sha256.New()
	h.Write([]byte(st.Compose + "\x00" + st.Env))
	files := append([]store.StackFile(nil), st.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, f := range files {
		x := "0"
		if f.Executable {
			x = "1"
		}
		// The length goes before the content, so no two file lists hash alike.
		h.Write([]byte("\x00file\x00" + f.Path + "\x00" + x + "\x00" + strconv.Itoa(len(f.Content)) + "\x00" + f.Content))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// wireFiles is how a stack's files travel to the agent.
func wireFiles(files []store.StackFile) []wire.StackFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]wire.StackFile, len(files))
	for i, f := range files {
		out[i] = wire.StackFile{Path: f.Path, Content: []byte(f.Content), Executable: f.Executable}
	}
	return out
}

// changedFiles lists the paths added, removed or changed between two
// versions of a stack's files, for the audit log (never their contents).
func changedFiles(before, after []store.StackFile) []string {
	old := map[string]store.StackFile{}
	for _, f := range before {
		old[f.Path] = f
	}
	var out []string
	seen := map[string]bool{}
	for _, f := range after {
		seen[f.Path] = true
		if o, ok := old[f.Path]; !ok || o != f {
			out = append(out, f.Path)
		}
	}
	for _, f := range before {
		if !seen[f.Path] {
			out = append(out, f.Path)
		}
	}
	sort.Strings(out)
	return out
}

func filePaths(files []store.StackFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	sort.Strings(out)
	return out
}

// hideStackFiles blanks what only admins may read: compose files, .env
// and the other files often hold passwords.
func hideStackFiles(st *store.Stack) {
	st.Compose, st.Env = "", ""
	st.Files = []store.StackFile{}
}
