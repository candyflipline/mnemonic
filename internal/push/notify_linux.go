package push

import (
	"bytes"
	"encoding/binary"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

const (
	dirEvents   = syscall.IN_MODIFY | syscall.IN_CLOSE_WRITE | syscall.IN_CREATE | syscall.IN_MOVED_TO | syscall.IN_DELETE | syscall.IN_MOVED_FROM | syscall.IN_DELETE_SELF
	entryEvents = syscall.IN_CREATE | syscall.IN_MOVED_TO | syscall.IN_DELETE | syscall.IN_MOVED_FROM
)

// notifier holds one inotify watch per directory; events name the file. A
// file path on changed means it was written, created or removed; a directory
// path means its entries changed; lostEvents means the queue overflowed.
type notifier struct {
	fd      int
	f       *os.File
	changed chan string
	done    chan struct{}
	exited  chan struct{}
	mu      sync.Mutex
	wds     map[string]int
	dirs    map[int]string
}

func newNotifier() (*notifier, error) {
	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		return nil, os.NewSyscallError("inotify_init1", err)
	}
	n := &notifier{
		fd:      fd,
		f:       os.NewFile(uintptr(fd), "inotify"),
		changed: make(chan string, 64),
		done:    make(chan struct{}),
		exited:  make(chan struct{}),
		wds:     make(map[string]int),
		dirs:    make(map[int]string),
	}
	go n.read()
	return n, nil
}

func (n *notifier) watch(path string, dir bool) error {
	if !dir {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.wds[path]; ok {
		return nil
	}
	wd, err := syscall.InotifyAddWatch(n.fd, path, dirEvents)
	if err != nil {
		return &fs.PathError{Op: "watch", Path: path, Err: err}
	}
	n.wds[path], n.dirs[wd] = wd, path
	return nil
}

func (n *notifier) forget(path string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if wd, ok := n.wds[path]; ok {
		syscall.InotifyRmWatch(n.fd, uint32(wd))
		delete(n.wds, path)
		delete(n.dirs, wd)
	}
}

func (n *notifier) read() {
	defer close(n.exited)
	buf := make([]byte, 64<<10)
	for {
		k, err := n.f.Read(buf)
		if err != nil {
			return
		}
		for off := 0; off+syscall.SizeofInotifyEvent <= k; {
			wd := int(int32(binary.NativeEndian.Uint32(buf[off:])))
			mask := binary.NativeEndian.Uint32(buf[off+4:])
			end := off + syscall.SizeofInotifyEvent + int(binary.NativeEndian.Uint32(buf[off+12:]))
			name := string(bytes.TrimRight(buf[off+syscall.SizeofInotifyEvent:end], "\x00"))
			off = end
			if !n.deliver(n.paths(wd, mask, name)...) {
				return
			}
		}
	}
}

func (n *notifier) paths(wd int, mask uint32, name string) []string {
	if mask&syscall.IN_Q_OVERFLOW != 0 {
		return []string{lostEvents}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	dir, ok := n.dirs[wd]
	switch {
	case !ok:
		return nil
	case mask&syscall.IN_IGNORED != 0:
		delete(n.wds, dir)
		delete(n.dirs, wd)
		return nil
	case name == "":
		return []string{dir}
	case mask&entryEvents != 0:
		return []string{dir, filepath.Join(dir, name)}
	}
	return []string{filepath.Join(dir, name)}
}

func (n *notifier) deliver(paths ...string) bool {
	for _, p := range paths {
		select {
		case n.changed <- p:
		case <-n.done:
			return false
		}
	}
	return true
}

// rearm and drain exist for the kqueue notifier, which delivers one batch per
// rearm; inotify reads park on the netpoller, so every event goes on changed.
func (n *notifier) rearm() {}

func (n *notifier) drain() []string { return nil }

func (n *notifier) close() {
	close(n.done)
	n.f.Close()
	<-n.exited
}
