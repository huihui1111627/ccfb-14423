// Package layer provides the lazy, multi-tenant reader over a CCFLZ blob.
//
// One SharedLayer instance is opened per blob digest and shared by every
// container that references it. Decompressed blocks live in the layer's page
// cache; a block stays resident while any container holds a reference to the
// layer, and the cache is emptied when the last container releases it.
//
// The layer reads the blob exclusively through the range-request source, so
// every byte spent shows up on the dashboard and in the tests.
package layer

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"ccflz/internal/format"
)

// Stats accumulates counters exposed on the dashboard.
type Stats struct {
	Requests          atomic.Int64 // logical chunk requests
	Coalesced         atomic.Int64 // chunks fetched together with another chunk
	Holes             atomic.Int64 // merged extra compressed bytes fetched
	RemoteBytes       atomic.Int64 // compressed bytes downloaded
	IndexBytes        atomic.Int64 // compressed bytes spent loading indexes
	SegmentsFetched   atomic.Int64
	CacheHits         atomic.Int64
	CacheMisses       atomic.Int64
	VerifyFailures    atomic.Int64
	BytesDecompressed atomic.Int64
}

// Opener abstracts the (simulated) remote range-request source.
type Opener interface {
	FetchRange(digest string, off, length int64) ([]byte, error)
	Stat(digest string) (int64, error)
}

// Options tunes a shared layer.
type Options struct {
	// MaxHole is the largest compressed gap a range merge may pull in to
	// close the gap between two requested segments.
	MaxHole int64
	// MaxCoalesce limits how many segments one merged request may cover.
	MaxCoalesce int
	// SmallFileThreshold: files at or below this size are fetched whole on
	// first access, because a second round trip costs more than the bytes.
	SmallFileThreshold int64
	// MaxParallelFetches bounds concurrent network requests per layer.
	MaxParallelFetches int
}

// DefaultOptions are tuned for narrow satellite links: gaps up to 64 KiB are
// cheaper to fetch than to pay another round trip.
func DefaultOptions() Options {
	return Options{
		MaxHole:            format.DefaultMaxHole,
		MaxCoalesce:        format.DefaultMaxCoalesce,
		SmallFileThreshold: format.SmallFileThreshold,
		MaxParallelFetches: 4,
	}
}

// SharedLayer is one blob open by possibly many containers.
type SharedLayer struct {
	Digest string
	Index  *format.Index
	Opts   Options
	remote Opener

	mu          sync.Mutex
	cache       map[int][]byte
	refs        int
	closed      bool
	heat        []atomic.Int64
	indexRaw    []byte
	layerLen    int64
	indexWindow int64
	openDur     time.Duration

	Stats Stats

	// fetchSem bounds concurrent network fetches per layer.
	fetchSem chan struct{}
	// flight deduplicates concurrent fetches of the same segment.
	flight map[int]*flightCall
}

type flightCall struct {
	done        chan struct{}
	data        []byte
	err         error
	claimedFlag bool
}

// IndexHint tells Open where to find the index. When the manifest carries the
// annotation, the index is fetched in a single request.
type IndexHint struct {
	// Length is the compressed index size in bytes, 0 if unknown.
	Length int64
}

// Open locates the index over the remote and returns a shared layer.
func Open(digest string, remote Opener, opts Options, hint IndexHint) (*SharedLayer, error) {
	start := time.Now()
	size, err := remote.Stat(digest)
	if err != nil {
		return nil, err
	}
	var ix *format.Index
	var raw []byte
	var window int64
	if hint.Length > 0 && hint.Length <= size {
		ix, raw, window, err = readIndexHinted(digest, size, hint.Length, remote)
	} else {
		ix, raw, window, err = readIndexRemote(digest, size, remote)
	}
	if err != nil {
		return nil, err
	}
	if err := validateIndex(ix); err != nil {
		return nil, err
	}
	sl := &SharedLayer{
		Digest: digest, Index: ix, Opts: opts, remote: remote,
		cache: make(map[int][]byte), heat: make([]atomic.Int64, len(ix.Segments)),
		indexRaw: raw, layerLen: size, indexWindow: window, openDur: time.Since(start),
		fetchSem: make(chan struct{}, parallel(opts)), flight: make(map[int]*flightCall),
	}
	sl.Stats.IndexBytes.Add(window)
	sl.Stats.RemoteBytes.Add(window)
	return sl, nil
}

