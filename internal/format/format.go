// Package format defines the CCFLZ lazy layer container.
//
// A CCFLZ layer is a concatenation of independent, separately-compressed
// segments:
//
//	[entry header segs][body data segs ...][trailer seg][index seg]
//
// Every segment is a complete gzip member, and every segment can be decoded on
// its own without touching any byte before it. File headers are small tar
// entry headers (PAX extended headers included); regular file bodies are cut
// into fixed-size data segments. Concatenating the decoded segments reproduces
// the original tar archive byte for byte, so ordinary tools (tar, gzip,
// bsdtar, python tarfile, ...) can still decompress and extract the layer as a
// normal .tar.gz. The index is the final gzip member, placed after tar's
// two zero end blocks, which every conforming tar reader ignores.
package format

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const (
	// Magic identifies a CCFLZ layer file, used in annotations and docs.
	Magic = "CCFLZ1"

	// IndexPath is the name recorded for the index entry. The entry lives
	// after the tar end-of-archive marker so ordinary extraction never sees
	// it.
	IndexPath = ".ccflz/index.json"

	// IndexMediaType is the media type of a packed CCFLZ layer.
	IndexMediaType = "application/vnd.ccflz.layer.v1.tar+gzip"

	// IndexAnnotation carries "sha256:<hex>" of the decompressed index bytes.
	// A layer whose index does not match its annotation is rejected.
	IndexAnnotation = "ccflz.index.digest"

	// FormatAnnotation records the container format version.
	FormatAnnotation = "ccflz.version"

	// IndexLengthAnnotation records the compressed size of the tail index
	// member, so a client can fetch the index with one range request.
	IndexLengthAnnotation = "ccflz.index.length"

	// DefaultChunkSize is the fixed body chunk size for large files.
	DefaultChunkSize = 256 * 1024

	// DefaultMaxHole is the largest compressed gap a range merge may pull in
	// to close the gap between two requested segments. On a narrow link the
	// saved round trip is worth far more than 64 KiB of extra bytes.
	DefaultMaxHole = 64 * 1024

	// DefaultMaxCoalesce bounds how many segments one merged request covers.
	DefaultMaxCoalesce = 8

	// SmallFileThreshold: files at or below this size are fetched whole on
	// first access, because a second round trip costs more than the bytes.
	SmallFileThreshold = 16 * 1024

	// IndexVersion is the on-disk index schema version.
	IndexVersion = 1
)

// SegmentKind classifies a segment in the layer stream.
type SegmentKind uint8

const (
	SegHeader  SegmentKind = iota + 1 // complete tar entries, headers only
	SegData                           // a body chunk of a regular file
	SegTrailer                        // tar end-of-archive marker
	SegIndex                          // compressed index JSON (last member)
)

func (k SegmentKind) String() string {
	switch k {
	case SegHeader:
		return "header"
	case SegData:
		return "data"
	case SegTrailer:
		return "trailer"
	case SegIndex:
		return "index"
	}
	return fmt.Sprintf("kind(%d)", k)
}

// Segment is one independently-decodable byte range of the layer file.
type Segment struct {
	ID   int         `json:"id"`
	Kind SegmentKind `json:"kind"`
	// Off is the byte offset of the gzip member in the layer file. The
	// gzip magic 1f 8b 08 is at Off.
	Off int64 `json:"off"`
	// CompLen is the compressed member length. It is 0 for the index
	// segment itself, whose length is implied by the end of the file.
	CompLen int64 `json:"clen"`
	// Len is the decompressed length.
	Len int64 `json:"len"`
	// SHA256 is the hex digest of the decompressed bytes ("" for the index
	// segment, whose integrity is committed by the manifest annotation).
	SHA256 string `json:"sha256"`
	// Heat is advisory access frequency from the packer's training pass;
	// the simulator uses it to place the dashboard's 90%-coverage prefetch.
	// It is part of the index digest, so it is committed like everything else.
	Heat int `json:"heat,omitempty"`
}

// File is one regular file body, split into contiguous data segments.
type File struct {
	// Path is the normalized slash path inside the image (no leading "./").
	Path string `json:"path"`
	// Size is the full decompressed size.
	Size int64 `json:"size"`
	// ArchiveOff is the offset of the file body in the decompressed tar
	// archive; HeaderOff is the offset of the entry's first tar header.
	ArchiveOff int64 `json:"aoff"`
	HeaderOff  int64 `json:"hoff"`
	// HeaderSeg is the segment holding the header bytes.
	HeaderSeg int `json:"hseg"`
	// HeaderCompOff is the compressed offset of the header segment in the
	// layer file.
	HeaderCompOff int64 `json:"hcoff"`
	// HeaderArchiveOff is the offset of the entry's first tar header within
	// the decompressed archive (equal to HeaderOff).
	HeaderArchiveOff int64 `json:"hhoff"`
	// Segs[i] holds bytes [i*ChunkSize, (i+1)*ChunkSize). Empty for files
	// with no body bytes.
	Segs []int `json:"segs,omitempty"`
	// CompOff is the compressed offset of the first body chunk, and
	// ChunkOffs lists the compressed offset of every chunk. Each entry can be
	// inflated on its own starting at that offset.
	CompOff   int64   `json:"coff"`
	ChunkOffs []int64 `json:"coffs,omitempty"`
	// ChunkSize is the size of every chunk except the last.
	ChunkSize int64 `json:"csize"`
	// Hardlink marks a BSD hardlink entry that carries data in the archive
	// stream (tar type '1' with size > 0). Its bytes are part of the stream
	// but the extraction view follows the link instead.
	Hardlink bool `json:"hardlink,omitempty"`
}

