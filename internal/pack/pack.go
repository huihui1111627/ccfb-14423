// Package pack builds CCFLZ lazy layers from plain tar streams.
//
// Format invariants that the rest of the system relies on:
//   - The decoded segments concatenate to the original tar archive exactly.
//   - Each segment is a self-contained gzip member; decompression never needs
//     bytes before the segment's own offset.
//   - The index is the last gzip member. Its own record has CompLen 0 and is
//     bounded by the end of the file.
//   - Index files record both the offset in the decompressed archive
//     (ArchiveOff/HeaderOff) and, per chunk, the compressed segment id, so any
//     chunk can be fetched and decoded alone.
package pack

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"ccflz/internal/format"
)

// Options controls packing.
type Options struct {
	ChunkSize int64
	// Level is the gzip compression level (default gzip.DefaultCompression).
	Level int
	// Patch, when non-nil, is applied to the finished index just before it is
	// serialized. Tools use it to add advisory data such as access heat.
	Patch func(*format.Index)
}

func (o *Options) chunkSize() int64 {
	if o.ChunkSize > 0 {
		return o.ChunkSize
	}
	return format.DefaultChunkSize
}

func (o *Options) level() int {
	if o.Level == 0 {
		return gzip.DefaultCompression
	}
	return o.Level
}

type writer struct {
	w     *bufio.Writer
	off   int64
	segs  []format.Segment
	level int
}

// writeSegment compresses data as one gzip member, writes it, and records it.
// The index segment is recorded with CompLen 0; its length is bounded by the
// end of the file.
func (b *writer) writeSegment(kind format.SegmentKind, data []byte) (int, error) {
	idx := len(b.segs)
	var cbuf byteBuf
	zw, err := gzip.NewWriterLevel(&cbuf, b.level)
	if err != nil {
		return -1, err
	}
	if _, err := zw.Write(data); err != nil {
		return -1, err
	}
	if err := zw.Close(); err != nil {
		return -1, err
	}
	if _, err := b.w.Write(cbuf.b); err != nil {
		return -1, err
	}
	seg := format.Segment{ID: idx, Kind: kind, Off: b.off, CompLen: int64(len(cbuf.b)), Len: int64(len(data))}
	if kind != format.SegIndex {
		sum := sha256.Sum256(data)
		seg.SHA256 = hex.EncodeToString(sum[:])
	}
	b.segs = append(b.segs, seg)
	b.off += int64(len(cbuf.b))
	return idx, nil
}

type byteBuf struct{ b []byte }

func (b *byteBuf) Write(p []byte) (int, error) {
	b.b = append(b.b, p...)
	return len(p), nil
}

// countingReader tracks how many bytes a tar.Reader has consumed.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Pad returns the tar padding required after a body of size n.
func Pad(n int64) int64 { return (512 - n%512) % 512 }

