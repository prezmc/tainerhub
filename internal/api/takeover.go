package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/wire"
)

// Taking over a compose project started outside TainerHub: its first
// deploy from TainerHub runs compose from a different folder, with only the
// pasted file and .env. Before that, the hub compares the pasted file with
// what the running containers use and lists what would change: data that
// would seem to vanish (bind mounts that now point somewhere else, volumes
// that would be swapped for new empty ones), services that would be
// removed, files and variables the original had that the paste lacks.

// Issue levels.
const (
	levelBlock   = "block"   // the deploy can't go ahead
	levelConfirm = "confirm" // needs an admin to say they've dealt with it
	levelInfo    = "info"
)

type takeoverIssue struct {
	Key    string `json:"key"`
	Level  string `json:"level"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"` // a command to run on the host, if one helps
}

type takeoverReport struct {
	Issues []takeoverIssue `json:"issues"`
	// Ack is what the admin sends back to confirm the issues that need it;
	// it changes when they do. Empty when there's nothing to confirm.
	Ack        string `json:"ack"`
	Blocked    bool   `json:"blocked"`
	WorkingDir string `json:"workingDir,omitempty"` // where the project was started from
	Dir        string `json:"dir"`                  // where TainerHub keeps it
	Running    int    `json:"running"`              // containers of the project now
}

// buildTakeoverReport compares the pasted files (resolved in TainerHub's
// folder) with the running project. compose is the pasted file itself.
func buildTakeoverReport(p wire.StackPreview, compose string) takeoverReport {
	rep := takeoverReport{Issues: []takeoverIssue{}, WorkingDir: p.WorkingDir, Dir: p.Dir, Running: len(p.Running)}
	add := func(i takeoverIssue) { rep.Issues = append(rep.Issues, i) }

	if p.ConfigError != "" {
		add(takeoverIssue{Key: "config", Level: levelBlock, Title: "The compose file doesn't work from TainerHub's folder",
			Detail: p.ConfigError + "\nRelative paths (./…) now mean " + p.Dir + "/…: a file such as an env_file has to be there, or use an absolute path."})
	}
	newSvc := map[string]bool{}
	for _, s := range p.Services {
		newSvc[s.Name] = true
		if s.Build {
			add(takeoverIssue{Key: "build:" + s.Name, Level: levelConfirm, Title: fmt.Sprintf("Service %s builds its image", s.Name),
				Detail: "TainerHub builds it from the stack's own files. Add its Dockerfile and everything the build copies as files of the stack (Add file in the editor) before deploying, or the build fails."})
		}
	}
	mounts := map[string]wire.StackMount{} // service + target -> new mount
	for _, m := range p.Mounts {
		mounts[m.Service+"\x00"+m.Target] = m
	}

	for _, c := range p.Running {
		if p.ConfigError == "" && c.Service != "" && !newSvc[c.Service] {
			add(takeoverIssue{Key: "orphan:" + c.Service, Level: levelConfirm,
				Title:  fmt.Sprintf("Service %s isn't in the pasted file", c.Service),
				Detail: fmt.Sprintf("Its container %s would be removed when the stack is deployed.", c.Name)})
			continue
		}
		if p.ConfigError != "" {
			continue
		}
		for _, old := range c.Mounts {
			key := c.Service + "\x00" + old.Destination
			nm, still := mounts[key]
			switch old.Type {
			case "bind":
				switch {
				case !still:
					add(takeoverIssue{Key: "gone:" + c.Service + ":" + old.Destination, Level: levelConfirm,
						Title:  fmt.Sprintf("%s would no longer get %s at %s", c.Service, old.Source, old.Destination),
						Detail: "The pasted file doesn't mount anything there."})
				case nm.Type != "bind":
					add(takeoverIssue{Key: "kind:" + c.Service + ":" + old.Destination, Level: levelConfirm,
						Title:  fmt.Sprintf("%s's %s would come from volume %s instead of %s", c.Service, old.Destination, nm.Source, old.Source),
						Detail: "A new volume starts empty."})
				case filepath.Clean(nm.Source) != filepath.Clean(old.Source):
					add(movedBind(c.Service, old, nm, p))
				}
			case "volume":
				if old.Name == "" || isAnonymous(old.Name) {
					continue // compose keeps anonymous volumes when it recreates a container
				}
				switch {
				case !still:
					add(takeoverIssue{Key: "gone:" + c.Service + ":" + old.Destination, Level: levelConfirm,
						Title:  fmt.Sprintf("%s would no longer use volume %s at %s", c.Service, old.Name, old.Destination),
						Detail: "The pasted file doesn't mount anything there. The volume itself is kept."})
				case nm.Type != "volume":
					add(takeoverIssue{Key: "kind:" + c.Service + ":" + old.Destination, Level: levelConfirm,
						Title:  fmt.Sprintf("%s's %s would come from %s instead of volume %s", c.Service, old.Destination, nm.Source, old.Name),
						Detail: "The data stays in the volume, unused."})
				case nm.Source != old.Name:
					add(takeoverIssue{Key: "volume:" + c.Service + ":" + old.Destination, Level: levelConfirm,
						Title: fmt.Sprintf("%s would use volume %s instead of %s at %s", c.Service, nm.Source, old.Name, old.Destination),
						Detail: "Volume names come from the project name and the volume's key in the file; a different one is created empty. " +
							"Keep the key the same, or set name: " + old.Name + " on the volume."})
				}
			}
		}
	}

	// A file TainerHub read from the project already merges all of them.
	merged := len(p.ConfigFiles) > 0 && strings.Contains(compose, "# Merged from: "+strings.Join(p.ConfigFiles, ", ")+"\n")
	if len(p.ConfigFiles) > 1 && !merged {
		add(takeoverIssue{Key: "files:" + strings.Join(p.ConfigFiles, ","), Level: levelConfirm,
			Title:  fmt.Sprintf("It was started with %d compose files", len(p.ConfigFiles)),
			Detail: strings.Join(p.ConfigFiles, "\n") + "\nTainerHub uses one file: make sure the pasted one includes everything from all of them (an override file, say)."})
	}
	var unset, other []string
	for _, w := range p.Warnings {
		if strings.Contains(w, "variable is not set") {
			unset = append(unset, w)
		} else {
			other = append(other, w)
		}
	}
	if len(unset) > 0 {
		add(takeoverIssue{Key: "vars:" + strings.Join(unset, "|"), Level: levelConfirm,
			Title:  "Some variables aren't set",
			Detail: strings.Join(unset, "\n") + "\nTainerHub runs compose with only the .env you paste, not your shell's variables. Add them to .env."})
	}
	if len(other) > 0 {
		add(takeoverIssue{Key: "warnings", Level: levelInfo, Title: "Compose warnings", Detail: strings.Join(other, "\n")})
	}
	if p.DirFiles > 0 {
		add(takeoverIssue{Key: "dir", Level: levelInfo, Title: p.Dir + " already has files",
			Detail: "They stay. Relative paths in the file use them."})
	}

	sort.SliceStable(rep.Issues, func(i, j int) bool { return levelRank(rep.Issues[i].Level) < levelRank(rep.Issues[j].Level) })
	var keys []string
	for _, i := range rep.Issues {
		switch i.Level {
		case levelBlock:
			rep.Blocked = true
		case levelConfirm:
			keys = append(keys, i.Key)
		}
	}
	if len(keys) > 0 {
		sort.Strings(keys)
		h := sha256.Sum256([]byte(strings.Join(keys, "\n")))
		rep.Ack = hex.EncodeToString(h[:8])
	}
	return rep
}

