// Package view merges an image's layers into a single filesystem tree and
// layers the container's writable directory on top.
//
// Merge rules reproduce a plain `tar -x` of each layer in order:
//   - every layer entry is applied in archive order;
//   - ".wh.<name>" removes the named path from the view;
//   - ".wh..wh..opq" removes every existing child of its directory;
//   - a later entry replaces an earlier entry at the same path;
//   - hardlink entries share content and metadata with their target;
//   - directory entries carry their own mode, owner, mtime and xattrs.
//
// File contents are lazy: a Node only names the layer file and its chunk
// segments. Writable files live in the container's writable directory; the
// first write to a lazy file fetches the whole file before copying it up.
package view

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ccflz/internal/format"
	"ccflz/internal/image"
)

// NodeType classifies a view node.
type NodeType string

const (
	TypeDir     NodeType = "dir"
	TypeFile    NodeType = "file"
	TypeSymlink NodeType = "symlink"
	TypeChar    NodeType = "char"
	TypeBlock   NodeType = "block"
	TypeFifo    NodeType = "fifo"
	TypeOther   NodeType = "other"
)

// Node is one entry in the merged view.
type Node struct {
	Path      string
	Type      NodeType
	Mode      int64
	Uid       int
	Gid       int
	Uname     string
	Gname     string
	MtimeSec  int64
	MtimeNsec int32
	Linkname  string
	Devmajor  int64
	Devminor  int64
	Xattrs    map[string]string
	Size      int64

	// Origin layer (index into image.Resolved.Layers) for lazy files.
	Layer int
	// File is the lazy body descriptor (nil for empty files).
	File *format.File
	// HardlinkTo is the resolved target path for hardlinks.
	HardlinkTo string
	// LinkGroup identifies the inode group: every node with the same
	// non-zero group was created by a link event and must share one inode.
	LinkGroup int

	// Writable marks a node stored in the writable directory.
	Writable     bool
	WritablePath string
}

// Snapshot is a merged view plus the writable overlay.
type Snapshot struct {
	Nodes       map[string]*Node
	Children    map[string][]string // dir -> sorted child base names
	WritableDir string
	Image       *image.Resolved

	mu      sync.RWMutex
	nextIno int
	// rootMeta is the explicit "/" entry, if a layer had one.
	rootMeta *Node
}

// New builds a snapshot from a resolved image.
func New(res *image.Resolved, writableDir string) (*Snapshot, error) {
	s := &Snapshot{
		Nodes: make(map[string]*Node), Children: make(map[string][]string),
		WritableDir: writableDir, Image: res,
	}
	for li, lr := range res.Layers {
		for _, e := range lr.Index.Entries {
			s.applyEntry(li, e)
		}
	}
	s.rebuildChildren()
	return s, nil
}

