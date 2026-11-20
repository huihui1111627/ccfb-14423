package container

import (
	"testing"

	"ccflz/internal/format"
	"ccflz/internal/gen"
	"ccflz/internal/image"
	"ccflz/internal/layer"
	"ccflz/internal/repo"
)

func TestCopyUpFetchesWholeFile(t *testing.T) {
	r, err := repo.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gen.GenerateImage(r, gen.Spec{Name: "cu", LayerCount: 1, Seed: 8, FilesPerLayer: 40}); err != nil {
		t.Fatal(err)
	}
	remote := r.NewRemote()
	stats := NewRemoteStats()
	remote.LogHook = func(rr repo.RangeRequest) { stats.Record(rr.Digest, rr.Bytes) }
	reg := image.NewRegistry(r, remote, layer.DefaultOptions())
	rt := &Runtime{Reg: reg, Root: t.TempDir()}
	c, err := rt.Start("cu-1", "cu")
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop(c)

	// Find a multi-chunk file.
	files, _ := c.View.Paths()
	var path string
	var f *format.File
	for _, p := range files {
		n := c.View.Lookup(p)
		if n != nil && n.File != nil && len(n.File.Segs) > 2 {
			path, f = p, n.File
			break
		}
	}
	if f == nil {
		t.Fatal("no multi-chunk file in the generated image")
	}
	stats.Reset()
	// Write a single byte in the middle: copy-up must fetch every chunk.
	if err := c.View.Write(path, f.Size/2, []byte("Z")); err != nil {
		t.Fatal(err)
	}
	// Whole file means every chunk segment was fetched (plus possibly a
	// header). No data segment of the file may be missing from the layer's
	// cache.
	sl := c.Spec.Layers[0].Shared
	cached := map[int]bool{}
	for _, id := range sl.CachedSegments() {
		cached[id] = true
	}
	for _, id := range f.Segs {
		if !cached[id] {
			t.Fatalf("copy-up left chunk segment %d unfetched", id)
		}
	}
	// The writable copy holds the whole file and the mutation is visible.
	data, err := c.View.ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != int(f.Size) || data[f.Size/2] != 'Z' {
		t.Fatalf("copy-up content wrong: len=%d byte=%q", len(data), data[f.Size/2])
	}
	if stats.Bytes() == 0 {
		t.Fatal("copy-up downloaded nothing")
	}
}
