package records

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// NewFile describes an already-stored object to record.
type NewFile struct {
	Scope string
	// Disk names a disk of the Store's manager; Key is the object on it.
	Disk string
	Key  string
	// Filename is the display name (use upload.SafeFilename on client
	// input).
	Filename string
	// ContentType, Size and Visibility default to what the disk reports.
	ContentType string
	Size        int64
	Visibility  filesystem.Visibility
	// SHA256 is the hex digest, when known (upload.Result has it): 64 hex
	// digits in either case, stored lowercase, so FileBySHA256 finds it.
	SHA256   string
	Metadata map[string]string
	// UploadedBy overrides the actor in the context (WithActor).
	UploadedBy string
	// Status is the file's processing status at creation: "" is
	// StatusReady; StatusProcessing for a file whose processing is about
	// to start (see SetStatus); StatusQuarantined for a file unusable
	// until its scan passes (see PassScan), which must not be public.
	// StatusDetail is its note.
	Status       Status
	StatusDetail string
	// Source overrides the source in the context (WithSource); its IP is
	// kept only with WithSourceIP.
	Source Source
	// ExpiresAt makes the file temporary: SweepExpired purges it once the
	// time has passed. ViewOnce makes it expire at its first view (see
	// Viewed). Both are for ephemeral media; leave them zero otherwise.
	ExpiresAt *time.Time
	ViewOnce  bool
	// Checksums are extra digests of the object beside SHA256, by
	// algorithm name (upload.Result.Checksums has them): names of 1 to 32
	// lowercase letters, digits or dashes, values hex of 1 to
	// MaxChecksumBytes bytes in either case, stored lowercase, at most
	// MaxChecksums of them. The Store records them as given; it does not
	// read the object to check them.
	Checksums map[string]string
}

