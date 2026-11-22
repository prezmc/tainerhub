package api

import (
	"strings"
	"testing"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

func TestJobOutputReplayAndTrim(t *testing.T) {
	var l jobList
	j, ok := l.start(&job{StackID: "s1"})
	if !ok {
		t.Fatal("first job refused")
	}
	if _, ok := l.start(&job{StackID: "s1"}); ok {
		t.Fatal("second job for the same stack should be refused while the first runs")
	}
	if _, ok := l.start(&job{StackID: "s2"}); !ok {
		t.Fatal("jobs for other stacks must run")
	}

	j.append([]byte("hello "))
	data, next, done, _, _ := j.read(0)
	if string(data) != "hello " || next != 6 || done {
		t.Fatalf("read: %q %d %v", data, next, done)
	}
	j.append([]byte("world"))
	if data, _, _, _, _ := j.read(next); string(data) != "world" {
		t.Fatalf("incremental read: %q", data)
	}

	// output beyond the cap is trimmed from the front; offsets stay absolute
	j.append([]byte(strings.Repeat("x", maxJobOutput)))
	data, next, _, _, _ = j.read(0)
	if len(data) != maxJobOutput || next != 11+maxJobOutput {
		t.Fatalf("after trim: len=%d next=%d", len(data), next)
	}

	j.finish("boom")
	if _, _, done, errText, _ := j.read(next); !done || errText != "boom" {
		t.Fatal("finish not visible")
	}
	if _, ok := l.start(&job{StackID: "s1"}); !ok {
		t.Fatal("a finished job must not block the next one")
	}
	if got := l.running(); len(got) != 2 {
		t.Fatalf("running = %v", got)
	}
}

func TestJobWakesWaiters(t *testing.T) {
	var l jobList
	j, _ := l.start(&job{StackID: "s"})
	_, _, _, _, changed := j.read(0)
	go func() { time.Sleep(10 * time.Millisecond); j.append([]byte("x")) }()
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("waiter not woken")
	}
}

func TestSummarizeStatus(t *testing.T) {
	cs := []wire.Container{
		{Project: "web", State: "running", Status: "Up"},
		{Project: "web", State: "exited"},
		{Project: "db", State: "running", Status: "Up (unhealthy)"},
	}
	now := time.Now()
	for _, tc := range []struct {
		sum  stackSummary
		want string
	}{
		{stackSummary{Name: "web", Online: true}, "partial"},
		{stackSummary{Name: "db", Online: true}, "running"},
		{stackSummary{Name: "web"}, "unknown"},
		{stackSummary{Name: "new", Online: true, Managed: true}, "not deployed"},
		{stackSummary{Name: "old", Online: true, Managed: true, DeployedAt: &now}, "down"},
	} {
		summarize(&tc.sum, cs)
		if tc.sum.Status != tc.want {
			t.Errorf("%s: got %q, want %q", tc.sum.Name, tc.sum.Status, tc.want)
		}
	}
	db := stackSummary{Name: "db", Online: true}
	summarize(&db, cs)
	if db.Unhealthy != 1 {
		t.Errorf("unhealthy = %d", db.Unhealthy)
	}
}

func TestStackInputValidation(t *testing.T) {
	good := stackInput{Name: " web ", Compose: "services: {}"}
	if err := good.validate(); err != nil || good.Name != "web" {
		t.Fatalf("valid input refused: %v (%q)", err, good.Name)
	}
	for _, in := range []stackInput{
		{Name: "Web", Compose: "x"},
		{Name: "a/b", Compose: "x"},
		{Name: "web", Compose: " \n"},
		{Name: "web", Compose: strings.Repeat("x", wire.MaxComposeBytes+1)},
		{Name: "web", Compose: "x", Env: strings.Repeat("x", wire.MaxEnvBytes+1)},
	} {
		if err := in.validate(); err == nil {
			t.Errorf("%q: expected an error", in.Name)
		}
	}
	if !validUUID("0b6b7a4e-55a4-4c2e-9d64-1c1f0f2a9b10") || validUUID("../../x") || validUUID("") {
		t.Fatal("validUUID")
	}
}