func (s *Snapshot) applyEntry(li int, e *format.Entry) {
	if base := path.Base(e.Path); strings.HasPrefix(base, ".wh.") {
		if base == ".wh..wh..opq" {
			s.removeChildren(path.Dir(AbsPath(e.Path)))
			return
		}
		s.remove(path.Join(path.Dir(AbsPath(e.Path)), strings.TrimPrefix(base, ".wh.")))
		return
	}
	p := AbsPath(e.Path)
	switch e.Type {
	case '5':
		s.ensureDirMeta(p, e, li)
		// Materializing a directory creates it, which updates implicit parents
		// only when the archive did not carry them. Explicit entries keep
		// their own metadata.
	case '0', '7':
		s.nextIno++
		n := &Node{
			LinkGroup: s.nextIno,
			Path:      p, Type: TypeFile, Mode: e.Mode, Uid: e.Uid, Gid: e.Gid,
			Uname: e.Uname, Gname: e.Gname, MtimeSec: e.MtimeSec, MtimeNsec: e.MtimeNsec,
			Xattrs: e.Xattrs, Size: e.Size, Layer: li,
		}
		if f := s.Image.Layers[li].Index.File(e.Path); f != nil && !f.Hardlink {
			n.File = f
			n.Size = f.Size
		}
		s.ensureParents(p)
		s.Nodes[p] = n
	case '2':
		n := &Node{
			Path: p, Type: TypeSymlink, Mode: e.Mode, Uid: e.Uid, Gid: e.Gid,
			Uname: e.Uname, Gname: e.Gname, MtimeSec: e.MtimeSec, MtimeNsec: e.MtimeNsec,
			Linkname: e.Linkname, Xattrs: e.Xattrs, Layer: li,
		}
		s.ensureParents(p)
		s.Nodes[p] = n
	case '3', '4', '6':
		n := &Node{
			Path: p, Type: devType(e), Mode: e.Mode, Uid: e.Uid, Gid: e.Gid,
			Uname: e.Uname, Gname: e.Gname, MtimeSec: e.MtimeSec, MtimeNsec: e.MtimeNsec,
			Devmajor: e.Devmajor, Devminor: e.Devminor, Xattrs: e.Xattrs, Layer: li,
		}
		s.ensureParents(p)
		s.Nodes[p] = n
	case '1':
		// Hardlink: metadata and content come from the target, matching what
		// extraction tools produce (the link is another name for the inode).
		target := AbsPath(e.Linkname)
		if tn, ok := s.Nodes[target]; ok {
			n := *tn
			n.Path = p
			n.HardlinkTo = target
			n.Writable = false
			n.WritablePath = ""
			// Share the target's content descriptor directly: hardlinks must
			// see exactly the write/copy-up state of their target.
			n.File = tn.File
			n.Layer = tn.Layer
			n.Size = tn.Size
			if n.LinkGroup == 0 {
				s.nextIno++
				n.LinkGroup = s.nextIno
				// Give the target the same group so both materialize on one
				// inode.
				tcopy := *tn
				tcopy.LinkGroup = n.LinkGroup
				s.Nodes[target] = &tcopy
			}
			s.ensureParents(p)
			s.Nodes[p] = &n
			return
		}
		n := &Node{
			Path: p, Type: TypeFile, Mode: e.Mode, Uid: e.Uid, Gid: e.Gid,
			Uname: e.Uname, Gname: e.Gname, MtimeSec: e.MtimeSec, MtimeNsec: e.MtimeNsec,
			Xattrs: e.Xattrs, HardlinkTo: target, Layer: li,
		}
		if f := s.Image.Layers[li].Index.File(e.Path); f != nil {
			n.File = f
			n.Size = f.Size
		}
		s.ensureParents(p)
		s.Nodes[p] = n
	}
}

func (s *Snapshot) remove(p string) {
	if _, ok := s.Nodes[p]; !ok {
		return
	}
	delete(s.Nodes, p)
	prefix := p + "/"
	for q := range s.Nodes {
		if strings.HasPrefix(q, prefix) {
			delete(s.Nodes, q)
		}
	}
}

func (s *Snapshot) removeChildren(dir string) {
	prefix := dir + "/"
	if dir == "/" {
		prefix = "/"
	}
	for q := range s.Nodes {
		if strings.HasPrefix(q, prefix) {
			delete(s.Nodes, q)
		}
	}
}

func (s *Snapshot) ensureParents(p string) {
	dir := path.Dir(p)
	if dir == "/" || dir == "." {
		return
	}
	s.ensureDirMeta(dir, nil, -1)
}

func (s *Snapshot) ensureDirMeta(p string, e *format.Entry, li int) {
	if p == "/" || p == "." {
		if e != nil {
			n := &Node{Path: "/", Type: TypeDir, Mode: e.Mode}
			s.setEntryMeta(n, e, li)
			s.rootMeta = n
		}
		return
	}
	if n, ok := s.Nodes[p]; ok && n.Type == TypeDir {
		if e != nil {
			s.setEntryMeta(n, e, li)
		}
		return
	}
	n := &Node{Path: p, Type: TypeDir, Mode: 0o755}
	if e != nil {
		s.setEntryMeta(n, e, li)
	}
	s.ensureParents(p)
	s.Nodes[p] = n
	if s.Children[p] == nil {
		s.Children[p] = []string{}
	}
}

