//go:build linux || darwin

package gitcache

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

func walkRoot(path string) (*os.File, error) {
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "//") {
		return nil, ErrPath
	}
	parts := strings.Split(path, "/")
	// parts[0] is empty because of the leading slash.
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, ErrPath
	}
	for _, name := range parts[1:] {
		if !componentOK(name) {
			syscall.Close(fd)
			return nil, ErrPath
		}
		next, err := openatDir(fd, name)
		syscall.Close(fd)
		if err != nil {
			return nil, ErrPath
		}
		fd = next
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		syscall.Close(fd)
		return nil, ErrPath
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		syscall.Close(fd)
		return nil, ErrPath
	}
	if st.Uid != uint32(os.Geteuid()) {
		syscall.Close(fd)
		return nil, ErrPath
	}
	if st.Mode&0777 != 0700 {
		syscall.Close(fd)
		return nil, ErrPath
	}
	dup, err := syscall.Dup(fd)
	syscall.Close(fd)
	if err != nil {
		return nil, ErrPath
	}
	return os.NewFile(uintptr(dup), "gitcache-root"), nil
}

func componentOK(name string) bool {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

type fsClient struct {
	cmd      *exec.Cmd
	in       io.WriteCloser
	out      io.Reader
	mu       sync.Mutex
	pgid     int
	dead     bool
	callHook func(op byte) error
}

// fsHelperArg is the helper argv. Production uses __gitcache_fs.
// A test may select an ACK stand-in that never completes a valid ready frame.
var fsHelperArg = "__gitcache_fs"

func startFS(helper string, root *os.File) (*fsClient, error) {
	if helper == "" || helper[0] != '/' {
		return nil, ErrPath
	}
	cmd := exec.Command(helper, fsHelperArg)
	cmd.Env = AllowEnv()
	cmd.Stdin = nil
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.ExtraFiles = []*os.File{root}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = stderrW
	if err := cmd.Start(); err != nil {
		stderrR.Close()
		stderrW.Close()
		return nil, err
	}
	stderrW.Close()
	go drainLimit(stderrR, cmd.Process.Pid, 64<<10)
	var ready [8]byte
	if err := readFullDeadline(stdout, ready[:], ackWait); err != nil || ready != [8]byte{} {
		qerr := QuiesceGroup(cmd.Process.Pid, cmd.Wait)
		stdin.Close()
		if qerr != nil {
			return nil, ErrNotQuiescent
		}
		return nil, ErrBusy
	}
	return &fsClient{cmd: cmd, in: stdin, out: stdout, pgid: cmd.Process.Pid}, nil
}

func readFullDeadline(r io.Reader, buf []byte, d time.Duration) error {
	if f, ok := r.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = f.SetReadDeadline(time.Now().Add(d))
		defer f.SetReadDeadline(time.Time{})
	}
	_, err := io.ReadFull(r, buf)
	return err
}

