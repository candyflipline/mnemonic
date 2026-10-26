package store

// SKETCH (docs/design/uploader.md): Linux's barrier is one syncfs(2) on the
// data dir's filesystem, whatever the batch touched, so a batch costs at most
// three syscalls however many sources it carries.

import "os"

// syncfsBarrier returns the barrier over root's filesystem. root stays open
// for the committer's life.
func syncfsBarrier(root *os.File) barrier {
	return func(touched) error {
		// TODO: return unix.Syncfs(int(root.Fd())) (golang.org/x/sys/unix,
		//       already linked through grpc, promoted to direct; std syscall
		//       has no SYS_SYNCFS on amd64). Linux >= 5.8 reports writeback
		//       errors through syncfs; the committer treats any error as fatal.
		panic("not implemented")
	}
}
