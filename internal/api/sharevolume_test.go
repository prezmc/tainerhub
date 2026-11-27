package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

const sharePW = "Sh4re-Pa55=word"

func TestCreateShareVolumeHidesPassword(t *testing.T) {
	x := newRegHarness(t)
	a := x.s.Reg.Attach("h1", "host1", func() {})
	sent := make(chan *wire.Operation, 4)
	fail := ""
	go func() {
		for m := range a.Out() {
			c := m.Command
			if c == nil || c.Op == nil {
				continue
			}
			sent <- c.Op
			if fail != "" {
				// An agent that (wrongly) repeats the password must still not leak it.
				a.Handle(wire.AgentMessage{Result: &wire.CommandResult{CommandID: c.ID, Error: fail}}, nil)
				continue
			}
			data, _ := json.Marshal(wire.ShareCreated{Name: c.Op.Share.Name, Entries: 3})
			a.Handle(wire.AgentMessage{Result: &wire.CommandResult{CommandID: c.ID, OK: true, Data: data}}, nil)
		}
	}()
	body := `{"name":"media","type":"cifs","server":"192.0.2.10","path":"media","username":"media","password":"` + sharePW + `","uid":"1000","gid":"1000"}`

	w := x.do("POST", "/api/hosts/h1/volumes/share", body)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"entries":3`) {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	op := <-sent
	if op.Kind != wire.OpVolumeCreateShare || op.Share.Password != sharePW || op.Share.Server != "192.0.2.10" {
		t.Fatalf("agent got %+v", op.Share)
	}

	fail = "mount failed: password=" + sharePW
	w = x.do("POST", "/api/hosts/h1/volumes/share", strings.Replace(body, `"media","type"`, `"media2","type"`, 1))
	<-sent
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), sharePW) {
		t.Fatalf("failed create: %d %s", w.Code, w.Body)
	}

	if len(x.audit) != 2 {
		t.Fatalf("audit entries: %d", len(x.audit))
	}
	for _, e := range x.audit {
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), sharePW) {
			t.Fatalf("password in audit entry: %s", raw)
		}
		if e.Action != wire.OpVolumeCreateShare || !strings.HasPrefix(e.Target, "host1/media") || !strings.Contains(string(raw), "192.0.2.10") {
			t.Fatalf("audit entry: %s", raw)
		}
	}

	// Bad input never reaches the agent.
	if w := x.do("POST", "/api/hosts/h1/volumes/share", strings.Replace(body, sharePW, "a,b", 1)); w.Code != http.StatusBadRequest {
		t.Fatalf("comma in password: %d", w.Code)
	}
	// Admins only.
	x.user.Role = "operator"
	if w := x.do("POST", "/api/hosts/h1/volumes/share", body); w.Code != http.StatusForbidden {
		t.Fatalf("operator: %d", w.Code)
	}
	select {
	case op := <-sent:
		t.Fatalf("sent %+v", op)
	default:
	}
}
