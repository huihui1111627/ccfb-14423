package fsmeta

import (
	"os"
	"path/filepath"
	"testing"

	"ccflz/internal/format"
)

func TestApplyMetadata(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &format.Entry{
		Path: "f", Mode: 0o640, MtimeSec: 1700000000, MtimeNsec: 987654321,
		Xattrs: map[string]string{"user.color": "blue"},
	}
	if err := Apply(p, e); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o", fi.Mode().Perm())
	}
	if fi.ModTime().Unix() != 1700000000 {
		t.Fatalf("mtime = %v", fi.ModTime())
	}
}

func TestSymlinkAndFifo(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	os.WriteFile(target, []byte("t"), 0o644)
	link := filepath.Join(dir, "link")
	e := &format.Entry{Path: "link", Mode: 0o777, MtimeSec: 1700000000}
	if err := MakeSymlink(link, "target", e); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(link); got != "target" {
		t.Fatalf("link = %q", got)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := MakeFifo(fifo, &format.Entry{Mode: 0o620, MtimeSec: 1700000000}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(fifo)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("not a fifo: %v", fi.Mode())
	}
	// Devices are rejected without privileges; that must surface as a
	// permission error so callers can record a skip.
	dev := filepath.Join(dir, "dev")
	err = MakeDevice(dev, &format.Entry{Type: '3', Mode: 0o600, Devmajor: 1, Devminor: 3})
	if err != nil && !IsPermission(err) {
		t.Fatalf("unexpected device error: %v", err)
	}
}