// ChunkCount returns the number of chunks.
func (f *File) ChunkCount() int { return len(f.Segs) }

// ChunkOf returns the chunk index containing file offset off, or -1.
func (f *File) ChunkOf(off int64) int {
	if len(f.Segs) == 0 || f.ChunkSize <= 0 || off < 0 || off >= f.Size {
		return -1
	}
	c := off / f.ChunkSize
	if c >= int64(len(f.Segs)) {
		c = int64(len(f.Segs)) - 1
	}
	return int(c)
}

// ChunkRange returns the file byte range [start,end) covered by chunk i.
func (f *File) ChunkRange(i int) (start, end int64) {
	start = int64(i) * f.ChunkSize
	end = start + f.ChunkSize
	if end > f.Size {
		end = f.Size
	}
	return
}

// Entry describes one metadata-bearing archive member, in archive order.
type Entry struct {
	Path      string            `json:"path"`
	Type      byte              `json:"type"` // tar type flag ('0', '5', ...)
	Mode      int64             `json:"mode"`
	Uid       int               `json:"uid"`
	Gid       int               `json:"gid"`
	Uname     string            `json:"uname,omitempty"`
	Gname     string            `json:"gname,omitempty"`
	MtimeSec  int64             `json:"mtime"`
	MtimeNsec int32             `json:"mtimens"`
	Linkname  string            `json:"link,omitempty"`
	Devmajor  int64             `json:"devmaj,omitempty"`
	Devminor  int64             `json:"devmin,omitempty"`
	Xattrs    map[string]string `json:"xattrs,omitempty"`
	Size      int64             `json:"size"`
	// HeaderOff is the archive offset of the header bytes.
	HeaderOff int64 `json:"hoff"`
	// HeaderSeg is the segment holding the header.
	HeaderSeg int `json:"hseg"`
	// HeaderCompOff is the compressed offset of the header segment in the
	// layer file.
	HeaderCompOff int64 `json:"hcoff"`
	// HeaderArchiveOff is the offset of the header in the decompressed
	// archive (equal to HeaderOff; kept for symmetry with the compressed
	// offset).
	HeaderArchiveOff int64 `json:"hhoff"`
}

// Stats summarizes a layer for reporting.
type Stats struct {
	ArchiveSize  int64 `json:"archive_size"`   // decompressed tar size
	LayerSize    int64 `json:"layer_size"`     // packed file size on disk
	IndexSize    int64 `json:"index_size"`     // compressed index size
	IndexRawSize int64 `json:"index_raw_size"` // uncompressed index size
	FileCount    int   `json:"file_count"`
	SegmentCount int   `json:"segment_count"`
	DataSegments int   `json:"data_segments"`
	ChunkSize    int64 `json:"chunk_size"`
}

// Index is the addressable catalog at the tail of a layer.
type Index struct {
	Version      int       `json:"version"`
	MediaType    string    `json:"media_type"`
	ChunkSize    int64     `json:"chunk_size"`
	ArchiveSize  int64     `json:"archive_size"`
	SegmentCount int       `json:"segment_count"`
	IndexSeg     int       `json:"index_seg"`
	Segments     []Segment `json:"segments"`
	// Entries lists every archive member in archive order, including
	// directories, links, devices and whiteouts.
	Entries []*Entry `json:"entries"`
	// Files lists bodies of regular files (and BSD hardlink bodies).
	Files []*File `json:"files"`
	Stats Stats   `json:"stats"`
}

// Segment returns the segment with the given id (nil when out of range).
func (ix *Index) Segment(id int) *Segment {
	if id < 0 || id >= len(ix.Segments) {
		return nil
	}
	return &ix.Segments[id]
}

// File returns the file body record with the given path (nil when absent).
func (ix *Index) File(path string) *File {
	for _, f := range ix.Files {
		if f.Path == path {
			return f
		}
	}
	return nil
}

// HexSHA returns the hex sha256 of b.
func HexSHA(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// DigestOf returns "sha256:<hex>" of b.
func DigestOf(b []byte) string { return "sha256:" + HexSHA(b) }

// ParseDigest validates "sha256:<hex>" or bare hex and returns the hex part.
func ParseDigest(d string) (string, error) {
	const p = "sha256:"
	h := d
	if len(h) > len(p) && h[:len(p)] == p {
		h = h[len(p):]
	}
	if len(h) != 64 {
		return "", fmt.Errorf("invalid digest %q", d)
	}
	for _, c := range h {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", fmt.Errorf("invalid digest %q", d)
		}
	}
	return h, nil
}

// VerifySegment checks decompressed segment bytes against the recorded length
// and digest. The index segment is exempt (its digest lives in the manifest).
func VerifySegment(s *Segment, data []byte) error {
	if s.Kind == SegIndex {
		return nil
	}
	if int64(len(data)) != s.Len {
		return fmt.Errorf("segment %d: length mismatch: got %d want %d", s.ID, len(data), s.Len)
	}
	got := HexSHA(data)
	if got != s.SHA256 {
		return fmt.Errorf("segment %d: digest mismatch: got %s want %s", s.ID, got, s.SHA256)
	}
	return nil
}
