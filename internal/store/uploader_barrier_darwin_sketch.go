//go:build darwin

package store

import "os"

// ARCHITECTURE SKETCH: same phase contract, explicit per-inode sync cost.
func syncStoreBarrier(root *os.File, files []*os.File, directories []string) error {
	// TODO: os.File.Sync each changed regular file; fsync changed directories
	// child-to-parent, including ancestors created this batch; propagate errors.
	// F_FULLFSYNC on regular files additionally flushes device caches for the
	// power-loss qualification run; never claim portable sync() is a barrier.
	// No O(batches) syscall claim for Darwin; local mode retains existing policy.
	panic("not implemented")
}
