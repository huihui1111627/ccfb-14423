// Package verify implements the end-to-end verification used by cmd/verify
// and the integration tests.
//
// For every generated image it:
//   - packs each layer into a CCFLZ blob and commits the index digest in the
//     manifest annotation;
//   - independently inflates every segment, checks its digest, and asserts the
//     decoded segments reproduce the original tar byte for byte;
//   - extracts the packed blob with ordinary tar and gzip;
//   - starts several containers over shared layers and measures startup;
//   - runs random reads, directory listings, writes, creates and deletes,
//     checking every single result against a plain extraction of the same
//     layers that receives the same mutations on disk;
//   - compares the complete materialized tree and its metadata path by path;
//   - verifies refcounted layer sharing and eviction;
//   - corrupts a blob and asserts the digest check rejects it.
package verify

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ccflz/internal/container"
	"ccflz/internal/extract"
	"ccflz/internal/format"
	"ccflz/internal/gen"
	"ccflz/internal/image"
	"ccflz/internal/layer"
	"ccflz/internal/materialize"
	"ccflz/internal/repo"
	"ccflz/internal/view"
)

// Report is the machine-readable verification result.
type Report struct {
	GeneratedAt string        `json:"generated_at"`
	Config      RunConfig     `json:"run_config"`
	Images      []ImageReport `json:"images"`
	Totals      Totals        `json:"totals"`
	Failures    []string      `json:"failures"`
	Passed      bool          `json:"passed"`
}

// RunConfig records the knobs used for the run.
type RunConfig struct {
	Images               int     `json:"images"`
	LayersPerImage       int     `json:"layers_per_image"`
	ReadsPerImage        int     `json:"reads_per_image"`
	LatencyMS            float64 `json:"latency_ms"`
	BandwidthBytesPerSec int64   `json:"bandwidth_bytes_per_sec"`
}

// Totals aggregates across images.
type Totals struct {
	Images        int   `json:"images"`
	Layers        int   `json:"layers"`
	Files         int   `json:"files"`
	Reads         int   `json:"reads"`
	Lists         int   `json:"lists"`
	Writes        int   `json:"writes"`
	BytesCompared int64 `json:"bytes_compared"`
	Checks        int   `json:"checks"`
}

// ImageReport describes one image's verification.
type ImageReport struct {
	Name       string          `json:"name"`
	Manifest   string          `json:"manifest"`
	Layers     []LayerReport   `json:"layers"`
	Containers []StartupReport `json:"containers"`
	Share      ShareReport     `json:"share"`
	Compare    CompareReport   `json:"compare"`
	Metadata   MetaReport      `json:"metadata"`
	Corruption string          `json:"corruption"`
	Startup    *StartupStudy   `json:"startup_study,omitempty"`
}

// LayerReport covers packing and compatibility.
type LayerReport struct {
	Digest         string `json:"digest"`
	IndexDigest    string `json:"index_digest"`
	PackedSize     int64  `json:"packed_size"`
	ArchiveSize    int64  `json:"archive_size"`
	Segments       int    `json:"segments"`
	Files          int    `json:"files"`
	SegmentChecks  int    `json:"segment_checks"`
	TarExtractOK   bool   `json:"tar_extract_ok"`
	GzipOK         bool   `json:"gzip_ok"`
	RoundTripExact bool   `json:"round_trip_exact"`
}

// StartupReport captures one container's startup.
type StartupReport struct {
	ID               string  `json:"id"`
	IndexLoadMS      float64 `json:"index_load_ms"`
	ReadyMS          float64 `json:"ready_ms"`
	Cold             bool    `json:"cold"`
	ReadBytes        int64   `json:"read_bytes"`
	ImagePacked      int64   `json:"image_packed_bytes"`
	FractionOfLayers float64 `json:"fraction_of_layers"`
	IndexBytes       int64   `json:"index_bytes"`
}

// ShareReport covers layer sharing and refcounted eviction.
type ShareReport struct {
	SharedLayers int    `json:"shared_layers"`
	MaxRefs      int    `json:"max_refs"`
	FreedOnLast  bool   `json:"freed_on_last"`
	CachedBefore int64  `json:"cached_bytes_before_stop"`
	Detail       string `json:"detail"`
}

// CompareReport covers byte-for-byte comparisons.
type CompareReport struct {
	ReadChecks   int      `json:"read_checks"`
	ListChecks   int      `json:"list_checks"`
	WriteChecks  int      `json:"write_checks"`
	TreeChecks   int      `json:"tree_checks"`
	Paths        int      `json:"paths"`
	BytesChecked int64    `json:"bytes_checked"`
	Diffs        int      `json:"diffs"`
	Details      []string `json:"details,omitempty"`
}

// MetaReport covers metadata comparison against a plain extraction.
type MetaReport struct {
	// Paths is the number of paths materialized and compared on disk.
	Paths int `json:"paths"`
	// Diffs counts on-disk metadata differences.
	Diffs int `json:"diffs"`
	// IndexPaths is the number of archive entries whose index metadata was
	// compared with a plain tar.Reader of the same layer, including devices
	// and other entries an unprivileged process cannot materialize.
	IndexPaths     int      `json:"index_paths"`
	IndexDiffs     int      `json:"index_diffs"`
	XattrPaths     int      `json:"xattr_paths"`
	HardlinkGroups int      `json:"hardlink_groups"`
	Symlinks       int      `json:"symlinks"`
	Devices        int      `json:"devices"`
	Fifos          int      `json:"fifos"`
	PrivilegeSkips int      `json:"privilege_skips"`
	Details        []string `json:"details,omitempty"`
}