// movedBind describes a bind mount whose host path would change.
func movedBind(svc string, old wire.DetailMount, nm wire.StackMount, p wire.StackPreview) takeoverIssue {
	i := takeoverIssue{Key: "bind:" + svc + ":" + old.Destination, Level: levelConfirm,
		Title: fmt.Sprintf("%s's %s would come from %s instead of %s", svc, old.Destination, nm.Source, old.Source)}
	n, inDir := p.Paths[nm.Source]
	switch {
	case !inDir:
		i.Detail = "Make sure " + nm.Source + " has the same contents on the host."
	case n < 0:
		i.Detail = nm.Source + " doesn't exist yet, so " + svc + " would start without this data. Copy it there first:"
		i.Fix = fmt.Sprintf("sudo mkdir -p %s && sudo cp -a %s %s", shellQuote(filepath.Dir(nm.Source)), shellQuote(old.Source), shellQuote(nm.Source))
	case n == 0:
		i.Detail = nm.Source + " is empty, so " + svc + " would start without this data. Copy it there first:"
		i.Fix = fmt.Sprintf("sudo cp -a %s/. %s/", shellQuote(old.Source), shellQuote(nm.Source))
	default:
		i.Detail = nm.Source + " already has files. Check they're the ones " + svc + " needs."
	}
	return i
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:@", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func levelRank(l string) int {
	switch l {
	case levelBlock:
		return 0
	case levelConfirm:
		return 1
	}
	return 2
}

func isAnonymous(name string) bool {
	if len(name) != 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !(name[i] >= '0' && name[i] <= '9' || name[i] >= 'a' && name[i] <= 'f') {
			return false
		}
	}
	return true
}