// CreateFile records an object that already exists on its disk. It checks
// the object exists (the object-first rule), fills size, content type and
// visibility from the disk when not given, runs the plugins' BeforeCreate
// and AfterCreate hooks and inserts the row, all in one transaction.
func (s *Store) CreateFile(ctx context.Context, in NewFile) (File, error) {
	disk, err := s.disks.Disk(in.Disk)
	if err != nil {
		return File{}, err
	}
	key, err := filesystem.CleanKey(in.Key)
	if err != nil {
		return File{}, err
	}
	sum, err := digest(in.SHA256, "")
	if err != nil {
		return File{}, err
	}
	if in.Status == "" {
		in.Status = StatusReady
	}
	if err := checkStatus(in.Status, in.StatusDetail); err != nil {
		return File{}, err
	}
	if in.Visibility != "" && !in.Visibility.Valid() {
		return File{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: unknown visibility %q", in.Visibility)
	}
	// A visibility the disk cannot give the object would make the row lie
	// about it: refused, as Disk.Write refuses it.
	if in.Visibility != "" && !disk.Capabilities().SetVisibility && in.Visibility != disk.DefaultVisibility() {
		return File{}, filesystem.Errorf(filesystem.CodeUnsupported, "records: disk %q has no per-object visibility; its objects are %s", disk.Name(), disk.DefaultVisibility())
	}
	e, err := disk.Stat(ctx, key)
	if err != nil {
		return File{}, err
	}
	f := File{
		ID:    s.ids(IDPrefixFile),
		Scope: in.Scope,
		Disk:  disk.Name(),
		Key:   key,
		// SafeFilename also bounds the name to 255 bytes, which fits the
		// 255-character filename column on every engine.
		Filename:    upload.SafeFilename(orDefault(in.Filename, key)),
		ContentType: orDefault(in.ContentType, e.ContentType),
		Size:        in.Size,
		SHA256:      orDefault(sum, reported(e.SHA256)),
		Visibility:  in.Visibility,
		Metadata:    in.Metadata,
		CreatedAt:   s.clock(),
		UploadedBy:  orDefault(in.UploadedBy, s.Actor(ctx)),
		Status:      in.Status, StatusDetail: in.StatusDetail,
		Source:    s.sourceOf(ctx, in.Source),
		ExpiresAt: utcPtr(in.ExpiresAt),
		ViewOnce:  in.ViewOnce,
	}
	if f.Checksums, err = checksumsOf(in.Checksums); err != nil {
		return File{}, err
	}
	if err := checkSource(f.Source); err != nil {
		return File{}, err
	}
	if len(f.UploadedBy) > MaxActorBytes {
		return File{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: actor is longer than %d bytes", MaxActorBytes)
	}
	if f.Size == 0 {
		f.Size = e.Size
	}
	if f.Visibility == "" {
		f.Visibility = e.Visibility
	}
	if f.Visibility == "" {
		f.Visibility = disk.DefaultVisibility()
	}
	err = s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		for _, h := range s.hooks() {
			if err := h.BeforeCreate(ctx, s, &f); err != nil {
				return err
			}
		}
		// After the hooks, so text a hook adds is held to the same limits,
		// and a visibility a hook chose cannot expose a quarantined file.
		if err := checkText(f.Metadata); err != nil {
			return err
		}
		if f.Quarantined() && f.Visibility == filesystem.VisibilityPublic {
			return ErrPublicQuarantine
		}
		q := sqlb.NewInsert(s.dialect).Into(TableFiles).Row(
			sqlb.Set(FileID, f.ID), sqlb.Set(FileScope, f.Scope), sqlb.Set(FileDisk, f.Disk), sqlb.Set(FileKey, f.Key),
			sqlb.Set(FileFilename, f.Filename), sqlb.Set(FileContentType, f.ContentType), sqlb.Set(FileSize, f.Size),
			sqlb.Set(FileSHA256, f.SHA256), sqlb.Set(FileVisibility, f.Visibility), sqlb.Set(FileMetadata, encodeMetadata(f.Metadata)),
			sqlb.Set(FileCreatedAt, f.CreatedAt), sqlb.Set(FileUploadedBy, f.UploadedBy),
			sqlb.Set(FileStatus, f.Status), sqlb.Set(FileStatusDetail, f.StatusDetail),
			sqlb.Set(FileSourceChannel, f.Source.Channel), sqlb.Set(FileSourceClient, f.Source.Client), sqlb.Set(FileSourceIP, f.Source.IP),
			sqlb.Set(FileExpiresAt, f.ExpiresAt), sqlb.Set(FileViewOnce, viewOnceState(f.ViewOnce)),
			sqlb.Set(FileChecksums, encodeChecksums(f.Checksums)),
		)
		if _, err := s.Exec(ctx, q); err != nil {
			if IsUniqueViolation(err) {
				return filesystem.NewError(filesystem.CodeExists, err)
			}
			return err
		}
		for _, h := range s.hooks() {
			if err := h.AfterCreate(ctx, s, f); err != nil {
				return err
			}
		}
		return s.emit(ctx, Event{Kind: EventFileCreated, Scope: f.Scope, FileID: f.ID, File: fileRef(f)})
	})
	if err != nil {
		return File{}, err
	}
	return f, nil
}

