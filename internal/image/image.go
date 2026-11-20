// Package image resolves manifests and holds the shared layer registry.
package image

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"ccflz/internal/format"
	"ccflz/internal/layer"
	"ccflz/internal/repo"
)

// Resolved is a manifest plus its config and open layers.
type Resolved struct {
	Digest   string
	Manifest repo.Manifest
	Raw      []byte
	Config   repo.ImageConfig
	// Layers in application order.
	Layers []*LayerRef
}

// LayerRef is a validated, opened layer.
type LayerRef struct {
	Descriptor  repo.Descriptor
	Shared      *layer.SharedLayer
	Index       *format.Index
	IndexDigest string
	// EntryByPath maps normalized archive paths to entries for this layer.
	EntryByPath map[string]*format.Entry
}

// Registry shares opened layers between containers. Digests are opened at
// most once even when several containers start at the same time.
type Registry struct {
	mu      sync.Mutex
	layers  map[string]*layer.SharedLayer
	opening map[string]*openCall
	remote  layer.Opener
	opts    layer.Options
	blobs   *repo.Repo
}

type openCall struct {
	done chan struct{}
	sl   *layer.SharedLayer
	err  error
}

// NewRegistry creates a registry over a repo.
func NewRegistry(r *repo.Repo, remote layer.Opener, opts layer.Options) *Registry {
	return &Registry{
		layers: make(map[string]*layer.SharedLayer), opening: make(map[string]*openCall),
		remote: remote, opts: opts, blobs: r,
	}
}

// Get returns the shared layer for a digest, opening it only when no live
// instance exists.
func (g *Registry) Get(digest string) (*layer.SharedLayer, bool, error) {
	return g.get(digest, layer.IndexHint{})
}

// GetWithHint opens a layer, telling the reader where the index starts when
// the manifest advertises it.
func (g *Registry) GetWithHint(digest string, hint layer.IndexHint) (*layer.SharedLayer, bool, error) {
	return g.get(digest, hint)
}

func (g *Registry) get(digest string, hint layer.IndexHint) (*layer.SharedLayer, bool, error) {
	g.mu.Lock()
	if sl, ok := g.layers[digest]; ok && !sl.Closed() {
		g.mu.Unlock()
		return sl, false, nil
	}
	if call, ok := g.opening[digest]; ok {
		g.mu.Unlock()
		<-call.done
		return call.sl, false, call.err
	}
	call := &openCall{done: make(chan struct{})}
	g.opening[digest] = call
	g.mu.Unlock()

	sl, err := layer.Open(digest, g.remote, g.opts, hint)

	g.mu.Lock()
	delete(g.opening, digest)
	if err == nil {
		g.layers[digest] = sl
	}
	call.sl, call.err = sl, err
	close(call.done)
	g.mu.Unlock()
	return sl, true, err
}

// Shared returns all shared layers.
func (g *Registry) Shared() map[string]*layer.SharedLayer {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]*layer.SharedLayer, len(g.layers))
	for k, v := range g.layers {
		out[k] = v
	}
	return out
}

// Resolve loads a manifest by tag or digest and validates every layer's index
// annotation before any layer is used. A mutated index is rejected here, before
// container startup.
func (g *Registry) Resolve(ref string) (*Resolved, error) {
	digest := ref
	if repo.IsTag(ref) {
		d, err := g.blobs.ResolveTag(ref)
		if err != nil {
			return nil, err
		}
		digest = d
	}
	raw, err := g.blobs.GetManifestBytes(digest)
	if err != nil {
		return nil, err
	}
	var m repo.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", digest, err)
	}
	res := &Resolved{Digest: digest, Manifest: m, Raw: raw}
	if m.Config.Digest != "" {
		cb, err := g.blobs.GetBlobBytes(m.Config.Digest)
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		if err := json.Unmarshal(cb, &res.Config); err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
	}
	for _, ld := range m.Layers {
		lr, err := g.openLayer(ld)
		if err != nil {
			return nil, err
		}
		res.Layers = append(res.Layers, lr)
	}
	return res, nil
}

func (g *Registry) openLayer(ld repo.Descriptor) (*LayerRef, error) {
	hint := indexHint(ld)
	sl, _, err := g.GetWithHint(ld.Digest, hint)
	if err != nil {
		return nil, fmt.Errorf("layer %s: %w", ld.Digest, err)
	}
	if sl.Closed() {
		return nil, fmt.Errorf("layer %s: released", ld.Digest)
	}
	lr := &LayerRef{Descriptor: ld, Shared: sl, Index: sl.Index, EntryByPath: make(map[string]*format.Entry)}
	// The index digest is recorded in the manifest annotation; any mismatch
	// rejects the layer before it is used.
	want := ld.Annotations[format.IndexAnnotation]
	if want == "" {
		return nil, fmt.Errorf("layer %s: manifest lacks the %s annotation", ld.Digest, format.IndexAnnotation)
	}
	rawIdx, err := sl.RawIndex()
	if err != nil {
		return nil, fmt.Errorf("layer %s: %w", ld.Digest, err)
	}
	got := format.DigestOf(rawIdx)
	lr.IndexDigest = got
	if _, err := format.ParseDigest(want); err != nil {
		return nil, fmt.Errorf("layer %s: malformed index annotation: %w", ld.Digest, err)
	}
	if got != want {
		return nil, fmt.Errorf("layer %s: index digest mismatch: annotation %s, actual %s — layer rejected before startup", ld.Digest, want, got)
	}
	for _, e := range sl.Index.Entries {
		lr.EntryByPath[e.Path] = e
	}
	return lr, nil
}

// indexHint extracts the index location annotation.
func indexHint(ld repo.Descriptor) layer.IndexHint {
	var h layer.IndexHint
	if v := ld.Annotations[format.IndexLengthAnnotation]; v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			h.Length = n
		}
	}
	return h
}
