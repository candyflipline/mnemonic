package store

import (
	"os"
	"path/filepath"
	"syscall"
)

func openBarrier(dataDir string) (barrier, func() error, error) {
	root := filepath.Join(dataDir, "raw")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, nil, err
	}
	f, err := os.Open(root)
	if err != nil {
		return nil, nil, err
	}
	return syncfsBarrier(f), f.Close, nil
}

// syncfsBarrier is one syncfs(2) on root's filesystem, whatever the batch
// touched.
func syncfsBarrier(root *os.File) barrier {
	return func(touched) error {
		if _, _, errno := syscall.Syscall(sysSyncfs, root.Fd(), 0, 0); errno != 0 {
			return errno
		}
		return nil
	}
}
