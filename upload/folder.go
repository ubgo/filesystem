package upload

import (
	"strings"

	"github.com/ubgo/filesystem"
)

// MaxPathDepth bounds the directories SafePath keeps from a client's
// relative path; deeper folders keep their innermost directories.
const MaxPathDepth = 32

// SafePath reduces a client-supplied relative path from a folder upload
// ("Holiday/2024/IMG_1.jpg", either separator) to one safe to show and to
// use under a prefix: each segment made safe as SafeFilename does it,
// empty, "." and ".." segments dropped (a client cannot climb out of the
// folder it dropped), at most MaxPathDepth directories, and the file name
// last. A name without directories comes back as SafeFilename(name).
func SafePath(name string) string {
	var segs []string
	for seg := range strings.SplitSeq(strings.ReplaceAll(name, `\`, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		segs = append(segs, SafeFilename(seg))
	}
	if len(segs) == 0 {
		return SafeFilename(name)
	}
	if dirs := len(segs) - 1; dirs > MaxPathDepth {
		segs = segs[dirs-MaxPathDepth:]
	}
	return strings.Join(segs, "/")
}

// FolderKey returns a KeyFunc that stores each file under dir at the
// relative path its client sent (Info.Path), so a dropped folder keeps its
// layout: "Holiday/2024/IMG_1.jpg" becomes "<dir>/Holiday/2024/IMG_1.jpg".
// Two files with one path meet at one key: choose what happens with
// Policy.Collision. Under several users' uploads, put the user in dir.
func FolderKey(dir string) KeyFunc {
	return func(info Info) (string, error) {
		return filesystem.JoinKey(dir, info.Path), nil
	}
}
