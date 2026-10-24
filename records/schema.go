// Package records stores files as database rows and attaches them to any
// model of the application: a user's avatar, a post's gallery, a message's
// attachments. It is the layer above filesystem.Disk.
//
// It is a separate module that requires only github.com/ubgo/sqlb (and the
// core filesystem module), and runs on SQLite, Postgres and MySQL. The
// application owns the *sql.DB and the driver; the library never imports a
// database driver.
//
// Two rules keep bytes and rows consistent, because an object store and a
// database can never share a transaction:
//
//   - Object first, row second. A file is recorded only after its object is
//     stored (CreateFile checks it exists); Upload deletes the object again
//     when the row cannot be inserted.
//   - Row first, object last. Purge deletes the rows inside the transaction
//     and the object only after the transaction commits, so a rollback never
//     leaves a row pointing at deleted bytes.
//
// What can still be left behind (an object whose row insert failed and
// whose cleanup also failed, or one whose deletion after a purge failed)
// is an orphan object, never a row without bytes; Store.SweepOrphans
// removes it, checking every object against the rows (and the plugins
// that own objects) first. Do not point upload.Sweep or an age-only bucket
// lifecycle rule at recorded files: they delete by age, recorded or not.
//
// Reads are open, writes are closed: applications may SELECT and JOIN the
// fs_ tables freely, but must change them only through this package, where
// the invariants live.
package records

import (
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/sqlb"
)

// Table names. Prefixed so they never collide with application tables.
const (
	TableNameFiles       = "fs_files"
	TableNameAttachments = "fs_attachments"
	TableNameMigrations  = "fs_migrations"
)

// Column names of fs_files.
const (
	ColFileID          = "id"
	ColFileScope       = "scope"
	ColFileDisk        = "disk"
	ColFileKey         = "storage_key"
	ColFileFilename    = "filename"
	ColFileContentType = "content_type"
	ColFileSize        = "size"
	ColFileSHA256      = "sha256"
	ColFileVisibility  = "visibility"
	ColFileMetadata    = "metadata"
	ColFileCreatedAt   = "created_at"
	ColFileDeletedAt   = "deleted_at"
	ColFileUploadedBy  = "uploaded_by"
	// ColFileStatus and ColFileStatusDetail hold the processing status
	// (core migration 0003).
	ColFileStatus       = "status"
	ColFileStatusDetail = "status_detail"
	// ColFileSourceChannel, ColFileSourceClient and ColFileSourceIP hold
	// File.Source (core migration 0005).
	ColFileSourceChannel = "source_channel"
	ColFileSourceClient  = "source_client"
	ColFileSourceIP      = "source_ip"
	// ColFileExpiresAt and ColFileViewOnce hold File.ExpiresAt and
	// File.ViewOnce (core migration 0006).
	ColFileExpiresAt = "expires_at"
	ColFileViewOnce  = "view_once"
	// ColFileChecksums holds File.Checksums as a JSON object (core
	// migration 0007); NULL for a file without extra digests.
	ColFileChecksums = "checksums"
)

// Column names of fs_attachments.
const (
	ColAttachmentID        = "id"
	ColAttachmentScope     = "scope"
	ColAttachmentFileID    = "file_id"
	ColAttachmentOwnerType = "owner_type"
	ColAttachmentOwnerID   = "owner_id"
	ColAttachmentRole      = "role"
	ColAttachmentPosition  = "position"
	ColAttachmentCreatedAt = "created_at"
)

// Tables, with aliases used in joins.
var (
	TableFiles       = sqlb.Table{Name: TableNameFiles, Alias: "f"}
	TableAttachments = sqlb.Table{Name: TableNameAttachments, Alias: "a"}
)

