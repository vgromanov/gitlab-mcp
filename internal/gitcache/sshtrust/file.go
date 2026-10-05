//go:build linux || darwin

package sshtrust

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

// replacePostRenameHook is a path-bounded post-rename/pre-capture test seam.
// Ordinary runtime leaves the atomic pointer unset.
type replacePostRenameHook struct {
	path string
	fn   func(path string)
}

var replacePostRenamePhase atomic.Pointer[replacePostRenameHook]

// SetReplacePostRenamePhaseForTest installs a race-safe, path-bounded seam
// invoked only for the exact intended path after rename and before owned
// post-state capture. Returns a clear func that removes only this install.
// Pass an empty path or nil fn to clear any install. Not a product surface.
func SetReplacePostRenamePhaseForTest(path string, fn func(path string)) (clear func()) {
	if path == "" || fn == nil {
		replacePostRenamePhase.Store(nil)
		return func() {}
	}
	hook := &replacePostRenameHook{path: path, fn: fn}
	replacePostRenamePhase.Store(hook)
	return func() { replacePostRenamePhase.CompareAndSwap(hook, nil) }
}

// replace uses a per-file exclusive lock, verifies the read snapshot before
// committing, and atomically renames a synced sibling. Changes completed before
// the final snapshot comparison are rejected. Other programs need not honor
// our advisory lock; this cannot provide an impossible
// cross-program transaction guarantee over the comparison/rename interval.
func replace(ctx context.Context, old snapshot, data []byte) error {
	_, err := replaceSnapshot(ctx, old, data)
	return err
}

func replaceSnapshot(ctx context.Context, old snapshot, data []byte) (snapshot, error) {
	if len(data) > maxFileBytes || ctx.Err() != nil {
		return snapshot{}, ErrTrustFiles
	}
	dir := filepath.Dir(old.path)
	// Do not create arbitrary directories or redirect through changed symlinks.
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return snapshot{}, ErrTrustFiles
	}
	lock, err := os.OpenFile(old.path+".native-ssh.lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return snapshot{}, ErrTrustFiles
	}
	lock.Close()
	defer os.Remove(old.path + ".native-ssh.lock")
	if !unchangedContext(ctx, old) {
		return snapshot{}, ErrTrustFiles
	}
	f, err := os.CreateTemp(dir, ".known-hosts-native-*")
	if err != nil {
		return snapshot{}, ErrTrustFiles
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()
	mode := os.FileMode(0600)
	if old.exists {
		if old.info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return snapshot{}, ErrUpdateUnsupported
		}
		mode = old.info.Mode().Perm()
		st, ok := old.info.Sys().(*syscall.Stat_t)
		if !ok {
			return snapshot{}, ErrTrustFiles
		}
		if err = f.Chown(int(st.Uid), int(st.Gid)); err != nil {
			return snapshot{}, ErrTrustFiles
		}
		if err := writeAttributes(f, old.attrs); err != nil {
			return snapshot{}, err
		}
	}
	if f.Chmod(mode) != nil {
		return snapshot{}, ErrTrustFiles
	}
	if _, err = f.Write(data); err != nil {
		return snapshot{}, ErrTrustFiles
	}
	if old.exists {
		attrs, e := readAttributes(f)
		st, e2 := f.Stat()
		if e != nil || e2 != nil || !reflect.DeepEqual(attrs, old.attrs) || st.Mode().Perm() != mode {
			return snapshot{}, ErrUpdateUnsupported
		}
		was, ok := old.info.Sys().(*syscall.Stat_t)
		now, ok2 := st.Sys().(*syscall.Stat_t)
		if !ok || !ok2 || was.Uid != now.Uid || was.Gid != now.Gid {
			return snapshot{}, ErrUpdateUnsupported
		}
	}
	if f.Sync() != nil {
		return snapshot{}, ErrTrustFiles
	}
	if ctx.Err() != nil || !unchangedContext(ctx, old) {
		return snapshot{}, ErrTrustFiles
	}
	// Retain writer-controlled metadata from the replacement descriptor before
	// exposing the inode. Rename may change ctime/mtime; those are not compared
	// against this intent.
	intendedInfo, err := f.Stat()
	if err != nil {
		return snapshot{}, ErrTrustFiles
	}
	intendedAttrs, err := readAttributes(f)
	if err != nil {
		return snapshot{}, err
	}
	if err = os.Rename(name, old.path); err != nil {
		return snapshot{}, ErrTrustFiles
	}
	// The rename is the commit point. Directory sync is best-effort on filesystems
	// that do not implement it; failure must not claim the committed write vanished.
	if d, e := os.Open(dir); e == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	if hook := replacePostRenamePhase.Load(); hook != nil && hook.path == old.path {
		hook.fn(old.path)
	}
	// Capture the exact committed file through the writer's still-held descriptor.
	// Never bless bytes obtained from an unbound pathname reread.
	st, err := f.Stat()
	if err != nil {
		return snapshot{}, ErrTrustFiles
	}
	attrs, err := readAttributes(f)
	if err != nil {
		return snapshot{}, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return snapshot{}, ErrTrustFiles
	}
	got, err := readBounded(ctx, f)
	if err != nil || !bytes.Equal(got, data) {
		return snapshot{}, ErrTrustFiles
	}
	after, err := f.Stat()
	if err != nil || !sameMetadata(st, after) {
		return snapshot{}, ErrTrustFiles
	}
	if !sameOwnedMetadata(intendedInfo, st) || !reflect.DeepEqual(intendedAttrs, attrs) {
		return snapshot{}, ErrTrustFiles
	}
	if err := f.Close(); err != nil {
		return snapshot{}, ErrTrustFiles
	}
	return snapshot{path: old.path, exists: true, data: append([]byte(nil), data...), info: after, attrs: attrs}, nil
}

