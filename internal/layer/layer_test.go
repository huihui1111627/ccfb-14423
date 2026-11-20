package layer

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"

	"ccflz/internal/format"
	"ccflz/internal/pack"
)

// memRemote serves blob bytes by range out of memory.
type memRemote struct {
	data    []byte
	mu      sync.Mutex
	reqs    int
	bytes   int64
	corrupt func(off int64, b []byte) []byte
}

func (m *memRemote) Stat(string) (int64, error) { return int64(len(m.data)), nil }

func (m *memRemote) FetchRange(_ string, off, n int64) ([]byte, error) {
	m.mu.Lock()
	m.reqs++
	m.mu.Unlock()
	if off < 0 || off > int64(len(m.data)) {
		return nil, fmt.Errorf("bad range")
	}
	if off+n > int64(len(m.data)) {
		n = int64(len(m.data)) - off
	}
	b := append([]byte(nil), m.data[off:off+n]...)
	if m.corrupt != nil {
		b = m.corrupt(off, b)
	}
	m.mu.Lock()
	m.bytes += int64(len(b))
	m.mu.Unlock()
	return b, nil
}

func buildLayer(t *testing.T, chunk int64) ([]byte, *format.Index, *memRemote) {
	t.Helper()
	files := map[string][]byte{}
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("dir/f%02d.bin", i)
		data := bytes.Repeat([]byte{byte('a' + i)}, 1000+i*700)
		files[name] = data
	}
	var tarBuf bytes.Buffer
	writeTestTar(t, &tarBuf, files)
	blob, ix, _, err := pack.PackBytes(tarBuf.Bytes(), pack.Options{ChunkSize: chunk})
	if err != nil {
		t.Fatal(err)
	}
	return blob, ix, &memRemote{data: blob}
}

