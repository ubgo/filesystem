package filesystem

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Driver is the contract a storage backend implements. It is deliberately
// small: everything portable (key cleaning, content-type detection, hashing,
// size limits, fallbacks, URLs, interceptors) is the Disk's job, so a driver
// only moves bytes.
//
// Contract every driver must honour, verified by the filesystemtest
// conformance suite:
//   - Keys and directories arrive already cleaned (see CleanKey).
//   - Stat and Read of a missing key return an error with CodeNotFound.
//   - Write streams body to completion and never closes it; the caller owns
//     it. If reading body fails, nothing is stored and the read error is
//     returned. A successful Write replaces any existing object atomically:
//     readers see the old bytes or the new bytes, never a mix.
//   - Delete of a missing key succeeds.
//   - List returns entries in ascending key order and honours Cursor and
//     Limit; a directory with no keys lists as empty, not CodeNotFound.
//   - Capabilities is constant for the driver's lifetime and true only for
//     what the driver really does.
type Driver interface {
	Stat(ctx context.Context, key string) (Entry, error)
	Read(ctx context.Context, key string, rng ByteRange) (io.ReadCloser, error)
	Write(ctx context.Context, key string, body io.Reader, opts WriteOptions) (Entry, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, dir string, opts ListOptions) (Page, error)
	Capabilities() Capabilities
}

// Capabilities declares what a driver can do beyond the Driver interface.
// Interface-backed fields must match the optional interfaces the driver
// implements; NewDisk checks this. The Disk calls an optional interface only
// when its field is true, which is what lets a wrapper driver implement every
// interface yet report only what the driver underneath supports.
type Capabilities struct {
	// Interface-backed capabilities.
	Copy          bool // implements Copier: server-side copy
	Move          bool // implements Mover: server-side or atomic rename
	SetVisibility bool // implements VisibilitySetter; Stat reports Visibility
	DeletePrefix  bool // implements PrefixDeleter: bulk delete in few calls
	DeleteKeys    bool // implements KeysDeleter: many keys deleted in few calls
	Presign       bool // implements Presigner: backend-signed URLs
	PublicURL     bool // implements PublicURLer
	Multipart     bool // implements MultipartUploader: parts uploaded through the server
	PresignPart   bool // implements PartPresigner: parts uploaded directly by clients
	PresignPost   bool // implements PostPresigner: browser form uploads the backend checks
	// SweepAbandoned: implements AbandonedSweeper, removing what unfinished
	// work left in the backend (multipart uploads never completed or
	// aborted, staging files of interrupted writes). Wrapper drivers that
	// narrow a disk to part of a backend (scoped, mirror) never forward it,
	// because the leftovers are backend-wide.
	SweepAbandoned bool
	// FindAbandoned: implements AbandonedFinder, listing what
	// SweepAbandoned would remove without removing it (a dry run). Needs
	// SweepAbandoned, and like it is never forwarded by a wrapper driver.
	FindAbandoned bool
	// Dirs: implements DirMaker. The backend has real directories, so
	// MakeDir creates an empty one that DirExists then reports. Without
	// it, MakeDir does nothing and a directory exists exactly while a key
	// is under it (see Disk.MakeDir).
	Dirs       bool
	Append     bool // implements Appender: add bytes to the end of an object
	SetModTime bool // implements ModTimeSetter: set an object's modification time
	// ConditionalRead: implements ConditionalReader, answering
	// If-None-Match and If-Modified-Since in the backend; without it
	// Disk.ReadIf compares against a Stat.
	ConditionalRead bool
	Tags            bool // implements Tagger: key-value tags stored with an object
	Hold            bool // implements Holder: legal holds and retention dates
	Versions        bool // implements VersionLister: an object's stored versions
	// VersionRead: implements VersionReader, reading one stored version and
	// making it the current content again. A separate bit from Versions, so
	// a backend that can only list versions stays honest. Needs Versions,
	// which is where version IDs come from.
	VersionRead  bool
	StorageClass bool // implements StorageClasser: change an object's storage class
	Restore      bool // implements Restorer: bring an archived object back online

	// Attribute capabilities, with no interface of their own.
	Metadata    bool // stores WriteOptions.Metadata and returns it from Stat
	ContentType bool // stores WriteOptions.ContentType and returns it from Stat
	Headers     bool // stores WriteOptions.Headers and returns them from Stat
	// PresignHeaders: the driver's presigned PUT (Presign) pins
	// PresignOptions.Headers, so the backend refuses an upload without them
	// and stores them. Needs Presign and Headers.
	PresignHeaders bool
	// PresignIP: the driver's presigned URLs (Presign) carry
	// PresignOptions.AllowedIP, so the backend refuses a request from an
	// address outside it. Needs Presign.
	PresignIP bool
	// PresignVisibility: the driver's presigned PUT (Presign) and POST
	// form (PresignPost), whichever it declares, pin
	// PresignOptions.Visibility and PostPolicy.Visibility, so the backend
	// stores the upload with that visibility and refuses a request that
	// changes it. Needs SetVisibility and one of Presign or PresignPost;
	// a driver whose backend has no per-object ACL (S3 with bucket owner
	// enforced, GCS with uniform access) does not declare it.
	PresignVisibility bool
	ConditionalWrite  bool // honours WriteOptions.IfNotExists atomically
	// ChecksumSHA256: the driver sends WriteOptions.SHA256 to the backend,
	// which refuses a body that does not match it. The Disk verifies the
	// digest itself on every driver; this adds the backend's own check.
	ChecksumSHA256 bool
	// CompareAndSwap: honours WriteOptions.IfMatch atomically, replacing an
	// object only while its ETag is still the one the caller read.
	CompareAndSwap bool
	// CaseInsensitive: keys differing only in letter case address one
	// object ("A.txt" and "a.txt"). Keys are case-sensitive on every other
	// backend; a driver over a case-insensitive filesystem (macOS, Windows
	// by default) reports it so callers can refuse such pairs themselves.
	CaseInsensitive bool
}