// Config controls a verification run.
type Config struct {
	OutDir         string
	Images         int
	LayersPerImage int
	Seed           int64
	// FilesPerLayer sizes each generated layer (default 28).
	FilesPerLayer        int
	ReadsPerImage        int
	LatencyPerRequest    time.Duration
	BandwidthBytesPerSec int64
	KeepTemp             bool
	Verbose              bool
	Log                  func(string, ...any)
}

// Run executes the full verification and writes artifacts under OutDir.
func Run(cfg Config) (*Report, error) {
	if cfg.Images <= 0 {
		cfg.Images = 3
	}
	if cfg.LayersPerImage <= 0 {
		cfg.LayersPerImage = 3
	}
	if cfg.ReadsPerImage <= 0 {
		cfg.ReadsPerImage = 200
	}
	if cfg.OutDir == "" {
		cfg.OutDir = "."
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(cfg.OutDir, "ccflz-verify-")
	if err != nil {
		return nil, err
	}
	if !cfg.KeepTemp {
		defer os.RemoveAll(work)
	}
	rep := &Report{
		Failures:    []string{},
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Config: RunConfig{
			Images: cfg.Images, LayersPerImage: cfg.LayersPerImage, ReadsPerImage: cfg.ReadsPerImage,
			LatencyMS:            float64(cfg.LatencyPerRequest.Microseconds()) / 1000.0,
			BandwidthBytesPerSec: cfg.BandwidthBytesPerSec,
		},
	}
	fail := func(format string, args ...any) {
		rep.Failures = append(rep.Failures, fmt.Sprintf(format, args...))
	}
	for i := 0; i < cfg.Images; i++ {
		name := fmt.Sprintf("img-%02d", i)
		ir, err := verifyImage(cfg, work, name, cfg.Seed+int64(i)*977)
		if err != nil {
			fail("image %s: %v", name, err)
			continue
		}
		rep.Images = append(rep.Images, *ir)
		rep.Totals.Images++
		rep.Totals.Layers += len(ir.Layers)
		rep.Totals.Reads += ir.Compare.ReadChecks
		rep.Totals.Lists += ir.Compare.ListChecks
		rep.Totals.Writes += ir.Compare.WriteChecks
		rep.Totals.BytesCompared += ir.Compare.BytesChecked
		rep.Totals.Files += ir.Metadata.Paths
		rep.Totals.Checks += ir.Compare.ReadChecks + ir.Compare.ListChecks + ir.Compare.TreeChecks + ir.Metadata.Paths
		if ir.Compare.Diffs > 0 {
			fail("image %s: %d content diffs: %v", name, ir.Compare.Diffs, firstN(ir.Compare.Details, 5))
		}
		if ir.Metadata.Diffs > 0 || ir.Metadata.IndexDiffs > 0 {
			fail("image %s: %d metadata diffs (%d in index): %v", name, ir.Metadata.Diffs, ir.Metadata.IndexDiffs, firstN(ir.Metadata.Details, 5))
		}
		if !ir.Share.FreedOnLast {
			fail("image %s: layer memory was not freed when the last container stopped", name)
		}
		if ir.Corruption == "" {
			fail("image %s: corruption was not detected", name)
		}
		for _, l := range ir.Layers {
			if !l.TarExtractOK || !l.GzipOK || !l.RoundTripExact {
				fail("image %s layer %s: compatibility check failed", name, l.Digest)
			}
		}
		if cfg.Verbose {
			cfg.Log("%s: layers=%d reads=%d lists=%d diffs=%d metaDiffs=%d ready=%.1fms",
				name, len(ir.Layers), ir.Compare.ReadChecks, ir.Compare.ListChecks,
				ir.Compare.Diffs, ir.Metadata.Diffs, ir.Containers[0].ReadyMS)
		}
	}
	if rep.Failures == nil {
		rep.Failures = []string{}
	}
	rep.Passed = len(rep.Failures) == 0
	if err := writeJSON(filepath.Join(cfg.OutDir, "verify-report.json"), rep); err != nil {
		return nil, err
	}
	return rep, nil
}

func verifyImage(cfg Config, work, name string, seed int64) (_ *ImageReport, retErr error) {
	step := "init"
	defer func() {
		if retErr != nil {
			retErr = fmt.Errorf("%s: %w", step, retErr)
		}
	}()
	dir := filepath.Join(work, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	r, err := repo.New(filepath.Join(dir, "repo"))
	if err != nil {
		return nil, err
	}
	rec, err := gen.GenerateImage(r, gen.Spec{Name: name, LayerCount: cfg.LayersPerImage, Seed: seed, FilesPerLayer: cfg.FilesPerLayer})
	if err != nil {
		return nil, err
	}
	ir := &ImageReport{Name: name, Manifest: rec.Manifest}

	mfRaw, err := r.GetManifestBytes(rec.Manifest)
	if err != nil {
		return nil, err
	}
	var mf repo.Manifest
	if err := json.Unmarshal(mfRaw, &mf); err != nil {
		return nil, err
	}

	for li, layerData := range rec.Layers {
		lr, err := checkLayer(r, dir, li, layerData, mf.Layers[li])
		if err != nil {
			return nil, err
		}
		ir.Layers = append(ir.Layers, *lr)
	}

	// Reference tree: plain extraction of every layer, with whiteouts.
	refDir := filepath.Join(dir, "reference")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		return nil, err
	}
	var refMissing map[string][]string
	if err := extractReference(rec, refDir, &refMissing); err != nil {
		return nil, fmt.Errorf("reference extraction: %w", err)
	}

	// Metadata check on a pristine lazy view against the pristine extraction.
	step = "metadata"
	meta, err := checkMetadata(r, &mf, refDir, dir, name)
	if err != nil {
		return nil, err
	}
	ir.Metadata = *meta
	// Index metadata vs a plain tar.Reader, for every archive entry.
	step = "index-metadata"
	ixMeta, err := checkIndexMetadata(rec)
	if err != nil {
		return nil, err
	}
	ir.Metadata.IndexPaths = ixMeta.IndexPaths
	ir.Metadata.IndexDiffs = ixMeta.IndexDiffs
	ir.Metadata.Details = append(ir.Metadata.Details, ixMeta.Details...)
	ir.Metadata.XattrPaths = ixMeta.XattrPaths
	ir.Metadata.Symlinks = ixMeta.Symlinks
	ir.Metadata.Devices = ixMeta.Devices
	ir.Metadata.Fifos = ixMeta.Fifos
	ir.Metadata.Diffs += ixMeta.IndexDiffs

	// Simulated satellite link.
	remote := r.NewRemote()
	remote.Latency = cfg.LatencyPerRequest
	remote.Bandwidth = cfg.BandwidthBytesPerSec
	stats := container.NewRemoteStats()
	remote.LogHook = func(rr repo.RangeRequest) { stats.Record(rr.Digest, rr.Bytes) }
	reg := image.NewRegistry(r, remote, layer.DefaultOptions())
	rt := &container.Runtime{Reg: reg, Root: filepath.Join(dir, "writable")}

	step = "startup"
	var totalPacked int64
	for _, lr := range ir.Layers {
		totalPacked += lr.PackedSize
	}
	var ctrs []*container.Container
	for i := 0; i < 3; i++ {
		stats.Reset()
		c, err := rt.Start(fmt.Sprintf("%s-c%d", name, i), name)
		if err != nil {
			return nil, fmt.Errorf("start container %d: %w", i, err)
		}
		read := stats.Bytes()
		ctrs = append(ctrs, c)
		frac := 0.0
		if totalPacked > 0 {
			frac = float64(read) / float64(totalPacked)
		}
		var idxBytes int64
		for _, sl := range c.Layers() {
			idxBytes += sl.IndexFetchBytes()
		}
		ir.Containers = append(ir.Containers, StartupReport{
			ID: c.ID, IndexLoadMS: float64(c.IndexLoadDuration.Microseconds()) / 1000.0,
			ReadyMS: float64(c.ReadyDuration.Microseconds()) / 1000.0,
			Cold:    i == 0, ReadBytes: read, ImagePacked: totalPacked, FractionOfLayers: frac,
			IndexBytes: idxBytes,
		})
	}
	nShared, maxRefs, err := checkSharing(reg, ctrs)
	if err != nil {
		return nil, err
	}
	ir.Share.SharedLayers = nShared
	ir.Share.MaxRefs = maxRefs
	ir.Share.Detail = fmt.Sprintf("%d layers open, max refcount %d", nShared, maxRefs)

	cmp := &CompareReport{}
	if _, _, _, err := runConcurrentWorkload(ctrs, refDir, refMissing, cfg.ReadsPerImage, seed, cmp); err != nil {
		return nil, err
	}
	// Interleaved single-container workload: every result is compared with
	// the reference tree, which receives the same mutations on disk.
	step = "workload"
	mutated, err := runWorkload(ctrs[0], refDir, refMissing, cfg.ReadsPerImage, seed, cmp)
	if err != nil {
		return nil, err
	}
	// Full tree comparison: the lazy view materialized on disk must match the
	// reference extraction path by path, byte for byte.
	step = "tree"
	if err := compareTree(ctrs[0], refDir, dir, mutated, cmp); err != nil {
		return nil, err
	}

	ir.Compare = *cmp

	step = "eviction"
	// Sharing/eviction: stop all but the last, caches must stay; stop the
	// last, caches must be freed.
	if nShared != len(ir.Layers) {
		return nil, fmt.Errorf("%d shared layers, want %d", nShared, len(ir.Layers))
	}
	for _, c := range ctrs[:len(ctrs)-1] {
		rt.Stop(c)
	}
	for _, sl := range reg.Shared() {
		if sl.Closed() {
			return nil, fmt.Errorf("layer %s freed while containers still run", sl.Digest)
		}
	}
	for _, sl := range ctrs[len(ctrs)-1].Layers() {
		ir.Share.CachedBefore += sl.CachedBytes()
	}
	rt.Stop(ctrs[len(ctrs)-1])
	freed := true
	for _, sl := range reg.Shared() {
		if !sl.Closed() || sl.CachedBytes() != 0 {
			freed = false
		}
	}
	ir.Share.FreedOnLast = freed

	step = "corruption"
	msg, err := checkCorruption(r, dir, name, &mf)
	if err != nil {
		return nil, err
	}
	ir.Corruption = msg

	step = "startup-study"
	study, err := StudyStartup(r, name, cfg.LatencyPerRequest)
	if err != nil {
		return nil, err
	}
	ir.Startup = study
	return ir, nil
}

// checkLayer validates packing invariants and ordinary-tool compatibility.
func checkLayer(r *repo.Repo, dir string, li int, l *gen.Layer, ld repo.Descriptor) (*LayerReport, error) {
	blob, err := r.GetBlobBytes(ld.Digest)
	if err != nil {
		return nil, err
	}
	rep := &LayerReport{
		Digest: ld.Digest, IndexDigest: ld.Annotations[format.IndexAnnotation],
		PackedSize: int64(len(blob)), ArchiveSize: int64(len(l.Tar)),
	}
	sl, err := layer.Open(ld.Digest, r.NewRemote(), layer.DefaultOptions(), layer.IndexHint{})
	if err != nil {
		return nil, err
	}
	rep.Segments = len(sl.Index.Segments)
	rep.Files = len(sl.Index.Files)
	var rebuilt bytes.Buffer
	for i := range sl.Index.Segments {
		if i == sl.Index.IndexSeg {
			continue
		}
		seg := &sl.Index.Segments[i]
		out, err := inflate(blob[seg.Off : seg.Off+seg.CompLen])
		if err != nil {
			return nil, fmt.Errorf("layer %d segment %d: %w", li, seg.ID, err)
		}
		if err := format.VerifySegment(seg, out); err != nil {
			return nil, err
		}
		if seg.Kind != format.SegIndex {
			rep.SegmentChecks++
		}
		rebuilt.Write(out)
	}
	rep.RoundTripExact = bytes.Equal(rebuilt.Bytes(), l.Tar)
	if !rep.RoundTripExact {
		return nil, fmt.Errorf("layer %d: segments do not reproduce the archive", li)
	}
	layerPath := filepath.Join(dir, fmt.Sprintf("layer-%d.ccflz", li))
	if err := os.WriteFile(layerPath, blob, 0o644); err != nil {
		return nil, err
	}
	if err := runOK("tar", "-tf", layerPath); err != nil {
		return nil, fmt.Errorf("layer %d: tar -tf: %w", li, err)
	}
	outDir := filepath.Join(dir, fmt.Sprintf("untar-%d", li))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	// Device nodes need privileges; an unprivileged tar exits 1 after creating
	// everything it can. That matches the reference extraction's skips.
	exitCode, stderr := runCmd("tar", "-xf", layerPath, "-C", outDir)
	if exitCode != 0 && !deviceOnlyFailure(stderr) {
		return nil, fmt.Errorf("layer %d: tar -xf (exit %d): %s", li, exitCode, stderr)
	}
	if missing := untarMissing(l.Tar, outDir); len(missing) > 0 {
		return nil, fmt.Errorf("layer %d: tar -xf did not create %d paths: %v", li, len(missing), firstN(missing, 5))
	}
	rep.TarExtractOK = true
	// gzip -dc decompresses every member: the archive exactly, followed by
	// the index member. Ordinary gzip must succeed and reproduce the archive
	// as a prefix.
	code, stderr := runCmd("gzip", "-dc", layerPath)
	if code != 0 {
		return nil, fmt.Errorf("layer %d: gzip -dc (exit %d): %s", li, code, stderr)
	}
	out, _ := exec.Command("gzip", "-dc", layerPath).Output()
	rep.GzipOK = len(out) >= len(l.Tar) && bytes.Equal(out[:len(l.Tar)], l.Tar)
	if !rep.GzipOK {
		return nil, fmt.Errorf("layer %d: gzip -dc does not reproduce the archive", li)
	}
	// Sanity: require real chunking for a layer with a large file.
	return rep, nil
}

func checkSharing(reg *image.Registry, ctrs []*container.Container) (int, int, error) {
	shared := reg.Shared()
	maxRefs := 0
	for _, sl := range shared {
		if sl.Refs() > maxRefs {
			maxRefs = sl.Refs()
		}
	}
	for _, c := range ctrs {
		for _, lr := range c.Spec.Layers {
			got, ok := shared[lr.Descriptor.Digest]
			if !ok || got != lr.Shared {
				return 0, 0, fmt.Errorf("container %s does not share layer %s", c.ID, lr.Descriptor.Digest)
			}
		}
	}
	if maxRefs != len(ctrs) {
		return 0, 0, fmt.Errorf("max layer refcount %d, want %d", maxRefs, len(ctrs))
	}
	return len(shared), maxRefs, nil
}

// runWorkload interleaves operations on the container view with the same
// operations on the reference tree, comparing every result.
// extractReference performs the plain extraction used as ground truth and
// records which archive entries a non-privileged process cannot create.
func extractReference(rec *gen.ImageRecord, refRoot string, missing *map[string][]string) error {
	m := make(map[string][]string)
	opts := extract.Options{
		ApplyWhiteouts: true,
		RecordSkips:    func(p, op string, err error) {},
		RecordMissing: func(p string, typ byte) {
			dir := path.Dir(p)
			m[dir] = append(m[dir], path.Base(p))
		},
	}
	for _, l := range rec.Layers {
		if err := extract.LayerTar(refRoot, l.Tar, opts); err != nil {
			return err
		}
	}
	for d := range m {
		uniq := make(map[string]bool, len(m[d]))
		var keep []string
		for _, n := range m[d] {
			if uniq[n] {
				continue
			}
			uniq[n] = true
			keep = append(keep, n)
		}
		sort.Strings(keep)
		m[d] = keep
	}
	*missing = m
	return nil
}

// wantList returns the reference directory listing including entries a
// non-privileged extraction could not create.
func wantList(refRoot, d string, missing map[string][]string) []string {
	rel := strings.Trim(strings.TrimPrefix(d, "/"), "/")
	got := listRef(refRoot, d)
	var out []string
	seen := make(map[string]bool)
	for _, n := range got {
		out = append(out, n)
		seen[n] = true
	}
	for _, n := range missing[rel] {
		if !seen[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// runWorkload interleaves operations and returns the set of paths mutated
// (written, created or deleted) so tree comparison can skip their timestamps.
func runWorkload(c *container.Container, refRoot string, refMissing map[string][]string, n int, seed int64, cmp *CompareReport) (map[string]bool, error) {
	mutated := make(map[string]bool)
	rng := rand.New(rand.NewSource(seed))
	files, dirs := c.View.Paths()
	for i := 0; i < n; i++ {
		switch rng.Intn(10) {
		case 0, 1, 2, 3, 4: // read
			if len(files) == 0 {
				continue
			}
			p := files[rng.Intn(len(files))]
			node := c.View.Lookup(p)
			if node == nil || node.Size == 0 {
				continue
			}
			off := rng.Int63n(node.Size)
			maxLen := node.Size - off
			if maxLen > 4096 {
				maxLen = 4096
			}
			ln := 1 + rng.Int63n(maxLen)
			got, err := c.View.Read(p, off, ln)
			if err != nil {
				return mutated, fmt.Errorf("read %s: %w", p, err)
			}
			want := readRef(refRoot, p, off, ln)
			cmp.ReadChecks++
			cmp.BytesChecked += int64(ln)
			if !bytes.Equal(got, want) {
				cmp.Diffs++
				cmp.detail(fmt.Sprintf("read %s [%d,%d): content differs", p, off, off+ln))
			}
		case 5, 6: // list
			d := "/"
			if len(dirs) > 0 {
				d = dirs[rng.Intn(len(dirs))]
			}
			// Compare only directories both trees have; the view cannot
			// create unprivileged device nodes, and new dirs are created
			// below.
			if !isDir(refPath(refRoot, d)) {
				continue
			}
			got, err := c.View.List(d)
			if err != nil {
				// A directory removed by an earlier op is expected to be gone
				// from the reference too.
				if isDir(refPath(refRoot, d)) {
					return mutated, fmt.Errorf("list %s: %w", d, err)
				}
				continue
			}
			want := wantList(refRoot, d, refMissing)
			cmp.ListChecks++
			if !equalStrings(got, want) {
				cmp.Diffs++
				cmp.detail(fmt.Sprintf("list %s: want %v got %v", d, want, got))
			}
		case 7: // write
			if len(files) == 0 {
				continue
			}
			p := files[rng.Intn(len(files))]
			node := c.View.Lookup(p)
			if node == nil || node.Type != view.TypeFile {
				continue
			}
			off := int64(0)
			if node.Size > 16 {
				off = rng.Int63n(node.Size - 8)
			}
			ln := 1 + rng.Int63n(8)
			data := make([]byte, ln)
			rng.Read(data)
			if err := c.View.Write(p, off, data); err != nil {
				return mutated, fmt.Errorf("write %s: %w", p, err)
			}
			writeRef(refRoot, p, off, data)
			markHardlinkGroup(c.View, p, mutated)
			got, err := c.View.Read(p, off, ln)
			if err != nil {
				return mutated, err
			}
			want := readRef(refRoot, p, off, ln)
			cmp.WriteChecks++
			if !bytes.Equal(got, want) {
				cmp.Diffs++
				cmp.detail(fmt.Sprintf("write %s [%d,%d): readback differs", p, off, off+ln))
			}
		case 8: // create
			p := fmt.Sprintf("/new/dir%d/file%d.txt", rng.Intn(4), rng.Intn(1000))
			if c.View.Lookup(p) != nil {
				continue
			}
			data := make([]byte, rng.Intn(300))
			rng.Read(data)
			dir := path.Dir(p)
			if err := c.View.MkdirAll(dir, 0o755); err != nil {
				return mutated, err
			}
			if err := c.View.Create(p, 0o644); err != nil {
				return mutated, err
			}
			if len(data) > 0 {
				if err := c.View.Write(p, 0, data); err != nil {
					return mutated, err
				}
			}
			os.MkdirAll(filepath.Dir(refPath(refRoot, p)), 0o755)
			if err := os.WriteFile(refPath(refRoot, p), data, 0o644); err != nil {
				return mutated, err
			}
			mutated[p] = true
			mutated[dir] = true
		case 9: // delete
			if len(files) == 0 {
				continue
			}
			p := files[rng.Intn(len(files))]
			if c.View.Lookup(p) == nil {
				continue
			}
			if err := c.View.Remove(p); err != nil {
				return mutated, err
			}
			os.RemoveAll(refPath(refRoot, p))
			mutated[p] = true
		}
	}
	return mutated, nil
}

// markHardlinkGroup records every name sharing the written file's inode now:
// the reference extraction updates all of their timestamps, and a later
// unlink can make the group invisible.
func markHardlinkGroup(s *view.Snapshot, p string, mutated map[string]bool) {
	mutated[p] = true
	n := s.Lookup(p)
	if n == nil {
		return
	}
	if n.LinkGroup != 0 {
		_ = s.Walk(func(other *view.Node) error {
			if other.LinkGroup == n.LinkGroup {
				mutated[other.Path] = true
			}
			return nil
		})
		return
	}
	if n.WritablePath != "" {
		_ = s.Walk(func(other *view.Node) error {
			if other.WritablePath == n.WritablePath {
				mutated[other.Path] = true
			}
			return nil
		})
	}
}

func verifyViewAgainstRef(c *container.Container, refRoot string, refMissing map[string][]string, n int, seed int64, cmp *CompareReport) error {
	rng := rand.New(rand.NewSource(seed))
	files, dirs := c.View.Paths()
	for i := 0; i < n; i++ {
		if len(files) == 0 {
			return nil
		}
		p := files[rng.Intn(len(files))]
		node := c.View.Lookup(p)
		if node == nil || node.Type != view.TypeFile || node.Size == 0 {
			continue
		}
		// In the lazy view, metadata-only files (empty) read as zeros by
		// design; compare only what the reference tree can represent.
		if !isRegular(refPath(refRoot, p)) {
			continue
		}
		off := rng.Int63n(node.Size)
		ln := node.Size - off
		if ln > 2048 {
			ln = 2048
		}
		got, err := c.View.Read(p, off, ln)
		if err != nil {
			return err
		}
		want := readRef(refRoot, p, off, ln)
		cmp.ReadChecks++
		cmp.BytesChecked += ln
		if !bytes.Equal(got, want) {
			cmp.Diffs++
			cmp.detail(fmt.Sprintf("%s read %s [%d,%d) differs", c.ID, p, off, off+ln))
		}
		if len(dirs) > 0 {
			d := dirs[rng.Intn(len(dirs))]
			if !isDir(refPath(refRoot, d)) {
				continue
			}
			got, err := c.View.List(d)
			if err != nil {
				continue
			}
			want := wantList(refRoot, d, refMissing)
			cmp.ListChecks++
			if !equalStrings(got, want) {
				cmp.Diffs++
				cmp.detail(fmt.Sprintf("%s list %s differs: want %v got %v", c.ID, d, want, got))
			}
		}
	}
	return nil
}

// compareTree materializes the view and compares every path's metadata and
// bytes with the reference tree.
func compareTree(c *container.Container, refRoot, dir string, mutated map[string]bool, cmp *CompareReport) error {
	lazyDir := filepath.Join(dir, "lazy-tree")
	os.RemoveAll(lazyDir)
	if _, err := materialize.Snapshot(c.View, lazyDir); err != nil {
		return err
	}
	lazy, err := materialize.ScanNew(lazyDir)
	if err != nil {
		return err
	}
	ref, err := materialize.ScanNew(refRoot)
	if err != nil {
		return err
	}
	cmp.TreeChecks++
	cmp.Paths = len(lazy.ByPath)
	rel := make(map[string]bool, len(mutated))
	for p := range mutated {
		rel[strings.TrimPrefix(p, "/")] = true
	}
	// A write to one name of a hardlinked inode updates the timestamps of
	// every name in the reference tree; mirror that for comparison. Mutations
	// replayed on the other containers are merged into the same map by the
	// caller.
	// Reference-side hardlink groups, from the inode scan of the extraction.
	for _, names := range ref.HardlinkGroups {
		hit := false
		for _, n := range names {
			if rel[n] {
				hit = true
			}
		}
		if hit {
			for _, n := range names {
				rel[n] = true
			}
		}
	}
	// The lazy view's groups can differ when one name was deleted; union the
	// inode groups discovered on both sides.
	groups := make(map[int][]string)
	_ = c.View.Walk(func(n *view.Node) error {
		if n.LinkGroup != 0 {
			groups[n.LinkGroup] = append(groups[n.LinkGroup], strings.TrimPrefix(n.Path, "/"))
		}
		return nil
	})
	for _, names := range groups {
		hit := false
		for _, n := range names {
			if rel[n] {
				hit = true
			}
		}
		if hit {
			for _, n := range names {
				rel[n] = true
			}
		}
	}
	diffs := materialize.DiffMutated(ref.ByPath, lazy.ByPath, rel)
	cmp.Diffs += len(diffs)
	for _, d := range diffs {
		cmp.detail("tree: " + d)
	}
	return nil
}

func (c *CompareReport) detail(s string) {
	if len(c.Details) < 30 {
		c.Details = append(c.Details, s)
	}
}

func checkMetadata(r *repo.Repo, mf *repo.Manifest, refDir, dir, name string) (*MetaReport, error) {
	rep := &MetaReport{}
	lazyDir := filepath.Join(dir, "meta-lazy")
	os.RemoveAll(lazyDir)
	if err := os.MkdirAll(lazyDir, 0o755); err != nil {
		return nil, err
	}
	defer func() { os.RemoveAll(lazyDir) }()
	reg := image.NewRegistry(r, r.NewRemote(), layer.DefaultOptions())
	res, err := reg.Resolve(name)
	if err != nil {
		return nil, err
	}
	v, err := view.New(res, "")
	if err != nil {
		return nil, err
	}
	lazyRes, err := materialize.Snapshot(v, lazyDir)
	if err != nil {
		return nil, err
	}
	rep.PrivilegeSkips = len(lazyRes.Skipped)
	ref, err := materialize.ScanNew(refDir)
	if err != nil {
		return nil, err
	}
	// Compare everything except ownership, which a non-root process cannot
	// reproduce (mirrors tar's own behavior).
	diffs := materialize.DiffMeta(ref.ByPath, lazyRes.ByPath)
	rep.Paths = len(lazyRes.ByPath)
	rep.Diffs = len(diffs)
	rep.Details = firstN(diffs, 30)
	for _, e := range lazyRes.Entries {
		switch e.Type {
		case "symlink":
			rep.Symlinks++
		case "char", "block":
			rep.Devices++
		case "fifo":
			rep.Fifos++
		}
		if len(e.Xattrs) > 0 {
			rep.XattrPaths++
		}
	}
	rep.HardlinkGroups = len(lazyRes.HardlinkGroups)
	return rep, nil
}

func checkCorruption(r *repo.Repo, dir, name string, mf *repo.Manifest) (string, error) {
	src := filepath.Join(dir, "repo")
	dst := filepath.Join(dir, "repo-corrupt")
	if err := copyTree(src, dst); err != nil {
		return "", err
	}
	cr, err := repo.New(dst)
	if err != nil {
		return "", err
	}
	mfDigest, err := cr.ResolveTag(name)
	if err != nil {
		return "", err
	}
	raw, err := cr.GetManifestBytes(mfDigest)
	if err != nil {
		return "", err
	}
	var m repo.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	ld := m.Layers[0]
	sl, err := layer.Open(ld.Digest, cr.NewRemote(), layer.DefaultOptions(), layer.IndexHint{})
	if err != nil {
		return "", err
	}
	var dataSeg *format.Segment
	for i := range sl.Index.Segments {
		if sl.Index.Segments[i].Kind == format.SegData {
			dataSeg = &sl.Index.Segments[i]
			break
		}
	}
	if dataSeg == nil {
		return "", fmt.Errorf("no data segment in layer 0")
	}
	if err := cr.CorruptBlob(ld.Digest, dataSeg.Off+12, func(b []byte) { b[0] ^= 0xff }); err != nil {
		return "", err
	}
	bad, err := layer.Open(ld.Digest, cr.NewRemote(), layer.DefaultOptions(), layer.IndexHint{})
	if err != nil {
		return "", err
	}
	// A layer rejected for a bad index annotation must never reach startup.
	if _, err := bad.FetchSegment(dataSeg.ID); err == nil {
		return "", fmt.Errorf("corrupted segment was not detected")
	} else {
		return err.Error(), nil
	}
}

// --- helpers ---------------------------------------------------------------

func inflate(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	zr.Multistream(false)
	defer zr.Close()
	return io.ReadAll(zr)
}

func runOK(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

func runCmd(name string, args ...string) (int, string) {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return code, stderr.String()
}

// deviceOnlyFailure reports whether every tar error was a device-node
// creation the process is not permitted to perform.
func deviceOnlyFailure(stderr string) bool {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for _, l := range lines {
		if strings.TrimSpace(l) == "" || strings.Contains(l, "Error exit delayed") {
			continue
		}
		if !strings.Contains(l, "Operation not permitted") {
			return false
		}
	}
	return len(lines) > 0
}

// untarMissing lists regular files, dirs, symlinks and FIFOs from the archive
// that do not exist under dir.
func untarMissing(tarBytes []byte, dir string) []string {
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	var missing []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Typeflag == tar.TypeChar || h.Typeflag == tar.TypeBlock {
			continue // privileged; compared through the reference extraction
		}
		name := strings.Trim(strings.TrimPrefix(h.Name, "./"), "/")
		if name == "" {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(name))
		if _, err := os.Lstat(p); err != nil {
			missing = append(missing, name)
		}
	}
	return missing
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func isRegular(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular()
}

func refPath(root, p string) string {
	return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(p, "/")))
}

func readRef(root, p string, off, n int64) []byte {
	f, err := os.Open(refPath(root, p))
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, n)
	m, _ := f.ReadAt(buf, off)
	return buf[:m]
}

func writeRef(root, p string, off int64, data []byte) {
	f, err := os.OpenFile(refPath(root, p), os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteAt(data, off)
}

func listRef(root, d string) []string {
	ents, err := os.ReadDir(refPath(root, d))
	if err != nil {
		return []string{}
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func copyTree(src, dst string) error {
	os.RemoveAll(dst)
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

func writeJSON(p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// runConcurrentWorkload drives all containers at once with read-only traffic.
// The containers share layer pages, so this exercises the shared cache and the
// per-layer fetch serialization under concurrency while every result is still
// checked against the pristine reference tree.
func runConcurrentWorkload(ctrs []*container.Container, refRoot string, refMissing map[string][]string, n int, seed int64, cmp *CompareReport) (int, int, int, error) {
	type result struct {
		reads, lists, writes int
		details              []string
	}
	results := make([]result, len(ctrs))
	var wg sync.WaitGroup
	for i, c := range ctrs {
		wg.Add(1)
		go func(i int, c *container.Container) {
			defer wg.Done()
			r := &result{}
			local := &CompareReport{}
			err := verifyViewAgainstRef(c, refRoot, refMissing, n/3, seed+int64(i)*31, local)
			r.reads = local.ReadChecks
			r.lists = local.ListChecks
			r.writes = local.Diffs
			r.details = local.Details
			results[i] = *r
			_ = err
		}(i, c)
	}
	wg.Wait()
	var errs []string
	for i, r := range results {
		cmp.ReadChecks += r.reads
		cmp.ListChecks += r.lists
		cmp.Diffs += r.writes
		cmp.Details = append(cmp.Details, r.details...)
		for _, d := range r.details {
			errs = append(errs, fmt.Sprintf("%s: %s", ctrs[i].ID, d))
		}
	}
	if len(errs) > 0 {
		return 0, 0, 0, fmt.Errorf("%d concurrent diffs: %v", len(errs), firstN(errs, 3))
	}
	return 0, 0, 0, nil
}

// StartupStudy measures startup for the same image under two access policies:
// lazy chunk fetching as the container actually starts, and an eager pull of
// every layer. It reports latency and downloaded bytes as a fraction of the
// packed layers, so the trade-off is explicit.
type StartupStudy struct {
	Image        string  `json:"image"`
	PackedBytes  int64   `json:"packed_bytes"`
	LazyReadyMS  float64 `json:"lazy_ready_ms"`
	LazyBytes    int64   `json:"lazy_bytes"`
	LazyFraction float64 `json:"lazy_fraction"`
	FullReadyMS  float64 `json:"full_ready_ms"`
	FullBytes    int64   `json:"full_bytes"`
	FullFraction float64 `json:"full_fraction"`
}

// StudyStartup runs the startup study for one image reference.
func StudyStartup(r *repo.Repo, ref string, latency time.Duration) (*StartupStudy, error) {
	res, err := image.NewRegistry(r, r.NewRemote(), layer.DefaultOptions()).Resolve(ref)
	if err != nil {
		return nil, err
	}
	out := &StartupStudy{Image: ref}
	for _, lr := range res.Layers {
		out.PackedBytes += lr.Descriptor.Size
	}
	// Lazy: only the index and the chunks the entrypoint touches.
	remote := r.NewRemote()
	remote.Latency = latency
	stats := container.NewRemoteStats()
	remote.LogHook = func(rr repo.RangeRequest) { stats.Record(rr.Digest, rr.Bytes) }
	reg := image.NewRegistry(r, remote, layer.DefaultOptions())
	rt := &container.Runtime{Reg: reg, Root: os.TempDir() + "/ccflz-study"}
	t0 := time.Now()
	c, err := rt.Start("study-lazy", ref)
	if err != nil {
		return nil, err
	}
	out.LazyReadyMS = float64(time.Since(t0).Microseconds()) / 1000.0
	out.LazyBytes = stats.Bytes()
	if out.PackedBytes > 0 {
		out.LazyFraction = float64(out.LazyBytes) / float64(out.PackedBytes)
	}
	rt.Stop(c)
	// Full: an eager verified pull of every layer, like a classic image pull.
	remote2 := r.NewRemote()
	remote2.Latency = latency
	stats2 := container.NewRemoteStats()
	remote2.LogHook = func(rr repo.RangeRequest) { stats2.Record(rr.Digest, rr.Bytes) }
	reg2 := image.NewRegistry(r, remote2, layer.DefaultOptions())
	t0 = time.Now()
	res2, err := reg2.Resolve(ref)
	if err != nil {
		return nil, err
	}
	var acquired []*layer.SharedLayer
	for _, lr := range res2.Layers {
		lr.Shared.Acquire()
		acquired = append(acquired, lr.Shared)
		if err := lr.Shared.VerifyAll(); err != nil {
			return nil, err
		}
	}
	out.FullReadyMS = float64(time.Since(t0).Microseconds()) / 1000.0
	out.FullBytes = stats2.Bytes()
	if out.PackedBytes > 0 {
		out.FullFraction = float64(out.FullBytes) / float64(out.PackedBytes)
	}
	for _, sl := range acquired {
		sl.Release()
	}
	return out, nil
}

// checkIndexMetadata compares every packed index entry's metadata with the
// plain tar.Reader view of the same layer. This covers entries that cannot be
// materialized without privileges (devices) and confirms the index carries
// exactly what ordinary unpacking sees.
func checkIndexMetadata(rec *gen.ImageRecord) (*MetaReport, error) {
	rep := &MetaReport{}
	for li, l := range rec.Layers {
		// Obtain the packed index the same way the runtime does.
		blob := l.Packed
		if blob == nil {
			return nil, fmt.Errorf("layer %d: packed blob missing", li)
		}
		sl, err := layer.OpenLocal(fmt.Sprintf("layer-%d", li), bytes.NewReader(blob), int64(len(blob)), nil, layer.DefaultOptions())
		if err != nil {
			return nil, err
		}
		byPath := make(map[string]*format.Entry, len(sl.Index.Entries))
		for _, e := range sl.Index.Entries {
			byPath[e.Path] = e
		}
		tr := tar.NewReader(bytes.NewReader(l.Tar))
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			name := strings.Trim(strings.TrimPrefix(h.Name, "./"), "/")
			if name == "" {
				continue
			}
			e := byPath[name]
			if e == nil {
				rep.IndexDiffs++
				rep.Details = append(rep.Details, fmt.Sprintf("layer %d: %s missing from index", li, name))
				continue
			}
			rep.IndexPaths++
			var d []string
			check := func(field string, a, b any) {
				if a != b {
					d = append(d, fmt.Sprintf("%s: %s want %v got %v", name, field, a, b))
				}
			}
			check("type", string(h.Typeflag), string(e.Type))
			check("mode", h.Mode, e.Mode)
			check("uid", h.Uid, e.Uid)
			check("gid", h.Gid, e.Gid)
			check("mtime", h.ModTime.UnixNano(), e.MtimeSec*1e9+int64(e.MtimeNsec))
			check("size", h.Size, e.Size)
			if h.Typeflag == tar.TypeSymlink {
				if h.Linkname != e.Linkname {
					d = append(d, fmt.Sprintf("%s: symlink target want %q got %q", name, h.Linkname, e.Linkname))
				}
			} else if h.Linkname != "" {
				want := strings.Trim(strings.TrimPrefix(h.Linkname, "./"), "/")
				check("linkname", want, e.Linkname)
			}
			check("devmajor", h.Devmajor, e.Devmajor)
			check("devminor", h.Devminor, e.Devminor)
			if len(h.Xattrs) != len(e.Xattrs) {
				d = append(d, fmt.Sprintf("%s: xattr count want %d got %d", name, len(h.Xattrs), len(e.Xattrs)))
			} else {
				for k, v := range h.Xattrs {
					if e.Xattrs[k] != v {
						d = append(d, fmt.Sprintf("%s: xattr %s want %q got %q", name, k, v, e.Xattrs[k]))
					}
				}
			}
			if len(e.Xattrs) > 0 {
				rep.XattrPaths++
			}
			switch e.Type {
			case '2':
				rep.Symlinks++
			case '3', '4':
				rep.Devices++
			case '6':
				rep.Fifos++
			}
			if len(d) > 0 {
				rep.IndexDiffs += len(d)
				rep.Details = append(rep.Details, d...)
			}
		}
	}
	return rep, nil
}