const attributeCap = 64 * 1024

// Metadata is captured from the opened descriptor and rechecked with content.
func sameMetadata(a, b os.FileInfo) bool {
	as, ok := a.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	bs, ok := b.Sys().(*syscall.Stat_t)
	return ok && os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) && as.Uid == bs.Uid && as.Gid == bs.Gid && sameChangeTime(as, bs)
}

// sameOwnedMetadata compares writer-controlled fields retained before expose
// against the post-rename capture. It allows rename-induced ctime/mtime changes.
func sameOwnedMetadata(intended, got os.FileInfo) bool {
	is, ok := intended.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	gs, ok := got.Sys().(*syscall.Stat_t)
	return ok && os.SameFile(intended, got) && intended.Mode() == got.Mode() && intended.Size() == got.Size() && is.Uid == gs.Uid && is.Gid == gs.Gid
}

func readAttributes(f *os.File) (map[string][]byte, error) {
	fd := int(f.Fd())
	size, err := unix.Flistxattr(fd, nil)
	if errors.Is(err, unix.ENOTSUP) {
		return map[string][]byte{}, nil
	}
	if err != nil || size > attributeCap {
		return nil, ErrUpdateUnsupported
	}
	names := make([]byte, size)
	n, err := unix.Flistxattr(fd, names)
	if err != nil || n > size {
		return nil, ErrUpdateUnsupported
	}
	out := map[string][]byte{}
	total := n
	for _, attr := range strings.Split(string(names[:n]), "\x00") {
		if attr == "" {
			continue
		}
		length, e := unix.Fgetxattr(fd, attr, nil)
		if e != nil || length > attributeCap-total {
			return nil, ErrUpdateUnsupported
		}
		value := make([]byte, length)
		n, e := unix.Fgetxattr(fd, attr, value)
		if e != nil || n != length {
			return nil, ErrUpdateUnsupported
		}
		total += length
		out[attr] = value
	}
	return out, nil
}
func writeAttributes(f *os.File, attrs map[string][]byte) error {
	// Remove inherited attributes absent from the reviewed snapshot as well.
	got, err := readAttributes(f)
	if err != nil {
		return err
	}
	for name := range got {
		if _, ok := attrs[name]; !ok {
			if unix.Fremovexattr(int(f.Fd()), name) != nil {
				return ErrUpdateUnsupported
			}
		}
	}
	for name, value := range attrs {
		if reflect.DeepEqual(value, got[name]) {
			continue
		}
		if unix.Fsetxattr(int(f.Fd()), name, value, 0) != nil {
			return ErrUpdateUnsupported
		}
	}
	got, err = readAttributes(f)
	if err != nil || !reflect.DeepEqual(got, attrs) {
		return ErrUpdateUnsupported
	}
	return nil
}
