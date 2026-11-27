package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/prezmc/tainerhub/internal/registry"
	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

// The preflight check runs before a new stack's first deploy (New container
// and New stack): it resolves the files on the chosen host and compares
// them with that host, so that what would only fail half-way through a
// deploy (a port in use, an image with no build for the host's CPU, a
// network that isn't there) shows up while the form is still open.
//
// Findings are "block" (the deploy would fail), "warn" (it may not work as
// meant) and "ok" (checked and fine), so the page can show what it looked at.

const levelWarn, levelOK = "warn", "ok"

type preflightItem struct {
	Key    string `json:"key"`
	Level  string `json:"level"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	// Fixes are changes that would settle it, which the New container form
	// can apply with one click. None when only the user can tell.
	Fixes []preflightFix `json:"fixes,omitempty"`
}

// preflightFix is one suggested change. Kinds: "port" (Target "8080/tcp",
// Value the new host port), "name", "cpus", "memory" (Value in MB),
// "gpu-off", "network-create" and "network-drop" (Target the network),
// "tag" (Target the image, Value the tag), "volume-source" (Target the path
// in the container, Value the new source) and "env-generate" (Target the
// variable, which gets a random value).
type preflightFix struct {
	Label  string `json:"label"`
	Kind   string `json:"kind"`
	Target string `json:"target,omitempty"`
	Value  string `json:"value,omitempty"`
}

type preflightReport struct {
	Items   []preflightItem `json:"items"`
	Blocked bool            `json:"blocked"`
	// Partial: the host's agent is too old for the full check, so only the
	// compose file itself was checked.
	Partial bool   `json:"partial,omitempty"`
	Host    string `json:"host"`
	Arch    string `json:"arch,omitempty"`
}

func (r *preflightReport) add(key, level, title, detail string, fixes ...preflightFix) {
	r.Items = append(r.Items, preflightItem{Key: key, Level: level, Title: title, Detail: detail, Fixes: fixes})
	if level == levelBlock {
		r.Blocked = true
	}
}

// preflightHost is what the hub knows about the host without asking it.
type preflightHost struct {
	Name       string
	Hello      *wire.Hello
	Containers []wire.Container
	Stacks     []store.Stack // every host's; only this host's matter
	HostID     string
	// SuggestTags lists a few tags of repository repo to use instead of
	// want that have a build for arch (nil: no suggestions). Only Docker Hub
	// can say.
	SuggestTags func(repo, want, arch string) []string
}

// freeName is name, or name-2, name-3… whichever no stack or container on the host has.
func freeName(name string, h preflightHost) string {
	taken := map[string]bool{}
	for _, st := range h.Stacks {
		if st.HostID == h.HostID {
			taken[st.Name] = true
		}
	}
	for _, c := range h.Containers {
		taken[c.Name] = true
		if c.Project != "" {
			taken[c.Project] = true
		}
	}
	base := name
	if len(base) > 58 {
		base = base[:58]
	}
	for i := 2; i < 100; i++ {
		if n := fmt.Sprintf("%s-%d", base, i); !taken[n] {
			return n
		}
	}
	return ""
}

// freePort is the first host port after from that nothing uses, or "".
func freePort(from, proto string, used, mine map[string]string) string {
	n, err := strconv.Atoi(from)
	if err != nil {
		return ""
	}
	for p := n + 1; p <= 65535 && p <= n+200; p++ {
		k := strconv.Itoa(p) + "/" + proto
		if _, ok := used[k]; ok {
			continue
		}
		if _, ok := mine[k]; ok {
			continue
		}
		return strconv.Itoa(p)
	}
	return ""
}

// buildPreflightReport compares what the agent found with the host.
func buildPreflightReport(p wire.StackPreflight, h preflightHost, name string) preflightReport {
	rep := preflightReport{Items: []preflightItem{}, Host: h.Name, Arch: p.Arch}
	if p.ConfigError != "" {
		rep.add("config", levelBlock, "Compose rejects the file", p.ConfigError)
		return rep
	}
	rep.add("config", levelOK, "The compose file is valid", "")
	for _, w := range p.Warnings {
		rep.add("compose-warning", levelWarn, "Compose: "+w, "")
	}

	// Names: the stack's and each container's.
	for _, st := range h.Stacks {
		if st.HostID == h.HostID && st.Name == name {
			rep.add("name", levelBlock, fmt.Sprintf("%s already has a stack called %s", h.Name, name), "Choose another name, or edit that stack instead.", nameFix(name, h)...)
		}
	}
	if unmanagedProject(h.Containers, h.Stacks, h.HostID, name) {
		rep.add("name", levelBlock, fmt.Sprintf("A compose project called %s is already running on %s", name, h.Name),
			"Choose another name, or take that project over with Manage in TainerHub on the Stacks page.", nameFix(name, h)...)
	}
	byName := map[string]wire.Container{}
	for _, c := range h.Containers {
		byName[c.Name] = c
	}
	for _, s := range p.Services {
		cn := s.ContainerName
		if cn == "" {
			continue // compose names it <stack>-<service>-1, which the stack name check covers
		}
		if c, ok := byName[cn]; ok && c.Project != name {
			rep.add("container-name:"+cn, levelBlock, fmt.Sprintf("%s already has a container called %s", h.Name, cn),
				"Container names are unique on a host. Rename it here, or remove the old container first.", nameFix(cn, h)...)
		}
	}

	// Ports.
	used := map[string]string{} // "8080/tcp" -> container
	for _, c := range h.Containers {
		if c.Project == name {
			continue
		}
		for _, port := range c.Ports {
			pub, rest, ok := strings.Cut(port, "->")
			if !ok {
				continue
			}
			_, proto, _ := strings.Cut(rest, "/")
			used[pub+"/"+proto] = c.Name
		}
	}
	var free []string
	mine := map[string]string{} // published port -> service, within the file
	hostNet := []string{}
	for _, s := range p.Services {
		if s.HostNetwork {
			hostNet = append(hostNet, s.Name)
		}
		if s.NetworkOf != "" && len(s.Ports) > 0 {
			rep.add("shared-net-ports:"+s.Name, levelBlock, fmt.Sprintf("%s uses the network of %s, so it can't publish ports", s.Name, s.NetworkOf),
				"Move its ports: entries to "+s.NetworkOf+": the app is reached through that container's ports (Docker refuses to start it otherwise).")
			continue
		}
		for _, port := range s.Ports {
			if port.Published == "" {
				continue // a random host port
			}
			key := port.Published + "/" + port.Protocol
			if other, ok := mine[key]; ok && other != s.Name {
				rep.add("port:"+key, levelBlock, fmt.Sprintf("Port %s is published twice in the file (%s and %s)", key, other, s.Name), "")
				continue
			}
			mine[key] = s.Name
			if c, ok := used[key]; ok {
				var fixes []preflightFix
				if alt := freePort(port.Published, port.Protocol, used, mine); alt != "" {
					fixes = append(fixes, preflightFix{Label: "Use port " + alt, Kind: "port", Target: key, Value: alt})
					mine[alt+"/"+port.Protocol] = s.Name // don't suggest it twice
				}
				rep.add("port:"+key, levelBlock, fmt.Sprintf("Port %s is already in use on %s by %s", key, h.Name, c),
					"Pick another host port (the left-hand number), or stop "+c+" first.", fixes...)
				continue
			}
			free = append(free, key)
		}
	}
	if len(free) > 0 {
		rep.add("ports", levelOK, fmt.Sprintf("%s %s free on %s", plural(len(free), "Port", "Ports")+" "+strings.Join(free, ", "), isAre(len(free)), h.Name), "")
	}
	if len(hostNet) > 0 {
		rep.add("host-network", levelWarn, fmt.Sprintf("%s %s the host's network, so TainerHub can't check its ports", strings.Join(hostNet, ", "), plural(len(hostNet), "uses", "use")),
			"If another app already listens on the same port, it won't start. Check the app's docs for the ports it uses.")
	}

	// Networks and volumes that must exist already.
	for _, n := range p.Networks {
		if n.Exists {
			rep.add("network:"+n.Name, levelOK, fmt.Sprintf("Network %s exists on %s", n.Name, h.Name), "")
		} else {
			rep.add("network:"+n.Name, levelBlock, fmt.Sprintf("Network %s doesn't exist on %s", n.Name, h.Name),
				"The file joins it as an existing (external) network. Should the stack create it, or leave it out? (Or create it on the host first: docker network create "+n.Name+".)",
				preflightFix{Label: "Create " + n.Name + " with the stack", Kind: "network-create", Target: n.Name},
				preflightFix{Label: "Leave it out", Kind: "network-drop", Target: n.Name})
		}
	}
	for _, v := range p.Volumes {
		if v.Exists {
			rep.add("volume:"+v.Name, levelOK, fmt.Sprintf("Volume %s exists on %s", v.Name, h.Name), "")
		} else {
			rep.add("volume:"+v.Name, levelBlock, fmt.Sprintf("Volume %s doesn't exist on %s", v.Name, h.Name),
				"The file expects an existing (external) volume. Create it first, or let the stack make its own.")
		}
	}

	// Images.
	for _, s := range p.Services {
		if s.Build {
			rep.add("image:"+s.Name, levelWarn, fmt.Sprintf("%s is built from source on %s", s.Name, h.Name), "Its Dockerfile and build files have to be part of the stack.")
			continue
		}
		c := s.ImageCheck
		if c == nil {
			continue
		}
		key := "image:" + s.Image
		if hasItem(rep.Items, key) {
			continue
		}
		switch c.Remote {
		case "found":
			if p.Arch != "" && len(c.Platforms) > 0 && !supportsArch(c.Platforms, p.Arch) {
				if c.Local {
					rep.add(key, levelWarn, fmt.Sprintf("%s has no linux/%s build in its registry", s.Image, p.Arch),
						fmt.Sprintf("%s already has a copy, so it can start, but updates will fail. It's built for %s.", h.Name, strings.Join(c.Platforms, ", ")))
				} else {
					rep.add(key, levelBlock, fmt.Sprintf("%s has no build for %s (linux/%s)", s.Image, h.Name, p.Arch),
						fmt.Sprintf("It's built for %s. Look for another tag or image that supports linux/%s.", strings.Join(c.Platforms, ", "), p.Arch),
						tagFixes(h, s.Image, p.Arch)...)
				}
				continue
			}
			detail := ""
			if p.Arch != "" && len(c.Platforms) > 0 {
				detail = "Built for linux/" + p.Arch + ", like " + h.Name + "."
			}
			rep.add(key, levelOK, fmt.Sprintf("Image %s found", s.Image), detail)
		case "not_found":
			if c.Local {
				rep.add(key, levelWarn, fmt.Sprintf("%s isn't in its registry, but %s has a copy", s.Image, h.Name), "It can start from that copy; updates will fail.")
			} else {
				rep.add(key, levelBlock, fmt.Sprintf("Image %s doesn't exist", s.Image), "Check the name and tag (the version after the colon) on the project's page or registry.",
					tagFixes(h, s.Image, p.Arch)...)
			}
		case "denied":
			if c.Local {
				rep.add(key, levelWarn, fmt.Sprintf("The registry won't show %s, but %s has a copy", s.Image, h.Name), "It can start from that copy. Add a login under Settings → Container registries for updates.")
			} else {
				rep.add(key, levelBlock, fmt.Sprintf("Image %s doesn't exist, or needs a login", s.Image),
					"Check the name for typos. If it's private, add a login for its registry under Settings → Container registries.")
			}
		default:
			if c.Local {
				rep.add(key, levelOK, fmt.Sprintf("Image %s is on %s", s.Image, h.Name), "")
			} else {
				rep.add(key, levelWarn, fmt.Sprintf("Couldn't check %s in its registry", s.Image), c.Error)
			}
		}
	}

	// Limits, GPUs, devices and folders.
	for _, s := range p.Services {
		if h.Hello != nil && h.Hello.NCPU > 0 && s.CPUs > float64(h.Hello.NCPU) {
			n := strconv.Itoa(h.Hello.NCPU)
			rep.add("cpus:"+s.Name, levelBlock, fmt.Sprintf("%s asks for %s CPUs; %s has %d", s.Name, strconv.FormatFloat(s.CPUs, 'f', -1, 64), h.Name, h.Hello.NCPU),
				"Docker refuses a CPU limit above the host's CPU count. Lower it under Advanced → Resources.",
				preflightFix{Label: "Use " + n + " CPUs", Kind: "cpus", Value: n})
		}
		if h.Hello != nil && h.Hello.MemTotal > 0 && s.Memory > h.Hello.MemTotal {
			mb := strconv.FormatInt(h.Hello.MemTotal/2/(1<<20), 10)
			rep.add("memory:"+s.Name, levelWarn, fmt.Sprintf("%s's memory limit (%s) is more than %s has (%s)", s.Name, humanBytes(s.Memory), h.Name, humanBytes(h.Hello.MemTotal)),
				"It works, but the limit won't hold anything back. What should it be? Half the host's memory is a common choice.",
				preflightFix{Label: "Use " + mb + " MB (half)", Kind: "memory", Value: mb})
		}
		if s.GPU && !p.GPURuntime {
			rep.add("gpu:"+s.Name, levelWarn, fmt.Sprintf("%s wants an NVIDIA GPU, but Docker on %s isn't set up for one", s.Name, h.Name),
				"Without the NVIDIA Container Toolkit the container won't start. Install it on the host (nvidia-ctk runtime configure --runtime=docker), or turn the GPU off.",
				preflightFix{Label: "Turn the GPU off", Kind: "gpu-off"})
		} else if s.GPU {
			rep.add("gpu:"+s.Name, levelOK, fmt.Sprintf("Docker on %s can hand out NVIDIA GPUs", h.Name), "")
		}
		if len(s.Devices) > 0 {
			rep.add("devices:"+s.Name, levelWarn, fmt.Sprintf("%s uses %s: make sure %s %s on %s", s.Name, strings.Join(s.Devices, ", "), plural(len(s.Devices), "it", "they"), plural(len(s.Devices), "exists", "exist"), h.Name),
				"TainerHub can't see the host's devices. If one is missing, the container won't start.")
		}
		for _, b := range s.Binds {
			if wire.LooksLikePlaceholder(b.Source) {
				alt := suggestSource(b.Target)
				rep.add("placeholder:"+s.Name+":"+b.Target, levelBlock, fmt.Sprintf("%s's folder for %s is an example from the docs: %s", s.Name, b.Target, b.Source),
					"Docker would create it empty on "+h.Name+", owned by root. Where should the data live? Enter a folder that exists on the host, or keep it in the stack's own folder ("+alt+"), which TainerHub creates and moves with the stack.",
					preflightFix{Label: "Use " + alt, Kind: "volume-source", Target: b.Target, Value: alt})
			}
		}
		for _, k := range s.PlaceholderEnv {
			var fixes []preflightFix
			if isSecretName(k) {
				fixes = append(fixes, preflightFix{Label: "Generate a value for " + k, Kind: "env-generate", Target: k})
			}
			rep.add("placeholder-env:"+s.Name+":"+k, levelWarn, fmt.Sprintf("%s's %s looks like an example from the docs", s.Name, k),
				"Replace it with your own value under Environment variables, or the app runs with the sample one.", fixes...)
		}
		var outside []string
		for _, b := range s.Binds {
			if wire.LooksLikePlaceholder(b.Source) {
				continue
			}
			if !b.InStackDir && !strings.HasPrefix(b.Source, "/var/run/") && !strings.HasPrefix(b.Source, "/run/") && b.Source != "/etc/localtime" {
				outside = append(outside, b.Source)
			}
		}
		if len(outside) > 0 {
			rep.add("binds:"+s.Name, levelWarn, fmt.Sprintf("%s uses %s on %s", s.Name, strings.Join(outside, ", "), h.Name),
				"TainerHub can't see the host's folders. Docker creates a missing one as an empty folder owned by root, so check the path, and that the app may write there.")
		}
	}
	sort.SliceStable(rep.Items, func(i, j int) bool { return preflightRank(rep.Items[i].Level) < preflightRank(rep.Items[j].Level) })
	return rep
}

// suggestSource is a folder in the stack's own folder for a mount at target
// ("/config" -> "./config"); web/src/placeholder.ts has the same rule.
func suggestSource(target string) string {
	path, _, _ := strings.Cut(target, ":")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	last := strings.ToLower(parts[len(parts)-1])
	clean := strings.TrimLeft(nonNameChars.ReplaceAllString(last, "-"), "-.")
	if clean == "" {
		clean = "data"
	}
	return "./" + clean
}

var (
	nonNameChars = regexp.MustCompile(`[^a-z0-9._-]+`)
	secretName   = regexp.MustCompile(`(?i)pass(word)?|secret|token|salt|api[_-]?key|private[_-]?key|(^|_)key$`)
)

func isSecretName(k string) bool { return secretName.MatchString(k) }

func nameFix(name string, h preflightHost) []preflightFix {
	if alt := freeName(name, h); alt != "" {
		return []preflightFix{{Label: "Use " + alt, Kind: "name", Value: alt}}
	}
	return nil
}

// tagFixes suggests tags of image that exist with a build for arch.
func tagFixes(h preflightHost, image, arch string) []preflightFix {
	if h.SuggestTags == nil || arch == "" {
		return nil
	}
	repo, want := image, "latest"
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo, want = repo[:i], repo[i+1:]
	}
	var out []preflightFix
	for _, t := range h.SuggestTags(repo, want, arch) {
		if repo+":"+t == image {
			continue
		}
		out = append(out, preflightFix{Label: "Use " + t, Kind: "tag", Target: image, Value: t})
	}
	return out
}

func preflightRank(l string) int {
	switch l {
	case levelBlock:
		return 0
	case levelWarn:
		return 1
	}
	return 2
}

func hasItem(items []preflightItem, key string) bool {
	for _, i := range items {
		if i.Key == key {
			return true
		}
	}
	return false
}

// supportsArch says whether platforms ("linux/arm64/v8") include arch ("arm64").
func supportsArch(platforms []string, arch string) bool {
	want := "linux/" + arch
	for _, p := range platforms {
		if p == want || strings.HasPrefix(p, want+"/") {
			return true
		}
	}
	return false
}

func isAre(n int) string { return plural(n, "is", "are") }

// preflightFromPreview is the check for agents too old for OpStackPreflight:
// compose's own verdict on the file.
func preflightFromPreview(p wire.StackPreview, h preflightHost, name string) preflightReport {
	rep := buildPreflightReport(wire.StackPreflight{ConfigError: p.ConfigError, Warnings: p.Warnings}, h, name)
	rep.Partial = true
	rep.add("old-agent", levelWarn, "Only the compose file was checked", "Update the agent on "+h.Name+" to also check ports, images, networks and more before deploying.")
	return rep
}

type preflightInput struct {
	HostID  string `json:"hostId"`
	Name    string `json:"name"`
	Compose string `json:"compose"`
	Env     string `json:"env"`
}

// checkPreflight answers POST /api/stacks/preflight.
func (s *Server) checkPreflight(w http.ResponseWriter, r *http.Request) {
	var in preflightInput
	if !readJSONLimit(w, r, &in, 1<<20) {
		return
	}
	if !validUUID(in.HostID) || !wire.ValidStackName(in.Name) {
		jsonErr(w, http.StatusBadRequest, "choose a host and a valid name")
		return
	}
	if !s.allowHosts(w, r, in.HostID) {
		return
	}
	if strings.TrimSpace(in.Compose) == "" {
		jsonErr(w, http.StatusBadRequest, "the compose file is empty")
		return
	}
	if err := wire.ValidStackCommand(wire.StackCommand{Action: wire.StackDeploy, Name: in.Name, Compose: in.Compose, Env: in.Env}); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	h, err := s.Store.GetHost(r.Context(), in.HostID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			jsonErr(w, http.StatusBadRequest, "that host no longer exists")
		} else {
			s.fail(w, err)
		}
		return
	}
	stacks, err := s.Store.ListStacks(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	rep, err := s.preflight(r.Context(), h, stacks, in)
	if errors.Is(err, registry.ErrOffline) {
		jsonErr(w, http.StatusServiceUnavailable, h.Name+" is offline")
		return
	}
	if err != nil {
		jsonErr(w, http.StatusBadGateway, "couldn't check on "+h.Name+": "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) preflight(ctx context.Context, h store.Host, stacks []store.Stack, in preflightInput) (preflightReport, error) {
	a := s.Reg.Get(h.ID)
	if a == nil {
		return preflightReport{}, registry.ErrOffline
	}
	st := a.Status()
	ph := preflightHost{Name: h.Name, Hello: st.Hello, Containers: st.Containers, Stacks: stacks, HostID: h.ID,
		SuggestTags: func(repo, want, arch string) []string { return s.hubTagsFor(ctx, repo, want, arch) }}
	files := &wire.StackFiles{Compose: in.Compose, Env: in.Env}
	if st.Hello == nil || st.Hello.Stacks == nil || !st.Hello.Stacks.Preflight {
		raw, err := a.Operate(ctx, wire.Operation{Kind: wire.OpStackPreview, Target: in.Name, Files: files})
		if err != nil {
			return preflightReport{}, err
		}
		var p wire.StackPreview
		if err := json.Unmarshal(raw, &p); err != nil {
			return preflightReport{}, fmt.Errorf("unreadable answer: %w", err)
		}
		return preflightFromPreview(p, ph, in.Name), nil
	}
	raw, err := a.Operate(ctx, wire.Operation{Kind: wire.OpStackPreflight, Target: in.Name, Files: files})
	if err != nil {
		return preflightReport{}, err
	}
	var p wire.StackPreflight
	if err := json.Unmarshal(raw, &p); err != nil {
		return preflightReport{}, fmt.Errorf("unreadable answer: %w", err)
	}
	return buildPreflightReport(p, ph, in.Name), nil
}

// hostFolders answers GET /api/hosts/{id}/folders: where TainerHub keeps
// stacks on the host, and the folders its containers mount, so the New
// container page can recommend folders that follow the host's own layout.
func (s *Server) hostFolders(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUUID(id) {
		jsonErr(w, http.StatusBadRequest, "invalid host")
		return
	}
	a := s.Reg.Get(id)
	if a == nil {
		jsonErr(w, http.StatusServiceUnavailable, "host is offline")
		return
	}
	out := struct {
		StacksDir string           `json:"stacksDir"`
		Binds     []wire.BindMount `json:"binds"`
	}{Binds: []wire.BindMount{}}
	if h := a.Status().Hello; h != nil && h.Stacks != nil {
		out.StacksDir = h.Stacks.Dir
	}
	raw, err := a.Operate(r.Context(), wire.Operation{Kind: wire.OpBindsList})
	switch {
	case err != nil && strings.Contains(err.Error(), "unknown operation"):
		// an older agent: recommend the stack's own folder only
	case err != nil:
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	default:
		if err := json.Unmarshal(raw, &out.Binds); err != nil {
			jsonErr(w, http.StatusBadGateway, "unreadable answer from the agent")
			return
		}
	}
	writeJSON(w, http.StatusOK, out)
}
