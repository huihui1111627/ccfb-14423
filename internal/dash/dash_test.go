package dash

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ccflz/internal/container"
	"ccflz/internal/gen"
	"ccflz/internal/image"
	"ccflz/internal/layer"
	"ccflz/internal/repo"
)

func TestDashboardServesStateAndStarts(t *testing.T) {
	r, err := repo.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gen.GenerateImage(r, gen.Spec{Name: "ui", LayerCount: 2, Seed: 2, FilesPerLayer: 30}); err != nil {
		t.Fatal(err)
	}
	remote := r.NewRemote()
	reg := image.NewRegistry(r, remote, layer.DefaultOptions())
	rt := &container.Runtime{Reg: reg, Root: t.TempDir()}
	s, err := New(reg, r, remote, rt, "ui")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// The page renders and includes the image name.
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		body.Write(buf[:n])
		if err != nil {
			break
		}
	}
	resp.Body.Close()
	if !strings.Contains(body.String(), "ui") || !strings.Contains(body.String(), "范围请求记录") {
		t.Fatalf("page missing expected content")
	}
	// Starting a container populates layers and requests.
	resp, err = http.Post(srv.URL+"/api/start", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp, err = http.Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st State
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if len(st.Layers) != 2 || len(st.Files) == 0 {
		t.Fatalf("state has %d layers, %d files", len(st.Layers), len(st.Files))
	}
	if len(st.Requests) == 0 {
		t.Fatal("no range requests recorded")
	}
	if len(st.Layers[0].Segments) == 0 {
		t.Fatal("no segments in the layout")
	}
	if st.Cold.ReadyMS <= 0 {
		t.Fatal("cold startup not recorded")
	}
}