// Pack reads a plain tar archive of archiveSize bytes from r and writes the
// CCFLZ layer to w. It returns the index and the digest of the serialized
// (uncompressed) index bytes, which belongs in the manifest annotation.
func Pack(r io.ReaderAt, archiveSize int64, w io.Writer, opts Options) (*format.Index, string, error) {
	if archiveSize <= 0 {
		return nil, "", errors.New("pack: empty archive")
	}
	bw := &writer{w: bufio.NewWriterSize(w, 1<<20), level: opts.level()}
	ix := &format.Index{
		Version:   format.IndexVersion,
		MediaType: format.IndexMediaType,
		ChunkSize: opts.chunkSize(),
	}

	cr := &countingReader{r: io.NewSectionReader(r, 0, archiveSize)}
	tr := tar.NewReader(cr)

	var pos int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("pack: tar read at %d: %w", cr.n, err)
		}
		bodyStart := cr.n
		if bodyStart < pos {
			return nil, "", fmt.Errorf("pack: tar layout went backwards at %q", hdr.Name)
		}
		if hdr.Size < 0 {
			return nil, "", fmt.Errorf("pack: entry %q has negative size", hdr.Name)
		}
		if hdr.Typeflag == tar.TypeGNUSparse {
			return nil, "", fmt.Errorf("pack: sparse entry %q is not supported", hdr.Name)
		}
		headerLen := bodyStart - pos
		if headerLen <= 0 {
			return nil, "", fmt.Errorf("pack: empty header for %q", hdr.Name)
		}
		hbuf := make([]byte, headerLen)
		if _, err := r.ReadAt(hbuf, pos); err != nil {
			return nil, "", fmt.Errorf("pack: read header %q: %w", hdr.Name, err)
		}
		hseg, err := bw.writeSegment(format.SegHeader, hbuf)
		if err != nil {
			return nil, "", err
		}

		name := NormalizeName(hdr.Name)
		ent := &format.Entry{
			Path:          name,
			Type:          hdr.Typeflag,
			Mode:          hdr.Mode,
			Uid:           hdr.Uid,
			Gid:           hdr.Gid,
			Uname:         hdr.Uname,
			Gname:         hdr.Gname,
			MtimeSec:      hdr.ModTime.Unix(),
			MtimeNsec:     int32(hdr.ModTime.Nanosecond()),
			Linkname:      NormalizeLink(hdr.Linkname, hdr.Typeflag),
			Devmajor:      hdr.Devmajor,
			Devminor:      hdr.Devminor,
			Size:          hdr.Size,
			HeaderOff:     pos,
			HeaderSeg:     hseg,
			HeaderCompOff: bw.segs[hseg].Off,
		}
		if len(hdr.Xattrs) > 0 {
			ent.Xattrs = make(map[string]string, len(hdr.Xattrs))
			for k, v := range hdr.Xattrs {
				ent.Xattrs[k] = v
			}
		}
		ix.Entries = append(ix.Entries, ent)

		bodyEnd := bodyStart + hdr.Size
		isBody := hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA
		if hdr.Typeflag == tar.TypeLink && hdr.Size > 0 {
			// BSD hardlink carrying data: keep the bytes in the stream so a
			// plain extractor sees a valid archive; the view follows the link.
			isBody = true
		}
		if isBody {
			f := &format.File{
				Path:             name,
				Size:             hdr.Size,
				ArchiveOff:       bodyStart,
				HeaderOff:        pos,
				HeaderSeg:        hseg,
				HeaderCompOff:    bw.segs[hseg].Off,
				HeaderArchiveOff: pos,
				ChunkSize:        ix.ChunkSize,
				Hardlink:         hdr.Typeflag == tar.TypeLink,
			}
			f.CompOff = bw.off
			for off := int64(0); off < hdr.Size; off += ix.ChunkSize {
				n := ix.ChunkSize
				if off+n > hdr.Size {
					n = hdr.Size - off
				}
				buf := make([]byte, n)
				if _, err := r.ReadAt(buf, bodyStart+off); err != nil {
					return nil, "", fmt.Errorf("pack: read body %q at %d: %w", hdr.Name, off, err)
				}
				sid, err := bw.writeSegment(format.SegData, buf)
				if err != nil {
					return nil, "", err
				}
				f.Segs = append(f.Segs, sid)
				f.ChunkOffs = append(f.ChunkOffs, bw.segs[sid].Off)
			}
			ix.Files = append(ix.Files, f)
		}

		// Trailing body padding stays in the stream: it is picked up as the
		// first bytes of the next header segment (or of the trailer for the
		// last entry), so the decoded segments reproduce the archive exactly.
		pos = bodyEnd
	}

	// Trailer: the tar end-of-archive marker. A well-formed tar has two zero
	// blocks here; the reader stops just before them, so reconstruct exactly
	// the remaining bytes of the source archive. Plain extractors ignore
	// anything after the zero blocks, so the index can follow safely.
	if pos > archiveSize {
		return nil, "", fmt.Errorf("pack: archive truncated (pos %d > size %d)", pos, archiveSize)
	}
	trailer := make([]byte, archiveSize-pos)
	if _, err := r.ReadAt(trailer, pos); err != nil && err != io.EOF {
		return nil, "", fmt.Errorf("pack: read trailer: %w", err)
	}
	if len(trailer) < 1024 {
		trailer = append(trailer, make([]byte, 1024-len(trailer))...)
	}
	if _, err := bw.writeSegment(format.SegTrailer, trailer); err != nil {
		return nil, "", err
	}
	ix.ArchiveSize = pos + int64(len(trailer))

	// Reserve the index record so the index describes itself. Its offset is
	// known now; its compressed extent runs to the end of the file, so it is
	// recorded with CompLen 0. The reader fills in the real length.
	ix.IndexSeg = len(bw.segs)
	ix.Segments = append(bw.segs, format.Segment{ID: ix.IndexSeg, Kind: format.SegIndex, Off: bw.off})
	ix.SegmentCount = len(ix.Segments)
	ix.Stats = format.Stats{
		ArchiveSize:  ix.ArchiveSize,
		FileCount:    len(ix.Files),
		SegmentCount: len(ix.Segments),
		ChunkSize:    ix.ChunkSize,
	}
	for _, f := range ix.Files {
		ix.Stats.DataSegments += len(f.Segs)
	}
	if opts.Patch != nil {
		opts.Patch(ix)
	}

	// The index must be stable: its digest is recorded in the manifest, so any
	// caller-supplied patch must already be applied above.
	raw, err := json.Marshal(ix)
	if err != nil {
		return nil, "", fmt.Errorf("pack: marshal index: %w", err)
	}
	var cbuf byteBuf
	zw, err := gzip.NewWriterLevel(&cbuf, opts.level())
	if err != nil {
		return nil, "", err
	}
	if _, err := zw.Write(raw); err != nil {
		return nil, "", err
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	if _, err := bw.w.Write(cbuf.b); err != nil {
		return nil, "", err
	}
	if err := bw.w.Flush(); err != nil {
		return nil, "", err
	}
	ix.Stats.LayerSize = bw.off
	ix.Stats.IndexSize = int64(len(cbuf.b))
	ix.Stats.IndexRawSize = int64(len(raw))
	return ix, format.DigestOf(raw), nil
}

// PackBytes is a convenience wrapper around Pack.
func PackBytes(archive []byte, opts Options) ([]byte, *format.Index, string, error) {
	var out byteBuf
	ix, dg, err := Pack(BytesReaderAt(archive), int64(len(archive)), &out, opts)
	if err != nil {
		return nil, nil, "", err
	}
	return out.b, ix, dg, nil
}

// BytesReaderAt adapts a byte slice to io.ReaderAt.
type BytesReaderAt []byte

// ReadAt implements io.ReaderAt.
func (b BytesReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// NormalizeName canonicalizes an archive member path.
func NormalizeName(n string) string {
	n = strings.TrimPrefix(n, "./")
	n = strings.TrimPrefix(n, "/")
	n = strings.TrimSuffix(n, "/")
	if n == "" || n == "." {
		return "."
	}
	return path.Clean(n)
}

// NormalizeLink canonicalizes a link target.
func NormalizeLink(l string, typ byte) string {
	if typ == tar.TypeSymlink || l == "" {
		return l
	}
	return NormalizeName(l)
}
