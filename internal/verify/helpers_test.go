package verify

import (
	"encoding/json"
	"fmt"
	"testing"

	"ccflz/internal/gen"
	"ccflz/internal/image"
	"ccflz/internal/layer"
	"ccflz/internal/repo"
)

func genImage(t *testing.T, r *repo.Repo, name string) (*gen.ImageRecord, error) {
	t.Helper()
	return gen.GenerateImage(r, gen.Spec{Name: name, LayerCount: 2, Seed: 4, FilesPerLayer: 20})
}

// corruptIndex flips a byte in the layer index after the manifest was written,
// so the annotation no longer matches. Resolve must refuse it.
func corruptIndex(t *testing.T, r *repo.Repo, dir, name string) (string, error) {
	t.Helper()
	d, err := r.ResolveTag(name)
	if err != nil {
		return "", err
	}
	raw, err := r.GetManifestBytes(d)
	if err != nil {
		return "", err
	}
	var mf repo.Manifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		return "", err
	}
	ld := mf.Layers[0]
	sl, err := layer.Open(ld.Digest, r.NewRemote(), layer.DefaultOptions(), layer.IndexHint{})
	if err != nil {
		return "", err
	}
	idxOff := sl.Index.Segments[sl.Index.IndexSeg].Off
	blob, err := r.GetBlobBytes(ld.Digest)
	if err != nil {
		return "", err
	}
	// Flip a byte in the middle of the index member's compressed payload.
	pos := idxOff + (int64(len(blob))-idxOff)/2
	if err := r.CorruptBlob(ld.Digest, pos, func(b []byte) { b[0] ^= 0x5a }); err != nil {
		return "", err
	}
	reg := image.NewRegistry(r, r.NewRemote(), layer.DefaultOptions())
	if _, err := reg.Resolve(name); err == nil {
		return "", fmt.Errorf("registry accepted a tampered index")
	} else {
		return err.Error(), nil
	}
}
