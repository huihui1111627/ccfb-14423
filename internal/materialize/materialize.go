// Package materialize writes a merged view into a real directory tree and
// produces a canonical metadata manifest for byte-for-byte comparison against
// a plain extraction of the same layers.
package materialize

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"ccflz/internal/format"
	"ccflz/internal/fsmeta"
	"ccflz/internal/view"
)

// Meta is one path's canonical metadata.
type Meta struct {
	Path      string            `json:"path"`
	Type      string            `json:"type"`
	Mode      uint32            `json:"mode"`
	Uid       int               `json:"uid"`
	Gid       int               `json:"gid"`
	MtimeSec  int64             `json:"mtime"`
	MtimeNsec int64             `json:"mtimens"`
	Link      string            `json:"link,omitempty"`
	Devmajor  int64             `json:"devmaj,omitempty"`
	Devminor  int64             `json:"devmin,omitempty"`
	Size      int64             `json:"size,omitempty"`
	Hash      string            `json:"sha256,omitempty"`
	Ino       uint64            `json:"ino,omitempty"`
	Xattrs    map[string]string `json:"xattrs,omitempty"`
	// Children lists a directory's entries, in sorted order.
	Children []string `json:"children,omitempty"`
}

// Result is the outcome of materializing a view.
type Result struct {
	Root    string
	Entries []*Meta
	ByPath  map[string]*Meta
	// HardlinkGroups maps a canonical path to the set of paths that share its
	// inode, as observed on disk.
	HardlinkGroups map[string][]string
	// Skipped lists entries that could not be created due to privileges
	// (devices, foreign ownership), mirroring tar's behavior as non-root.
	Skipped []Skip
}

// Skip records a metadata operation the process was not allowed to perform.
type Skip struct {
	Path string
	Op   string
	Err  string
}