// Upload stores body on the named disk under policy p (see upload.Receive)
// and records it. If the row cannot be inserted, or an enclosing Tx later
// rolls back, the stored object is deleted again, so a failed upload leaves
// neither a row nor bytes. An empty diskName chooses the disk by the
// policy's visibility (WithVisibilityDisks). Options reuse an existing
// object with the same content (Dedup) or make the file temporary
// (Expires, ViewOnce).
func (s *Store) Upload(ctx context.Context, scope, diskName string, body io.Reader, filename string, p upload.Policy, opts ...UploadOption) (File, error) {
	var o uploadOptions
	for _, opt := range opts {
		opt(&o)
	}
	if diskName == "" {
		var err error
		if diskName, err = s.diskFor(p.Visibility); err != nil {
			return File{}, err
		}
	}
	disk, err := s.disks.Disk(diskName)
	if err != nil {
		return File{}, err
	}
	started := s.clock()
	res, err := upload.Receive(ctx, disk, body, filename, p)
	if err != nil {
		return File{}, err
	}
	discard := s.discarder(disk, res.Key, p.Settle != nil)
	if p.Settle != nil {
		// A settled key may be an older object reused as it is (content
		// addressing), possibly an orphan SweepOrphans is about to judge
		// by its age: renew its age before the row that names it commits.
		if err := s.renewIfReused(ctx, disk, res.Key, started); err != nil {
			return File{}, err
		}
	}
	in := NewFile{
		Scope: scope, Disk: disk.Name(), Key: res.Key, Filename: res.Filename,
		ContentType: res.ContentType, Size: res.Size, SHA256: res.SHA256, Visibility: p.Visibility,
		ExpiresAt: o.expiresAt, ViewOnce: o.viewOnce, Checksums: res.Checksums,
	}
	if o.quarantine {
		in.Status = StatusQuarantined
	}
	var f File
	err = s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if o.dedup && res.SHA256 != "" {
			v := p.Visibility
			if v == "" {
				v = disk.DefaultVisibility()
			}
			existing, err := s.sameContent(ctx, scope, disk.Name(), strings.ToLower(res.SHA256), v)
			if err != nil {
				return err
			}
			if existing != "" {
				in.Key = existing
				s.AfterCommit(ctx, discard) // the upload's own copy is not needed
			}
		}
		var err error
		f, err = s.CreateFile(ctx, in)
		return err
	})
	if err != nil {
		discard(ctx)
		return File{}, err
	}
	s.OnRollback(ctx, discard)
	return f, nil
}

// File returns the file with id in scope, including a trashed one.
func (s *Store) File(ctx context.Context, scope, id string) (File, error) {
	var r fileRow
	q := sqlb.NewSelect(s.dialect).Columns(fileBindings(&r)...).From(TableFiles).
		Where(sqlb.Eq(FileScope, scope), sqlb.Eq(FileID, id))
	if err := s.One(ctx, q); err != nil {
		return File{}, notFound(err, "file", id)
	}
	return r.decode(), nil
}

// sha256HexLen is the length of a hex SHA-256 digest, the form File.SHA256
// holds.
const sha256HexLen = 2 * sha256.Size

// digest returns a caller's SHA-256 as stored: lowercase hex, or fallback
// when sum is "". Anything else is CodeInvalidArgument: a digest stored in
// another form would never be found by FileBySHA256.
func digest(sum, fallback string) (string, error) {
	if sum == "" {
		return fallback, nil
	}
	if _, err := hex.DecodeString(sum); err != nil || len(sum) != sha256HexLen {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "records: a SHA-256 is %d hex digits", sha256HexLen)
	}
	return strings.ToLower(sum), nil
}

// reported is a digest a disk reported, as stored: lowercase hex, or ""
// when it is not a hex SHA-256 (a driver must not make a record fail).
func reported(sum string) string {
	s, err := digest(sum, "")
	if err != nil {
		return ""
	}
	return s
}

