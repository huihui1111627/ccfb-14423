// Package fsmeta applies archive metadata to real files: modes, ownership,
// timestamps, extended attributes, symlinks, hardlinks, FIFOs and device
// nodes. The implementation mirrors what tar does, including which steps a
// non-privileged process can and cannot perform.
package fsmeta

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"ccflz/internal/format"
)

// Apply sets metadata on path for entry e. For files it is called after the
// content has been written; for links with noFollow on the link itself.
func Apply(path string, e *format.Entry) error {
	if err := ApplyMode(path, e); err != nil {
		return err
	}
	if err := ApplyTimes(path, e); err != nil {
		return err
	}
	return ApplyXattrs(path, e)
}

// ApplyMode sets permission bits. Chmod is always safe on unix.
func ApplyMode(path string, e *format.Entry) error {
	return os.Chmod(path, os.FileMode(e.Mode&0o7777))
}

// ApplyTimes sets mtime (and atime) with nanosecond precision, following the
// final path component only when the node is not a symlink.
func ApplyTimes(path string, e *format.Entry) error {
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, timespecs(e), unix.AT_SYMLINK_NOFOLLOW)
}

func timespecs(e *format.Entry) []unix.Timespec {
	ts := unix.NsecToTimespec(e.MtimeSec*1e9 + int64(e.MtimeNsec))
	return []unix.Timespec{ts, ts}
}

// ApplyXattrs writes extended attributes. On Linux user.* attributes can only
// be set by the owner; other namespaces may require privileges. Errors are
// reported, and callers decide whether that counts as a mismatch.
func ApplyXattrs(path string, e *format.Entry) error {
	if len(e.Xattrs) == 0 {
		return nil
	}
	var firstErr error
	for k, v := range e.Xattrs {
		if err := unix.Lsetxattr(path, k, []byte(v), 0); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("setxattr %s on %s: %w", k, path, err)
			}
		}
	}
	return firstErr
}

// MakeDir creates a directory with the entry's mode.
func MakeDir(path string, e *format.Entry) error {
	if err := os.Mkdir(path, os.FileMode(e.Mode&0o7777)|0o700); err != nil {
		return err
	}
	return Apply(path, e)
}

// MakeSymlink creates a symbolic link.
func MakeSymlink(path, target string, e *format.Entry) error {
	if err := os.Symlink(target, path); err != nil {
		return err
	}
	if err := ApplyXattrs(path, e); err != nil {
		return err
	}
	return ApplyTimes(path, e)
}

// MakeHardlink links newpath to oldpath.
func MakeHardlink(oldpath, newpath string) error {
	return os.Link(oldpath, newpath)
}

// MakeFifo creates a named pipe.
func MakeFifo(path string, e *format.Entry) error {
	if err := unix.Mkfifo(path, uint32(e.Mode&0o7777)); err != nil {
		return err
	}
	if err := Apply(path, e); err != nil {
		return err
	}
	return nil
}

// MakeDevice creates a character or block device. This requires privileges;
// on an unprivileged process the kernel returns EPERM, exactly as it does for
// tar. The caller records the skip and still compares the archive metadata.
func MakeDevice(path string, e *format.Entry) error {
	var t uint32
	switch e.Type {
	case '3':
		t = unix.S_IFCHR
	case '4':
		t = unix.S_IFBLK
	default:
		return fmt.Errorf("not a device entry: %c", e.Type)
	}
	dev := int(unix.Mkdev(uint32(e.Devmajor), uint32(e.Devminor)))
	if err := unix.Mknod(path, t|uint32(e.Mode&0o7777), dev); err != nil {
		return err
	}
	return Apply(path, e)
}

// Chown sets ownership. Non-root processes can call the syscall but the
// kernel refuses ownership changes to other users; callers record that as a
// skip, which is what tar does too.
func Chown(path string, uid, gid int) error {
	return unix.Lchown(path, uid, gid)
}

// IsPermission reports whether err is the EPERM/EACCES family that non-root
// extraction normally hits for devices and foreign ownership.
func IsPermission(err error) bool {
	return errors.Is(err, unix.EPERM) || errors.Is(err, os.ErrPermission) || errors.Is(err, unix.EACCES)
}
