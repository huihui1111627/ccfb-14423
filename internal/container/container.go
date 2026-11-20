// Package container simulates a running container over a merged view.
package container

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"ccflz/internal/format"
	"ccflz/internal/image"
	"ccflz/internal/layer"
	"ccflz/internal/materialize"
	"ccflz/internal/view"
)

// Container is one running container.
type Container struct {
	ID   string
	Spec image.Resolved
	// View is the merged filesystem view.
	View *view.Snapshot
	// WritableDir is the writable layer's directory.
	WritableDir string

	// Startup metrics.
	IndexLoadDuration time.Duration
	ReadyDuration     time.Duration
	LayerBytes        int64

	layers []*layer.SharedLayer
	closed bool
	mu     sync.Mutex
}

// Runtime starts and stops containers over shared layers.
type Runtime struct {
	Reg    *image.Registry
	Opts   layer.Options
	Root   string // directory for writable layers
	Remote *RemoteStats
}

// RemoteStats collects per-layer and aggregate download counters.
type RemoteStats struct {
	mu          sync.Mutex
	bytes       int64
	requests    int
	byLayer     map[string]int64
	reqsByLayer map[string]int
}

// NewRemoteStats creates a collector.
func NewRemoteStats() *RemoteStats {
	return &RemoteStats{byLayer: make(map[string]int64), reqsByLayer: make(map[string]int)}
}

// Record adds one request.
func (r *RemoteStats) Record(digest string, n int64) {
	r.mu.Lock()
	r.bytes += n
	r.requests++
	r.byLayer[digest] += n
	r.reqsByLayer[digest]++
	r.mu.Unlock()
}

// Bytes returns downloaded bytes.
func (r *RemoteStats) Bytes() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bytes
}

// Requests returns the request count.
func (r *RemoteStats) Requests() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests
}

// LayerBytes returns downloaded bytes per layer.
func (r *RemoteStats) LayerBytes() map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int64, len(r.byLayer))
	for k, v := range r.byLayer {
		out[k] = v
	}
	return out
}

// Reset clears counters.
func (r *RemoteStats) Reset() {
	r.mu.Lock()
	r.bytes, r.requests = 0, 0
	r.byLayer = make(map[string]int64)
	r.reqsByLayer = make(map[string]int)
	r.mu.Unlock()
}

// Start launches a container: it acquires every layer, validates index
// annotations (the registry rejects bad layers here, before startup), and
// records how long readiness took.
func (rt *Runtime) Start(id, ref string) (*Container, error) {
	t0 := time.Now()
	res, err := rt.Reg.Resolve(ref)
	if err != nil {
		return nil, err
	}
	c := &Container{ID: id, Spec: *res, WritableDir: rt.Root + "/" + id}
	if len(rt.Reg.Shared()) > 0 && res.Layers != nil {
		// Shared layers are already open; startup only pays for what was not
		// loaded yet. OpenDuration reports the per-layer load cost once.
	}
	var indexDur time.Duration
	var layerBytes int64
	for _, lr := range res.Layers {
		lr.Shared.Acquire()
		c.layers = append(c.layers, lr.Shared)
		indexDur += lr.Shared.OpenDuration()
		layerBytes += lr.Shared.IndexFetchBytes()
	}
	c.IndexLoadDuration = indexDur
	c.LayerBytes = layerBytes
	v, err := view.New(res, c.WritableDir)
	if err != nil {
		rt.stopLayers(c)
		return nil, err
	}
	c.View = v
	// Readiness: the container's entry files must be readable. We simulate
	// the supervisor touching the command and working-directory files.
	if err := rt.warmStartup(c); err != nil {
		rt.stopLayers(c)
		return nil, err
	}
	c.ReadyDuration = time.Since(t0)
	return c, nil
}

// warmStartup reads the files a process needs before it can run: the command
// binary and everything in its working directory.
func (rt *Runtime) warmStartup(c *Container) error {
	cmd := c.Spec.Config.Config.Cmd
	if len(cmd) == 0 {
		return nil
	}
	bin := strings.TrimPrefix(cmd[0], "/")
	if n := c.View.Lookup(bin); n != nil && n.Type == view.TypeFile {
		if _, err := c.View.ReadAll(bin); err != nil {
			return err
		}
	}
	wd := c.Spec.Config.Config.WorkingDir
	if wd == "" {
		wd = "/"
	}
	children, err := c.View.List(wd)
	if err != nil {
		return nil
	}
	for _, child := range children {
		p := strings.TrimSuffix(wd, "/") + "/" + child
		if n := c.View.Lookup(p); n != nil && n.Type == view.TypeFile {
			if _, err := c.View.ReadAll(p); err != nil {
				return err
			}
		}
	}
	return nil
}

func (rt *Runtime) stopLayers(c *Container) {
	for _, sl := range c.layers {
		sl.Release()
	}
	c.layers = nil
}