// takeoverCheck asks the host's agent to compare files with the running
// project. An agent too old to check gives a report that says so and needs
// confirming, rather than blocking the takeover.
func (s *Server) takeoverCheck(ctx context.Context, hostID, name, compose, env string) (takeoverReport, error) {
	a := s.Reg.Get(hostID)
	if a == nil {
		return takeoverReport{}, registry.ErrOffline
	}
	raw, err := a.Operate(ctx, wire.Operation{Kind: wire.OpStackPreview, Target: name, Files: &wire.StackFiles{Compose: compose, Env: env}})
	if err != nil {
		if strings.Contains(err.Error(), "unknown operation") {
			rep := takeoverReport{Issues: []takeoverIssue{{Key: "old-agent", Level: levelConfirm,
				Title:  "This host's agent is too old to check the takeover",
				Detail: "Update it to check automatically. Until then, check relative paths (./…), volume names, override files and variables yourself."}}}
			h := sha256.Sum256([]byte("old-agent"))
			rep.Ack = hex.EncodeToString(h[:8])
			return rep, nil
		}
		return takeoverReport{}, err
	}
	var p wire.StackPreview
	if err := json.Unmarshal(raw, &p); err != nil {
		return takeoverReport{}, fmt.Errorf("unreadable answer: %w", err)
	}
	return buildTakeoverReport(p, compose), nil
}

// isTakeover reports whether deploying st would be TainerHub's first deploy
// of a project that's already running on its host (started elsewhere).
func (s *Server) isTakeover(hostID, name string, deployed bool) bool {
	if deployed || s.Reg == nil {
		return false
	}
	a := s.Reg.Get(hostID)
	if a == nil {
		return false
	}
	for _, c := range a.Status().Containers {
		if c.Project == name {
			return true
		}
	}
	return false
}

var errTakeoverUnconfirmed = errors.New("check the takeover first")

// requireTakeoverAck runs the check for a first deploy of a taken-over
// project. It writes a 409 with the report, and returns false, unless the
// check passes and ack confirms its issues.
func (s *Server) requireTakeoverAck(w http.ResponseWriter, r *http.Request, hostID, name, compose, env, ack string) bool {
	rep, err := s.takeoverCheck(r.Context(), hostID, name, compose, env)
	if err != nil {
		jsonErr(w, http.StatusBadGateway, "couldn't check the takeover: "+err.Error())
		return false
	}
	switch {
	case rep.Blocked:
		writeJSON(w, http.StatusConflict, map[string]any{"error": "the takeover check found a problem: " + firstBlock(rep), "takeover": rep})
		return false
	case rep.Ack != "" && rep.Ack != ack:
		writeJSON(w, http.StatusConflict, map[string]any{"error": errTakeoverUnconfirmed.Error() + ": confirm what it found", "takeover": rep})
		return false
	}
	return true
}

func firstBlock(rep takeoverReport) string {
	for _, i := range rep.Issues {
		if i.Level == levelBlock {
			return i.Title
		}
	}
	return ""
}

type takeoverInput struct {
	HostID  string `json:"hostId"`
	Name    string `json:"name"`
	Compose string `json:"compose"`
	Env     string `json:"env"`
}

// checkTakeover answers POST /api/stacks/takeover-check.
func (s *Server) checkTakeover(w http.ResponseWriter, r *http.Request) {
	var in takeoverInput
	if !readJSONLimit(w, r, &in, 1<<20) {
		return
	}
	if !validUUID(in.HostID) || !wire.ValidStackName(in.Name) {
		jsonErr(w, http.StatusBadRequest, "choose a host and a valid name")
		return
	}
	if strings.TrimSpace(in.Compose) == "" {
		jsonErr(w, http.StatusBadRequest, "the compose file is empty")
		return
	}
	rep, err := s.takeoverCheck(r.Context(), in.HostID, in.Name, in.Compose, in.Env)
	if errors.Is(err, registry.ErrOffline) {
		jsonErr(w, http.StatusServiceUnavailable, "host is offline")
		return
	}
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// takeoverSource answers GET /api/stacks/takeover-source?host=&name=: the
// project's own compose files, merged by compose with relative paths made
// absolute, and its .env, read from the folder it was started in.
func (s *Server) takeoverSource(w http.ResponseWriter, r *http.Request) {
	hostID, name := r.URL.Query().Get("host"), r.URL.Query().Get("name")
	if !validUUID(hostID) || !wire.ValidStackName(name) {
		jsonErr(w, http.StatusBadRequest, "choose a host and a valid name")
		return
	}
	a := s.Reg.Get(hostID)
	if a == nil {
		jsonErr(w, http.StatusServiceUnavailable, "host is offline")
		return
	}
	raw, err := a.Operate(r.Context(), wire.Operation{Kind: wire.OpStackRead, Target: name})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "unknown operation") {
			msg = "this host's agent is too old to read the project; update it, or paste the compose file"
		}
		jsonErr(w, http.StatusBadGateway, msg)
		return
	}
	var src wire.StackSource
	if err := json.Unmarshal(raw, &src); err != nil {
		jsonErr(w, http.StatusBadGateway, "unreadable answer from the agent")
		return
	}
	u := userFrom(r)
	s.record(r, &u, "stack.takeover_read", s.hostName(r.Context(), hostID)+"/"+name, map[string]any{"from": src.WorkingDir, "files": src.ConfigFiles, "copied": src.Copied})
	writeJSON(w, http.StatusOK, src)
}
