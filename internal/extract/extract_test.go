package extract

import (
	"archive/tar"
	"bytes"
	"testing"
	"time"

	"ccflz/internal/materialize"
)

func mkTar(t *testing.T) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	when := time.Unix(1700000000, 111)
	add := func(h *tar.Header, data string) {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if data != "" {
			tw.Write([]byte(data))
		}
	}
	add(&tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o751, ModTime: when, Format: tar.FormatPAX}, "")
	add(&tar.Header{Name: "d/f.txt", Typeflag: tar.TypeReg, Mode: 0o640, Size: 3, ModTime: when, Format: tar.FormatPAX,
		Xattrs: map[string]string{"user.a": "b"}}, "abc")
	add(&tar.Header{Name: "d/link", Typeflag: tar.TypeSymlink, Linkname: "f.txt", Mode: 0o777, ModTime: when, Format: tar.FormatPAX}, "")
	add(&tar.Header{Name: "d/hard", Typeflag: tar.TypeLink, Linkname: "d/f.txt", ModTime: when, Format: tar.FormatPAX}, "")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestLayerTarMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := LayerTar(dir, mkTar(t), Options{}); err != nil {
		t.Fatal(err)
	}
	res, err := materialize.ScanNew(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := res.ByPath["d/f.txt"]
	if f == nil || f.Type != "file" || f.MtimeSec != 1700000000 || f.MtimeNsec != 111 {
		t.Fatalf("file metadata: %+v", f)
	}
	if f.Xattrs["user.a"] != "b" {
		t.Fatalf("xattr missing: %+v", f.Xattrs)
	}
	d := res.ByPath["d"]
	if d.Mode&0o777 != 0o751 {
		t.Fatalf("dir mode %o", d.Mode)
	}
	if len(res.HardlinkGroups) != 1 {
		t.Fatalf("hardlink not preserved: %v", res.HardlinkGroups)
	}
}
