package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// openBarrier opens the raw root once for the committer's life, so syncfs
// reports a writeback error that happened between two batches.
func openBarrier(dataDir string) (barrier, func() error, error) {
	if err := checkKernel(); err != nil {
		return nil, nil, err
	}
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

// checkKernel refuses kernels before 5.8, whose syncfs does not return
// writeback errors.
func checkKernel() error {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return err
	}
	var b []byte
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	release := string(b)
	major, rest, _ := strings.Cut(release, ".")
	minor, _, _ := strings.Cut(rest, ".")
	if i := strings.IndexFunc(minor, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		minor = minor[:i]
	}
	maj, err1 := strconv.Atoi(major)
	mnr, err2 := strconv.Atoi(minor)
	if err1 != nil || err2 != nil || maj < 5 || maj == 5 && mnr < 8 {
		return fmt.Errorf("store: kernel %q: syncfs needs Linux 5.8 or later to report writeback errors", release)
	}
	return nil
}

func syncfsBarrier(root *os.File) barrier {
	return func(touched) error {
		if _, _, errno := syscall.Syscall(sysSyncfs, root.Fd(), 0, 0); errno != 0 {
			return errno
		}
		return nil
	}
}
