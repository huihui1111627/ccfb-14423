// Package extract implements a plain, eager extraction of tar layers. It is
// the reference the lazy view is compared against: the same archive semantics,
// the same whiteout rules, the same metadata syscalls, but every byte is
// downloaded and unpacked upfront.
package extract

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"ccflz/internal/format"
	"ccflz/internal/fsmeta"
)

// Options controls reference extraction.
type Options struct {
	// ApplyWhiteouts enables OCI whiteout semantics (needed to merge layers).
	ApplyWhiteouts bool
	// RecordSkips collects metadata operations the OS refused.
	RecordSkips func(path, op string, err error)
	// RecordMissing collects archive entries that could not be created at
	// all (device nodes without privileges), with their tar type flag.
	RecordMissing func(path string, typ byte)
}

// LayerTar extracts one plain tar archive into root. Directory metadata is
// restored after the layer's entries have been written, exactly as tar does
// at the end of an extraction.
func LayerTar(root string, tarBytes []byte, opts Options) error {
	type dirMeta struct {
		path string
		e    *format.Entry
	}
	var dirs []dirMeta
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("extract: %w", err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		name = strings.TrimSuffix(name, "/")
		if name == "." || name == "" {
			continue
		}
		base := path.Base(name)
		if opts.ApplyWhiteouts && strings.HasPrefix(base, ".wh.") {
			if base == ".wh..wh..opq" {
				removeChildren(root, path.Dir(name))
			} else {
				remove(root, path.Join(path.Dir(name), strings.TrimPrefix(base, ".wh.")))
			}
			continue
		}
		dst := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		e := entryOf(hdr, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, os.FileMode(hdr.Mode&0o7777)|0o700); err != nil {
				return err
			}
			dirs = append(dirs, dirMeta{path: dst, e: e})
		case tar.TypeReg, tar.TypeRegA:
			// Keep hardlinked inodes intact: O_TRUNC on a shared inode would
			// also truncate the other names, so unlink only if the path is
			// not already a regular file with links.
			removeIfExists(dst)
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			applyMeta(dst, e, opts)
		case tar.TypeLink:
			// A BSD hardlink may carry body bytes in the stream.
			if hdr.Size > 0 {
				if _, err := io.Copy(io.Discard, tr); err != nil {
					return err
				}
			}
			target := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(hdr.Linkname, "./")))
			os.Remove(dst)
			if err := os.Link(target, dst); err != nil {
				return fmt.Errorf("hardlink %s -> %s: %w", name, hdr.Linkname, err)
			}
		case tar.TypeSymlink:
			removeIfExists(dst)
			if err := fsmeta.MakeSymlink(dst, hdr.Linkname, e); err != nil {
				return err
			}
		case tar.TypeFifo:
			removeIfExists(dst)
			if err := fsmeta.MakeFifo(dst, e); err != nil {
				if opts.RecordSkips != nil && fsmeta.IsPermission(err) {
					opts.RecordSkips(name, "mkfifo", err)
					continue
				}
				return err
			}
		case tar.TypeChar, tar.TypeBlock:
			// A non-privileged extraction cannot create device nodes; tar
			// continues with the rest of the archive and exits non-zero.
			if err := fsmeta.MakeDevice(dst, e); err != nil {
				if fsmeta.IsPermission(err) {
					if opts.RecordMissing != nil {
						opts.RecordMissing(name, hdr.Typeflag)
					}
					continue
				}
				return err
			}
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := fsmeta.Apply(dirs[i].path, dirs[i].e); err != nil && opts.RecordSkips != nil {
			opts.RecordSkips(dirs[i].e.Path, "meta", err)
		}
	}
	return nil
}

func applyMeta(p string, e *format.Entry, opts Options) {
	if e.Uid != 0 || e.Gid != 0 {
		if err := fsmeta.Chown(p, e.Uid, e.Gid); err != nil && opts.RecordSkips != nil {
			opts.RecordSkips(e.Path, "chown", err)
		}
	}
	if err := fsmeta.Apply(p, e); err != nil && opts.RecordSkips != nil {
		opts.RecordSkips(e.Path, "meta", err)
	}
}

func entryOf(h *tar.Header, name string) *format.Entry {
	e := &format.Entry{
		Path: name, Type: h.Typeflag, Mode: h.Mode, Uid: h.Uid, Gid: h.Gid,
		Uname: h.Uname, Gname: h.Gname,
		MtimeSec: h.ModTime.Unix(), MtimeNsec: int32(h.ModTime.Nanosecond()),
		Linkname: h.Linkname, Devmajor: h.Devmajor, Devminor: h.Devminor,
		Size: h.Size,
	}
	if len(h.Xattrs) > 0 {
		e.Xattrs = make(map[string]string, len(h.Xattrs))
		for k, v := range h.Xattrs {
			e.Xattrs[k] = v
		}
	}
	return e
}

// removeIfExists unlinks a path that a later entry is about to replace.
func removeIfExists(p string) {
	fi, err := os.Lstat(p)
	if err != nil {
		return
	}
	if fi.IsDir() {
		os.RemoveAll(p)
		return
	}
	os.Remove(p)
}

func remove(root, rel string) {
	if rel == "." || rel == "/" {
		return
	}
	os.RemoveAll(filepath.Join(root, filepath.FromSlash(rel)))
}

func removeChildren(root, rel string) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	ents, err := os.ReadDir(full)
	if err != nil {
		return
	}
	for _, e := range ents {
		os.RemoveAll(filepath.Join(full, e.Name()))
	}
}

// Image extracts all layers of a resolved image, in order, into root.
func Image(root string, layers [][]byte) error {
	for i, l := range layers {
		if err := LayerTar(root, l, Options{ApplyWhiteouts: true}); err != nil {
			return fmt.Errorf("layer %d: %w", i, err)
		}
	}
	return nil
}

// ErrSkipped reports a metadata operation the OS refused.
var ErrSkipped = errors.New("skipped")
