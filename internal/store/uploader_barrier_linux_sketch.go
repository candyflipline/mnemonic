//go:build linux

package store

import "os"

// ARCHITECTURE SKETCH: one filesystem, one retained FD for writeback errors.
func syncStoreBarrier(root *os.File, files []*os.File, directories []string) error {
	// TODO: unix.Syncfs(int(root.Fd())); retry EINTR, propagate EIO/ENOSPC/EDQUOT.
	// OpenCommitter checks mount boundaries and kernel >=5.8 before admission.
	// Do not substitute sync(), sync_file_range(), or a WAL-only fsync.
	panic("not implemented")
}