// Copier copies an object inside the backend without streaming it through
// the application. dst is overwritten if present.
type Copier interface {
	Copy(ctx context.Context, src, dst string) (Entry, error)
}

// Mover renames an object inside the backend. dst is overwritten if present;
// src no longer exists afterwards.
type Mover interface {
	Move(ctx context.Context, src, dst string) (Entry, error)
}

// VisibilitySetter changes an existing object's visibility.
type VisibilitySetter interface {
	SetVisibility(ctx context.Context, key string, v Visibility) error
}

// KeysDeleter deletes many keys in as few calls as the backend allows (S3
// deletes 1000 per request). Keys are already cleaned. Deleting a missing
// key succeeds. A partial failure is a *BatchError listing the keys that
// were not deleted, with every other key deleted; any other error means
// the call failed and no key can be assumed deleted.
type KeysDeleter interface {
	DeleteKeys(ctx context.Context, keys []string) error
}

// PrefixDeleter deletes every key under a directory in as few calls as the
// backend allows. dir is clean and never the root: the Disk refuses to
// delete a whole disk through DeletePrefix.
type PrefixDeleter interface {
	DeletePrefix(ctx context.Context, dir string) error
}

// Presigner issues URLs signed by the backend itself (S3 SigV4, GCS V4).
// Options arrive validated by the Disk.
type Presigner interface {
	Presign(ctx context.Context, key string, opts PresignOptions) (SignedRequest, error)
}

// PublicURLer returns the backend's own public URL for a key. A Disk
// configured with WithPublicURL ignores it in favour of the configured base
// (typically a CDN).
type PublicURLer interface {
	PublicURL(ctx context.Context, key string) (string, error)
}

// Part identifies one uploaded part of a multipart upload. Number starts at
// 1; ETag is what the backend returned for the part and must be passed back
// unchanged to CompleteMultipart.
type Part struct {
	Number int
	ETag   string
	Size   int64
}

// MultipartUploader uploads large objects in parts. Parts uploaded through
// the server use UploadPart; a driver that can also sign part uploads for
// clients implements PartPresigner, which together with this interface is
// the contract browser uploaders such as Uppy's S3 multipart plugin expect.
type MultipartUploader interface {
	CreateMultipart(ctx context.Context, key string, opts WriteOptions) (uploadID string, err error)
	UploadPart(ctx context.Context, key, uploadID string, number int, body io.Reader, size int64) (Part, error)
	ListParts(ctx context.Context, key, uploadID string) ([]Part, error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) (Entry, error)
	AbortMultipart(ctx context.Context, key, uploadID string) error
}

// PartPresigner signs a PUT for one part of a multipart upload so a client
// can send it straight to the backend.
type PartPresigner interface {
	PresignPart(ctx context.Context, key, uploadID string, number int, expires time.Duration) (SignedRequest, error)
}

