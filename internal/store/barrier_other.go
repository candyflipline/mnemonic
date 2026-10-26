//go:build !linux

package store

import "os"

func openBarrier(string) (barrier, func() error, error) {
	return fsyncBarrier, func() error { return nil }, nil
}

// fsyncBarrier fsyncs each touched file, then each touched dir; os.File.Sync
// is F_FULLFSYNC on darwin.
func fsyncBarrier(t touched) error {
	for _, p := range append(t.files[:len(t.files):len(t.files)], t.dirs...) {
		if err := fsync(p); err != nil {
			return err
		}
	}
	return nil
}

func fsync(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
