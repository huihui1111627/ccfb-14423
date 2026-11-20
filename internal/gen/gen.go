// Package gen creates test images, plain tar layers and file trees with a
// wide spread of metadata: modes, ownership, timestamps, xattrs, symlinks,
// hardlinks, FIFOs, devices, whiteouts and large chunked files.
package gen

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"path"
	"sort"
	"strings"
	"time"

	"ccflz/internal/format"
	"ccflz/internal/pack"
	"ccflz/internal/repo"
)

// Spec describes one image to generate.
type Spec struct {
	Name       string
	LayerCount int
	Seed       int64
	// FilesPerLayer overrides the default file count per layer (default 28).
	FilesPerLayer int
}

// Layer is a generated plain tar layer.
type Layer struct {
	Tar     []byte
	TarGz   []byte
	Entries []*Entry
	// Packed is the CCFLZ blob produced from Tar.
	Packed []byte
}

// Entry is a tar member description used both to write tar and to check.
type Entry struct {
	Header *tar.Header
	Body   []byte
}

// ImageRecord is the generator's description of an image for checks.
type ImageRecord struct {
	Name     string   `json:"name"`
	Layers   []*Layer `json:"-"`
	Manifest string   `json:"manifest"`
	Config   string   `json:"config"`
	// LayerTars are the raw tar digests in application order.
	LayerTars []string `json:"layer_tars"`
}

var baseTime = time.Unix(1672531200, 0) // 2023-01-01T00:00:00Z

// tarEntry produces an entry in the deterministic style used by the tests.
type tarEntry struct {
	Path     string
	Type     byte
	Mode     int64
	Uid      int
	Gid      int
	Uname    string
	Gname    string
	Mtime    time.Time
	Link     string
	Devmajor int64
	Devminor int64
	Xattrs   map[string]string
	Data     []byte
}

// GenerateImage builds an image with n layers and returns the layers and the
// manifest digest.
func GenerateImage(r *repo.Repo, spec Spec) (*ImageRecord, error) {
	if spec.LayerCount <= 0 {
		spec.LayerCount = 3
	}
	rng := rand.New(rand.NewSource(spec.Seed))
	rec := &ImageRecord{Name: spec.Name}
	var layerDescs []repo.Descriptor
	var diffIDs []string
	var totalSize int64
	for li := 0; li < spec.LayerCount; li++ {
		files := spec.FilesPerLayer
		if files <= 0 {
			files = 28
		}
		layer := generateLayer(rng, spec.Name, li, files)
		// Compute tar digest for the manifest config diff_ids, like OCI does.
		th := sha256.Sum256(layer.Tar)
		diffIDs = append(diffIDs, "sha256:"+hex.EncodeToString(th[:]))
		totalSize += int64(len(layer.Tar))
		packed, ix, idxDigest, err := packLayer(layer, spec.Seed, li)
		if err != nil {
			return nil, err
		}
		layer.Packed = packed
		blob, err := r.PutBlob(packed)
		if err != nil {
			return nil, err
		}
		layerDescs = append(layerDescs, repo.Descriptor{
			MediaType: format.IndexMediaType, Digest: blob, Size: int64(len(packed)),
			Annotations: map[string]string{
				format.IndexAnnotation:  idxDigest,
				format.FormatAnnotation: format.Magic,
				"ccflz.segments":        fmt.Sprintf("%d", len(ix.Segments)),
				// The index lives at the tail; advertise its extent so clients
				// fetch it in one request instead of probing.
				format.IndexLengthAnnotation: fmt.Sprintf("%d", ix.Stats.IndexSize),
			},
		})
		rec.Layers = append(rec.Layers, layer)
	}
	cfg := repo.ImageConfig{
		Architecture: "arm64", OS: "linux",
		Config: repo.RunConfig{
			Env: []string{"PATH=/usr/bin:/bin"}, Cmd: []string{"/app/bin/tool"}, WorkingDir: "/app",
		},
		RootFS: repo.RootFS{Type: "layers", DiffIDs: diffIDs},
	}
	cfgRaw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	cfgDigest, err := r.PutBlob(cfgRaw)
	if err != nil {
		return nil, err
	}
	mf := repo.Manifest{
		SchemaVersion: 2, MediaType: "application/vnd.oci.image.manifest.v1+json",
		Config: repo.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: cfgDigest, Size: int64(len(cfgRaw))},
		Layers: layerDescs,
	}
	mfRaw, err := json.Marshal(mf)
	if err != nil {
		return nil, err
	}
	md, err := r.PutManifest(mfRaw)
	if err != nil {
		return nil, err
	}
	if err := r.SetTag(spec.Name, md); err != nil {
		return nil, err
	}
	rec.Manifest = md
	rec.Config = cfgDigest
	return rec, nil
}