// AbandonedSweeper removes what unfinished work left behind in the backend:
// multipart uploads that were never completed or aborted, and any staging
// files of writes that were interrupted. Those cost storage and are
// invisible to List, so nothing else ever removes them.
//
// Work counts as abandoned when its last activity (a part received, a
// staging file written) is before before; a backend that records only when
// an upload started uses that. Work at or after before is left alone, so a
// caller must choose before well beyond the longest upload it allows; a
// sweep with a recent before aborts uploads in progress. Stored objects are
// never touched. It returns how many uploads and staging files it removed;
// a failure part-way returns the count so far with the error, and a rerun
// continues.
type AbandonedSweeper interface {
	SweepAbandoned(ctx context.Context, before time.Time) (int, error)
}

// AbandonedKind names what kind of leftover an Abandoned is.
type AbandonedKind string

// The closed set of leftovers a sweep removes.
const (
	// AbandonedMultipart is a multipart upload never completed or aborted;
	// sweeping it aborts it and frees its parts.
	AbandonedMultipart AbandonedKind = "multipart"
	// AbandonedStaging is a staging file of a write interrupted before
	// it was renamed into place (local, sftp, ftp, webdav).
	AbandonedStaging AbandonedKind = "staging"
)

// AbandonedKindValues lists every AbandonedKind.
var AbandonedKindValues = []AbandonedKind{AbandonedMultipart, AbandonedStaging}

// Abandoned is one leftover a sweep would remove, as FindAbandoned reports
// it.
type Abandoned struct {
	Kind AbandonedKind
	// ID is the multipart upload ID, or the staging file's name inside
	// the driver's staging directory.
	ID string
	// Key is the key a multipart upload was started for, as the backend
	// driver received it (a scoped disk's prefix included). It is empty
	// for a staging file, and for an upload whose record no longer names
	// its key.
	Key string
	// LastActivity is the time the sweep compares with its cutoff: the
	// last part received, or, where the backend records only that, when
	// the upload started (S3, Azure); a staging file's last write.
	LastActivity time.Time
}

// AbandonedFinder is the dry run of AbandonedSweeper: FindAbandoned
// returns what SweepAbandoned(ctx, before) would remove at this moment,
// applying exactly the same rules (the same cutoff, the same names it
// leaves alone), and changes nothing. Work that starts, completes or is
// aborted between the two calls makes them differ, so a sweep's count can
// be lower or higher than the preview's length. A driver implements both
// from one scan, so the two cannot drift apart.
type AbandonedFinder interface {
	FindAbandoned(ctx context.Context, before time.Time) ([]Abandoned, error)
}

// DirMaker is implemented by backends with real directories (local disk,
// SFTP, FTP, WebDAV). Directories arrive cleaned and never empty (the
// root always exists).
//
// MakeDir creates dir and any missing parents, and succeeds when dir
// already exists; when an object already has dir's key it fails with
// CodeExists. DirExists reports whether dir exists as a directory, empty
// or not; an object at dir's key is not a directory.
type DirMaker interface {
	MakeDir(ctx context.Context, dir string) error
	DirExists(ctx context.Context, dir string) (bool, error)
}

// Unwrapper is implemented by wrapper drivers (scoped, mirror) so that the
// typed escape hatch As can reach the backend driver underneath.
type Unwrapper interface {
	Unwrap() Driver
}

// KeyMapper is implemented by wrapper drivers that change keys on the way
// to the driver they wrap (a scoped disk adds its prefix). Disk.DriverKey
// follows it, so a backend-specific option bound to one key (an S3
// request option, say) can name the key the backend really receives.
type KeyMapper interface {
	InnerKey(key string) string
}

