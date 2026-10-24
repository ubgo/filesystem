// Package archive reads zip, tar, tar.gz and gzip files that
// users upload, without trusting them. List walks an archive without
// storing anything, Extract stores its files on a Disk, and Inspect plugs
// the walk into an upload.Policy so a bomb or a zip-slip archive is
// refused before anything reads it later.
//
// Every limit is enforced while the archive is being decompressed, never
// after: the walk counts each decompressed byte as it is produced and
// stops at the first byte past a limit, so a bomb costs at most the limit
// in time and memory. Every entry name goes through filesystem.CleanKey;
// absolute and traversing names, symlinks, hard links, devices, FIFOs and
// duplicate names are refused (ErrUnsafeEntry) rather than skipped, so an
// archive built to attack an extractor is never half-extracted. Archives
// nested inside the archive are walked too, and count against
// Limits.MaxDepth.
//
// Formats are detected from content, never names: gzip and tar by their
// magic at the start, zip by its end-of-central-directory record wherever
// it sits, as archive/zip finds it, so a polyglot (a zip behind a PDF
// header) is walked like any zip; a streamed entry whose last bytes hold
// a zip directory it did not start with is refused.
package archive

import (
	"errors"
	"fmt"
	"time"

	"github.com/ubgo/filesystem"
)

// Format is an archive container the package reads.
type Format string

// The closed set of formats, detected from the first bytes, never from a
// file name or a client's content type.
const (
	FormatZip     Format = "zip"
	FormatTar     Format = "tar"
	FormatTarGzip Format = "tar.gz"
	// FormatGzip is a gzip file holding something other than a tar,
	// walked as an archive of one entry so its content is checked too.
	FormatGzip Format = "gzip"
)

// FormatValues lists every Format.
var FormatValues = []Format{FormatZip, FormatTar, FormatTarGzip, FormatGzip}

// Defaults for a zero Limits field. They suit an archive a person uploads
// through a web form (a folder of documents or photos); an application that
// takes backups or datasets sets larger values.
const (
	// DefaultMaxEntries is the most entries (files and directories, at
	// every depth) one archive may hold.
	DefaultMaxEntries = 10_000
	// DefaultMaxTotal is the most bytes the whole archive may decompress
	// to, nested archives included: 1 GiB.
	DefaultMaxTotal int64 = 1 << 30
	// DefaultMaxRatio is the most decompressed bytes allowed per byte of
	// the archive. Real documents compress 2 to 20 times; a bomb compresses
	// thousands of times.
	DefaultMaxRatio int64 = 100
	// DefaultMaxDepth is the most archive levels: the uploaded archive is
	// level 1, an archive inside it level 2.
	DefaultMaxDepth = 2
	// DefaultMaxNested is the largest nested archive, in bytes. A nested
	// archive is read into memory to be walked (zip needs random access),
	// so this bounds the memory one walk can take, well below MaxTotal.
	DefaultMaxNested int64 = 64 << 20
)

// RatioGrace is the number of decompressed bytes allowed before MaxRatio
// applies, so a tiny archive of text (a 300-byte zip holding 40 KB of CSV)
// is not refused for a ratio no bomb could exploit at that size.
const RatioGrace int64 = 1 << 20

// Limits bound what an archive may expand to. A zero field means its
// Default constant, so the zero Limits is the safe one; there is no
// "unlimited", and an application that needs more sets a larger number.
type Limits struct {
	// MaxEntries caps the number of entries at every depth together.
	MaxEntries int
	// MaxTotal caps the decompressed bytes at every depth together. The
	// bytes of a nested archive count once as a file of its parent and
	// again as what it decompresses to.
	MaxTotal int64
	// MaxRatio caps the decompressed bytes per byte of the uploaded
	// archive, once more than RatioGrace bytes have been decompressed.
	MaxRatio int64
	// MaxDepth caps the archive levels; 1 refuses any nested archive. A
	// gzip file counts as a level, as does a tar.gz.
	MaxDepth int
	// MaxNested caps the size of one nested archive, which the walk holds
	// in memory while it walks it.
	MaxNested int64
}

// withDefaults fills every zero field with its Default constant.
func (l Limits) withDefaults() Limits {
	if l.MaxEntries <= 0 {
		l.MaxEntries = DefaultMaxEntries
	}
	if l.MaxTotal <= 0 {
		l.MaxTotal = DefaultMaxTotal
	}
	if l.MaxRatio <= 0 {
		l.MaxRatio = DefaultMaxRatio
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = DefaultMaxDepth
	}
	if l.MaxNested <= 0 {
		l.MaxNested = DefaultMaxNested
	}
	return l
}

// Entry describes one entry of an archive.
type Entry struct {
	// Name is the entry's path made safe by filesystem.CleanKey, relative
	// to the archive that holds it. Extract stores the file at
	// filesystem.JoinKey(prefix, Name).
	Name string
	// In is "" for an entry of the walked archive itself; for an entry of
	// a nested archive, the nested archive's path, levels joined by
	// NestedSeparator ("docs/inner.zip!/deep.tar").
	In string
	// Depth is the archive level: 1 for entries of the walked archive.
	Depth int
	// Format is the container the entry was read from.
	Format Format
	// IsDir is set for a directory entry, which has no content.
	IsDir bool
	// Size is the entry's decompressed size, as counted while it was read
	// (zip and tar also declare it, and the readers hold each entry to its
	// declaration).
	Size int64
	// ModTime is the time recorded in the archive, untrusted.
	ModTime time.Time
	// Executable is set when the content starts like a program: ELF,
	// Windows PE, Mach-O or a "#!" script.
	Executable bool
	// Archive is set when the entry is itself a zip, tar, tar.gz or gzip
	// archive, which the walk entered (its entries follow it, with In
	// naming it).
	Archive bool
}

// Path is the entry's full location: In, NestedSeparator and Name, or
// just Name at depth 1.
func (e Entry) Path() string {
	if e.In == "" {
		return e.Name
	}
	return e.In + NestedSeparator + e.Name
}

// NestedSeparator joins the levels of a nested entry's path, as Java's
// jar URLs do; "!" cannot be mistaken for a key separator.
const NestedSeparator = "!/"

// Sentinel errors. Every error the walk returns for the archive's content
// is a *filesystem.Error with CodeRejected that wraps one of these, so
// errors.Is tells a bomb from an attack from a file that is no archive.
var (
	// ErrLimit: a Limits field was exceeded.
	ErrLimit = errors.New("archive: limit exceeded")
	// ErrUnsafeEntry: an entry an extractor must never follow (absolute
	// or traversing name, symlink, hard link, device, FIFO, duplicate).
	ErrUnsafeEntry = errors.New("archive: unsafe entry")
	// ErrFormat: the input is not a zip, tar, tar.gz or gzip file, or is
	// corrupt.
	ErrFormat = errors.New("archive: not a readable zip, tar, tar.gz or gzip file")
)

// rejected wraps a sentinel as the CodeRejected error the walk returns.
func rejected(sentinel error, format string, args ...any) error {
	return filesystem.NewError(filesystem.CodeRejected, fmt.Errorf("%w: "+format, append([]any{sentinel}, args...)...))
}