func packLayer(layer *Layer, seed int64, li int) ([]byte, *format.Index, string, error) {
	blob, ix, dg, err := pack.PackBytes(layer.Tar, pack.Options{
		ChunkSize: format.DefaultChunkSize,
		Patch: func(ix *format.Index) {
			// Advisory access heat, deterministic per layer. It is covered by
			// the index digest and used only by the dashboard.
			rng := rand.New(rand.NewSource(seed*100 + int64(li)))
			for i := range ix.Segments {
				if ix.Segments[i].Kind != format.SegData {
					continue
				}
				ix.Segments[i].Heat = rng.Intn(100)
			}
		},
	})
	if err != nil {
		return nil, nil, "", err
	}
	return blob, ix, dg, nil
}

// generateLayer builds one layer with deterministic content.
func generateLayer(rng *rand.Rand, image string, li, filesPerLayer int) *Layer {
	var ents []*tarEntry
	layer := &Layer{}
	dirs := []string{"etc", "app", "app/bin", "app/lib", "var", "var/log", "var/cache", "usr", "usr/share", "usr/share/doc", "opt", "opt/data"}
	for i, d := range dirs {
		if (i+li)%3 == 0 || li == 0 {
			ents = append(ents, &tarEntry{
				Path: d + "/", Type: tar.TypeDir, Mode: 0o755, Uid: 1000, Gid: 1000,
				Uname: "app", Gname: "app", Mtime: baseTime.Add(time.Duration(li*3600+i) * time.Second),
			})
		}
	}
	// Shared directory with per-layer metadata differences (mode, owner, xattr).
	ents = append(ents, &tarEntry{
		Path: "shared/", Type: tar.TypeDir, Mode: int64(0o755 - li), Uid: 1000 + li, Gid: 1000,
		Uname: "app", Gname: "app", Mtime: baseTime.Add(time.Duration(li) * time.Hour),
		Xattrs: map[string]string{"user.layer": fmt.Sprintf("L%d", li)},
	})
	fileCount := filesPerLayer - 10
	if fileCount < 4 {
		fileCount = 4
	}
	for i := 0; i < fileCount; i++ {
		name := path.Join("shared", fmt.Sprintf("f%03d.dat", i))
		size := rng.Intn(3) * 4000
		if i%11 == 0 {
			size = 40_000 + rng.Intn(120_000) // exercises chunking
		}
		data := make([]byte, size)
		rng.Read(data)
		e := &tarEntry{
			Path: name, Type: tar.TypeReg, Mode: 0o640, Uid: 1000, Gid: 1000,
			Uname: "app", Gname: "app", Mtime: baseTime.Add(time.Duration(li*1000+i) * time.Second),
			Data: data,
		}
		if i%4 == 0 {
			e.Xattrs = map[string]string{"user.note": fmt.Sprintf("layer%d-file%d", li, i)}
		}
		if (i+li)%7 == 0 {
			e.Uid = 0
			e.Uname = "root"
			e.Mode = 0o4755
		}
		ents = append(ents, e)
	}
	// Big file: several fixed-size chunks, deterministic per layer.
	if li == 0 {
		big := make([]byte, 3*256*1024+1234)
		rng.Read(big)
		ents = append(ents, &tarEntry{
			Path: "app/bin/big.bin", Type: tar.TypeReg, Mode: 0o755, Uid: 0, Gid: 0,
			Uname: "root", Gname: "root", Mtime: baseTime.Add(90 * time.Minute), Data: big,
		})
	}
	// Symlink, hardlink, FIFO, char/block devices.
	ents = append(ents, &tarEntry{
		Path: "app/bin/tool", Type: tar.TypeReg, Mode: 0o755, Uid: 0, Gid: 0,
		Uname: "root", Gname: "root", Mtime: baseTime.Add(86400 * 3),
		Data:   []byte("#!/bin/sh\necho tool\n"),
		Xattrs: map[string]string{"user.cap": "net_bind_service"},
	})
	ents = append(ents, &tarEntry{
		Path: "app/bin/tool-link", Type: tar.TypeSymlink, Mode: 0o777, Uid: 0, Gid: 0,
		Uname: "root", Gname: "root", Link: "tool", Mtime: baseTime.Add(86400 * 3),
	})
	ents = append(ents, &tarEntry{
		Path: "app/bin/tool-hard", Type: tar.TypeLink, Mode: 0o755, Uid: 0, Gid: 0,
		Uname: "root", Gname: "root", Link: "app/bin/tool", Mtime: baseTime.Add(86400 * 3),
	})
	ents = append(ents, &tarEntry{
		Path: "var/log/pipe", Type: tar.TypeFifo, Mode: 0o620, Uid: 1000, Gid: 1000,
		Uname: "app", Gname: "app", Mtime: baseTime.Add(7200 * time.Second),
	})
	ents = append(ents, &tarEntry{
		Path: "dev/null", Type: tar.TypeChar, Mode: 0o666, Uid: 0, Gid: 0,
		Uname: "root", Gname: "root", Devmajor: 1, Devminor: 3, Mtime: baseTime,
	})
	ents = append(ents, &tarEntry{
		Path: "dev/sda", Type: tar.TypeBlock, Mode: 0o660, Uid: 0, Gid: 6,
		Uname: "root", Gname: "disk", Devmajor: 8, Devminor: 16, Mtime: baseTime,
	})
	// A symlink whose name and target are both long enough to force PAX
	// extended headers, with an attribute on the link itself.
	longName := "etc/" + strings.Repeat("ln-", 60) + "link"
	longTarget := strings.Repeat("../", 60) + "opt/data/deep-target.txt"
	ents = append(ents, &tarEntry{
		Path: longName, Type: tar.TypeSymlink, Mode: 0o644, Uid: 0, Gid: 0,
		Uname: "root", Gname: "root", Link: longTarget, Mtime: baseTime.Add(2 * time.Hour),
		Xattrs: map[string]string{"user.link": "long"},
	})
	if li == 0 {
		// A user file with a long path, in a directory that only this layer
		// defines, to exercise PAX headers on regular files too.
		deep := "opt/data/" + strings.Repeat("segment-", 30) + "notes.txt"
		ents = append(ents, &tarEntry{
			Path: deep, Type: tar.TypeReg, Mode: 0o600, Uid: 0, Gid: 0,
			Uname: "root", Gname: "root", Mtime: baseTime.Add(3 * time.Hour),
			Data:   []byte("deep path content\n"),
			Xattrs: map[string]string{"user.deep": "1"},
		})
	}
	// A symlink with an attribute, and a target for the "follow" tests.
	ents = append(ents, &tarEntry{
		Path: "etc/config.conf", Type: tar.TypeReg, Mode: 0o644, Uid: 0, Gid: 0,
		Uname: "root", Gname: "root", Mtime: baseTime.Add(time.Hour),
		Data:   []byte(fmt.Sprintf("layer=%d\nkey=value\n", li)),
		Xattrs: map[string]string{"user.tagged": "yes"},
	})
	ents = append(ents, &tarEntry{
		Path: "etc/config.link", Type: tar.TypeSymlink, Mode: 0o644, Uid: 0, Gid: 0,
		Uname: "root", Gname: "root", Link: "config.conf", Mtime: baseTime.Add(time.Hour),
	})
	if li > 0 {
		// Whiteout: remove a file and mark a directory opaque.
		ents = append(ents, &tarEntry{
			Path: "shared/.wh.f01.dat", Type: tar.TypeReg, Mode: 0, Uid: 0, Gid: 0, Mtime: baseTime,
		})
		if li == 1 {
			ents = append(ents, &tarEntry{
				Path: "var/log/.wh..wh..opq", Type: tar.TypeReg, Mode: 0, Uid: 0, Gid: 0, Mtime: baseTime,
			})
		}
	}
	// Sort entries: parents before children (so extraction sees directories
	// first), otherwise stable by path.
	sort.SliceStable(ents, func(i, j int) bool {
		a, b := ents[i], ents[j]
		ad, bd := strings.Count(a.Path, "/"), strings.Count(b.Path, "/")
		// Directories before files at the same depth.
		if ad != bd {
			return ad < bd
		}
		if (a.Type == tar.TypeDir) != (b.Type == tar.TypeDir) {
			return a.Type == tar.TypeDir
		}
		return a.Path < b.Path
	})
	// Rewrite the layer's tar in that order.
	layer.Entries = nil
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range ents {
		h := &tar.Header{
			Name: e.Path, Mode: e.Mode, Uid: e.Uid, Gid: e.Gid, Uname: e.Uname, Gname: e.Gname,
			ModTime: e.Mtime, Typeflag: e.Type, Linkname: e.Link,
			Devmajor: e.Devmajor, Devminor: e.Devminor, Format: tar.FormatPAX,
			Xattrs: e.Xattrs,
		}
		if e.Type == tar.TypeReg {
			h.Size = int64(len(e.Data))
		}
		if err := tw.WriteHeader(h); err != nil {
			panic(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write(e.Data); err != nil {
				panic(err)
			}
		}
		layer.Entries = append(layer.Entries, &Entry{Header: h, Body: e.Data})
	}
	if err := tw.Close(); err != nil {
		panic(err)
	}
	layer.Tar = buf.Bytes()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(layer.Tar)
	zw.Close()
	layer.TarGz = gz.Bytes()
	return layer
}