// Typed columns of fs_files. Nullable columns are pointers, so null is
// visible in the type system.
var (
	FileID          = sqlb.Col[string](TableFiles, ColFileID)
	FileScope       = sqlb.Col[string](TableFiles, ColFileScope)
	FileDisk        = sqlb.Col[string](TableFiles, ColFileDisk)
	FileKey         = sqlb.Col[string](TableFiles, ColFileKey)
	FileFilename    = sqlb.Col[string](TableFiles, ColFileFilename)
	FileContentType = sqlb.Col[string](TableFiles, ColFileContentType)
	FileSize        = sqlb.Col[int64](TableFiles, ColFileSize)
	FileSHA256      = sqlb.Col[string](TableFiles, ColFileSHA256)
	FileVisibility  = sqlb.Col[filesystem.Visibility](TableFiles, ColFileVisibility)
	FileMetadata    = sqlb.Col[string](TableFiles, ColFileMetadata)
	FileCreatedAt   = sqlb.Col[time.Time](TableFiles, ColFileCreatedAt)
	// FileDeletedAt is nil for a live file and set when it is trashed.
	FileDeletedAt = sqlb.Col[*time.Time](TableFiles, ColFileDeletedAt)
	// FileUploadedBy is "" when no actor was known.
	FileUploadedBy = sqlb.Col[string](TableFiles, ColFileUploadedBy)
	// FileStatus is the processing status; filter with it to list the
	// files still processing or failed.
	FileStatus       = sqlb.Col[Status](TableFiles, ColFileStatus)
	FileStatusDetail = sqlb.Col[string](TableFiles, ColFileStatusDetail)
	// FileSourceChannel, FileSourceClient and FileSourceIP are where the
	// file came from (File.Source); "" when not known.
	FileSourceChannel = sqlb.Col[string](TableFiles, ColFileSourceChannel)
	FileSourceClient  = sqlb.Col[string](TableFiles, ColFileSourceClient)
	FileSourceIP      = sqlb.Col[string](TableFiles, ColFileSourceIP)
	// FileExpiresAt is nil for a file that never expires (SweepExpired).
	FileExpiresAt = sqlb.Col[*time.Time](TableFiles, ColFileExpiresAt)
	// FileViewOnce is 0 for an ordinary file, viewOnceUnviewed for a
	// view-once file not yet viewed and viewOnceViewed once Viewed served
	// it (an integer, so every engine stores it the same way).
	FileViewOnce = sqlb.Col[int64](TableFiles, ColFileViewOnce)
	// FileChecksums is File.Checksums as JSON text, nil for none.
	FileChecksums = sqlb.Col[*string](TableFiles, ColFileChecksums)
)

// Typed columns of fs_attachments.
var (
	AttachmentID        = sqlb.Col[string](TableAttachments, ColAttachmentID)
	AttachmentScope     = sqlb.Col[string](TableAttachments, ColAttachmentScope)
	AttachmentFileID    = sqlb.Col[string](TableAttachments, ColAttachmentFileID)
	AttachmentOwnerType = sqlb.Col[string](TableAttachments, ColAttachmentOwnerType)
	AttachmentOwnerID   = sqlb.Col[string](TableAttachments, ColAttachmentOwnerID)
	AttachmentRole      = sqlb.Col[string](TableAttachments, ColAttachmentRole)
	AttachmentPosition  = sqlb.Col[int64](TableAttachments, ColAttachmentPosition)
	AttachmentCreatedAt = sqlb.Col[time.Time](TableAttachments, ColAttachmentCreatedAt)
)

