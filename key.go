package filesystem

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxKeyLength is the longest key, in bytes, that CleanKey accepts. It is
// S3's limit, the strictest of the common object stores, so a key that is
// valid on one disk is valid on every disk.
const MaxKeyLength = 1024

// separator is the only path separator in a key. A backslash in input is
// converted to it, because Windows clients send backslash paths and a key
// that means a different object on different backends is a portability bug.
const separator = "/"

// CleanKey normalises a caller-supplied key and rejects unsafe ones. Every
// Disk method runs it before a driver sees the key, which is what makes path
// traversal impossible regardless of the driver's own care.
//
// Normalisation: backslashes become slashes, leading slashes are dropped,
// empty and "." segments are removed, and runs of slashes collapse.
//
// Rejection (CodeInvalidKey): an empty result, any ".." segment (rejected
// rather than resolved, so "a/../b" is an error and never silently "b"),
// invalid UTF-8, a control character (including NUL), or more than
// MaxKeyLength bytes.
func CleanKey(raw string) (string, error) {
	key, err := cleanPath(raw)
	if err != nil {
		return "", err
	}
	if key == "" {
		return "", Errorf(CodeInvalidKey, "empty key")
	}
	return key, nil
}

// CleanDir normalises a directory argument (for List and DeletePrefix) the
// same way as CleanKey, except that the empty string, "/" and "." are valid
// and mean the root of the disk. The result never has a trailing slash.
func CleanDir(raw string) (string, error) {
	return cleanPath(raw)
}

func cleanPath(raw string) (string, error) {
	if !utf8.ValidString(raw) {
		return "", Errorf(CodeInvalidKey, "key is not valid UTF-8")
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return "", Errorf(CodeInvalidKey, "key contains control character %U", r)
		}
	}
	parts := strings.Split(strings.ReplaceAll(raw, `\`, separator), separator)
	kept := parts[:0]
	for _, p := range parts {
		switch p {
		case "", ".":
			continue
		case "..":
			// The key itself is not echoed: this message reaches clients
			// (invalid_key is about their input), and the Disk's error
			// already records the key for logs.
			return "", Errorf(CodeInvalidKey, "key contains a %q segment", "..")
		}
		kept = append(kept, p)
	}
	key := strings.Join(kept, separator)
	if len(key) > MaxKeyLength {
		return "", Errorf(CodeInvalidKey, "key is %d bytes, longer than %d", len(key), MaxKeyLength)
	}
	return key, nil
}

// JoinKey joins already-clean key parts with the separator, skipping empty
// parts. It does not clean its input; wrapper drivers use it to prefix keys
// that the Disk has already cleaned.
func JoinKey(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, separator)
}

// KeyInDir reports whether key lies under dir (a clean directory as returned
// by CleanDir, where "" is the root). A key equal to dir is not inside it.
func KeyInDir(key, dir string) bool {
	if dir == "" {
		return key != ""
	}
	return strings.HasPrefix(key, dir+separator)
}

// RelativeKey returns key with the dir prefix removed. Callers must have
// checked KeyInDir first; for a key outside dir it returns key unchanged.
func RelativeKey(key, dir string) string {
	if dir == "" {
		return key
	}
	return strings.TrimPrefix(key, dir+separator)
}
