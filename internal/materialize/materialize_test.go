package materialize

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMetaScanAndDiff(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string, mode os.FileMode) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("a/b.txt", "hello", 0o640)
	os.Symlink("b.txt", filepath.Join(dir, "a/link"))
	if err := os.Link(filepath.Join(dir, "a/b.txt"), filepath.Join(dir, "a/hard")); err != nil {
		t.Fatal(err)
	}
	res, err := ScanNew(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.ByPath["a/b.txt"] == nil {
		t.Fatal("file missing from scan")
	}
	if res.ByPath["a/link"].Type != "symlink" || res.ByPath["a/link"].Link != "b.txt" {
		t.Fatalf("symlink metadata wrong: %+v", res.ByPath["a/link"])
	}
	if len(res.HardlinkGroups) != 1 {
		t.Fatalf("hardlink groups = %d, want 1", len(res.HardlinkGroups))
	}
	if diffs := Diff(res.ByPath, res.ByPath); len(diffs) != 0 {
		t.Fatalf("self diff = %v", diffs)
	}
	// Mutate a file and check the diff catches it.
	os.WriteFile(filepath.Join(dir, "a/b.txt"), []byte("HELLO"), 0o640)
	res2, err := ScanNew(dir)
	if err != nil {
		t.Fatal(err)
	}
	if diffs := Diff(res.ByPath, res2.ByPath); len(diffs) == 0 {
		t.Fatal("content change not detected")
	}
}
