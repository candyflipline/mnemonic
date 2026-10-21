package push

import (
	"io/fs"
	"os"
	"sync"
	"syscall"
)

const vnodeEvents = syscall.NOTE_WRITE | syscall.NOTE_EXTEND | syscall.NOTE_DELETE | syscall.NOTE_RENAME

// notifier holds one O_EVTONLY descriptor per watched file and directory on
// a kqueue. A file path on changed means it was written, removed or renamed;
// a directory path means its entries changed.
type notifier struct {
	kq      int
	changed chan string
	done    chan struct{}
	exited  chan struct{}
	mu      sync.Mutex
	fds     map[string]int
	paths   map[int]string
}

func newNotifier() (*notifier, error) {
	kq, err := syscall.Kqueue()
	if err != nil {
		return nil, os.NewSyscallError("kqueue", err)
	}
	syscall.CloseOnExec(kq)
	wake := []syscall.Kevent_t{{Filter: syscall.EVFILT_USER, Flags: syscall.EV_ADD | syscall.EV_CLEAR}}
	if _, err := syscall.Kevent(kq, wake, nil, nil); err != nil {
		syscall.Close(kq)
		return nil, os.NewSyscallError("kevent", err)
	}
	n := &notifier{
		kq:      kq,
		changed: make(chan string, 64),
		done:    make(chan struct{}),
		exited:  make(chan struct{}),
		fds:     make(map[string]int),
		paths:   make(map[int]string),
	}
	go n.read()
	return n, nil
}

func (n *notifier) watch(path string, _ bool) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.fds[path]; ok {
		return nil
	}
	fd, err := syscall.Open(path, syscall.O_EVTONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return &fs.PathError{Op: "watch", Path: path, Err: err}
	}
	ev := []syscall.Kevent_t{{Ident: uint64(fd), Filter: syscall.EVFILT_VNODE, Flags: syscall.EV_ADD | syscall.EV_CLEAR, Fflags: vnodeEvents}}
	if _, err := syscall.Kevent(n.kq, ev, nil, nil); err != nil {
		syscall.Close(fd)
		return &fs.PathError{Op: "watch", Path: path, Err: err}
	}
	n.fds[path], n.paths[fd] = fd, path
	return nil
}

func (n *notifier) forget(path string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.drop(path)
}

func (n *notifier) drop(path string) {
	if fd, ok := n.fds[path]; ok {
		syscall.Close(fd)
		delete(n.fds, path)
		delete(n.paths, fd)
	}
}

// read delivers each event's path. A delete or rename leaves the descriptor
// on a vnode the path no longer names, so it is closed and the loop's next
// watch of the path opens whatever the path names now.
func (n *notifier) read() {
	defer close(n.exited)
	var buf [64]syscall.Kevent_t
	for {
		k, err := syscall.Kevent(n.kq, nil, buf[:], nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return
		}
		for _, ev := range buf[:k] {
			if ev.Filter == syscall.EVFILT_USER {
				return
			}
			n.mu.Lock()
			path, ok := n.paths[int(ev.Ident)]
			if ok && ev.Fflags&(syscall.NOTE_DELETE|syscall.NOTE_RENAME) != 0 {
				n.drop(path)
			}
			n.mu.Unlock()
			if !ok {
				continue
			}
			select {
			case n.changed <- path:
			case <-n.done:
				return
			}
		}
	}
}

func (n *notifier) close() {
	close(n.done)
	trigger := []syscall.Kevent_t{{Filter: syscall.EVFILT_USER, Fflags: syscall.NOTE_TRIGGER}}
	syscall.Kevent(n.kq, trigger, nil, nil)
	<-n.exited
	syscall.Close(n.kq)
	n.mu.Lock()
	defer n.mu.Unlock()
	for path := range n.fds {
		n.drop(path)
	}
}