// FileBySHA256 returns the newest live file of scope whose content has the
// SHA-256 digest sum (hex, either case), or CodeNotFound: the lookup for
// deduplication, so an upload of bytes the scope already holds can reuse
// that file (attach it again) instead of storing a copy. It reads the
// (scope, sha256) index. Trashed files are left out, since they are on
// their way out, and so are files recorded without a digest: a sum that
// is not 64 hex digits is CodeInvalidArgument, never a match for them.
// Every file with the digest, a page at a time, is Files with the filter
// sqlb.Eq(FileSHA256, sum) (lowercase).
func (s *Store) FileBySHA256(ctx context.Context, scope, sum string) (File, error) {
	if sum == "" {
		return File{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: a SHA-256 is %d hex digits", sha256HexLen)
	}
	sum, err := digest(sum, "")
	if err != nil {
		return File{}, err
	}
	page, err := s.Files(ctx, FileQuery{Scope: scope, Limit: 1, Filters: []sqlb.Predicate{sqlb.Eq(FileSHA256, sum)}})
	if err != nil {
		return File{}, err
	}
	if len(page.Files) == 0 {
		return File{}, filesystem.Errorf(filesystem.CodeNotFound, "records: no file with SHA-256 %s", sum)
	}
	return page.Files[0], nil
}

// FileQuery selects a page of files.
type FileQuery struct {
	Scope string
	// Trashed lists only trashed files; otherwise only live ones.
	Trashed bool
	// Limit caps the page; 0 means DefaultPageSize.
	Limit int
	// Cursor continues from FilePage.NextCursor.
	Cursor string
	// Filters are extra conditions on the files table (TableFiles), for
	// plugins that narrow a listing (a folder's files, a tag's files)
	// without re-implementing its order and pagination. A cursor is only
	// valid with the filters it was issued for.
	Filters []sqlb.Predicate
	// Order is the listing's order, OrderNewest ("") by default: by name
	// or by size for a drive's sort menu, or a name the Store added with
	// WithFileOrder. A cursor is only valid with the
	// order it was issued for; one from another order is
	// CodeInvalidArgument. Orders other than the default and OrderOldest
	// are not backed by an index of their own: the scope index narrows
	// the rows and the database sorts them.
	Order FileOrder
}

// FilePage is one page of files, in the query's Order.
type FilePage struct {
	Files      []File
	NextCursor string
}

// DefaultPageSize is the page size when FileQuery.Limit is 0.
const DefaultPageSize = 50

// MaxPageSize caps FileQuery.Limit, so a client cannot ask for every row.
const MaxPageSize = 1000

// Files lists a scope's files newest first (or in fq.Order), with keyset
// pagination on (key, id): stable while files are added, and fast at any
// depth, unlike OFFSET.
func (s *Store) Files(ctx context.Context, fq FileQuery) (FilePage, error) {
	limit, err := pageLimit(fq.Limit)
	if err != nil {
		return FilePage{}, err
	}
	ord, err := s.orderingOf(fq.Order)
	if err != nil {
		return FilePage{}, err
	}
	where := fileWhere(fq)
	if fq.Cursor != "" {
		after, err := decodeOrderCursor(fq.Order, ord, fq.Cursor)
		if err != nil {
			return FilePage{}, err
		}
		where = append(where, after)
	}
	var r fileRow
	cols := fileBindings(&r)
	// keys holds each row's order key as the database computed it, for an
	// Ordering's cursor (WithFileOrder); built-in orders leave it empty.
	var keys []string
	var keyOf func() string
	if ord.key != nil {
		var b sqlb.Binding
		b, keyOf = ord.key()
		cols = append(cols, b)
	}
	q := sqlb.NewSelect(s.dialect).Columns(cols...).From(TableFiles).Where(where...).
		Order(ord.terms...).Limit(limit + 1)
	var page FilePage
	err = s.Each(ctx, q, func() error {
		page.Files = append(page.Files, r.decode())
		if keyOf != nil {
			keys = append(keys, keyOf())
		}
		return nil
	})
	if err != nil {
		return FilePage{}, err
	}
	if len(page.Files) > limit {
		page.Files = page.Files[:limit]
		last := page.Files[limit-1]
		var key string
		if keys != nil {
			key = keys[limit-1]
		}
		page.NextCursor = encodeOrderCursor(fq.Order, ord, last, key)
	}
	return page, nil
}

// cursorSeparator splits a cursor's time from its ID; IDs never contain it.
const cursorSeparator = "|"

// EncodeCursor is the keyset cursor after the row (at, id) of a listing
// ordered by (time, id) descending. It is exported so plugins with their
// own listings (an audit log, share links) paginate the way Files does.
func EncodeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(at.UnixMicro(), 10) + cursorSeparator + id))
}

