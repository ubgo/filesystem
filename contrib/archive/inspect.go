package archive

import (
	"context"
	"errors"
	"io"
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/upload"
)

// Inspect returns an upload.Inspector that walks each stored upload under
// limits and runs check (when not nil) on every entry, nested ones
// included: a bomb, an unsafe entry or an entry check refuses is
// CodeRejected, and the upload deletes the object. An upload that is not
// an archive passes, so the inspector can sit in a policy that
// also accepts other types; a policy that must receive archives refuses
// other types through Policy.Allowed.
//
// The stored object is read through ReaderAt, in ranged reads, so the
// archive is never held in memory.
func Inspect(limits Limits, check func(Entry) error) upload.Inspector {
	return func(ctx context.Context, disk *filesystem.Disk, r upload.Result) error {
		ra, err := OpenReaderAt(ctx, disk, r.Key)
		if err != nil {
			return err
		}
		err = walkArchive(ctx, ra, ra.Size(), limits, func(_ context.Context, e Entry, content io.Reader) error {
			if check == nil {
				return nil
			}
			if err := counted(&e, content); err != nil {
				return err
			}
			if err := check(e); err != nil {
				var fe *filesystem.Error
				if errors.As(err, &fe) {
					return err
				}
				return filesystem.NewError(filesystem.CodeRejected, err)
			}
			return nil
		})
		if errors.Is(err, ErrFormat) && !isArchiveContent(ra, ra.Size()) {
			return nil
		}
		return err
	}
}

// DefaultExecutableExtensions are the file name extensions
// DenyExecutables refuses: programs, installers, scripts, shortcuts, disk
// images and registry files that Windows, macOS, Linux or Android run or
// mount when opened. Lower case, with the dot. It returns a new slice, so
// an application extends it with append and passes the result to
// DenyExecutablesWith.
func DefaultExecutableExtensions() []string {
	return []string{
		// Windows programs and installers.
		".exe", ".dll", ".com", ".scr", ".pif", ".cpl", ".msi", ".msp", ".mst", ".msc", ".msix", ".appx", ".gadget", ".application", ".sys", ".drv", ".ocx",
		// Windows scripts, shortcuts and settings.
		".bat", ".cmd", ".ps1", ".psm1", ".psd1", ".vbs", ".vbe", ".js", ".jse", ".wsf", ".wsh", ".ws", ".hta", ".lnk", ".url", ".scf", ".inf", ".reg",
		// Disk images, which mount and run their contents.
		".iso", ".img", ".vhd", ".vhdx", ".dmg",
		// Office add-ins that load code.
		".xll", ".wll",
		// Other platforms.
		".jar", ".apk", ".app", ".pkg", ".deb", ".rpm", ".sh", ".bash", ".csh", ".ksh", ".zsh", ".command", ".run", ".bin", ".elf",
	}
}

// DenyExecutables is an entry check for Inspect that refuses programs,
// with DefaultExecutableExtensions. See DenyExecutablesWith.
func DenyExecutables(e Entry) error {
	return denyExecutables(e, DefaultExecutableExtensions())
}

// DenyExecutablesWith returns an entry check for Inspect that refuses
// entries whose content starts like a program (Entry.Executable: ELF,
// Windows PE, Mach-O, "#!") and entries whose name ends in one of
// extensions (lower case, with the dot), whatever their content. The name
// is compared after the trailing dots and spaces Windows strips are
// removed, so "setup.exe. " is refused like "setup.exe".
func DenyExecutablesWith(extensions []string) func(Entry) error {
	exts := slices.Clone(extensions)
	return func(e Entry) error { return denyExecutables(e, exts) }
}

// denyExecutables refuses e when it is a program by content or by name.
func denyExecutables(e Entry, extensions []string) error {
	if e.IsDir {
		return nil
	}
	if e.Executable {
		return filesystem.Errorf(filesystem.CodeRejected, "archive entry %q is a program", e.Path())
	}
	name := strings.ToLower(strings.TrimRight(e.Name, ". "))
	if slices.Contains(extensions, path.Ext(name)) {
		return filesystem.Errorf(filesystem.CodeRejected, "archive entry %q has a program extension", e.Path())
	}
	return nil
}

// readBlock is how much ReaderAt fetches per ranged read, so the many
// small reads of a decompressor cost one request per mebibyte rather than
// one per call.
const readBlock = 1 << 20

// ReaderAt reads a stored object through ranged reads of the Disk, for
// List, Extract and Inspect over an archive already on a disk. It keeps
// the last block it fetched. It is safe for concurrent use.
//
// The context given to OpenReaderAt bounds every read; the reader must not
// be used after it ends.
type ReaderAt struct {
	ctx  context.Context
	disk *filesystem.Disk
	key  string
	size int64

	mu    sync.Mutex
	off   int64
	block []byte
}

// OpenReaderAt stats key on disk and returns a ReaderAt over it.
func OpenReaderAt(ctx context.Context, disk *filesystem.Disk, key string) (*ReaderAt, error) {
	e, err := disk.Stat(ctx, key)
	if err != nil {
		return nil, err
	}
	return &ReaderAt{ctx: ctx, disk: disk, key: key, size: e.Size, off: -1}, nil
}

// Size is the object's size when it was opened.
func (r *ReaderAt) Size() int64 { return r.size }

// ReadAt implements io.ReaderAt.
func (r *ReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, filesystem.Errorf(filesystem.CodeInvalidArgument, "negative offset %d", off)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if pos >= r.size {
			return n, io.EOF
		}
		start := pos - pos%readBlock
		if r.off != start {
			if err := r.fetch(start); err != nil {
				return n, err
			}
		}
		n += copy(p[n:], r.block[pos-start:])
	}
	return n, nil
}

// fetch reads the block starting at start.
func (r *ReaderAt) fetch(start int64) error {
	rc, err := r.disk.ReadRange(r.ctx, r.key, filesystem.ByteRange{Offset: start, Length: min(readBlock, r.size-start)})
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	if int64(len(b)) != min(readBlock, r.size-start) {
		return filesystem.Errorf(filesystem.CodeConflict, "object %q changed while it was read", r.key)
	}
	r.off, r.block = start, b
	return nil
}
