package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/prezmc/tainerhub/internal/wire"
)

func TestRemoveWithVolumes(t *testing.T) {
	anon := strings.Repeat("ab", 32)
	var mu sync.Mutex
	var deletedVolumes []string
	var removeQuery string

	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"Mounts": []map[string]string{
			{"Type": "volume", "Name": "appdata"},
			{"Type": "volume", "Name": "shared"},
			{"Type": "volume", "Name": anon},
			{"Type": "bind", "Name": ""},
		}})
	})
	mux.HandleFunc("DELETE /containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		removeQuery = r.URL.RawQuery
		mu.Unlock()
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("filters"), `"shared"`) {
			_, _ = w.Write([]byte(`[{"Names":["/other-app"]}]`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("DELETE /volumes/{name}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		deletedVolumes = append(deletedVolumes, r.PathValue("name"))
		mu.Unlock()
		w.WriteHeader(204)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()

	d := NewSocketDocker(sock)
	op := wire.Operation{Kind: wire.OpContainerRemoveVolumes, Target: "c1", Arg: "appdata,shared,notmine"}
	if err := wire.ValidOperation(op); err != nil {
		t.Fatal(err)
	}
	out, err := d.Operate(context.Background(), op)
	if err != nil {
		t.Fatal(err)
	}
	res := out.(wire.RemoveVolumesResult)

	if !strings.Contains(removeQuery, "force=1") || !strings.Contains(removeQuery, "v=1") {
		t.Fatalf("container removed without force/v: %q", removeQuery)
	}
	if res.Anonymous != 1 {
		t.Errorf("anonymous = %d", res.Anonymous)
	}
	if !reflect.DeepEqual(res.Removed, []string{"appdata"}) || !reflect.DeepEqual(deletedVolumes, []string{"appdata"}) {
		t.Fatalf("removed %v, deleted %v", res.Removed, deletedVolumes)
	}
	reasons := map[string]string{}
	for _, k := range res.Kept {
		reasons[k.Name] = k.Reason
	}
	if !strings.Contains(reasons["shared"], "other-app") || reasons["notmine"] != "not used by this container" {
		t.Fatalf("kept = %+v", res.Kept)
	}
}

func TestRemoveVolumeNamesValidation(t *testing.T) {
	for _, bad := range []string{"../x", "a,,b", "a/b", strings.Repeat("v,", wire.MaxRemoveVolumes) + "v"} {
		if _, err := wire.RemoveVolumeNames(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if n, err := wire.RemoveVolumeNames(""); err != nil || n != nil {
		t.Fatal("empty list should mean no named volumes")
	}
}