// OpenDuration reports how long locating and loading the index took.
func (s *SharedLayer) OpenDuration() time.Duration { return s.openDur }

// IndexFetchBytes reports the compressed bytes spent loading the index.
func (s *SharedLayer) IndexFetchBytes() int64 { return s.indexWindow }

// LayerSize reports the packed layer size.
func (s *SharedLayer) LayerSize() int64 { return s.layerLen }

// readIndexRemote fetches the smallest tail window that contains a decodable
// index. The index is the last gzip member, so growing the window until the
// member's start is covered keeps bytes-on-wire close to the index size.
func readIndexRemote(digest string, size int64, remote Opener) (*format.Index, []byte, int64, error) {
	if size < 32 {
		return nil, nil, 0, fmt.Errorf("layer: file too small (%d bytes)", size)
	}
	// Start small: a typical index for a few hundred files is a few KiB
	// compressed, and a satellite round trip costs far more than growing the
	// window once.
	window := int64(8 << 10)
	// The gzip trailer of the index member gives its exact length, so one
	// tail read of the member's last bytes is enough in the common case.
	probe := int64(8 << 10)
	if probe > size {
		probe = size
	}
	tail, err := remote.FetchRange(digest, size-probe, probe)
	if err != nil {
		return nil, nil, probe, err
	}
	fetched := probe
	if n, ok := memberStartFromTrailer(tail, size-probe); ok {
		start := n
		member := tail[start-(size-probe):]
		extra := int64(0)
		if start < size-probe {
			// The member is longer than the probe window; fetch the rest.
			head, err := remote.FetchRange(digest, start, size-probe-start)
			if err != nil {
				return nil, nil, fetched, err
			}
			extra = int64(len(head))
			member = append(append([]byte(nil), head...), member...)
		}
		if ix, raw, off, err := findIndexAt(member, start); err == nil {
			ix.Segments[ix.IndexSeg].CompLen = size - off
			return ix, raw, fetched + extra, nil
		}
	}
	for {
		if window > size {
			window = size
		}
		start := size - window
		buf, err := remote.FetchRange(digest, start, window)
		if err != nil {
			return nil, nil, window, err
		}
		ix, raw, off, err := findIndex(buf, start)
		if err == nil {
			ix.Segments[ix.IndexSeg].CompLen = size - off
			return ix, raw, int64(len(buf)), nil
		}
		if window == size {
			return nil, nil, window, fmt.Errorf("layer %s: locate index in last %d bytes: %w", digest, window, err)
		}
		window *= 2
		if window > size {
			window = size
		}
	}
}

// findIndex scans buf (covering absolute range base..base+len(buf)) backwards
// for gzip members that decode to a plausible CCFLZ index.
func findIndex(buf []byte, base int64) (*format.Index, []byte, int64, error) {
	var lastErr error
	for i := len(buf) - 2; i >= 0; i-- {
		if buf[i] != 0x1f || buf[i+1] != 0x8b {
			continue
		}
		off := base + int64(i)
		raw, err := inflateMember(buf[i:])
		if err != nil {
			lastErr = err
			continue
		}
		var ix format.Index
		if err := json.Unmarshal(raw, &ix); err != nil {
			lastErr = err
			continue
		}
		if ix.Version != format.IndexVersion || len(ix.Segments) == 0 || ix.IndexSeg != len(ix.Segments)-1 {
			lastErr = fmt.Errorf("candidate at %d is not a CCFLZ index", off)
			continue
		}
		if ix.Segments[ix.IndexSeg].Off != off {
			lastErr = fmt.Errorf("index offset mismatch at %d", off)
			continue
		}
		if ix.IndexSeg > 0 {
			prev := ix.Segments[ix.IndexSeg-1]
			if prev.Off+prev.CompLen != off {
				lastErr = fmt.Errorf("index is not adjacent to the previous segment at %d", off)
				continue
			}
		}
		return &ix, raw, off, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no gzip member found")
	}
	return nil, nil, 0, lastErr
}

