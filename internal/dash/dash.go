// Package dash serves the lazy-loading dashboard: layer layout, per-chunk
// fetch status, range-request log and startup timings.
package dash

import (
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"ccflz/internal/container"
	"ccflz/internal/format"
	"ccflz/internal/image"
	"ccflz/internal/layer"
	"ccflz/internal/repo"
)

// Server exposes the dashboard and a small JSON API.
type Server struct {
	Registry *image.Registry
	Repo     *repo.Repo
	Remote   *repo.Remote
	Runtime  *container.Runtime
	Image    string // reference served by the page

	mu       sync.Mutex
	requests []repo.RangeRequest
	startups []Startup
	tmpl     *template.Template
}

// New builds a dashboard server.
func New(reg *image.Registry, r *repo.Repo, remote *repo.Remote, rt *container.Runtime, ref string) (*Server, error) {
	s := &Server{Registry: reg, Repo: r, Remote: remote, Runtime: rt, Image: ref}
	t, err := template.New("dash").Parse(pageTemplate)
	if err != nil {
		return nil, err
	}
	s.tmpl = t
	remote.LogHook = func(rr repo.RangeRequest) {
		s.mu.Lock()
		s.requests = append(s.requests, rr)
		if len(s.requests) > 400 {
			s.requests = s.requests[len(s.requests)-400:]
		}
		s.mu.Unlock()
	}
	return s, nil
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.page)
	mux.HandleFunc("/api/state", s.state)
	mux.HandleFunc("/api/start", s.start)
	mux.HandleFunc("/api/prefetch", s.prefetch)
	mux.HandleFunc("/api/reset", s.reset)
	return mux
}

// LayerState is one layer's dashboard data.
type LayerState struct {
	Digest        string     `json:"digest"`
	Short         string     `json:"short"`
	Size          int64      `json:"size"`
	ArchiveSize   int64      `json:"archive_size"`
	IndexBytes    int64      `json:"index_bytes"`
	Segments      []SegState `json:"segments"`
	Files         int        `json:"files"`
	Refs          int        `json:"refs"`
	Closed        bool       `json:"closed"`
	CachedBytes   int64      `json:"cached_bytes"`
	Downloaded    int64      `json:"downloaded_bytes"`
	DownloadedPct float64    `json:"downloaded_pct"`
	Fetches       int64      `json:"fetches"`
	VerifyFails   int64      `json:"verify_failures"`
	// Holes is the extra compressed bytes pulled in by range merging;
	// Coalesced is how many chunk fetches rode along with another request.
	Holes     int64 `json:"coalesced_hole_bytes"`
	Coalesced int64 `json:"coalesced_chunks"`
	Hits      int64 `json:"cache_hits"`
	Misses    int64 `json:"cache_misses"`
}

// SegState is one segment in the layout view.
type SegState struct {
	ID      int     `json:"id"`
	Kind    string  `json:"kind"`
	Off     int64   `json:"off"`
	CompLen int64   `json:"comp_len"`
	Len     int64   `json:"len"`
	Fetched bool    `json:"fetched"`
	Heat    int64   `json:"heat"`
	Top     float64 `json:"top"`   // layout position 0..1
	Width   float64 `json:"width"` // fraction of the layer file
}

// FileState is one file entry for the directory view.
type FileState struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Chunks   int    `json:"chunks"`
	Fetched  int    `json:"fetched_chunks"`
	LayerIdx int    `json:"layer"`
}

// State is the JSON payload.
type State struct {
	Image    string              `json:"image"`
	Layers   []LayerState        `json:"layers"`
	Files    []FileState         `json:"files"`
	Requests []repo.RangeRequest `json:"requests"`
	Startups []Startup           `json:"startups"`
	Cold     StartupSummary      `json:"cold"`
	Warm     StartupSummary      `json:"warm"`
	Now      string              `json:"now"`
	Note     string              `json:"note"`
}

// Startup is one container start.
type Startup struct {
	ID       string  `json:"id"`
	IndexMS  float64 `json:"index_ms"`
	ReadyMS  float64 `json:"ready_ms"`
	Bytes    int64   `json:"bytes"`
	Fraction float64 `json:"fraction"`
	Cold     bool    `json:"cold"`
}

// StartupSummary aggregates startup timing.
type StartupSummary struct {
	IndexMS  float64 `json:"index_ms"`
	ReadyMS  float64 `json:"ready_ms"`
	Bytes    int64   `json:"bytes"`
	Fraction float64 `json:"fraction"`
}

