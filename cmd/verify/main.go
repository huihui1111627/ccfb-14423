// Command verify runs the end-to-end verification suite: it generates images,
// packs lazy layers, runs containers over a simulated range-request remote,
// and compares every read, listing, write and piece of metadata against a
// plain extraction of the same layers.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"ccflz/internal/verify"
)

func main() {
	var (
		out       = flag.String("out", "verify-out", "artifact directory")
		images    = flag.Int("images", 3, "number of generated images")
		layers    = flag.Int("layers", 3, "layers per image")
		files     = flag.Int("files", 28, "files per layer")
		reads     = flag.Int("reads", 300, "random reads/lists/writes per image")
		seed      = flag.Int64("seed", 1, "base seed")
		latency   = flag.Duration("latency", 40*time.Millisecond, "simulated per-request latency")
		bandwidth = flag.Int64("bandwidth", 64*1024, "simulated link bytes/sec (0 = unlimited)")
		keep      = flag.Bool("keep", false, "keep temporary trees")
		verbose   = flag.Bool("v", true, "print per-image progress")
	)
	flag.Parse()
	rep, err := verify.Run(verify.Config{
		OutDir: *out, Images: *images, LayersPerImage: *layers, Seed: *seed, FilesPerLayer: *files,
		ReadsPerImage: *reads, LatencyPerRequest: *latency, BandwidthBytesPerSec: *bandwidth,
		KeepTemp: *keep, Verbose: *verbose,
		Log: func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		os.Exit(1)
	}
	if !rep.Passed {
		fmt.Println("FAILED")
		for _, f := range rep.Failures {
			fmt.Println(" -", f)
		}
		os.Exit(1)
	}
	fmt.Printf("PASSED: %d images, %d layers, %d files, %d reads, %d listings, %d writes, %.1f MiB compared\n",
		rep.Totals.Images, rep.Totals.Layers, rep.Totals.Files, rep.Totals.Reads, rep.Totals.Lists,
		rep.Totals.Writes, float64(rep.Totals.BytesCompared)/(1<<20))
	fmt.Println("report:", *out+"/verify-report.json")
}