// File is one stored file. Its bytes live on Disk under Key; the row holds
// what the application needs without touching the disk.
type File struct {
	ID string
	// Scope is the tenant, workspace or user the file belongs to, opaque to
	// the library ("" for an application without tenants). Every query
	// filters by it.
	Scope       string
	Disk        string
	Key         string
	Filename    string
	ContentType string
	Size        int64
	SHA256      string
	Visibility  filesystem.Visibility
	Metadata    map[string]string
	CreatedAt   time.Time
	// DeletedAt is set while the file is in the trash; nil otherwise.
	DeletedAt *time.Time
	// UploadedBy is who recorded the file: NewFile.UploadedBy, else the
	// actor in the context (WithActor), else "". A replacement keeps it.
	UploadedBy string
	// Status is where the file's processing stands (StatusReady unless a
	// plugin or the application said otherwise, see SetStatus), and
	// StatusDetail the reason or progress note that came with it.
	Status       Status
	StatusDetail string
	// Source is where the file came from (see WithSource); zero when not
	// known. Source.IP is kept only with WithSourceIP.
	Source Source
	// ExpiresAt, when set, is when SweepExpired purges the file; a
	// view-once file gets it at its first view (Viewed).
	ExpiresAt *time.Time
	// ViewOnce marks a file to be served once: Viewed reports the first
	// view and expires the file at that moment.
	ViewOnce bool
	// Checksums are the extra digests of the content beside SHA256, by
	// algorithm name ("md5", "crc32c"), lowercase hex: what
	// upload.Policy.Checksums asked for (Upload and Replace keep
	// upload.Result.Checksums), or NewFile.Checksums. nil when none was
	// asked for. A replacement sets the new content's, so they never
	// describe bytes the file no longer has.
	Checksums map[string]string
}

// Owner identifies the application row a file is attached to: a type the
// application registered (WithOwnerTypes) and that row's ID.
type Owner struct {
	Type string
	ID   string
}

// Attachment links a file to an owner under a role ("avatar", "gallery").
// Position orders the attachments of one owner and role, from 1.
type Attachment struct {
	ID        string
	Scope     string
	FileID    string
	Owner     Owner
	Role      string
	Position  int64
	CreatedAt time.Time
}

// fileRow is File as stored: metadata and checksums as JSON text, the
// view-once flag as an integer.
type fileRow struct {
	File
	metadata  string
	viewOnce  int64
	checksums *string
}

func fileBindings(dst *fileRow) []sqlb.Binding {
	return []sqlb.Binding{
		sqlb.Bind(FileID, &dst.ID),
		sqlb.Bind(FileScope, &dst.Scope),
		sqlb.Bind(FileDisk, &dst.Disk),
		sqlb.Bind(FileKey, &dst.Key),
		sqlb.Bind(FileFilename, &dst.Filename),
		sqlb.Bind(FileContentType, &dst.ContentType),
		sqlb.Bind(FileSize, &dst.Size),
		sqlb.Bind(FileSHA256, &dst.SHA256),
		sqlb.Bind(FileVisibility, &dst.Visibility),
		sqlb.Bind(FileMetadata, &dst.metadata),
		sqlb.Bind(FileCreatedAt, &dst.CreatedAt),
		sqlb.Bind(FileDeletedAt, &dst.DeletedAt),
		sqlb.Bind(FileUploadedBy, &dst.UploadedBy),
		sqlb.Bind(FileStatus, &dst.Status),
		sqlb.Bind(FileStatusDetail, &dst.StatusDetail),
		sqlb.Bind(FileSourceChannel, &dst.Source.Channel),
		sqlb.Bind(FileSourceClient, &dst.Source.Client),
		sqlb.Bind(FileSourceIP, &dst.Source.IP),
		sqlb.Bind(FileExpiresAt, &dst.ExpiresAt),
		sqlb.Bind(FileViewOnce, &dst.viewOnce),
		sqlb.Bind(FileChecksums, &dst.checksums),
	}
}

func attachmentBindings(dst *Attachment) []sqlb.Binding {
	return []sqlb.Binding{
		sqlb.Bind(AttachmentID, &dst.ID),
		sqlb.Bind(AttachmentScope, &dst.Scope),
		sqlb.Bind(AttachmentFileID, &dst.FileID),
		sqlb.Bind(AttachmentOwnerType, &dst.Owner.Type),
		sqlb.Bind(AttachmentOwnerID, &dst.Owner.ID),
		sqlb.Bind(AttachmentRole, &dst.Role),
		sqlb.Bind(AttachmentPosition, &dst.Position),
		sqlb.Bind(AttachmentCreatedAt, &dst.CreatedAt),
	}
}
