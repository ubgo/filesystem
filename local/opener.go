package local

import (
	"context"
	"net/url"
	"runtime"

	"github.com/ubgo/filesystem"
)

// Scheme is the DSN scheme of the local driver. These forms are accepted:
//
//	file:///var/app/uploads   absolute path
//	file:uploads              path relative to the working directory
//	file:///C:/app/uploads    absolute Windows path (on Windows)
//	file:C:/app/uploads       the same, opaque form
const Scheme = "file"

// goosWindows is runtime.GOOS on Windows, where a DSN path may start with a
// drive letter.
const goosWindows = "windows"

// Opener opens the directory named by a file DSN. It accepts no parameters
// beyond the Mux's own.
func Opener(_ context.Context, u *url.URL) (filesystem.Driver, error) {
	if err := filesystem.RejectParams(u); err != nil {
		return nil, err
	}
	dir, err := dsnDir(u, runtime.GOOS)
	if err != nil {
		return nil, err
	}
	return New(dir)
}

// dsnDir extracts the directory from a file DSN as the operating system
// goos spells it. A URL path always starts with "/", so file:///C:/x has
// the path "/C:/x"; on Windows that leading slash is dropped to give the
// drive path "C:/x" (Windows accepts forward slashes). On every other
// system "/C:/x" is an ordinary absolute path and is kept as written. A
// bare drive (file:///C: or file:C:) is that drive's root, "C:/".
//
// A host is refused: file://C:/x puts the drive in the host, and
// file://server/share is a UNC path the driver does not open.
func dsnDir(u *url.URL, goos string) (string, error) {
	if u.Host != "" {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "file DSN has a host %q; use file:///absolute/path, file:///C:/path on Windows, or file:relative/path", u.Host)
	}
	dir := u.Path
	if u.Opaque != "" {
		dir = u.Opaque
	}
	if dir == "" {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "file DSN has no path")
	}
	if goos != goosWindows {
		return dir, nil
	}
	if len(dir) >= 3 && dir[0] == '/' && isDriveLetter(dir[1]) && dir[2] == ':' {
		dir = dir[1:]
	}
	// A bare drive ("C:") names the current directory on that drive in
	// Windows, which depends on process state; the root of the drive is
	// what a DSN means.
	if len(dir) == 2 && isDriveLetter(dir[0]) && dir[1] == ':' {
		dir += "/"
	}
	return dir, nil
}

// isDriveLetter reports whether c can name a Windows drive.
func isDriveLetter(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}
