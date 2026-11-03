//go:build unix

package core

import (
	"os"
	"syscall"
)

// diskSpace returns the bytes available to unprivileged writers and the
// filesystem size.
func diskSpace(dir string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return uint64(st.Bavail) * bs, uint64(st.Blocks) * bs, nil
}

func lockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }

// statDisk is diskSpace, replaceable in tests.
var statDisk = diskSpace