// DecodeCursor reverses EncodeCursor; anything else is
// CodeInvalidArgument.
func DecodeCursor(c string) (time.Time, string, error) {
	invalid := filesystem.Errorf(filesystem.CodeInvalidArgument, "records: invalid cursor")
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", invalid
	}
	micros, id, ok := strings.Cut(string(raw), cursorSeparator)
	n, err := strconv.ParseInt(micros, 10, 64)
	if !ok || err != nil || id == "" {
		return time.Time{}, "", invalid
	}
	return time.UnixMicro(n).UTC(), id, nil
}

// Trash moves a file to the trash: it stays stored and attached, but
// Attachments and Files no longer list it until Restore. A public file's
// object stops being reachable at its public URL while it is trashed:
// with WithVisibilityDisks it moves to the private disk (the row keeps
// its visibility, so Restore moves it back); otherwise its disk makes it
// private in place where it has the SetVisibility capability (a disk
// without it cannot hide one object, so there the object stays as it
// is). The object changes before the row and is put back if the
// transaction fails, as in SetVisibility. Trashing a trashed file does
// nothing.
// IfUnchanged trashes it only while its content is the one the caller
// read.
func (s *Store) Trash(ctx context.Context, scope, id string, opts ...ChangeOption) error {
	now := s.clock()
	return s.setDeleted(ctx, scope, id, &now, opts)
}

// Restore takes a file out of the trash and, for a public file, makes its
// object public again (see Trash). IfUnchanged restores it only while its
// content is the one the caller read.
func (s *Store) Restore(ctx context.Context, scope, id string, opts ...ChangeOption) error {
	return s.setDeleted(ctx, scope, id, nil, opts)
}

// setDeleted trashes (at set) or restores (at nil) a file. A file already
// in the wanted state is left alone: re-trashing must not restart the
// trash clock SweepTrash purges by.
func (s *Store) setDeleted(ctx context.Context, scope, id string, at *time.Time, opts []ChangeOption) error {
	o := changeOptionsOf(opts)
	if err := errors.Join(o.notForPurgeOnly("Trash or Restore"), o.notForReplaceOnly("Trash or Restore")); err != nil {
		return err
	}
	f, err := s.File(ctx, scope, id)
	if err != nil {
		return err
	}
	if err := o.check(f); err != nil {
		return err
	}
	if (f.DeletedAt == nil) == (at == nil) {
		return nil
	}
	moved, undo, err := s.trashExposure(ctx, f, at != nil)
	if err != nil {
		return err
	}
	return s.recordObjectChange(ctx, f, moved, undo, func(ctx context.Context) error {
		if err := s.recheck(ctx, f, o); err != nil {
			return err
		}
		q := sqlb.NewUpdate(s.dialect).Table(TableFiles).Set(sqlb.Set(FileDeletedAt, at), sqlb.Set(FileDisk, moved.Disk)).
			Where(sqlb.Eq(FileScope, scope), sqlb.Eq(FileID, id))
		if _, err := s.Exec(ctx, q); err != nil {
			return err
		}
		kind := EventFileRestored
		if at != nil {
			kind = EventFileTrashed
		}
		after := f
		after.DeletedAt, after.Disk = at, moved.Disk
		return s.emit(ctx, Event{Kind: kind, Scope: scope, FileID: id, File: fileRef(after)})
	})
}

