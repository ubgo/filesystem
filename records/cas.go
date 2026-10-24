package records

import (
	"context"
	"errors"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/sqlb"
)

// UploadOption changes how Upload records a file. Without any, Upload
// stores and records every body as its own object.
type UploadOption func(*uploadOptions)

type uploadOptions struct {
	dedup      bool
	expiresAt  *time.Time
	viewOnce   bool
	quarantine bool
}

// Dedup makes Upload reuse an existing object with the same content: when
// a live file of the same scope, on the same disk and with the same
// visibility, already has the body's SHA-256, the new file is recorded on
// that file's object and the uploaded copy is deleted after the commit.
// The two files then share one object (see References); each keeps its
// own row, name, metadata and attachments, and the object is deleted only
// when the last file naming it is purged. The body is still received and
// hashed in full (the digest is known only at the end), so Dedup saves
// storage, not upload bandwidth. Reuse never crosses scopes, so one
// tenant's upload never reveals or depends on another tenant's object.
func Dedup() UploadOption { return func(o *uploadOptions) { o.dedup = true } }

// Expires makes the uploaded file temporary: NewFile.ExpiresAt.
func Expires(at time.Time) UploadOption { return func(o *uploadOptions) { o.expiresAt = &at } }

// ViewOnce makes the uploaded file expire at its first view:
// NewFile.ViewOnce.
func ViewOnce() UploadOption { return func(o *uploadOptions) { o.viewOnce = true } }

// sameContent returns the key of a live file of scope on disk with
// visibility v whose content has digest sum, locked so it cannot be purged
// before the new reference commits, or "" when there is none. A
// quarantined file's object is never reused: its bytes are unscanned, and
// a failed scan may purge it.
func (s *Store) sameContent(ctx context.Context, scope, disk, sum string, v filesystem.Visibility) (string, error) {
	var key string
	q := sqlb.NewSelect(s.dialect).Columns(sqlb.Bind(FileKey, &key)).From(TableFiles).
		Where(sqlb.Eq(FileScope, scope), sqlb.Eq(FileSHA256, sum), sqlb.Eq(FileDisk, disk), sqlb.Eq(FileVisibility, v), sqlb.IsNull(FileDeletedAt), sqlb.NotEq(FileStatus, StatusQuarantined)).
		Order(sqlb.OrderBy(FileCreatedAt, sqlb.SortAsc), sqlb.OrderBy(FileID, sqlb.SortAsc)).Limit(1)
	err := s.oneLocked(ctx, q, lockShare)
	if errors.Is(err, sqlb.ErrNoRows) {
		return "", nil
	}
	return key, err
}