// memberStartFromTrailer finds the start of the last gzip member from a tail
// buffer. The layer always ends with the index member, so this is the index
// offset: a gzip member ends with an 8-byte trailer (CRC32 + ISIZE), and the
// decompressed size tells us nothing about the compressed start, so we scan
// for magic bytes and keep the first candidate that inflates to the end.
func memberStartFromTrailer(tail []byte, base int64) (int64, bool) {
	for i := 0; i+18 <= len(tail); i++ {
		if tail[i] != 0x1f || tail[i+1] != 0x8b || tail[i+2] != 0x08 {
			continue
		}
		// A candidate must inflate cleanly AND consume every byte through the
		// end of the tail: a false magic inside compressed data will not.
		cr := &countingReader{r: bytes.NewReader(tail[i:])}
		zr, err := gzip.NewReader(cr)
		if err != nil {
			continue
		}
		zr.Multistream(false)
		if _, err := io.ReadAll(zr); err != nil {
			zr.Close()
			continue
		}
		zr.Close()
		if cr.n != int64(len(tail)-i) {
			continue
		}
		return base + int64(i), true
	}
	return 0, false
}

// countingReader counts consumed bytes.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// findIndexAt decodes a whole-member buffer whose first byte is the index.
func findIndexAt(buf []byte, base int64) (*format.Index, []byte, int64, error) {
	raw, err := inflateMember(buf)
	if err != nil {
		return nil, nil, 0, err
	}
	var ix format.Index
	if err := json.Unmarshal(raw, &ix); err != nil {
		return nil, nil, 0, err
	}
	if ix.Version != format.IndexVersion || len(ix.Segments) == 0 || ix.IndexSeg != len(ix.Segments)-1 {
		return nil, nil, 0, fmt.Errorf("not a CCFLZ index")
	}
	if ix.Segments[ix.IndexSeg].Off != base {
		return nil, nil, 0, fmt.Errorf("index offset mismatch")
	}
	return &ix, raw, base, nil
}

func inflateMember(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	zr.Multistream(false)
	defer zr.Close()
	return io.ReadAll(zr)
}

func validateIndex(ix *format.Index) error {
	if len(ix.Segments) == 0 {
		return fmt.Errorf("layer: empty index")
	}
	prevEnd := int64(0)
	for i := range ix.Segments {
		s := &ix.Segments[i]
		if s.ID != i {
			return fmt.Errorf("layer: segment %d has id %d", i, s.ID)
		}
		if s.Off != prevEnd {
			return fmt.Errorf("layer: segment %d at %d leaves a gap after %d", i, s.Off, prevEnd)
		}
		if s.CompLen < 0 || s.Len < 0 {
			return fmt.Errorf("layer: segment %d has invalid lengths", i)
		}
		if i == ix.IndexSeg {
			return nil // last member; its extent runs to the end of the file
		}
		if s.CompLen == 0 {
			return fmt.Errorf("layer: segment %d has zero compressed length", i)
		}
		prevEnd = s.Off + s.CompLen
	}
	return nil
}

// Acquire registers one container using the layer.
func (s *SharedLayer) Acquire() {
	s.mu.Lock()
	s.refs++
	s.mu.Unlock()
}

// Release drops one container reference. The last release empties the page
// cache so the layer's memory is freed; freed reports whether that happened.
func (s *SharedLayer) Release() (freed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refs > 0 {
		s.refs--
	}
	if s.refs == 0 && !s.closed {
		s.closed = true
		s.cache = nil
		return true
	}
	return false
}

// Refs returns the number of live containers.
func (s *SharedLayer) Refs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refs
}