// Purge deletes a file for good: its attachments and row in one
// transaction (after the plugins' BeforePurge hooks), then its object once
// the transaction commits. Called outside a Tx it returns an object
// deletion failure; inside one the deletion happens at the outer commit and
// a failure is passed to the handler set with WithObjectErrorHandler.
//
// Options narrow the purge to the state the caller read (IfTrashed,
// IfUnchanged), checked under the row lock.
func (s *Store) Purge(ctx context.Context, scope, id string, opts ...ChangeOption) error {
	o := changeOptionsOf(opts)
	if err := o.notForReplaceOnly("Purge"); err != nil {
		return err
	}
	outer := txFrom(ctx) != nil
	var objectErr error
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		// Locked until the commit, so an Attach racing the purge either
		// finishes first (and its attachment is deleted below) or waits
		// and then finds no file.
		f, err := s.lockForPurge(ctx, scope, id)
		if err != nil {
			return err
		}
		if err := o.check(f); err != nil {
			return err
		}
		if err := s.stillMatches(ctx, f, o.stillMatches); err != nil {
			return err
		}
		for _, h := range s.hooks() {
			if err := h.BeforePurge(ctx, s, f); err != nil {
				return err
			}
		}
		delAtt := sqlb.NewDelete(s.dialect).From(TableAttachments).Where(sqlb.Eq(AttachmentScope, scope), sqlb.Eq(AttachmentFileID, id))
		if _, err := s.Exec(ctx, delAtt); err != nil {
			return err
		}
		delFile := sqlb.NewDelete(s.dialect).From(TableFiles).Where(sqlb.Eq(FileScope, scope), sqlb.Eq(FileID, id))
		if _, err := s.Exec(ctx, delFile); err != nil {
			return err
		}
		if err := s.emit(ctx, Event{Kind: EventFilePurged, Scope: scope, FileID: id, File: fileRef(f)}); err != nil {
			return err
		}
		// A shared object stays for the files still naming it (blobs.go).
		if shared, err := s.referenced(ctx, f); err != nil || shared {
			return err
		}
		s.AfterCommit(ctx, func(ctx context.Context) {
			err := s.deleteObject(ctx, f)
			switch {
			case err == nil:
			case outer:
				s.objectErrors(ctx, f, err)
			default:
				objectErr = err
			}
		})
		return nil
	})
	if err != nil {
		return err
	}
	return objectErr
}

// SweepTrash purges every file, in any scope, trashed before cutoff, and
// returns how many it purged. Schedule it (the library starts no background
// work) to give the trash a retention period. Each file is purged in its
// own transaction, so call it outside one. A file that cannot be purged (a
// database failure, or a plugin refusing, such as a legal hold) does not
// stop the sweep: the rest are still purged, and the returned error joins
// one error per failed file, naming it, so one stuck file never holds
// back the others. An object whose deletion fails after its row was
// purged goes to WithObjectErrorHandler, as with any deletion after a
// commit, and is left for SweepOrphans. A cancelled ctx stops the sweep.
// FindTrash lists what a sweep would purge without purging it.
func (s *Store) SweepTrash(ctx context.Context, cutoff time.Time) (int, error) {
	return s.purgeWhere(ctx, trashedBefore(cutoff)...)
}

// FindTrash is SweepTrash's dry run: it calls found with every file, in
// any scope, trashed before cutoff, in ID order, and returns how many it
// found, changing nothing. A real sweep may purge fewer (a plugin can
// refuse a purge, such as a legal hold), never more for the same cutoff.
// found runs after the rows were read, so it may query the Store.
func (s *Store) FindTrash(ctx context.Context, cutoff time.Time, found func(File)) (int, error) {
	var r fileRow
	var due []File
	q := sqlb.NewSelect(s.dialect).Columns(fileBindings(&r)...).From(TableFiles).
		Where(trashedBefore(cutoff)...).Order(sqlb.OrderBy(FileID, sqlb.SortAsc))
	if err := s.Each(ctx, q, func() error { due = append(due, r.decode()); return nil }); err != nil {
		return 0, err
	}
	for _, f := range due {
		found(f)
	}
	return len(due), nil
}

