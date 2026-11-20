package image

import (
	"testing"

	"ccflz/internal/format"
	"ccflz/internal/gen"
	"ccflz/internal/layer"
	"ccflz/internal/repo"
)

func newRepo(t *testing.T) *repo.Repo {
	t.Helper()
	r, err := repo.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestTamperedIndexRejectedBeforeStartup(t *testing.T) {
	r := newRepo(t)
	rec, err := gen.GenerateImage(r, gen.Spec{Name: "img", LayerCount: 1, Seed: 3})
	if err != nil {
		t.Fatal(err)
	}
	_ = rec
	// Rewrite one layer blob's index so its bytes no longer match the
	// annotation, while keeping the layer structurally valid.
	mfRaw, err := r.GetManifestBytes(rec.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	var mf repo.Manifest
	if err := jsonUnmarshal(mfRaw, &mf); err != nil {
		t.Fatal(err)
	}
	ld := mf.Layers[0]
	blob, err := r.GetBlobBytes(ld.Digest)
	if err != nil {
		t.Fatal(err)
	}
	idxStart := findIndexStart(t, blob)
	var ix format.Index
	raw, err := inflate(blob[idxStart:])
	if err != nil {
		t.Fatal(err)
	}
	if err := jsonUnmarshal(raw, &ix); err != nil {
		t.Fatal(err)
	}
	// Mutate a file's recorded size in the index.
	ix.Files[0].Size++
	newIdx := marshal(t, &ix)
	tampered := append(append([]byte(nil), blob[:idxStart]...), deflate(newIdx)...)
	dg, err := r.PutBlob(tampered)
	if err != nil {
		t.Fatal(err)
	}
	// Put a new manifest that points at the tampered blob but keeps the old
	// annotation, exactly like a poisoned mirror would.
	mf.Layers[0].Digest = dg
	newMf := marshal(t, &mf)
	md, err := r.PutManifest(newMf)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetTag("bad", md); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(r, r.NewRemote(), layer.DefaultOptions())
	if _, err := reg.Resolve("bad"); err == nil {
		t.Fatal("registry accepted a layer whose index was modified")
	}
}

func findIndexStart(t *testing.T, blob []byte) int64 {
	t.Helper()
	// The index is the final gzip member; locate it from the tail by trying
	// every 1f8b magic until one decodes into a valid index.
	for i := len(blob) - 2; i >= 0; i-- {
		if blob[i] != 0x1f || blob[i+1] != 0x8b {
			continue
		}
		raw, err := inflate(blob[i:])
		if err != nil {
			continue
		}
		var ix format.Index
		if jsonUnmarshal(raw, &ix) == nil && ix.IndexSeg == len(ix.Segments)-1 {
			return int64(i)
		}
	}
	t.Fatal("index not found")
	return 0
}

func TestLayerSharingAcrossContainers(t *testing.T) {
	r := newRepo(t)
	if _, err := gen.GenerateImage(r, gen.Spec{Name: "share", LayerCount: 2, Seed: 5}); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(r, r.NewRemote(), layer.DefaultOptions())
	res1, err := reg.Resolve("share")
	if err != nil {
		t.Fatal(err)
	}
	res2, err := reg.Resolve("share")
	if err != nil {
		t.Fatal(err)
	}
	for i := range res1.Layers {
		if res1.Layers[i].Shared != res2.Layers[i].Shared {
			t.Fatal("two resolutions opened different layer instances")
		}
	}
	for _, lr := range res1.Layers {
		lr.Shared.Acquire()
		lr.Shared.Acquire()
	}
	for _, lr := range res1.Layers {
		if lr.Shared.Refs() != 2 {
			t.Fatalf("refs=%d, want 2", lr.Shared.Refs())
		}
		if lr.Shared.Release() {
			t.Fatal("freed with a live container")
		}
		if !lr.Shared.Release() {
			t.Fatal("not freed after the last container")
		}
	}
}
