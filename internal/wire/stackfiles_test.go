package wire

import (
	"strings"
	"testing"
)

func TestValidStackFilePath(t *testing.T) {
	for _, tc := range []struct {
		path string
		ok   bool
	}{
		{"Dockerfile", true},
		{"entrypoint.sh", true},
		{"conf/nginx.conf", true},
		{"a/b/c/d.txt", true},    // depth 4
		{"a/b/c/d/e.txt", false}, // depth 5
		{"web/.env", true},       // only the top-level .env has its own field
		{"api.Dockerfile", true},
		{"", false},
		{"/etc/passwd", false}, // absolute
		{"../x", false},        // traversal
		{"a/../b", false},
		{"./Dockerfile", false},         // '.' segment
		{"a//b", false},                 // empty segment
		{"a/", false},                   // trailing slash
		{"a\\b", false},                 // backslash
		{"my file", false},              // space
		{"naïve.txt", false},            // non-ASCII
		{".tainerhub-files", false},     // the manifest
		{".tainerhub-staging/x", false}, // the staging folder
		{"sub/.tainerhubx", false},
		{"compose.yaml", false}, // own field
		{"compose.yml", false},
		{"docker-compose.yml", false},
		{"docker-compose.yaml", false},
		{".env", false},
		{strings.Repeat("a", 201), false},
		{strings.Repeat("a", 200), true},
	} {
		if err := ValidStackFilePath(tc.path); (err == nil) != tc.ok {
			t.Errorf("%q: err=%v, want ok=%v", tc.path, err, tc.ok)
		}
	}
}

func TestValidStackFilesLimits(t *testing.T) {
	f := func(p, c string) StackFile { return StackFile{Path: p, Content: []byte(c)} }
	if err := ValidStackFiles([]StackFile{f("Dockerfile", "FROM alpine\n"), f("conf/a.ini", "")}); err != nil {
		t.Fatalf("valid files refused: %v", err)
	}
	many := make([]StackFile, MaxStackFiles+1)
	for i := range many {
		many[i] = f("f"+strings.Repeat("x", i), "")
	}
	big := strings.Repeat("x", MaxStackFileBytes)
	nine := make([]StackFile, 9) // 9 * 256 KB > 2 MB
	for i := range nine {
		nine[i] = f("f"+strings.Repeat("x", i), big)
	}
	for name, tc := range map[string]struct {
		files []StackFile
		want  string
	}{
		"too many":   {many, "at most 30"},
		"one large":  {[]StackFile{f("big", big+"x")}, `"big" is too large`},
		"total":      {nine, "too large together"},
		"NUL":        {[]StackFile{f("bin", "ab\x00c")}, `"bin" isn't text`},
		"not UTF-8":  {[]StackFile{f("img.png", "\x89PNG\xff\xfe")}, `"img.png" isn't text`},
		"duplicate":  {[]StackFile{f("a", ""), f("a", "")}, "listed twice"},
		"file+dir":   {[]StackFile{f("conf", ""), f("conf/a", "")}, `inside "conf"`},
		"bad path":   {[]StackFile{f("../x", "")}, `file "../x"`},
		"compose":    {[]StackFile{f("compose.yaml", "")}, "own tab"},
		"exact size": {[]StackFile{f("ok", big)}, ""},
	} {
		err := ValidStackFiles(tc.files)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
	// Files only travel with a deploy.
	if err := ValidStackCommand(StackCommand{Action: StackStop, Name: "a", Files: []StackFile{f("x", "")}}); err == nil {
		t.Error("files on a stop accepted")
	}
	if err := ValidStackCommand(StackCommand{Action: StackDeploy, Name: "a", Compose: "x", Files: []StackFile{f("/x", "")}}); err == nil {
		t.Error("bad file path on a deploy accepted")
	}
}