// Closed reports whether the cache has been dropped.
func (s *SharedLayer) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// CachedSegments lists resident segment ids, sorted.
func (s *SharedLayer) CachedSegments() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, 0, len(s.cache))
	for id := range s.cache {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

// CachedBytes returns resident decompressed bytes.
func (s *SharedLayer) CachedBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, b := range s.cache {
		n += int64(len(b))
	}
	return n
}

// Heat returns per-segment request counts.
func (s *SharedLayer) Heat() []int64 {
	out := make([]int64, len(s.heat))
	for i := range s.heat {
		out[i] = s.heat[i].Load()
	}
	return out
}

// Segment returns segment metadata by id (nil when out of range).
func (s *SharedLayer) Segment(id int) *format.Segment { return s.Index.Segment(id) }

// RawIndex returns the decompressed index bytes, as committed by the manifest
// annotation.
func (s *SharedLayer) RawIndex() ([]byte, error) {
	if s.indexRaw == nil {
		return nil, fmt.Errorf("layer %s: index not loaded", s.Digest)
	}
	return s.indexRaw, nil
}

// FetchSegment returns the decompressed bytes of segment id, caching them.
func (s *SharedLayer) FetchSegment(id int) ([]byte, error) {
	data, _, err := s.BatchFetch([]int{id})
	if err != nil {
		return nil, err
	}
	return data[id], nil
}

// FetchRange returns file bytes [off, off+n) of the given file.
func (s *SharedLayer) FetchRange(f *format.File, off, n int64) ([]byte, error) {
	if off < 0 || n < 0 || off+n > f.Size {
		return nil, fmt.Errorf("layer: range [%d,%d) outside %q (size %d)", off, off+n, f.Path, f.Size)
	}
	if n == 0 {
		return []byte{}, nil
	}
	out := make([]byte, n)
	first := f.ChunkOf(off)
	last := f.ChunkOf(off + n - 1)
	if first < 0 {
		return out, nil
	}
	ids := make([]int, 0, last-first+1)
	for c := first; c <= last; c++ {
		ids = append(ids, f.Segs[c])
	}
	data, _, err := s.BatchFetch(ids)
	if err != nil {
		return nil, err
	}
	for c := first; c <= last; c++ {
		cs, _ := f.ChunkRange(c)
		chunk := data[f.Segs[c]]
		if chunk == nil {
			return nil, fmt.Errorf("layer: missing chunk %d of %q", c, f.Path)
		}
		lo := int64(0)
		if off > cs {
			lo = off - cs
		}
		hi := int64(len(chunk))
		if off+n < cs+hi {
			hi = off + n - cs
		}
		if lo >= hi {
			continue
		}
		copy(out[cs+lo-off:], chunk[lo:hi])
	}
	return out, nil
}

// FetchFile returns a whole file, requesting all of its chunks at once.
func (s *SharedLayer) FetchFile(f *format.File) ([]byte, error) {
	return s.FetchRange(f, 0, f.Size)
}

