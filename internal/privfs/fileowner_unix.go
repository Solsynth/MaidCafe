//go:build !windows

package privfs

import (
	"io/fs"
	"syscall"
)

// fileOwner reports the owning uid of a file, which the profile loader checks
// against root.
func fileOwner(info fs.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}