func drainLimit(r io.ReadCloser, pid, n int) {
	defer r.Close()
	buf := make([]byte, 4096)
	total := 0
	for total < n {
		k, err := r.Read(buf)
		total += k
		if err != nil {
			return
		}
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

func (c *fsClient) call(op byte, payload []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead {
		return nil, ErrClosed
	}
	if c.callHook != nil {
		if err := c.callHook(op); err != nil {
			return nil, err
		}
	}
	if len(payload) > 1<<20 {
		return nil, ErrQuota
	}
	var hdr [5]byte
	binary.LittleEndian.PutUint32(hdr[:4], uint32(len(payload)))
	hdr[4] = op
	if _, err := c.in.Write(hdr[:]); err != nil {
		return nil, err
	}
	if _, err := c.in.Write(payload); err != nil {
		return nil, err
	}
	var rh [8]byte
	if err := readFullDeadline(c.out, rh[:], ackWait); err != nil {
		c.killLocked()
		return nil, ErrNotQuiescent
	}
	status := binary.LittleEndian.Uint32(rh[:4])
	n := binary.LittleEndian.Uint32(rh[4:])
	if n > 1<<20 {
		return nil, ErrCorrupt
	}
	buf := make([]byte, n)
	if err := readFullDeadline(c.out, buf, ackWait); err != nil {
		c.killLocked()
		return nil, ErrNotQuiescent
	}
	if status != 0 {
		return buf, fsStatus(status)
	}
	return buf, nil
}

func fsStatus(code uint32) error {
	switch code {
	case 1:
		return ErrBusy
	case 2:
		return ErrCorrupt
	case 3:
		return ErrPath
	case 4:
		return ErrQuota
	case 6:
		return ErrAbsent
	default:
		return ErrUnsupported
	}
}

func (c *fsClient) killLocked() {
	if c.dead {
		return
	}
	c.dead = true
	if c.pgid <= 1 {
		return
	}
	_ = syscall.Kill(-c.pgid, syscall.SIGKILL)
	if c.cmd != nil {
		_ = c.cmd.Wait()
	}
}

func (c *fsClient) crash() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dead = true
	if c.pgid <= 1 {
		return
	}
	_ = syscall.Kill(-c.pgid, syscall.SIGKILL)
	if c.cmd != nil {
		_ = c.cmd.Wait()
	}
	_ = syscall.Kill(-c.pgid, 0)
}

func (c *fsClient) stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead {
		return nil
	}
	c.dead = true
	_ = c.in.Close()
	waitErr := c.cmd.Wait()
	err := syscall.Kill(-c.pgid, 0)
	if err == nil {
		_ = syscall.Kill(-c.pgid, syscall.SIGKILL)
		return ErrNotQuiescent
	}
	if !errors.Is(err, syscall.ESRCH) {
		return ErrNotQuiescent
	}
	if waitErr != nil {
		return waitErr
	}
	return nil
}