// BatchFetch fetches and caches the given segments, merging adjacent ranges
// within MaxHole and MaxCoalesce. It returns decompressed data by segment id
// and the number of network requests issued.
func (s *SharedLayer) BatchFetch(ids []int) (map[int][]byte, int, error) {
	for _, id := range ids {
		if id < 0 || id >= len(s.Index.Segments) {
			return nil, 0, fmt.Errorf("layer: segment id %d out of range", id)
		}
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, 0, fmt.Errorf("layer %s: released (no live containers)", s.Digest)
	}

	// Join in-flight fetches for the same segments (concurrent containers),
	// then fetch whatever is still missing. The goroutine that registers a
	// flight call owns it; everyone else waits for its result.
	out := make(map[int][]byte, len(ids))
	var waits []*flightCall
	var waitIDs []int
	var mine []*flightCall
	var mineIDs []int
	seenReg := make(map[int]bool, len(ids))
	s.mu.Lock()
	for _, id := range ids {
		if seenReg[id] {
			continue
		}
		seenReg[id] = true
		if b, ok := s.cache[id]; ok {
			out[id] = b
			s.Stats.CacheHits.Add(1)
			continue
		}
		if call, inflight := s.flight[id]; inflight {
			waits = append(waits, call)
			waitIDs = append(waitIDs, id)
			continue
		}
		call := &flightCall{done: make(chan struct{})}
		s.flight[id] = call
		mine = append(mine, call)
		mineIDs = append(mineIDs, id)
	}
	s.mu.Unlock()
	for i, call := range waits {
		<-call.done
		if call.err != nil {
			return nil, 0, call.err
		}
		out[waitIDs[i]] = call.data
	}
	finish := func(err error) {
		s.mu.Lock()
		for i, call := range mine {
			call.err = err
			if err == nil {
				call.data = out[mineIDs[i]]
			}
			delete(s.flight, mineIDs[i])
			close(call.done)
		}
		s.mu.Unlock()
	}
	if len(mine) == 0 {
		return out, 0, nil
	}

	for _, id := range ids {
		s.heat[id].Add(1)
		s.Stats.Requests.Add(1)
	}
	missing := append([]int(nil), mineIDs...)
	s.Stats.CacheMisses.Add(int64(len(missing)))
	if len(missing) == 0 {
		finish(nil)
		return out, 0, nil
	}
	sort.Ints(missing)
	groups := s.plan(missing)
	// Fetch independent groups concurrently, bounded by fetchSem. Each
	// goroutine returns its own map so there is no shared write.
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	var outMu sync.Mutex
	for _, g := range groups {
		wg.Add(1)
		go func(g group) {
			defer wg.Done()
			s.fetchSem <- struct{}{}
			defer func() { <-s.fetchSem }()
			got := make(map[int][]byte, len(g.segs))
			if err := s.fetchGroup(g, got); err != nil {
				errOnce.Do(func() { firstErr = err })
				return
			}
			outMu.Lock()
			for k, v := range got {
				out[k] = v
			}
			outMu.Unlock()
		}(g)
	}
	wg.Wait()
	if firstErr != nil {
		finish(firstErr)
		return nil, len(groups), firstErr
	}
	finish(nil)
	return out, len(groups), nil
}

// parallel returns the per-layer request concurrency.
func parallel(o Options) int {
	if o.MaxParallelFetches > 0 {
		return o.MaxParallelFetches
	}
	return 1
}

type group struct {
	segs     []int
	from, to int64 // compressed byte range
	hole     int64
}

// plan merges missing segments into fetch groups. Segments are sorted by id,
// and the packer lays chunks out contiguously per file, so id order is file
// order.
func (s *SharedLayer) plan(missing []int) []group {
	if len(missing) == 0 {
		return nil
	}
	maxHole := s.Opts.MaxHole
	maxCo := s.Opts.MaxCoalesce
	if maxCo <= 0 {
		maxCo = 1
	}
	first := &s.Index.Segments[missing[0]]
	groups := []group{{segs: []int{missing[0]}, from: first.Off, to: first.Off + first.CompLen}}
	cur := &groups[len(groups)-1]
	for _, id := range missing[1:] {
		seg := &s.Index.Segments[id]
		gap := seg.Off - cur.to
		if gap >= 0 && gap <= maxHole && len(cur.segs) < maxCo {
			cur.segs = append(cur.segs, id)
			cur.hole += gap
			cur.to = seg.Off + seg.CompLen
			s.Stats.Coalesced.Add(1)
			s.Stats.Holes.Add(gap)
			continue
		}
		groups = append(groups, group{segs: []int{id}, from: seg.Off, to: seg.Off + seg.CompLen})
		cur = &groups[len(groups)-1]
	}
	return groups
}