func (s *Server) snapshot() (*State, error) {
	st := &State{Image: s.Image, Now: time.Now().Format(time.RFC3339)}
	res, err := s.Registry.Resolve(s.Image)
	if err != nil {
		return st, err
	}
	for li, lr := range res.Layers {
		sl := lr.Shared
		ls := LayerState{
			Digest: sl.Digest, Short: shortDigest(sl.Digest), Size: sl.LayerSize(),
			ArchiveSize: sl.Index.ArchiveSize, IndexBytes: sl.IndexFetchBytes(),
			Files: len(sl.Index.Files), Refs: sl.Refs(), Closed: sl.Closed(),
			CachedBytes: sl.CachedBytes(), Fetches: sl.Stats.SegmentsFetched.Load(),
			VerifyFails: sl.Stats.VerifyFailures.Load(),
			Holes:       sl.Stats.Holes.Load(), Coalesced: sl.Stats.Coalesced.Load(),
			Hits: sl.Stats.CacheHits.Load(), Misses: sl.Stats.CacheMisses.Load(),
		}
		if ls.Size > 0 {
			ls.DownloadedPct = float64(sl.Stats.RemoteBytes.Load()) / float64(ls.Size) * 100
		}
		cached := map[int]bool{}
		for _, id := range sl.CachedSegments() {
			cached[id] = true
		}
		heat := sl.Heat()
		for i := range sl.Index.Segments {
			seg := &sl.Index.Segments[i]
			ss := SegState{ID: seg.ID, Kind: seg.Kind.String(), Off: seg.Off, CompLen: seg.CompLen,
				Len: seg.Len, Fetched: cached[seg.ID]}
			if i < len(heat) {
				ss.Heat = heat[i]
			}
			if ls.Size > 0 {
				ss.Top = float64(seg.Off) / float64(ls.Size)
				ss.Width = math.Max(float64(seg.CompLen)/float64(ls.Size), 0.0008)
			}
			ls.Segments = append(ls.Segments, ss)
		}
		for _, f := range sl.Index.Files {
			fs := FileState{Path: f.Path, Size: f.Size, Chunks: len(f.Segs), LayerIdx: li}
			for _, id := range f.Segs {
				if cached[id] {
					fs.Fetched++
				}
			}
			st.Files = append(st.Files, fs)
		}
		st.Layers = append(st.Layers, ls)
	}
	s.mu.Lock()
	start := len(s.requests) - minInt(len(s.requests), 120)
	st.Requests = append([]repo.RangeRequest{}, s.requests[start:]...)
	s.mu.Unlock()
	for i := len(st.Requests)/2 - 1; i >= 0; i-- {
		j := len(st.Requests) - 1 - i
		st.Requests[i], st.Requests[j] = st.Requests[j], st.Requests[i]
	}
	if st.Layers == nil {
		st.Layers = []LayerState{}
	}
	if st.Files == nil {
		st.Files = []FileState{}
	}
	if st.Startups == nil {
		st.Startups = []Startup{}
	}
	st.Note = "数据每 1.5 秒刷新；启动阶段与下载比例均为本次会话实测。"
	// Startup summaries: the first container started is the cold one, later
	// ones reuse the shared layers.
	for i, su := range s.startups {
		st.Startups = append(st.Startups, su)
		if i == 0 {
			st.Cold = StartupSummary{IndexMS: su.IndexMS, ReadyMS: su.ReadyMS, Bytes: su.Bytes, Fraction: su.Fraction}
		} else {
			st.Warm = StartupSummary{IndexMS: su.IndexMS, ReadyMS: su.ReadyMS, Bytes: su.Bytes, Fraction: su.Fraction}
		}
	}
	return st, nil
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	st, err := s.snapshot()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(st)
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	st, err := s.snapshot()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	data := map[string]any{
		"Image": s.Image,
		"State": string(b),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, data); err != nil {
		fmt.Fprintln(os.Stderr, "dash template:", err)
	}
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	t0 := time.Now()
	c, err := s.Runtime.Start(fmt.Sprintf("dash-%d", time.Now().UnixNano()%100000), s.Image)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var totalPacked int64
	for _, lr := range c.Spec.Layers {
		totalPacked += lr.Descriptor.Size
	}
	bytes := c.LayerBytes
	frac := 0.0
	if totalPacked > 0 {
		frac = float64(bytes) / float64(totalPacked)
	}
	s.mu.Lock()
	cold := len(s.startups) == 0
	s.startups = append(s.startups, Startup{
		ID: c.ID, IndexMS: float64(c.IndexLoadDuration.Microseconds()) / 1000.0,
		ReadyMS: float64(time.Since(t0).Microseconds()) / 1000.0,
		Bytes:   bytes, Fraction: frac, Cold: cold,
	})
	s.mu.Unlock()
	// Warm a few files so the page shows progress; then keep the container
	// running so the layer stays referenced.
	files, _ := c.View.Paths()
	for i := 0; i < len(files) && i < 6; i++ {
		c.View.ReadAll(files[i])
	}
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]any{"id": c.ID, "ready_ms": c.ReadyDuration.Milliseconds()})
}

func (s *Server) prefetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	res, err := s.Registry.Resolve(s.Image)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Fetch the hottest 25% of segments, as an access-pattern prefetcher would.
	type cand struct {
		sl   *layer.SharedLayer
		id   int
		heat int64
	}
	var cands []cand
	for _, lr := range res.Layers {
		heat := lr.Shared.Heat()
		for i := range lr.Index.Segments {
			if lr.Index.Segments[i].Kind != format.SegData || i >= len(heat) {
				continue
			}
			cands = append(cands, cand{lr.Shared, i, heat[i]})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].heat > cands[j].heat })
	limit := len(cands) / 4
	for _, c := range cands[:limit] {
		c.sl.FetchSegment(c.id)
	}
	json.NewEncoder(w).Encode(map[string]int{"segments": limit})
}

func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = nil
	s.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
