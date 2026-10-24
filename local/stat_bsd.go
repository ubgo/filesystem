//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package local

import (
	"io/fs"
	"syscall"
)

// statStamp is the file's inode number and change time (ctime): the
// kernel sets the ctime on every change, and nothing can set it back.
func statStamp(info fs.FileInfo) (ino uint64, ctime int64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(st.Ino), st.Ctimespec.Nano(), true
}