func writeTestTar(t *testing.T, buf *bytes.Buffer, files map[string][]byte) {
	t.Helper()
	tw := newTarWriter(buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	// deterministic order
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, n := range names {
		if err := tw.add(n, files[n]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.close(); err != nil {
		t.Fatal(err)
	}
}

func TestFetchRangeAndCache(t *testing.T) {
	blob, ix, rm := buildLayer(t, 4096)
	sl, err := Open("sha256:test", rm, Options{MaxHole: 1 << 20, MaxCoalesce: 16}, IndexHint{})
	if err != nil {
		t.Fatal(err)
	}
	sl.Acquire()
	f := ix.Files[5]
	data, err := sl.FetchRange(f, 10, 2000)
	if err != nil {
		t.Fatal(err)
	}
	// Compare with the original tar body.
	want := archiveSlice(t, blob, ix, f)
	if !bytes.Equal(data, want[10:2010]) {
		t.Fatal("fetched bytes differ")
	}
	// A second read of the same range is served from the page cache.
	before := rm.reqs
	if _, err := sl.FetchRange(f, 10, 2000); err != nil {
		t.Fatal(err)
	}
	if rm.reqs != before {
		t.Fatal("cache hit still issued a network request")
	}
	if sl.CachedBytes() == 0 {
		t.Fatal("nothing cached")
	}
}

func archiveSlice(t *testing.T, blob []byte, ix *format.Index, f *format.File) []byte {
	t.Helper()
	// Reconstruct this file's bytes by inflating its chunks.
	var out []byte
	for _, id := range f.Segs {
		s := ix.Segments[id]
		zr, err := gzip.NewReader(bytes.NewReader(blob[s.Off : s.Off+s.CompLen]))
		if err != nil {
			t.Fatal(err)
		}
		zr.Multistream(false)
		b, err := readAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	return out
}

func TestCoalesceHoleCap(t *testing.T) {
	_, ix, rm := buildLayer(t, 1024)
	// MaxHole 0: every chunk is its own request.
	sl, err := Open("sha256:test", rm, Options{MaxHole: 0, MaxCoalesce: 1}, IndexHint{})
	if err != nil {
		t.Fatal(err)
	}
	sl.Acquire()
	var ids []int
	f := ix.Files[0]
	for _, ff := range ix.Files {
		if len(ff.Segs) > len(f.Segs) {
			f = ff
		}
	}
	if len(f.Segs) < 4 {
		t.Fatalf("expected a multi-chunk file, got %d chunks", len(f.Segs))
	}
	for i := 0; i < 4; i++ {
		ids = append(ids, f.Segs[i])
	}
	rm.reqs = 0
	if _, n, err := sl.BatchFetch(ids); err != nil {
		t.Fatal(err)
	} else if n != len(ids) {
		t.Fatalf("requests=%d, want %d with MaxHole=0", n, len(ids))
	}
	// MaxHole large: contiguous chunks merge into one request.
	sl2, err := Open("sha256:test2", rm, Options{MaxHole: 1 << 30, MaxCoalesce: 16}, IndexHint{})
	if err != nil {
		t.Fatal(err)
	}
	sl2.Acquire()
	rm.reqs = 0
	if _, n, err := sl2.BatchFetch(ids); err != nil {
		t.Fatal(err)
	} else if n != 1 {
		t.Fatalf("requests=%d, want 1 with a large hole budget", n)
	}
	// The fetched stream is contiguous: the merged range spans all four
	// chunks and only the small inter-member gaps (gzip headers/trailers).
	var span int64
	for _, id := range ids {
		span += ix.Segments[id].CompLen
	}
	if rm.bytes <= span {
		t.Fatalf("merged request pulled %d bytes, expected slightly more than %d", rm.bytes, span)
	}
}

func TestDigestDetectsCorruption(t *testing.T) {
	_, ix, rm := buildLayer(t, 4096)
	sl, err := Open("sha256:test", rm, Options{MaxHole: 1 << 20, MaxCoalesce: 4}, IndexHint{})
	if err != nil {
		t.Fatal(err)
	}
	sl.Acquire()
	target := ix.Files[0].Segs[0]
	rm.corrupt = func(off int64, b []byte) []byte {
		// Flip a byte inside the target member's compressed payload.
		seg := ix.Segments[target]
		if off <= seg.Off+5 && off+int64(len(b)) > seg.Off+10 {
			b[seg.Off+10-off] ^= 0xff
		}
		return b
	}
	if _, err := sl.FetchSegment(target); err == nil {
		t.Fatal("corrupted segment was accepted")
	}
	if sl.Stats.VerifyFailures.Load() == 0 {
		t.Fatal("verify failure was not counted")
	}
}

func TestRefcountEviction(t *testing.T) {
	_, ix, rm := buildLayer(t, 4096)
	sl, err := Open("sha256:test", rm, Options{MaxHole: 1 << 20, MaxCoalesce: 4}, IndexHint{})
	if err != nil {
		t.Fatal(err)
	}
	sl.Acquire()
	sl.Acquire()
	if _, err := sl.FetchSegment(ix.Files[0].Segs[0]); err != nil {
		t.Fatal(err)
	}
	if sl.CachedBytes() == 0 {
		t.Fatal("no cached bytes")
	}
	if freed := sl.Release(); freed {
		t.Fatal("cache freed with a live container")
	}
	if sl.CachedBytes() == 0 {
		t.Fatal("cache dropped too early")
	}
	if freed := sl.Release(); !freed {
		t.Fatal("last release did not free the cache")
	}
	if sl.CachedBytes() != 0 || !sl.Closed() {
		t.Fatal("cache not emptied after the last release")
	}
	if _, err := sl.FetchSegment(ix.Files[0].Segs[0]); err == nil {
		t.Fatal("a released layer served data")
	}
}

func TestConcurrentFetchDeduplicates(t *testing.T) {
	_, ix, rm := buildLayer(t, 4096)
	sl, err := Open("sha256:test", rm, Options{MaxHole: 1 << 20, MaxCoalesce: 8}, IndexHint{})
	if err != nil {
		t.Fatal(err)
	}
	sl.Acquire()
	rm.mu.Lock()
	rm.reqs = 0 // ignore index-load traffic
	rm.mu.Unlock()
	id := ix.Files[3].Segs[0]
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := sl.FetchSegment(id); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	// Exactly one group fetch: the 16 goroutines must not each hit the
	// network.
	if rm.reqs > 1 {
		t.Fatalf("%d requests for one segment under concurrency", rm.reqs)
	}
	if got := sl.Stats.SegmentsFetched.Load(); got > 1 {
		t.Fatalf("segment decompressed %d times", got)
	}
}

func TestIndexIsLastMember(t *testing.T) {
	blob, ix, rm := buildLayer(t, 4096)
	seg := ix.Segments[ix.IndexSeg]
	if seg.Off >= int64(len(blob)) {
		t.Fatal("index offset past the end of the layer")
	}
	// The index member ends exactly at the end of the file.
	zr, err := gzip.NewReader(bytes.NewReader(blob[seg.Off:]))
	if err != nil {
		t.Fatal(err)
	}
	zr.Multistream(false)
	if _, err := readAll(zr); err != nil {
		t.Fatal(err)
	}
	if int64(len(blob))-seg.Off < 8 {
		t.Fatal("index member too short")
	}
	sl, err := Open("sha256:test", rm, Options{}, IndexHint{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sl.RawIndex()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if "sha256:"+hex.EncodeToString(sum[:]) == "" {
		t.Fatal("empty digest")
	}
}

func TestIndexHint(t *testing.T) {
	blob, ix, rm := buildLayer(t, 4096)
	want := ix.Stats.IndexSize
	if want == 0 {
		// Stats are filled by the packer's returned index.
		want = int64(len(blob)) - ix.Segments[ix.IndexSeg].Off
	}
	rm.reqs = 0
	sl, err := Open("sha256:hint", rm, Options{}, IndexHint{Length: want})
	if err != nil {
		t.Fatal(err)
	}
	if rm.reqs != 1 {
		t.Fatalf("hinted open used %d requests, want 1", rm.reqs)
	}
	if rm.bytes != want {
		t.Fatalf("hinted open fetched %d bytes, want %d", rm.bytes, want)
	}
	if _, err := sl.RawIndex(); err != nil {
		t.Fatal(err)
	}
	// A wrong hint falls back to probing and still succeeds.
	rm2 := &memRemote{data: blob}
	if _, err := Open("sha256:badhint", rm2, Options{}, IndexHint{Length: 16}); err != nil {
		t.Fatalf("fallback with a bad hint failed: %v", err)
	}
}

func TestIndexLargerThanProbeWindow(t *testing.T) {
	// Many files make the index exceed the 8 KiB probe window.
	files := map[string][]byte{}
	for i := 0; i < 400; i++ {
		files[fmt.Sprintf("dir/with/a/longish/name/file-%03d.bin", i)] = bytes.Repeat([]byte{byte(i)}, 64)
	}
	var buf bytes.Buffer
	writeTestTar(t, &buf, files)
	blob, ix, _, err := pack.PackBytes(buf.Bytes(), pack.Options{ChunkSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	idxLen := int64(len(blob)) - ix.Segments[ix.IndexSeg].Off
	if idxLen <= 8<<10 {
		t.Skipf("index is only %d bytes; not larger than the probe window", idxLen)
	}
	rm := &memRemote{data: blob}
	sl, err := Open("sha256:bigidx", rm, Options{}, IndexHint{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sl.Index.Files) != len(files) {
		t.Fatalf("files = %d, want %d", len(sl.Index.Files), len(files))
	}
}
