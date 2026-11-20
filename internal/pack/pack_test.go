package pack

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"ccflz/internal/format"
)

func writeTar(t *testing.T, entries []*tar.Header, bodies [][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for i, h := range entries {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if len(bodies[i]) > 0 {
			if _, err := tw.Write(bodies[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func sampleTar(t *testing.T) []byte {
	when := time.Unix(1700000000, 123456789)
	big := bytes.Repeat([]byte("lash-"), 200000)
	return writeTar(t, []*tar.Header{
		{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o750, Uid: 1000, Gid: 1000, Uname: "u", Gname: "g", ModTime: when, Format: tar.FormatPAX, Xattrs: map[string]string{"user.dir": "yes"}},
		{Name: "d/small.txt", Typeflag: tar.TypeReg, Mode: 0o640, Size: 5, ModTime: when, Format: tar.FormatPAX, Xattrs: map[string]string{"user.k": "v"}},
		{Name: "d/sym", Typeflag: tar.TypeSymlink, Linkname: "small.txt", Mode: 0o777, ModTime: when, Format: tar.FormatPAX},
		{Name: "d/hard", Typeflag: tar.TypeLink, Linkname: "d/small.txt", Mode: 0o640, ModTime: when, Format: tar.FormatPAX},
		{Name: "d/fifo", Typeflag: tar.TypeFifo, Mode: 0o620, ModTime: when, Format: tar.FormatPAX},
		{Name: "d/dev", Typeflag: tar.TypeChar, Mode: 0o600, Devmajor: 1, Devminor: 3, ModTime: when, Format: tar.FormatPAX},
		{Name: "d/big.bin", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(big)), ModTime: when, Format: tar.FormatPAX},
	}, [][]byte{nil, []byte("hello"), nil, nil, nil, nil, big})
}

func TestRoundTripAndCompatibility(t *testing.T) {
	archive := sampleTar(t)
	blob, ix, digest, err := PackBytes(archive, Options{ChunkSize: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if digest == "" {
		t.Fatal("empty index digest")
	}
	// The decoded segments concatenate to the original tar.
	var rebuilt bytes.Buffer
	for i := range ix.Segments {
		if i == ix.IndexSeg {
			continue
		}
		s := ix.Segments[i]
		zr, err := gzip.NewReader(bytes.NewReader(blob[s.Off : s.Off+s.CompLen]))
		if err != nil {
			t.Fatalf("segment %d: %v", i, err)
		}
		zr.Multistream(false)
		data, err := readAll(zr)
		if err != nil {
			t.Fatalf("segment %d: %v", i, err)
		}
		if err := format.VerifySegment(&s, data); err != nil {
			t.Fatal(err)
		}
		rebuilt.Write(data)
	}
	if !bytes.Equal(rebuilt.Bytes(), archive) {
		t.Fatal("segments do not reproduce the archive")
	}
	// Chunking: the big file has several segments.
	f := ix.File("d/big.bin")
	if f == nil || len(f.Segs) < 2 {
		t.Fatalf("big file not chunked: %+v", f)
	}
	for i, id := range f.Segs {
		if ix.Segments[id].Off != f.ChunkOffs[i] {
			t.Fatalf("chunk %d compressed offset mismatch", i)
		}
		if got, want := ix.Segments[id].Off, f.CompOff; i == 0 && got != want {
			t.Fatalf("CompOff %d != first chunk %d", got, want)
		}
	}
	// Ordinary tools: tar and gzip.
	dir := t.TempDir()
	p := filepath.Join(dir, "layer.ccflz")
	if err := os.WriteFile(p, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "x")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("tar", "-xf", p, "-C", out).Run(); err != nil {
		// Device nodes require privileges; tar still extracts everything else.
		t.Logf("tar exited with %v (device nodes need root)", err)
	}
	got, err := os.ReadFile(filepath.Join(out, "d/small.txt"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("extracted content %q, %v", got, err)
	}
}

func readAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var b bytes.Buffer
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			if err.Error() == "EOF" {
				return b.Bytes(), nil
			}
			return b.Bytes(), err
		}
	}
}

func TestSegmentIndependence(t *testing.T) {
	archive := sampleTar(t)
	blob, ix, _, err := PackBytes(archive, Options{ChunkSize: 32 << 10})
	if err != nil {
		t.Fatal(err)
	}
	f := ix.File("d/big.bin")
	// Decode the last chunk alone, with no bytes before it.
	last := len(f.Segs) - 1
	s := ix.Segments[f.Segs[last]]
	zr, err := gzip.NewReader(bytes.NewReader(blob[s.Off : s.Off+s.CompLen]))
	if err != nil {
		t.Fatal(err)
	}
	zr.Multistream(false)
	data, err := readAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	start, end := f.ChunkRange(last)
	if !bytes.Equal(data, archive[f.ArchiveOff+start:f.ArchiveOff+end]) {
		t.Fatal("last chunk does not decode independently to the right bytes")
	}
}
