//go:build !linux

package store

// SKETCH (docs/design/uploader.md): off Linux (the Mac runs tests and local
// mode) the barrier fsyncs each touched file, then each touched dir; Go's
// os.File.Sync is F_FULLFSYNC on darwin. O(touched) per batch, which the
// fsync-count gate does not measure (it runs on Linux).

func fsyncBarrier() barrier {
	return func(t touched) error {
		// TODO: for each of t.files then t.dirs: open, Sync, Close; first
		//       error wins.
		panic("not implemented")
	}
}