// stillMatches returns errNoLongerDue unless the locked file f still
// meets where; without conditions it does nothing.
func (s *Store) stillMatches(ctx context.Context, f File, where []sqlb.Predicate) error {
	if len(where) == 0 {
		return nil
	}
	var id string
	q := sqlb.NewSelect(s.dialect).Columns(sqlb.Bind(FileID, &id)).From(TableFiles).
		Where(append([]sqlb.Predicate{sqlb.Eq(FileScope, f.Scope), sqlb.Eq(FileID, f.ID)}, where...)...)
	switch err := s.One(ctx, q); {
	case errors.Is(err, sqlb.ErrNoRows):
		return errNoLongerDue
	default:
		return err
	}
}

// trashedBefore selects the files SweepTrash purges for cutoff, so the
// sweep and its dry run (FindTrash) can never disagree.
func trashedBefore(cutoff time.Time) []sqlb.Predicate {
	utcCutoff := cutoff.UTC()
	return []sqlb.Predicate{sqlb.IsNotNull(FileDeletedAt), sqlb.Less(FileDeletedAt, &utcCutoff)}
}

// purgeWhere purges, each in its own transaction, every file of any scope
// matching where, and returns how many it purged: the body of every sweep
// (SweepTrash, SweepExpired). Each file is checked against where again
// with its row locked, so one that stopped matching after the selection
// (restored mid-sweep) or that is already gone is skipped and not
// counted. A file that cannot be purged does not stop the sweep; the
// returned error joins one error per failed file, naming it. A cancelled
// ctx stops it.
func (s *Store) purgeWhere(ctx context.Context, where ...sqlb.Predicate) (int, error) {
	type target struct{ scope, id string }
	var targets []target
	var t target
	q := sqlb.NewSelect(s.dialect).Columns(sqlb.Bind(FileScope, &t.scope), sqlb.Bind(FileID, &t.id)).From(TableFiles).Where(where...)
	if err := s.Each(ctx, q, func() error { targets = append(targets, t); return nil }); err != nil {
		return 0, err
	}
	purged := 0
	var errs []error
	for _, tg := range targets {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		// Inside a Tx, Purge hands a failed object deletion to the handler
		// instead of returning it: the row is gone either way. It
		// re-checks where with the row locked, so a file restored (or
		// whose expiry moved) since the selection is skipped, not purged.
		err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
			return s.Purge(ctx, tg.scope, tg.id, whileMatching(where))
		})
		if errors.Is(err, errNoLongerDue) || filesystem.CodeOf(err) == filesystem.CodeNotFound {
			continue // restored, changed or purged by someone else meanwhile
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("records: purge file %q of scope %q: %w", tg.id, tg.scope, err))
			continue
		}
		purged++
	}
	return purged, errors.Join(errs...)
}

// URL returns a link to the file: its public URL when it is public and the
// disk can build one, otherwise a signed GET valid for ttl. Every link it
// issues is reported with Served (ServeLink unless ServedAs says
// otherwise, Bytes the file's Size) before it is returned, and none is
// returned when that fails, so every route, listing and plugin that hands
// out links is metered without remembering to be. A public URL never
// expires, so for a public file this counts issuance, not fetches.
func (s *Store) URL(ctx context.Context, f File, ttl time.Duration, opts ...LinkOption) (string, error) {
	disk, err := s.disks.Disk(f.Disk)
	if err != nil {
		return "", err
	}
	if f.Visibility == filesystem.VisibilityPublic {
		if u, err := disk.PublicURL(ctx, f.Key); err == nil {
			return s.issued(ctx, f, u, opts)
		}
	}
	req, err := disk.SignedURL(ctx, f.Key, filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: ttl, ContentType: f.ContentType})
	if err != nil {
		return "", err
	}
	return s.issued(ctx, f, req.URL, opts)
}