func (s *SharedLayer) fetchGroup(g group, out map[int][]byte) error {
	raw, err := s.remote.FetchRange(s.Digest, g.from, g.to-g.from)
	if err != nil {
		return fmt.Errorf("layer %s: fetch [%d,%d): %w", s.Digest, g.from, g.to, err)
	}
	s.Stats.RemoteBytes.Add(int64(len(raw)))
	for _, id := range g.segs {
		seg := &s.Index.Segments[id]
		lo := seg.Off - g.from
		hi := lo + seg.CompLen
		if lo < 0 || hi > int64(len(raw)) {
			return fmt.Errorf("layer: merged range does not cover segment %d", id)
		}
		data, err := inflateMember(raw[lo:hi])
		if err != nil {
			s.Stats.VerifyFailures.Add(1)
			return fmt.Errorf("layer %s segment %d: %w", s.Digest, id, err)
		}
		if err := format.VerifySegment(seg, data); err != nil {
			s.Stats.VerifyFailures.Add(1)
			return fmt.Errorf("layer %s: %w", s.Digest, err)
		}
		s.Stats.SegmentsFetched.Add(1)
		s.Stats.BytesDecompressed.Add(int64(len(data)))
		s.mu.Lock()
		if s.cache != nil {
			if _, exists := s.cache[id]; exists {
				data = s.cache[id]
			} else {
				s.cache[id] = data
			}
		}
		s.mu.Unlock()
		out[id] = data
	}
	return nil
}

// VerifyAll fetches every segment and checks its digest. Used by tests.
func (s *SharedLayer) VerifyAll() error {
	for i := range s.Index.Segments {
		if i == s.Index.IndexSeg {
			continue
		}
		if _, err := s.FetchSegment(i); err != nil {
			return err
		}
	}
	return nil
}

// FetchChunksOrFile returns file bytes [off, off+n). Small files are fetched
// whole: the extra bytes cost less than a second round trip on a narrow link.
func (s *SharedLayer) FetchChunksOrFile(f *format.File, off, n int64) ([]byte, error) {
	if s.Opts.SmallFileThreshold > 0 && f.Size > 0 && f.Size <= s.Opts.SmallFileThreshold &&
		len(f.Segs) <= 2 && (off != 0 || n != f.Size) {
		full, err := s.FetchFile(f)
		if err != nil {
			return nil, err
		}
		if off+n > f.Size {
			n = f.Size - off
		}
		return full[off : off+n], nil
	}
	return s.FetchRange(f, off, n)
}

// readIndexHinted fetches exactly the tail member whose compressed length the
// manifest advertises.
func readIndexHinted(digest string, size, length int64, remote Opener) (*format.Index, []byte, int64, error) {
	start := size - length
	if start < 0 {
		return nil, nil, 0, fmt.Errorf("layer %s: index length %d exceeds layer size %d", digest, length, size)
	}
	buf, err := remote.FetchRange(digest, start, length)
	if err != nil {
		return nil, nil, length, err
	}
	ix, raw, _, err := findIndexAt(buf, start)
	if err != nil {
		// Fall back to probing; a stale annotation must not break startup.
		return readIndexRemote(digest, size, remote)
	}
	ix.Segments[ix.IndexSeg].CompLen = length
	return ix, raw, length, nil
}

// OpenLocal opens a layer from a local blob. The remote remains the byte
// source for segment data; only the index is read from disk.
func OpenLocal(digest string, blob io.ReaderAt, size int64, remote Opener, opts Options) (*SharedLayer, error) {
	window := int64(1 << 20)
	if window > size {
		window = size
	}
	for {
		buf := make([]byte, window)
		if _, err := blob.ReadAt(buf, size-window); err != nil && err != io.EOF {
			return nil, err
		}
		ix, raw, off, err := findIndex(buf, size-window)
		if err == nil {
			ix.Segments[ix.IndexSeg].CompLen = size - off
			if err := validateIndex(ix); err != nil {
				return nil, err
			}
			return &SharedLayer{
				Digest: digest, Index: ix, Opts: opts, remote: remote,
				cache: make(map[int][]byte), heat: make([]atomic.Int64, len(ix.Segments)),
				indexRaw: raw, layerLen: size, indexWindow: window,
				fetchSem: make(chan struct{}, 1), flight: make(map[int]*flightCall),
			}, nil
		}
		if window == size {
			return nil, fmt.Errorf("layer: locate index: %w", err)
		}
		window *= 4
		if window > size {
			window = size
		}
	}
}