// Stop releases every layer. The last container to stop a layer frees its
// page cache.
func (rt *Runtime) Stop(c *Container) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	rt.stopLayers(c)
}

// Layers returns the shared layers held by the container.
func (c *Container) Layers() []*layer.SharedLayer {
	out := make([]*layer.SharedLayer, len(c.layers))
	copy(out, c.layers)
	return out
}

// Activity simulates a running container: random reads, directory listings,
// writes and modifications. Every operation is also performed on the
// reference tree by the caller so the two can be compared.
type Activity struct {
	Reads   int
	Lists   int
	Writes  int
	Creates int
	Deletes int
	Seed    int64
}

// Run performs a randomized workload and returns the operations performed, so
// the verifier can replay them against a plain extraction.
func (c *Container) Run(a Activity) ([]Op, error) {
	rng := rand.New(rand.NewSource(a.Seed))
	var files []string
	var dirs []string
	if err := c.View.Walk(func(n *view.Node) error {
		if n.Type == view.TypeFile {
			files = append(files, n.Path)
		} else if n.Type == view.TypeDir {
			dirs = append(dirs, n.Path)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(files)
	sort.Strings(dirs)
	var ops []Op
	for i := 0; i < a.Reads && len(files) > 0; i++ {
		p := files[rng.Intn(len(files))]
		n := c.View.Lookup(p)
		ops = append(ops, Op{Kind: "read", Path: p, Off: 0, Len: n.Size})
	}
	for i := 0; i < a.Writes && len(files) > 0; i++ {
		p := files[rng.Intn(len(files))]
		n := c.View.Lookup(p)
		size := n.Size
		off := int64(0)
		if size > 8 {
			off = rng.Int63n(size - 4)
		}
		ln := rng.Int63n(min64(8, size-off+8))
		data := make([]byte, ln)
		rng.Read(data)
		ops = append(ops, Op{Kind: "write", Path: p, Off: off, Data: data})
	}
	for i := 0; i < a.Lists; i++ {
		var d string
		if len(dirs) > 0 {
			d = dirs[rng.Intn(len(dirs))]
		} else {
			d = "/"
		}
		if !c.exists(d) {
			continue
		}
		ops = append(ops, Op{Kind: "list", Path: d})
	}
	for i := 0; i < a.Creates; i++ {
		p := fmt.Sprintf("/new/dir%d/file%d.txt", rng.Intn(4), i)
		data := make([]byte, rng.Intn(200))
		rng.Read(data)
		ops = append(ops, Op{Kind: "create", Path: p, Data: data})
	}
	for i := 0; i < a.Deletes && len(files) > 0; i++ {
		p := files[rng.Intn(len(files))]
		if !c.exists(p) {
			continue
		}
		ops = append(ops, Op{Kind: "delete", Path: p})
	}
	return ops, c.Apply(ops)
}

// Op is one filesystem operation.
type Op struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
	Off  int64  `json:"off,omitempty"`
	Len  int64  `json:"len,omitempty"`
	Data []byte `json:"data,omitempty"`
}

// Apply performs ops on the container view.
func (c *Container) Apply(ops []Op) error {
	for _, op := range ops {
		switch op.Kind {
		case "read":
			if _, err := c.View.Read(op.Path, op.Off, op.Len); err != nil {
				return fmt.Errorf("read %s: %w", op.Path, err)
			}
		case "list":
			if _, err := c.View.List(op.Path); err != nil {
				return fmt.Errorf("list %s: %w", op.Path, err)
			}
		case "write":
			if err := c.View.Write(op.Path, op.Off, op.Data); err != nil {
				return fmt.Errorf("write %s: %w", op.Path, err)
			}
		case "create":
			dir := op.Path[:strings.LastIndex(op.Path, "/")]
			if err := c.View.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			if err := c.View.Create(op.Path, 0o644); err != nil {
				return fmt.Errorf("create %s: %w", op.Path, err)
			}
			if len(op.Data) > 0 {
				if err := c.View.Write(op.Path, 0, op.Data); err != nil {
					return err
				}
			}
		case "delete":
			if err := c.View.Remove(op.Path); err != nil {
				return fmt.Errorf("delete %s: %w", op.Path, err)
			}
		}
	}
	return nil
}

func (c *Container) exists(p string) bool {
	return c.View.Lookup(p) != nil
}

// Snapshot materializes the current view for comparison.
func (c *Container) Snapshot(dir string) (*materialize.Result, error) {
	return materialize.Snapshot(c.View, dir)
}

// Indexes returns the indexes of the layers used by this container.
func (c *Container) Indexes() []*format.Index {
	out := make([]*format.Index, 0, len(c.Spec.Layers))
	for _, lr := range c.Spec.Layers {
		out = append(out, lr.Index)
	}
	return out
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