func (s *Snapshot) setEntryMeta(n *Node, e *format.Entry, li int) {
	n.Mode, n.Uid, n.Gid = e.Mode, e.Uid, e.Gid
	n.Uname, n.Gname = e.Uname, e.Gname
	n.MtimeSec, n.MtimeNsec = e.MtimeSec, e.MtimeNsec
	n.Xattrs = e.Xattrs
	n.Layer = li
}

func (s *Snapshot) rebuildChildren() {
	s.Children = make(map[string][]string)
	for p, n := range s.Nodes {
		if p == "/" || p == "." {
			continue
		}
		dir := path.Dir(p)
		s.Children[dir] = append(s.Children[dir], path.Base(p))
		if n.Type == TypeDir {
			if s.Children[p] == nil {
				s.Children[p] = []string{}
			}
		}
	}
	for d := range s.Children {
		sort.Strings(s.Children[d])
	}
	// Hardlinks resolve their target's bytes.
	for _, n := range s.Nodes {
		if n.HardlinkTo == "" {
			continue
		}
		seen := map[string]bool{}
		t := n
		for t.HardlinkTo != "" && !seen[t.Path] {
			seen[t.Path] = true
			if nt := s.Nodes[t.HardlinkTo]; nt != nil {
				t = nt
			} else {
				break
			}
		}
		if t != n {
			n.File = t.File
			n.Layer = t.Layer
			n.Size = t.Size
			n.Uid, n.Gid, n.Mode = t.Uid, t.Gid, t.Mode
			n.Uname, n.Gname = t.Uname, t.Gname
			n.MtimeSec, n.MtimeNsec = t.MtimeSec, t.MtimeNsec
			n.Xattrs = t.Xattrs
		}
	}
}

// List returns a directory's children in sorted order.
func (s *Snapshot) List(dir string) ([]string, error) {
	dir = normalize(dir)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if dir != "/" {
		n, ok := s.Nodes[dir]
		if !ok {
			return nil, os.ErrNotExist
		}
		if n.Type != TypeDir {
			return nil, fmt.Errorf("%s: not a directory", dir)
		}
	}
	out := s.Children[dir]
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// Lookup returns the node at p (nil when absent).
func (s *Snapshot) Lookup(p string) *Node {
	p = normalize(p)
	if p == "/" {
		return &Node{Path: "/", Type: TypeDir, Mode: 0o755}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Nodes[p]
}

// Walk visits every node in deterministic order.
func (s *Snapshot) Walk(fn func(*Node) error) error {
	s.mu.RLock()
	nodes := make(map[string]*Node, len(s.Nodes))
	for p, n := range s.Nodes {
		nodes[p] = n
	}
	s.mu.RUnlock()
	paths := make([]string, 0, len(nodes))
	for p := range nodes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		n := nodes[p]
		// Hardlinks carry a copy of the target's metadata; visit them as
		// their own node so callers can materialize the link itself.
		if err := fn(n); err != nil {
			return err
		}
	}
	return nil
}

// Read returns file bytes [off, off+n) of a node, fetching lazily.
func (s *Snapshot) Read(p string, off, n int64) ([]byte, error) {
	s.mu.RLock()
	node := s.Nodes[normalize(p)]
	s.mu.RUnlock()
	if node == nil {
		return nil, os.ErrNotExist
	}
	if node.Type != TypeFile {
		return nil, fmt.Errorf("%s: is a %s, not a file", p, node.Type)
	}
	if off < 0 || n < 0 || off+n > node.Size {
		return nil, fmt.Errorf("%s: range [%d,%d) outside size %d", p, off, off+n, node.Size)
	}
	if n == 0 {
		return []byte{}, nil
	}
	if node.File == nil && !node.Writable {
		return make([]byte, n), nil
	}
	if node.Writable {
		f, err := os.Open(node.WritablePath)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		buf := make([]byte, n)
		if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
			return nil, err
		}
		return buf, nil
	}
	return s.Image.Layers[node.Layer].Shared.FetchChunksOrFile(node.File, off, n)
}

// ReadAll returns the whole content of a node.
func (s *Snapshot) ReadAll(p string) ([]byte, error) {
	node := s.Lookup(p)
	if node == nil {
		return nil, os.ErrNotExist
	}
	return s.readAllNode(node)
}