func runFSHelper() int {
	root := 3
	if err := syscall.Fchdir(root); err != nil {
		return 2
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(root, &st); err != nil {
		return 2
	}
	h := &fsHelper{root: root, dev: uint64(st.Dev)}
	if _, err := os.Stdout.Write(make([]byte, 8)); err != nil {
		return 2
	}
	in := os.Stdin
	for {
		var hdr [5]byte
		if _, err := io.ReadFull(in, hdr[:]); err != nil {
			return 0
		}
		n := binary.LittleEndian.Uint32(hdr[:4])
		if n > 1<<20 {
			writeStatus(os.Stdout, 4, nil)
			return 4
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(in, payload); err != nil {
			return 2
		}
		status, body := h.op(hdr[4], payload)
		if err := writeStatus(os.Stdout, status, body); err != nil {
			return 2
		}
	}
}

func writeStatus(w io.Writer, status uint32, body []byte) error {
	var rh [8]byte
	binary.LittleEndian.PutUint32(rh[:4], status)
	binary.LittleEndian.PutUint32(rh[4:], uint32(len(body)))
	if _, err := w.Write(rh[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

type fsHelper struct {
	root int
	dev  uint64
}

func (h *fsHelper) op(op byte, payload []byte) (uint32, []byte) {
	switch op {
	case 1: // write path\0 uint64 max, uint64 off, data
		return h.write(payload)
	case 2: // read
		return h.read(payload)
	case 3:
		return h.mkdir(payload)
	case 4:
		return h.rename(payload)
	case 5:
		return h.unlink(payload)
	case 6:
		return h.lstat(payload)
	case 7:
		return h.list(payload)
	case 8:
		if err := syscall.Fsync(h.root); err != nil {
			return 5, nil
		}
		return 0, nil
	default:
		return 5, nil
	}
}

func (h *fsHelper) at(rel string, fn func(base string) error) error {
	if err := syscall.Fchdir(h.root); err != nil {
		return err
	}
	parts, err := splitRel(rel)
	if err != nil {
		return err
	}
	for _, p := range parts[:len(parts)-1] {
		if err := h.step(p); err != nil {
			return err
		}
	}
	return fn(parts[len(parts)-1])
}

func (h *fsHelper) step(name string) error {
	var st syscall.Stat_t
	if err := syscall.Lstat(name, &st); err != nil {
		return err
	}
	if st.Mode&syscall.S_IFMT == syscall.S_IFLNK || uint64(st.Dev) != h.dev {
		return ErrPath
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return ErrPath
	}
	fd, err := syscall.Open(name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	err = syscall.Fchdir(fd)
	syscall.Close(fd)
	return err
}

func splitRel(rel string) ([]string, error) {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, "//") {
		return nil, ErrPath
	}
	parts := strings.Split(rel, "/")
	if len(parts) == 0 || len(parts) > 8 {
		return nil, ErrPath
	}
	for _, p := range parts {
		if !componentOK(p) {
			return nil, ErrPath
		}
	}
	return parts, nil
}

func (h *fsHelper) write(payload []byte) (uint32, []byte) {
	z := bytes.IndexByte(payload, 0)
	if z <= 0 || len(payload) < z+1+16 {
		return 3, nil
	}
	rel := string(payload[:z])
	max := binary.LittleEndian.Uint64(payload[z+1 : z+9])
	off := binary.LittleEndian.Uint64(payload[z+9 : z+17])
	data := payload[z+17:]
	if uint64(len(data)) > max || off > max-uint64(len(data)) {
		return 4, nil
	}
	var n int
	err := h.at(rel, func(base string) error {
		var st syscall.Stat_t
		lerr := syscall.Lstat(base, &st)
		if lerr == nil && (st.Mode&syscall.S_IFMT == syscall.S_IFLNK || st.Nlink != 1 || uint64(st.Dev) != h.dev) {
			return ErrPath
		}
		flags := syscall.O_WRONLY | syscall.O_NOFOLLOW
		if errors.Is(lerr, syscall.ENOENT) {
			flags |= syscall.O_CREAT | syscall.O_EXCL
		} else if lerr != nil {
			return lerr
		}
		if off == 0 {
			flags |= syscall.O_TRUNC
		}
		fd, err := syscall.Open(base, flags, 0600)
		if err != nil {
			return err
		}
		defer syscall.Close(fd)
		if _, err := syscall.Seek(fd, int64(off), io.SeekStart); err != nil {
			return err
		}
		wn, err := syscall.Write(fd, data)
		n = wn
		if err != nil {
			return err
		}
		if wn != len(data) {
			return ErrQuota
		}
		if err := syscall.Fsync(fd); err != nil {
			return err
		}
		return fsyncCwd()
	})
	var out [4]byte
	binary.LittleEndian.PutUint32(out[:], uint32(n))
	if err != nil {
		return mapErr(err), out[:]
	}
	return 0, out[:]
}

func (h *fsHelper) read(payload []byte) (uint32, []byte) {
	rel := string(payload)
	var buf []byte
	err := h.at(rel, func(base string) error {
		var st syscall.Stat_t
		if err := syscall.Lstat(base, &st); err != nil {
			return err
		}
		if st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Nlink != 1 || uint64(st.Dev) != h.dev {
			return ErrPath
		}
		if st.Size < 0 || uint64(st.Size) > 1<<20 {
			return ErrQuota
		}
		fd, err := syscall.Open(base, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		defer syscall.Close(fd)
		buf = make([]byte, st.Size)
		return readFullFD(fd, buf)
	})
	if err != nil {
		return mapErr(err), nil
	}
	return 0, buf
}

func (h *fsHelper) mkdir(payload []byte) (uint32, []byte) {
	err := h.at(string(payload), func(base string) error {
		if err := syscall.Mkdir(base, 0700); err != nil {
			return err
		}
		return fsyncCwd()
	})
	if err != nil {
		return mapErr(err), nil
	}
	_ = syscall.Fchdir(h.root)
	_ = syscall.Fsync(h.root)
	return 0, nil
}

func (h *fsHelper) rename(payload []byte) (uint32, []byte) {
	parts := bytes.Split(payload, []byte{0})
	if len(parts) != 2 {
		return 3, nil
	}
	oldRel, newRel := string(parts[0]), string(parts[1])
	oldParts, err := splitRel(oldRel)
	if err != nil {
		return 3, nil
	}
	newParts, err := splitRel(newRel)
	if err != nil {
		return 3, nil
	}
	if len(oldParts) != len(newParts) {
		return 3, nil
	}
	for i := 0; i < len(oldParts)-1; i++ {
		if oldParts[i] != newParts[i] {
			return 3, nil
		}
	}
	err = h.at(oldRel, func(base string) error {
		if err := syscall.Rename(base, newParts[len(newParts)-1]); err != nil {
			return err
		}
		return fsyncCwd()
	})
	if err != nil {
		return mapErr(err), nil
	}
	return 0, nil
}

func (h *fsHelper) unlink(payload []byte) (uint32, []byte) {
	err := h.at(string(payload), func(base string) error {
		var st syscall.Stat_t
		if err := syscall.Lstat(base, &st); err != nil {
			return err
		}
		if st.Mode&syscall.S_IFMT == syscall.S_IFLNK {
			if err := syscall.Unlink(base); err != nil {
				return err
			}
			return fsyncCwd()
		}
		if st.Mode&syscall.S_IFMT == syscall.S_IFDIR {
			if err := syscall.Rmdir(base); err != nil {
				return err
			}
			return fsyncCwd()
		}
		if st.Nlink != 1 || uint64(st.Dev) != h.dev {
			return ErrPath
		}
		if err := syscall.Unlink(base); err != nil {
			return err
		}
		return fsyncCwd()
	})
	if err != nil {
		return mapErr(err), nil
	}
	return 0, nil
}

func (h *fsHelper) lstat(payload []byte) (uint32, []byte) {
	var line string
	err := h.at(string(payload), func(base string) error {
		var st syscall.Stat_t
		if err := syscall.Lstat(base, &st); err != nil {
			return err
		}
		line = fmt.Sprintf("%d %d %d %d %d", st.Mode, st.Nlink, st.Size, st.Uid, st.Dev)
		return nil
	})
	if err != nil {
		return mapErr(err), nil
	}
	return 0, []byte(line)
}

func (h *fsHelper) list(payload []byte) (uint32, []byte) {
	if err := syscall.Fchdir(h.root); err != nil {
		return 5, nil
	}
	rel := string(payload)
	if rel != "" && rel != "." {
		parts, err := splitRel(rel)
		if err != nil {
			return 3, nil
		}
		for _, p := range parts {
			if err := h.step(p); err != nil {
				return mapErr(err), nil
			}
		}
	}
	fd, err := syscall.Open(".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return 5, nil
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFDIR || uint64(st.Dev) != h.dev {
		syscall.Close(fd)
		return 3, nil
	}
	f := os.NewFile(uintptr(fd), "cwd")
	defer f.Close()
	var names []string
	var nbytes int
	for {
		chunk, err := f.Readdirnames(32)
		if err != nil && len(chunk) == 0 {
			if err == io.EOF {
				break
			}
			return 5, nil
		}
		for _, name := range chunk {
			if len(name) > 255 || nbytes+len(name)+1 > 8192 || len(names) >= 128 {
				return 2, nil
			}
			names = append(names, name)
			nbytes += len(name) + 1
		}
		if err == io.EOF {
			break
		}
	}
	return 0, []byte(strings.Join(names, "\n"))
}

func fsyncCwd() error {
	fd, err := syscall.Open(".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	return syscall.Fsync(fd)
}

func readFullFD(fd int, buf []byte) error {
	for len(buf) > 0 {
		n, err := syscall.Read(fd, buf)
		if n > 0 {
			buf = buf[n:]
			continue
		}
		if err != nil {
			return err
		}
		return io.EOF
	}
	return nil
}

func mapErr(err error) uint32 {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrBusy), errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
		return 1
	case errors.Is(err, ErrCorrupt):
		return 2
	case errors.Is(err, ErrPath), errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR):
		return 3
	case errors.Is(err, ErrQuota), errors.Is(err, syscall.EFBIG):
		return 4
	case errors.Is(err, syscall.ENOENT):
		return 6
	default:
		return 5
	}
}
