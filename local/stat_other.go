//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package local

import "io/fs"

// statStamp has no inode or change time to report on this system (Windows
// among others), so the driver does not declare compare-and-swap here.
func statStamp(fs.FileInfo) (ino uint64, ctime int64, ok bool) { return 0, 0, false }