// verifyCapabilities refuses a driver whose declared interface-backed
// capabilities disagree with the interfaces it implements, in the direction
// that would make the Disk call a method that does not exist. (Implementing
// an interface while declaring it false is allowed: wrapper drivers do that.)
func verifyCapabilities(d Driver) error {
	c := d.Capabilities()
	checks := []struct {
		name     string
		declared bool
		has      bool
	}{
		{"Copy", c.Copy, implements[Copier](d)},
		{"Move", c.Move, implements[Mover](d)},
		{"SetVisibility", c.SetVisibility, implements[VisibilitySetter](d)},
		{"DeletePrefix", c.DeletePrefix, implements[PrefixDeleter](d)},
		{"DeleteKeys", c.DeleteKeys, implements[KeysDeleter](d)},
		{"Presign", c.Presign, implements[Presigner](d)},
		{"PublicURL", c.PublicURL, implements[PublicURLer](d)},
		{"Multipart", c.Multipart, implements[MultipartUploader](d)},
		{"PresignPart", c.PresignPart, implements[PartPresigner](d)},
		{"PresignPost", c.PresignPost, implements[PostPresigner](d)},
		{"SweepAbandoned", c.SweepAbandoned, implements[AbandonedSweeper](d)},
		{"FindAbandoned", c.FindAbandoned, implements[AbandonedFinder](d)},
		{"Dirs", c.Dirs, implements[DirMaker](d)},
		{"Append", c.Append, implements[Appender](d)},
		{"SetModTime", c.SetModTime, implements[ModTimeSetter](d)},
		{"ConditionalRead", c.ConditionalRead, implements[ConditionalReader](d)},
		{"Tags", c.Tags, implements[Tagger](d)},
		{"Hold", c.Hold, implements[Holder](d)},
		{"Versions", c.Versions, implements[VersionLister](d)},
		{"VersionRead", c.VersionRead, implements[VersionReader](d)},
		{"StorageClass", c.StorageClass, implements[StorageClasser](d)},
		{"Restore", c.Restore, implements[Restorer](d)},
	}
	if c.PresignPart && !c.Multipart {
		return Errorf(CodeInvalidArgument, "driver %T declares PresignPart without Multipart", d)
	}
	if c.VersionRead && !c.Versions {
		return Errorf(CodeInvalidArgument, "driver %T declares VersionRead without Versions", d)
	}
	if c.FindAbandoned && !c.SweepAbandoned {
		return Errorf(CodeInvalidArgument, "driver %T declares FindAbandoned without SweepAbandoned", d)
	}
	if c.PresignHeaders && (!c.Presign || !c.Headers) {
		return Errorf(CodeInvalidArgument, "driver %T declares PresignHeaders without Presign and Headers", d)
	}
	if c.PresignIP && !c.Presign {
		return Errorf(CodeInvalidArgument, "driver %T declares PresignIP without Presign", d)
	}
	if c.PresignVisibility && (!c.SetVisibility || (!c.Presign && !c.PresignPost)) {
		return Errorf(CodeInvalidArgument, "driver %T declares PresignVisibility without SetVisibility and one of Presign or PresignPost", d)
	}
	for _, ch := range checks {
		if ch.declared && !ch.has {
			return Errorf(CodeInvalidArgument, "driver %T declares capability %s but does not implement its interface", d, ch.name)
		}
	}
	return nil
}

func implements[I any](d Driver) bool {
	_, ok := d.(I)
	return ok
}

// String renders the true capabilities, for logs and diagnostics.
func (c Capabilities) String() string {
	names := []struct {
		name string
		on   bool
	}{
		{"copy", c.Copy}, {"move", c.Move}, {"set-visibility", c.SetVisibility},
		{"delete-prefix", c.DeletePrefix}, {"delete-keys", c.DeleteKeys}, {"presign", c.Presign}, {"public-url", c.PublicURL},
		{"multipart", c.Multipart}, {"presign-part", c.PresignPart}, {"presign-post", c.PresignPost}, {"sweep-abandoned", c.SweepAbandoned}, {"find-abandoned", c.FindAbandoned}, {"dirs", c.Dirs}, {"metadata", c.Metadata}, {"content-type", c.ContentType}, {"headers", c.Headers}, {"presign-headers", c.PresignHeaders}, {"presign-ip", c.PresignIP}, {"presign-visibility", c.PresignVisibility},
		{"conditional-write", c.ConditionalWrite}, {"checksum-sha256", c.ChecksumSHA256},
		{"append", c.Append}, {"set-mod-time", c.SetModTime}, {"conditional-read", c.ConditionalRead},
		{"tags", c.Tags}, {"hold", c.Hold}, {"versions", c.Versions}, {"version-read", c.VersionRead}, {"storage-class", c.StorageClass}, {"restore", c.Restore},
		{"compare-and-swap", c.CompareAndSwap}, {"case-insensitive", c.CaseInsensitive},
	}
	out := ""
	for _, n := range names {
		if n.on {
			if out != "" {
				out += ","
			}
			out += n.name
		}
	}
	return fmt.Sprintf("[%s]", out)
}
