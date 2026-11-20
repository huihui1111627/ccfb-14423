package verify

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEndToEnd is the full verification suite at a size that runs in CI. The
// cmd/verify entry point runs the larger configuration.
func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	rep, err := Run(Config{
		OutDir:         dir,
		Images:         2,
		LayersPerImage: 2,
		FilesPerLayer:  40,
		ReadsPerImage:  80,
		Seed:           11,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Passed {
		for _, f := range rep.Failures {
			t.Error(f)
		}
		t.FailNow()
	}
	if _, err := os.Stat(filepath.Join(dir, "verify-report.json")); err != nil {
		t.Fatal("report not written:", err)
	}
	// The suite must actually exercise the interesting paths.
	var reads, lists int
	for _, im := range rep.Images {
		reads += im.Compare.ReadChecks
		lists += im.Compare.ListChecks
		if im.Metadata.XattrPaths == 0 {
			t.Error("no xattrs exercised")
		}
		if im.Metadata.HardlinkGroups == 0 {
			t.Error("no hardlinks exercised")
		}
		if im.Corruption == "" {
			t.Error("corruption check did not run")
		}
		if !im.Share.FreedOnLast {
			t.Error("layer memory not freed")
		}
	}
	if reads < 50 || lists < 20 {
		t.Fatalf("workload too small: %d reads, %d lists", reads, lists)
	}
}