func (s *Snapshot) readAllNode(node *Node) ([]byte, error) {
	if node.Type != TypeFile {
		return nil, fmt.Errorf("%s: is a %s, not a file", node.Path, node.Type)
	}
	if node.Writable {
		return os.ReadFile(node.WritablePath)
	}
	if node.File == nil {
		return make([]byte, node.Size), nil
	}
	return s.Image.Layers[node.Layer].Shared.FetchFile(node.File)
}

// CopyUp materializes a lazy file into the writable directory. The whole file
// is fetched before anything is written.
func (s *Snapshot) CopyUp(p string) (*Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p = normalize(p)
	node := s.Nodes[p]
	if node == nil {
		return nil, os.ErrNotExist
	}
	if node.Writable {
		return node, nil
	}
	if node.Type != TypeFile {
		return nil, fmt.Errorf("%s: cannot copy up a %s", p, node.Type)
	}
	data, err := s.readAllNode(node)
	if err != nil {
		return nil, err
	}
	wp := filepath.Join(s.WritableDir, filepath.FromSlash(strings.TrimPrefix(p, "/")))
	if err := os.MkdirAll(filepath.Dir(wp), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(wp, data, 0o644); err != nil {
		return nil, err
	}
	nn := *node
	nn.Writable = true
	nn.WritablePath = wp
	nn.File = nil
	nn.HardlinkTo = ""
	s.Nodes[p] = &nn
	// Every hardlink to this path shares the copy-up. Materialize links by
	// inode group; for the lazy view, redirect their reads to the copy.
	if nn.LinkGroup != 0 {
		for q, other := range s.Nodes {
			if q != p && other.LinkGroup == nn.LinkGroup {
				o := *other
				o.Writable = true
				o.WritablePath = wp
				o.File = nil
				o.Size = nn.Size
				s.Nodes[q] = &o
			}
		}
	}
	return &nn, nil
}

// Write writes data at off to a node, copying it up first if needed.
func (s *Snapshot) Write(p string, off int64, data []byte) error {
	p = normalize(p)
	node := s.Lookup(p)
	if node == nil {
		return os.ErrNotExist
	}
	if node.Type != TypeFile {
		return fmt.Errorf("%s: not a file", p)
	}
	if !node.Writable {
		var err error
		node, err = s.CopyUp(p)
		if err != nil {
			return err
		}
	}
	f, err := os.OpenFile(node.WritablePath, os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteAt(data, off); err != nil {
		return err
	}
	s.mu.Lock()
	if end := off + int64(len(data)); end > node.Size {
		node.Size = end
	}
	s.mu.Unlock()
	return nil
}

// Create makes a new writable file.
func (s *Snapshot) Create(p string, mode int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p = normalize(p)
	if s.Nodes[p] != nil {
		return os.ErrExist
	}
	if err := s.ensureParentsLive(p); err != nil {
		return err
	}
	wp := filepath.Join(s.WritableDir, filepath.FromSlash(strings.TrimPrefix(p, "/")))
	if err := os.MkdirAll(filepath.Dir(wp), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(wp, nil, os.FileMode(mode&0o777)); err != nil {
		return err
	}
	s.Nodes[p] = &Node{Path: p, Type: TypeFile, Mode: mode, Writable: true, WritablePath: wp}
	s.rebuildChildren()
	return nil
}

// Mkdir creates a directory in the writable overlay.
func (s *Snapshot) Mkdir(p string, mode int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p = normalize(p)
	if s.Nodes[p] != nil {
		return os.ErrExist
	}
	if err := s.ensureParentsLive(p); err != nil {
		return err
	}
	wp := filepath.Join(s.WritableDir, filepath.FromSlash(strings.TrimPrefix(p, "/")))
	if err := os.MkdirAll(wp, 0o755); err != nil {
		return err
	}
	s.Nodes[p] = &Node{Path: p, Type: TypeDir, Mode: mode, Writable: true, WritablePath: wp}
	s.rebuildChildren()
	return nil
}

// Remove deletes a node and its subtree from the view. For a hardlink whose
// writable inode is still named by another path, only the name is removed;
// the shared file must survive, exactly like unlinking one of several hard
// links on a real filesystem.
func (s *Snapshot) Remove(p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p = normalize(p)
	node := s.Nodes[p]
	if node == nil {
		return os.ErrNotExist
	}
	sharedInode := false
	if node.Writable && node.WritablePath != "" {
		for q, other := range s.Nodes {
			if q != p && other.Writable && other.WritablePath == node.WritablePath {
				sharedInode = true
				break
			}
		}
	}
	delete(s.Nodes, p)
	prefix := p + "/"
	for q := range s.Nodes {
		if strings.HasPrefix(q, prefix) {
			delete(s.Nodes, q)
		}
	}
	if s.WritableDir != "" && !sharedInode {
		os.RemoveAll(filepath.Join(s.WritableDir, filepath.FromSlash(strings.TrimPrefix(p, "/"))))
	}
	s.rebuildChildren()
	return nil
}

func (s *Snapshot) ensureParentsLive(p string) error {
	dir := path.Dir(p)
	if dir == "/" || dir == "." {
		return nil
	}
	if n, ok := s.Nodes[dir]; ok {
		if n.Type != TypeDir {
			return fmt.Errorf("%s: parent is not a directory", p)
		}
		return nil
	}
	if err := s.ensureParentsLive(dir); err != nil {
		return err
	}
	s.Nodes[dir] = &Node{Path: dir, Type: TypeDir, Mode: 0o755, Writable: true}
	return nil
}

// State is the persisted writable overlay manifest.
type State struct {
	Entries map[string]*Node `json:"entries"`
}

// SaveState writes the overlay manifest.
func (s *Snapshot) SaveState() error {
	if s.WritableDir == "" {
		return nil
	}
	st := State{Entries: make(map[string]*Node)}
	for p, n := range s.Nodes {
		if n.Writable {
			st.Entries[p] = n
		}
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.WritableDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.WritableDir, ".ccflz-state.json"), b, 0o644)
}

// ModTime returns a node's modification time.
func (n *Node) ModTime() time.Time { return time.Unix(n.MtimeSec, int64(n.MtimeNsec)) }

// IsDir reports whether the node is a directory.
func (n *Node) IsDir() bool { return n.Type == TypeDir }

func devType(e *format.Entry) NodeType {
	switch e.Type {
	case '3':
		return TypeChar
	case '4':
		return TypeBlock
	case '6':
		return TypeFifo
	}
	return TypeOther
}

// AbsPath converts an archive-relative path to an absolute view path.
func AbsPath(p string) string { return normalize(p) }

func normalize(p string) string {
	if p == "" {
		return "/"
	}
	p = path.Clean("/" + p)
	return p
}

// RootMeta records the metadata of the view root, if any layer carried an
// explicit "/" entry.
func (s *Snapshot) RootMeta() *Node {
	return s.rootMeta
}

// MkdirAll creates a directory and any missing parents in the writable
// overlay.
func (s *Snapshot) MkdirAll(p string, mode int64) error {
	p = normalize(p)
	if n, ok := s.Nodes[p]; ok {
		if n.Type != TypeDir {
			return fmt.Errorf("%s: not a directory", p)
		}
		return nil
	}
	if err := s.ensureParentsLive(p); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wp := filepath.Join(s.WritableDir, filepath.FromSlash(strings.TrimPrefix(p, "/")))
	if err := os.MkdirAll(wp, 0o755); err != nil {
		return err
	}
	if n, ok := s.Nodes[p]; !ok || n.Type != TypeDir {
		s.Nodes[p] = &Node{Path: p, Type: TypeDir, Mode: mode, Writable: true, WritablePath: wp}
	}
	s.rebuildChildren()
	return nil
}

// Paths returns all file paths and directory paths in the view, sorted.
func (s *Snapshot) Paths() (files, dirs []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for p, n := range s.Nodes {
		switch n.Type {
		case TypeFile:
			files = append(files, p)
		case TypeDir:
			dirs = append(dirs, p)
		}
	}
	sort.Strings(files)
	sort.Strings(dirs)
	return
}
