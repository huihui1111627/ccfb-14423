package format

import (
	"encoding/json"
	"testing"
)

func TestIndexJSONRoundTrip(t *testing.T) {
	ix := &Index{
		Version: IndexVersion, MediaType: IndexMediaType, ChunkSize: 4096,
		SegmentCount: 3,
		Segments: []Segment{
			{ID: 0, Kind: SegHeader, Off: 0, CompLen: 100, Len: 512, SHA256: "aa"},
			{ID: 1, Kind: SegData, Off: 100, CompLen: 50, Len: 4096, SHA256: "bb"},
			{ID: 2, Kind: SegIndex, Off: 150, Len: 20},
		},
		Entries: []*Entry{{Path: "a/b", Type: '0', Mode: 0o644, Xattrs: map[string]string{"user.k": "v"}}},
		Files: []*File{{
			Path: "a/b", Size: 4096, ArchiveOff: 1024, HeaderOff: 512, HeaderSeg: 0,
			HeaderCompOff: 0, HeaderArchiveOff: 512, CompOff: 100, ChunkOffs: []int64{100},
			Segs: []int{1}, ChunkSize: 4096,
		}},
		IndexSeg: 2,
	}
	b, err := json.Marshal(ix)
	if err != nil {
		t.Fatal(err)
	}
	var got Index
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	f := got.File("a/b")
	if f == nil || f.HeaderCompOff != 0 || f.HeaderArchiveOff != 512 || f.ArchiveOff != 1024 {
		t.Fatalf("file offsets lost: %+v", f)
	}
	if got.Segment(1).CompLen != 50 || got.Segment(99) != nil {
		t.Fatal("segment accessors wrong")
	}
}

func TestParseDigest(t *testing.T) {
	h := "sha256:" + HexSHA([]byte("x"))
	if got, err := ParseDigest(h); err != nil || got == "" {
		t.Fatalf("valid digest rejected: %v", err)
	}
	for _, bad := range []string{"", "sha256:xyz", "sha256:" + h[7:60]} {
		if _, err := ParseDigest(bad); err == nil {
			t.Fatalf("accepted invalid digest %q", bad)
		}
	}
}

func TestVerifySegment(t *testing.T) {
	s := &Segment{ID: 1, Kind: SegData, Len: 3, SHA256: HexSHA([]byte("abc"))}
	if err := VerifySegment(s, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := VerifySegment(s, []byte("abd")); err == nil {
		t.Fatal("wrong bytes accepted")
	}
	if err := VerifySegment(s, []byte("ab")); err == nil {
		t.Fatal("wrong length accepted")
	}
	// The index segment carries its digest in the manifest instead.
	idx := &Segment{ID: 9, Kind: SegIndex, Len: 100}
	if err := VerifySegment(idx, []byte("anything")); err != nil {
		t.Fatal(err)
	}
}