// Snapshot materializes a merged view under root. Sizes are not trimmed by
// inconsistencies: any read error aborts.
func Snapshot(s *view.Snapshot, root string) (*Result, error) {
	res := &Result{Root: root, ByPath: make(map[string]*Meta), HardlinkGroups: make(map[string][]string)}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	if rm := s.RootMeta(); rm != nil {
		if err := fsmeta.Apply(root, entryOf(rm)); err != nil {
			res.Skipped = append(res.Skipped, Skip{Path: "/", Op: "meta", Err: err.Error()})
		}
	}
	// Directories first, shallowest first, so parents exist before children.
	type item struct {
		n     *view.Node
		depth int
	}
	var dirs, others []item
	if err := s.Walk(func(n *view.Node) error {
		it := item{n: n, depth: strings.Count(n.Path, "/")}
		if n.Type == view.TypeDir {
			dirs = append(dirs, it)
		} else {
			others = append(others, it)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(dirs, func(i, j int) bool {
		if dirs[i].depth != dirs[j].depth {
			return dirs[i].depth < dirs[j].depth
		}
		return dirs[i].n.Path < dirs[j].n.Path
	})
	sort.Slice(others, func(i, j int) bool { return others[i].n.Path < others[j].n.Path })

	seenHard := make(map[string]string) // link group -> materialized path
	for _, it := range dirs {
		n := it.n
		p := filepath.Join(root, filepath.FromSlash(n.Path))
		if err := os.MkdirAll(p, 0o755); err != nil {
			return nil, err
		}
		if n.Writable && n.Mode == 0 {
			continue
		}
		e := entryOf(n)
		if err := fsmeta.Apply(p, e); err != nil {
			res.Skipped = append(res.Skipped, Skip{Path: n.Path, Op: "meta", Err: err.Error()})
		}
		applyOwner(res, p, n)
	}
	for _, it := range others {
		n := it.n
		p := filepath.Join(root, filepath.FromSlash(n.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		e := entryOf(n)
		switch n.Type {
		case view.TypeFile:
			if n.LinkGroup != 0 {
				if src, ok := seenHard[fmt.Sprint(n.LinkGroup)]; ok {
					os.Remove(p)
					if err := os.Link(src, p); err != nil {
						return nil, fmt.Errorf("hardlink %s -> %s: %w", n.Path, src, err)
					}
					applyOwner(res, p, n)
					break
				}
				seenHard[fmt.Sprint(n.LinkGroup)] = p
			}
			data, err := s.ReadAll(n.Path)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", n.Path, err)
			}
			if err := os.WriteFile(p, data, 0o600); err != nil {
				return nil, err
			}
			if err := fsmeta.Apply(p, e); err != nil {
				res.Skipped = append(res.Skipped, Skip{Path: n.Path, Op: "meta", Err: err.Error()})
			}
			applyOwner(res, p, n)
		case view.TypeSymlink:
			if err := fsmeta.MakeSymlink(p, n.Linkname, e); err != nil {
				return nil, err
			}
			applyOwner(res, p, n)
		case view.TypeFifo:
			if err := fsmeta.MakeFifo(p, e); err != nil {
				if fsmeta.IsPermission(err) {
					res.Skipped = append(res.Skipped, Skip{Path: n.Path, Op: "mkfifo", Err: err.Error()})
					continue
				}
				return nil, err
			}
		case view.TypeChar, view.TypeBlock:
			if err := fsmeta.MakeDevice(p, e); err != nil {
				if fsmeta.IsPermission(err) {
					res.Skipped = append(res.Skipped, Skip{Path: n.Path, Op: "mknod", Err: err.Error()})
					continue
				}
				return nil, err
			}
		default:
			continue
		}
	}
	// A second pass restores directory metadata: creating children touches
	// parent mtimes.
	for _, it := range dirs {
		n := it.n
		if n.Writable && n.Mode == 0 {
			continue
		}
		p := filepath.Join(root, filepath.FromSlash(n.Path))
		e := entryOf(n)
		if err := fsmeta.Apply(p, e); err != nil {
			res.Skipped = append(res.Skipped, Skip{Path: n.Path, Op: "meta", Err: err.Error()})
		}
		applyOwner(res, p, n)
	}
	if err := Scan(root, res); err != nil {
		return nil, err
	}
	return res, nil
}

func applyOwner(res *Result, p string, n *view.Node) {
	if n.Uid == 0 && n.Gid == 0 {
		return
	}
	if err := fsmeta.Chown(p, n.Uid, n.Gid); err != nil {
		res.Skipped = append(res.Skipped, Skip{Path: n.Path, Op: "chown", Err: err.Error()})
	}
}

func entryOf(n *view.Node) *format.Entry {
	e := &format.Entry{
		Path: n.Path, Mode: n.Mode, Uid: n.Uid, Gid: n.Gid,
		Uname: n.Uname, Gname: n.Gname,
		MtimeSec: n.MtimeSec, MtimeNsec: n.MtimeNsec,
		Xattrs: n.Xattrs, Linkname: n.Linkname,
		Devmajor: n.Devmajor, Devminor: n.Devminor,
	}
	switch n.Type {
	case view.TypeDir:
		e.Type = '5'
	case view.TypeFile:
		e.Type = '0'
	case view.TypeSymlink:
		e.Type = '2'
	case view.TypeChar:
		e.Type = '3'
	case view.TypeBlock:
		e.Type = '4'
	case view.TypeFifo:
		e.Type = '6'
	}
	return e
}

// Scan reads back a directory tree into a canonical metadata manifest.
func Scan(root string, res *Result) error {
	type inoKey struct {
		dev uint64
		ino uint64
	}
	byIno := make(map[inoKey][]string)
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if rel == ".ccflz-state.json" {
			return nil
		}
		m, err := MetaOf(p, rel, fi)
		if err != nil {
			return err
		}
		res.Entries = append(res.Entries, m)
		res.ByPath[rel] = m
		if fi.Mode().IsRegular() {
			st := fi.Sys().(*syscall.Stat_t)
			k := inoKey{dev: uint64(st.Dev), ino: st.Ino}
			byIno[k] = append(byIno[k], rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(res.Entries, func(i, j int) bool { return res.Entries[i].Path < res.Entries[j].Path })
	for _, paths := range byIno {
		if len(paths) < 2 {
			continue
		}
		sort.Strings(paths)
		res.HardlinkGroups[paths[0]] = paths
	}
	return nil
}

// MetaOf builds the canonical metadata of one path.
func MetaOf(p, rel string, fi os.FileInfo) (*Meta, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("no stat information for %s", p)
	}
	m := &Meta{
		Path:      rel,
		Mode:      uint32(st.Mode),
		Uid:       int(st.Uid),
		Gid:       int(st.Gid),
		MtimeSec:  st.Mtimespec.Sec,
		MtimeNsec: st.Mtimespec.Nsec,
		Ino:       st.Ino,
		Size:      fi.Size(),
	}
	switch {
	case fi.Mode().IsDir():
		m.Type = "dir"
		m.Size = 0
		ents, err := os.ReadDir(p)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		m.Children = names
	case fi.Mode().IsRegular():
		m.Type = "file"
	case fi.Mode()&os.ModeSymlink != 0:
		m.Type = "symlink"
		t, err := os.Readlink(p)
		if err != nil {
			return nil, err
		}
		m.Link = t
		m.Size = 0
	case fi.Mode()&os.ModeCharDevice != 0:
		m.Type = "char"
		m.Devmajor = int64(unix.Major(uint64(st.Rdev)))
		m.Devminor = int64(unix.Minor(uint64(st.Rdev)))
		m.Size = 0
	case fi.Mode()&os.ModeDevice != 0:
		m.Type = "block"
		m.Devmajor = int64(unix.Major(uint64(st.Rdev)))
		m.Devminor = int64(unix.Minor(uint64(st.Rdev)))
		m.Size = 0
	case fi.Mode()&os.ModeNamedPipe != 0:
		m.Type = "fifo"
		m.Size = 0
	default:
		m.Type = "other"
	}
	if m.Type == "file" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		h := sha256.Sum256(b)
		m.Hash = hex.EncodeToString(h[:])
	}
	xs, err := ListXattrs(p)
	if err != nil {
		return nil, err
	}
	if len(xs) > 0 {
		m.Xattrs = xs
	}
	return m, nil
}

// ListXattrs lists a path's extended attributes, not following symlinks.
func ListXattrs(p string) (map[string]string, error) {
	sz, err := unix.Llistxattr(p, nil)
	if err != nil {
		if err == unix.ENOTSUP {
			return nil, nil
		}
		return nil, err
	}
	if sz == 0 {
		return nil, nil
	}
	buf := make([]byte, sz)
	sz, err = unix.Llistxattr(p, buf)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string)
	for _, name := range strings.Split(string(buf[:sz]), "\x00") {
		if name == "" || strings.HasPrefix(name, "com.apple.") {
			continue
		}
		vsz, err := unix.Lgetxattr(p, name, nil)
		if err != nil {
			return nil, err
		}
		v := make([]byte, vsz)
		if _, err := unix.Lgetxattr(p, name, v); err != nil {
			return nil, err
		}
		out[name] = string(v)
	}
	return out, nil
}

// Diff compares two manifests and returns human-readable differences.
func Diff(want, got map[string]*Meta) []string {
	var diffs []string
	seen := make(map[string]bool)
	for p, w := range want {
		seen[p] = true
		g := got[p]
		if g == nil {
			diffs = append(diffs, fmt.Sprintf("%s: missing (want %s)", p, w.Type))
			continue
		}
		diffs = append(diffs, diffOne(p, w, g)...)
	}
	for p := range got {
		if !seen[p] {
			diffs = append(diffs, fmt.Sprintf("%s: unexpected", p))
		}
	}
	sort.Strings(diffs)
	return diffs
}

func diffOne(p string, w, g *Meta) []string {
	var d []string
	add := func(f string, wv, gv any) {
		d = append(d, fmt.Sprintf("%s: %s want %v got %v", p, f, wv, gv))
	}
	if w.Type != g.Type {
		add("type", w.Type, g.Type)
		return d
	}
	if w.Mode != g.Mode {
		add("mode", fmt.Sprintf("%#o", w.Mode), fmt.Sprintf("%#o", g.Mode))
	}
	if w.Uid != g.Uid {
		add("uid", w.Uid, g.Uid)
	}
	if w.Gid != g.Gid {
		add("gid", w.Gid, g.Gid)
	}
	if w.MtimeSec != g.MtimeSec || w.MtimeNsec != g.MtimeNsec {
		add("mtime", fmt.Sprintf("%d.%09d", w.MtimeSec, w.MtimeNsec), fmt.Sprintf("%d.%09d", g.MtimeSec, g.MtimeNsec))
	}
	if w.Link != g.Link {
		add("link", w.Link, g.Link)
	}
	if w.Devmajor != g.Devmajor || w.Devminor != g.Devminor {
		add("device", fmt.Sprintf("%d,%d", w.Devmajor, w.Devminor), fmt.Sprintf("%d,%d", g.Devmajor, g.Devminor))
	}
	if w.Hash != g.Hash {
		add("sha256", w.Hash, g.Hash)
	}
	if w.Type == "dir" && !equalStrings(w.Children, g.Children) {
		add("children", w.Children, g.Children)
	}
	if len(w.Xattrs) != len(g.Xattrs) {
		add("xattrs", w.Xattrs, g.Xattrs)
	} else {
		for k, v := range w.Xattrs {
			if gv, ok := g.Xattrs[k]; !ok || gv != v {
				add("xattr "+k, v, g.Xattrs[k])
			}
		}
	}
	return d
}

// ScanNew reads a directory tree and returns the result.
func ScanNew(root string) (*Result, error) {
	res := &Result{Root: root, ByPath: make(map[string]*Meta), HardlinkGroups: make(map[string][]string)}
	if err := Scan(root, res); err != nil {
		return nil, err
	}
	return res, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// DiffMeta compares metadata fields a non-root process can reproduce:
// type, permissions, mtime, link target, device numbers, xattrs and content.
// Ownership and directory mtimes are excluded: an unprivileged run cannot
// create devices or foreign-owned files, and creating them updates parent
// directory timestamps. Ownership is compared separately through the
// extraction's reported privilege skips, matching tar's own behavior.
func DiffMeta(want, got map[string]*Meta) []string {
	seen := make(map[string]bool)
	var diffs []string
	for p, w := range want {
		seen[p] = true
		g := got[p]
		if g == nil {
			diffs = append(diffs, fmt.Sprintf("%s: missing from lazy view (want %s)", p, w.Type))
			continue
		}
		wc, gc := *w, *g
		wc.Uid, wc.Gid, gc.Uid, gc.Gid = 0, 0, 0, 0
		if wc.Type == "dir" {
			wc.MtimeSec, wc.MtimeNsec, gc.MtimeSec, gc.MtimeNsec = 0, 0, 0, 0
		}
		// Device nodes and FIFOs cannot be recreated without privileges, so
		// glob entries are compared only where both sides materialized them.
		if wc.Type == "char" || wc.Type == "block" || wc.Type == "fifo" {
			wc.Ino, gc.Ino = 0, 0
		}
		diffs = append(diffs, diffOne(p, &wc, &gc)...)
	}
	for p := range got {
		if !seen[p] {
			diffs = append(diffs, fmt.Sprintf("%s: unexpected in lazy view", p))
		}
	}
	sort.Strings(diffs)
	return diffs
}

// DiffMutated compares two trees after a workload. It ignores ownership and
// directory timestamps (an unprivileged run cannot reproduce them), and skips
// file timestamps for paths the workload mutated: a real write updates mtime,
// while the lazy view keeps archive metadata until materialization.
func DiffMutated(want, got map[string]*Meta, mutated map[string]bool) []string {
	seen := make(map[string]bool)
	var diffs []string
	for p, w := range want {
		seen[p] = true
		g := got[p]
		if g == nil {
			diffs = append(diffs, fmt.Sprintf("%s: missing (want %s)", p, w.Type))
			continue
		}
		wc, gc := *w, *g
		wc.Uid, wc.Gid, gc.Uid, gc.Gid = 0, 0, 0, 0
		if wc.Type == "dir" || mutated[p] || (wc.Type == "file" && wc.Size == 0 && gc.Size == 0) {
			// Mutated paths carry fresh timestamps in the reference, and
			// newly created empty files have no archive metadata to compare.
			wc.MtimeSec, wc.MtimeNsec, gc.MtimeSec, gc.MtimeNsec = 0, 0, 0, 0
		}
		if wc.Type == "char" || wc.Type == "block" || wc.Type == "fifo" {
			wc.Ino, gc.Ino = 0, 0
		}
		diffs = append(diffs, diffOne(p, &wc, &gc)...)
	}
	for p := range got {
		if !seen[p] {
			diffs = append(diffs, fmt.Sprintf("%s: unexpected in lazy view", p))
		}
	}
	sort.Strings(diffs)
	return diffs
}
