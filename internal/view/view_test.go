package view

import (
	"bytes"
	"testing"

	"ccflz/internal/format"
	"ccflz/internal/image"
	"ccflz/internal/layer"
)

// fakeLayer builds a LayerRef whose index carries the given entries and
// bodies, without any network or repo.
type fakeSource struct{ data map[string][]byte }

func (f *fakeSource) fetch(rel string) []byte { return f.data[rel] }

func buildRes(entries []*format.Entry, files []*format.File) *image.Resolved {
	return &image.Resolved{Layers: []*image.LayerRef{{
		Index: &format.Index{Segments: []format.Segment{{ID: 0, Kind: format.SegIndex}},
			Entries: entries, Files: files},
	}}}
}

func TestWhiteoutsAndCopyUp(t *testing.T) {
	entries := []*format.Entry{
		{Path: "a", Type: '5', Mode: 0o755},
		{Path: "a/keep", Type: '0', Mode: 0o644, Size: 3},
		{Path: "a/gone", Type: '0', Mode: 0o644, Size: 3},
		{Path: "a/.wh.gone", Type: '0', Mode: 0},
		{Path: "b", Type: '5', Mode: 0o755},
		{Path: "b/opq-old", Type: '0', Mode: 0o644, Size: 1},
		{Path: "b/.wh..wh..opq", Type: '0', Mode: 0},
		{Path: "b/new", Type: '0', Mode: 0o644, Size: 1},
	}
	files := []*format.File{
		{Path: "a/keep", Size: 3, ChunkSize: 16, Segs: []int{1}},
		{Path: "a/gone", Size: 3, ChunkSize: 16, Segs: []int{2}},
		{Path: "b/opq-old", Size: 1, ChunkSize: 16, Segs: []int{3}},
		{Path: "b/new", Size: 1, ChunkSize: 16, Segs: []int{4}},
	}
	res := buildRes(entries, files)
	s, err := New(res, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s.Lookup("/a/gone") != nil {
		t.Fatal("whiteout did not remove a/gone")
	}
	if s.Lookup("/b/opq-old") != nil {
		t.Fatal("opaque marker did not remove existing children")
	}
	if s.Lookup("/b/new") == nil {
		t.Fatal("entry after opaque marker was removed")
	}
	if _, err := s.List("/a"); err != nil {
		t.Fatal(err)
	}
	// Listings must be non-nil even for empty dirs.
	if got, _ := s.List("/"); len(got) != 2 {
		t.Fatalf("root listing = %v", got)
	}
	_ = layer.DefaultOptions
	_ = bytes.MinRead
}

func TestWritingUnknownFileFails(t *testing.T) {
	res := buildRes(nil, nil)
	s, _ := New(res, t.TempDir())
	if err := s.Write("/nope", 0, []byte("x")); err == nil {
		t.Fatal("write to a missing file succeeded")
	}
}
