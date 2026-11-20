package verify

import (
	"os"
	"path/filepath"
	"testing"

	"ccflz/internal/repo"
)

// Every compatibility and integrity flag in an image report must be on.
func TestReportFlags(t *testing.T) {
	dir := t.TempDir()
	rep, err := Run(Config{OutDir: dir, Images: 1, LayersPerImage: 2, FilesPerLayer: 20, ReadsPerImage: 40, Seed: 99})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Passed {
		t.Fatalf("failures: %v", rep.Failures)
	}
	im := rep.Images[0]
	for _, l := range im.Layers {
		if !l.RoundTripExact || !l.TarExtractOK || !l.GzipOK || l.SegmentChecks == 0 {
			t.Fatalf("layer checks incomplete: %+v", l)
		}
	}
	if im.Compare.ReadChecks == 0 || im.Compare.ListChecks == 0 || im.Compare.WriteChecks == 0 || im.Compare.TreeChecks == 0 {
		t.Fatalf("workload incomplete: %+v", im.Compare)
	}
	if im.Metadata.Paths == 0 || im.Metadata.PrivilegeSkips == 0 {
		t.Fatalf("metadata checks incomplete: %+v", im.Metadata)
	}
	if im.Startup == nil || im.Startup.LazyBytes == 0 || im.Startup.FullBytes == 0 {
		t.Fatalf("startup study incomplete: %+v", im.Startup)
	}
	if im.Startup.LazyFraction >= im.Startup.FullFraction {
		t.Fatalf("lazy startup did not save bytes: %.3f vs %.3f", im.Startup.LazyFraction, im.Startup.FullFraction)
	}
	if len(im.Containers) != 3 || !im.Containers[0].Cold {
		t.Fatalf("startup reports: %+v", im.Containers)
	}
}

// A blob whose index does not match its annotation must be rejected before a
// container can start.
func TestTamperedIndexRejectedAtStartup(t *testing.T) {
	dir := t.TempDir()
	r, err := repo.New(filepath.Join(dir, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := genImage(t, r, "tamper")
	if err != nil {
		t.Fatal(err)
	}
	_ = rec
	msg, err := corruptIndex(t, r, dir, "tamper")
	if err != nil {
		t.Fatal(err)
	}
	if msg == "" {
		t.Fatal("tampered layer accepted")
	}
	_ = os.RemoveAll(dir)
}