// DownloadURL returns a signed GET valid for ttl that makes browsers save
// the file under its original name instead of displaying it, reported
// with Served as URL's links are.
func (s *Store) DownloadURL(ctx context.Context, f File, ttl time.Duration, opts ...LinkOption) (string, error) {
	disk, err := s.disks.Disk(f.Disk)
	if err != nil {
		return "", err
	}
	req, err := disk.SignedURL(ctx, f.Key, filesystem.PresignOptions{
		Method: filesystem.MethodGet, Expires: ttl, ContentType: f.ContentType,
		ContentDisposition: AttachmentDisposition(f.Filename),
	})
	if err != nil {
		return "", err
	}
	return s.issued(ctx, f, req.URL, opts)
}

// issued reports link to f as served and returns it, or returns no link
// when the report fails.
func (s *Store) issued(ctx context.Context, f File, link string, opts []LinkOption) (string, error) {
	sv := Served{Kind: ServeLink, Scope: f.Scope, FileID: f.ID, Bytes: f.Size}
	for _, o := range opts {
		o(&sv)
	}
	if err := s.Served(ctx, sv); err != nil {
		return "", err
	}
	return link, nil
}

// AttachmentDisposition renders a Content-Disposition that saves a file as
// name, with an ASCII fallback and the exact UTF-8 name (RFC 6266/5987), so
// non-ASCII names survive every browser.
func AttachmentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return `attachment; filename="` + ascii + `"; filename*=UTF-8''` + url.PathEscape(name)
}

func (r fileRow) decode() File {
	f := r.File
	f.Metadata = decodeMetadata(r.metadata)
	f.ViewOnce = r.viewOnce != 0
	f.Checksums = decodeChecksums(r.checksums)
	return f
}

// Limits of File.Checksums.
const (
	// MaxChecksums caps the digests of one file: generous for every
	// algorithm an application keeps, small enough that the JSON column
	// never grows unbounded.
	MaxChecksums = 16
	// MaxChecksumBytes is the longest digest, in bytes (SHA-512's 64).
	MaxChecksumBytes = 64
)

// checksumNamePattern is filesystem.Checksum's name rule.
var checksumNamePattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// checksumsOf checks extra digests and returns them with lowercase hex,
// nil for none.
func checksumsOf(in map[string]string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxChecksums {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: at most %d checksums per file", MaxChecksums)
	}
	out := make(map[string]string, len(in))
	for name, v := range in {
		sum, err := hex.DecodeString(v)
		if !checksumNamePattern.MatchString(name) || err != nil || len(sum) == 0 || len(sum) > MaxChecksumBytes {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: checksum %q must be named by 1 to 32 lowercase letters, digits or dashes and be hex of 1 to %d bytes", name, MaxChecksumBytes)
		}
		out[name] = hex.EncodeToString(sum)
	}
	return out, nil
}

// encodeChecksums stores checksums as a JSON object, NULL for none.
func encodeChecksums(m map[string]string) *string {
	if len(m) == 0 {
		return nil
	}
	b, _ := json.Marshal(m) // a map of strings cannot fail to marshal
	s := string(b)
	return &s
}

// decodeChecksums reads stored checksums; NULL, and anything that is not
// a JSON object of strings (a hand-edited row), is nil.
func decodeChecksums(s *string) map[string]string {
	if s == nil {
		return nil
	}
	return decodeMetadata(*s)
}

// encodeMetadata stores metadata as JSON; a map of strings cannot fail to
// marshal.
func encodeMetadata(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// decodeMetadata reads stored metadata. Rows written by this package are
// always valid JSON objects; anything else (a hand-edited row) decodes to
// nil rather than failing the read.
func decodeMetadata(s string) map[string]string {
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil || len(m) == 0 {
		return nil
	}
	return m
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// deleteObject deletes f's object from its disk.
func (s *Store) deleteObject(ctx context.Context, f File) error {
	disk, err := s.disks.Disk(f.Disk)
	if err != nil {
		return err
	}
	return disk.Delete(context.WithoutCancel(ctx), f.Key)
}
