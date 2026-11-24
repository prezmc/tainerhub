package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// addrOption is one choice for "Hub address for agents" in the Add host
// dialog, best first.
type addrOption struct {
	Addr string `json:"addr"`
	Note string `json:"note"`
}

// agentAddrDefault answers what the Add host dialog should offer: the
// addresses the hub knows work or were chosen, best first. agentAddr is the
// one to pre-select ("" when there is no safe guess).
func (s *Server) agentAddrDefault(w http.ResponseWriter, r *http.Request) {
	opts := s.agentAddrOptions(r)
	best := ""
	if len(opts) > 0 {
		best = opts[0].Addr
	}
	writeJSON(w, http.StatusOK, map[string]any{"agentAddr": best, "options": opts})
}

func (s *Server) agentAddrOptions(r *http.Request) []addrOption {
	stored := ""
	if s.AgentAddr == "" {
		stored, _ = s.Store.Setting(r.Context(), settingAgentAddr)
	}
	return s.buildAddrOptions(s.AgentAddr, s.agentsHubAddrs(r.Context()), stored, r.Host)
}

// buildAddrOptions orders the candidates: TAINERHUB_AGENT_ADDR, addresses
// connected agents use (most hosts first), the last address chosen, and the
// address this page was opened on. Anything that names the reverse proxy is
// left out.
func (s *Server) buildAddrOptions(env string, used map[string][]string, stored, reqHost string) []addrOption {
	var out []addrOption
	seen := map[string]bool{}
	add := func(addr, note string) {
		addr = strings.TrimSpace(addr)
		h, p, err := net.SplitHostPort(addr)
		if err != nil || h == "" || p == "" || seen[strings.ToLower(addr)] || s.proxyAddrProblem(h) != "" {
			return
		}
		seen[strings.ToLower(addr)] = true
		out = append(out, addrOption{Addr: addr, Note: note})
	}
	if env != "" {
		add(env, "set with TAINERHUB_AGENT_ADDR")
	}
	addrs := make([]string, 0, len(used))
	for a := range used {
		addrs = append(addrs, a)
	}
	sort.Slice(addrs, func(i, j int) bool {
		if len(used[addrs[i]]) != len(used[addrs[j]]) {
			return len(used[addrs[i]]) > len(used[addrs[j]])
		}
		return addrs[i] < addrs[j]
	})
	for _, a := range addrs {
		add(a, "used by "+namesList(used[a]))
	}
	if stored != "" {
		add(stored, "used last time")
	}
	host := reqHost
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host = strings.Trim(host, "[]"); host != "" {
		add(net.JoinHostPort(host, "7443"), "the address you opened TainerHub on")
	}
	return out
}

func namesList(names []string) string {
	sort.Strings(names)
	if len(names) > 3 {
		return strings.Join(names[:3], ", ") + " and " + strconv.Itoa(len(names)-3) + " more"
	}
	return strings.Join(names, ", ")
}

// agentsHubAddrs asks each connected agent (except the hub server's own,
// which reaches the hub over Docker's network) which hub address it was set
// up with: the TAINERHUB_HUB variable of its tainerhub-agent container. Those
// addresses demonstrably work from other servers. Works with any agent
// version, as it only inspects a container.
func (s *Server) agentsHubAddrs(ctx context.Context) map[string][]string {
	out := map[string][]string{}
	if s.Reg == nil || s.Store == nil {
		return out
	}
	hs, err := s.Store.ListHosts(ctx)
	if err != nil {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	local := s.localHostID()
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, h := range hs {
		a := s.Reg.Get(h.ID)
		if a == nil || h.ID == local {
			continue
		}
		var target string
		for _, c := range a.Status().Containers {
			if strings.TrimPrefix(c.Name, "/") == "tainerhub-agent" || strings.Contains(c.Image, "tainerhub-agent") {
				target = c.ID
				break
			}
		}
		if target == "" {
			continue
		}
		wg.Add(1)
		go func(name, target string) {
			defer wg.Done()
			data, err := a.Operate(ctx, wire.Operation{Kind: wire.OpContainerInspect, Target: target})
			if err != nil {
				return
			}
			var d wire.ContainerDetail
			if json.Unmarshal(data, &d) != nil {
				return
			}
			if addr := hubAddrFromEnv(d.Env); addr != "" {
				mu.Lock()
				out[addr] = append(out[addr], name)
				mu.Unlock()
			}
		}(h.Name, target)
	}
	wg.Wait()
	return out
}

func hubAddrFromEnv(env []string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "TAINERHUB_HUB="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
