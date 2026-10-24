// Package versions is a records plugin that keeps the previous contents of
// a file when it is replaced (records.Store.Replace), lists them, and
// restores one: the version history of document managers and drives.
//
// A kept version is the object the file pointed to before, so keeping it
// costs no copy. Each file keeps at most a set number of versions (oldest
// dropped first); a dropped version's object is deleted after its
// transaction commits, and a purged file takes its versions with it.
// Restoring a version copies its object and makes the copy the file's
// content, so the current content becomes a version in turn and history
// is never rewritten.
package versions

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// PluginName is the plugin's name in the migration ledger.
const PluginName = "versions"

// TableNameVersions is the table the plugin owns.
const TableNameVersions = "fs_file_versions"

// Column names of fs_file_versions.
const (
	ColID          = "id"
	ColScope       = "scope"
	ColFileID      = "file_id"
	ColNumber      = "number"
	ColDisk        = "disk"
	ColKey         = "object_key"
	ColFilename    = "filename"
	ColContentType = "content_type"
	ColSize        = "size"
	ColSHA256      = "sha256"
	ColReplacedAt  = "replaced_at"
)

// Table and typed columns.
var (
	Table       = sqlb.Table{Name: TableNameVersions, Alias: "fv"}
	ID          = sqlb.Col[string](Table, ColID)
	Scope       = sqlb.Col[string](Table, ColScope)
	FileID      = sqlb.Col[string](Table, ColFileID)
	Number      = sqlb.Col[int64](Table, ColNumber)
	Disk        = sqlb.Col[string](Table, ColDisk)
	Key         = sqlb.Col[string](Table, ColKey)
	Filename    = sqlb.Col[string](Table, ColFilename)
	ContentType = sqlb.Col[string](Table, ColContentType)
	Size        = sqlb.Col[int64](Table, ColSize)
	SHA256      = sqlb.Col[string](Table, ColSHA256)
	ReplacedAt  = sqlb.Col[time.Time](Table, ColReplacedAt)
)

// Limits.
const (
	// IDPrefix starts every version ID.
	IDPrefix = "ver_"
	// DefaultKeep is how many versions a file keeps without WithKeep.
	DefaultKeep = 10
	// restoreDir is where restored copies are stored, beside the disk's
	// other keys.
	restoreDir = "versions/"
)

// Version is a previous content of a file. Number counts up from 1 per
// file; ReplacedAt is when it stopped being the current content.
type Version struct {
	ID          string
	Scope       string
	FileID      string
	Number      int64
	Disk        string
	Key         string
	Filename    string
	ContentType string
	Size        int64
	SHA256      string
	ReplacedAt  time.Time
}

func bindings(v *Version) []sqlb.Binding {
	return []sqlb.Binding{
		sqlb.Bind(ID, &v.ID), sqlb.Bind(Scope, &v.Scope), sqlb.Bind(FileID, &v.FileID), sqlb.Bind(Number, &v.Number),
		sqlb.Bind(Disk, &v.Disk), sqlb.Bind(Key, &v.Key), sqlb.Bind(Filename, &v.Filename), sqlb.Bind(ContentType, &v.ContentType),
		sqlb.Bind(Size, &v.Size), sqlb.Bind(SHA256, &v.SHA256), sqlb.Bind(ReplacedAt, &v.ReplacedAt),
	}
}

// held describes a version's object to records.Store.Hold and Release,
// so a usage plugin (quota with WithHeldObjects) counts kept versions.
func (v Version) held() records.HeldObject {
	return records.HeldObject{Plugin: PluginName, Scope: v.Scope, FileID: v.FileID, Disk: v.Disk, Key: v.Key, Size: v.Size}
}

// object describes a version's object as a records.File, which is what
// Store.DeleteObjectAfterCommit and its error handler take.
func (v Version) object() records.File {
	return records.File{ID: v.FileID, Scope: v.Scope, Disk: v.Disk, Key: v.Key, Filename: v.Filename}
}

// Plugin keeps versions. Construct it with New and install it with
// records.WithPlugins.
type Plugin struct {
	keep int
	// keepFunc is WithKeepFunc; nil keeps keep versions of every file.
	keepFunc KeepFunc
	// restoreKey is WithRestoreKey; nil stores restored copies under
	// restoreDir with a random name.
	restoreKey RestoreKeyFunc
	now        func() time.Time
}

// Option configures a Plugin.
type Option func(*Plugin)

// WithKeep sets how many versions each file keeps (at least 1).
func WithKeep(n int) Option { return func(p *Plugin) { p.keep = n } }

// KeepPolicy is how much history one file keeps.
type KeepPolicy struct {
	// Count is the most versions the file keeps, at least 1; the oldest
	// past it are dropped.
	Count int
	// MaxAge drops versions that stopped being current longer ago than
	// this; 0 keeps them whatever their age. It is applied when the file
	// is next replaced, and by Trim.
	MaxAge time.Duration
}

// KeepRequest is what a KeepFunc decides for. It is a struct so fields
// can be added without breaking implementations.
type KeepRequest struct {
	// File is the file whose history is being trimmed, as it is now (in
	// KeepVersion, as it was before the replacement).
	File records.File
}

// KeepFunc returns the history policy of one file: from its scope's plan
// ("30 days of versions on Pro"), its content type, or its owner.
type KeepFunc func(ctx context.Context, req KeepRequest) (KeepPolicy, error)

// WithKeepFunc decides each file's history with fn, asked on every
// replacement (and by Trim), instead of WithKeep's one count for every
// file. It fails closed: an error from fn, or a policy with a Count below
// 1 or a negative MaxAge, refuses the replacement (rolling it back) rather
// than losing history or keeping it unbounded. It replaces WithKeep.
func WithKeepFunc(fn KeepFunc) Option { return func(p *Plugin) { p.keepFunc = fn } }

// RestoreKeyRequest is what a RestoreKeyFunc names a key for.
type RestoreKeyRequest struct {
	// File is the file being restored, as it is now; its Disk is where
	// the copy goes.
	File records.File
	// Version is the version whose content is copied.
	Version Version
}

// RestoreKeyFunc names the key a restored copy is stored under, on the
// file's own disk. Every call must name a new key: one that already holds
// an object is refused with CodeExists rather than overwritten.
type RestoreKeyFunc func(ctx context.Context, req RestoreKeyRequest) (string, error)

// WithRestoreKey names the key of every restored copy with fn instead of
// "versions/" and a random name at the disk's root, so the copy lands
// under the prefix the file's tenant owns (prefix-based IAM, per-tenant
// KMS or lifecycle rules, per-tenant prefix deletes). RestoreBesideFile
// is a ready one. It fails closed: an error from fn, an empty key, or a
// key that is the file's or the version's own or already holds an object
// refuses the restore before anything is written.
func WithRestoreKey(fn RestoreKeyFunc) Option { return func(p *Plugin) { p.restoreKey = fn } }

// RestoreBesideFile is a RestoreKeyFunc that stores a restored copy in a
// "versions" directory beside the file's current object, with a random
// name: a file under "tenants/t1/docs/a.pdf" is restored to
// "tenants/t1/docs/versions/<random>", inside the file's own prefix.
func RestoreBesideFile(_ context.Context, req RestoreKeyRequest) (string, error) {
	dir := path.Dir(req.File.Key)
	if dir == "." {
		dir = ""
	} else {
		dir += "/"
	}
	return dir + randomRestoreKey(), nil
}

// randomRestoreKey is the default restored copy's key: restoreDir and 128
// random bits, which no two restores share.
func randomRestoreKey() string { return restoreDir + strings.ToLower(rand.Text()) }

// WithClock gives this plugin its own clock, for tests. Without it the
// plugin reads records.Store.Now, so records.WithClock rules the plugin
// and the Store alike; times are kept in UTC at microsecond precision
// either way.
func WithClock(now func() time.Time) Option { return func(p *Plugin) { p.now = now } }

// New returns a Plugin.
func New(opts ...Option) (*Plugin, error) {
	p := &Plugin{keep: DefaultKeep}
	for _, o := range opts {
		o(p)
	}
	if p.keepFunc == nil && p.keep < 1 {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "versions: a file must keep at least 1 version")
	}
	return p, nil
}

// clock is WithClock's time when set, else the Store's (records.WithClock),
// in UTC at microsecond precision, the precision every engine keeps.
func (p *Plugin) clock(s *records.Store) time.Time {
	if p.now == nil {
		return s.Now()
	}
	return p.now().UTC().Truncate(time.Microsecond)
}

// Name implements records.Plugin.
func (p *Plugin) Name() string { return PluginName }

// Migrations implements records.Migrator.
func (p *Plugin) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	table, err1 := records.NewDDL(engine).CreateTable(records.Table{
		Table: Table,
		Columns: []ddl.ColumnSpec{
			records.Column(ID, t.Key), records.Column(Scope, t.Key), records.Column(FileID, t.Key), records.Column(Number, t.Int),
			records.Column(Disk, t.Short), records.Column(Key, t.Long), records.Column(Filename, t.Text), records.Column(ContentType, t.Text),
			records.Column(Size, t.Int), records.Column(SHA256, t.Short), records.Column(ReplacedAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{ID},
		Indexes: []records.Index{{Name: "fs_file_versions_number", Unique: true, Columns: []records.IndexColumn{
			records.Key(FileID), records.Key(Number),
		}}},
	}).Statements()
	// HeldBytes totals a scope's versions.
	byScope, err2 := records.NewDDL(engine).CreateIndex(Table, records.Index{Name: indexByScope, Columns: []records.IndexColumn{records.Key(Scope)}}).Statements()
	return []records.Migration{
		{Owner: PluginName, Name: "0001_file_versions", Statements: table},
		{Owner: PluginName, Name: "0002_file_versions_scope", Statements: byScope},
	}, errors.Join(err1, err2)
}

// indexByScope is the index HeldBytes reads by.
const indexByScope = "fs_file_versions_scope"

// KeepVersion implements records.Versioner: it records prev as the
// file's newest version, holds its object (records.Store.Hold, so a usage
// plugin may charge it, or refuse the replacement past a limit), and
// drops the oldest past the cap.
func (p *Plugin) KeepVersion(ctx context.Context, s *records.Store, prev records.File) (bool, error) {
	pol, err := p.policy(ctx, prev)
	if err != nil {
		return false, err
	}
	list, err := p.Versions(ctx, s, prev.Scope, prev.ID)
	if err != nil {
		return false, err
	}
	var number int64 = 1
	if len(list) > 0 {
		number = list[0].Number + 1
	}
	v := Version{
		ID: s.NewID(IDPrefix), Scope: prev.Scope, FileID: prev.ID, Number: number, Disk: prev.Disk, Key: prev.Key,
		Filename: prev.Filename, ContentType: prev.ContentType, Size: prev.Size, SHA256: prev.SHA256, ReplacedAt: p.clock(s),
	}
	ins := sqlb.NewInsert(s.Dialect()).Into(Table).Row(
		sqlb.Set(ID, v.ID), sqlb.Set(Scope, v.Scope), sqlb.Set(FileID, v.FileID),
		sqlb.Set(Number, v.Number), sqlb.Set(Disk, v.Disk), sqlb.Set(Key, v.Key), sqlb.Set(Filename, v.Filename),
		sqlb.Set(ContentType, v.ContentType), sqlb.Set(Size, v.Size), sqlb.Set(SHA256, v.SHA256),
		sqlb.Set(ReplacedAt, v.ReplacedAt),
	)
	if _, err := s.Exec(ctx, ins); err != nil {
		return false, err
	}
	// list is newest first and does not hold the new version yet, which
	// takes one of the policy's places.
	if err := p.drop(ctx, s, pol.expired(list, 1, v.ReplacedAt)); err != nil {
		return false, err
	}
	// Held after the drop, so a file at its cap first releases its oldest
	// version: under a byte limit it then needs room only for the
	// difference. A refusal rolls the whole replacement back, the drop
	// included.
	if err := s.Hold(ctx, v.held()); err != nil {
		return false, err
	}
	return true, nil
}

// policy is the history policy of f: WithKeepFunc's answer, checked, or
// WithKeep's count.
func (p *Plugin) policy(ctx context.Context, f records.File) (KeepPolicy, error) {
	if p.keepFunc == nil {
		return KeepPolicy{Count: p.keep}, nil
	}
	pol, err := p.keepFunc(ctx, KeepRequest{File: f})
	if err != nil {
		return KeepPolicy{}, err
	}
	if pol.Count < 1 || pol.MaxAge < 0 {
		return KeepPolicy{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "versions: file %q: a policy keeps at least 1 version and no negative age", f.ID)
	}
	return pol, nil
}

// expired returns the versions of list (newest first) the policy drops at
// now, when taken places of the count are already used by versions not
// in list (1 for the version KeepVersion is adding, 0 for Trim).
func (pol KeepPolicy) expired(list []Version, taken int, now time.Time) []Version {
	var out []Version
	for i, v := range list {
		if i+taken >= pol.Count || (pol.MaxAge > 0 && v.ReplacedAt.Before(now.Add(-pol.MaxAge))) {
			out = append(out, v)
		}
	}
	return out
}

// Trim applies a file's history policy now, dropping the versions past
// its count or older than its MaxAge (WithKeepFunc), and returns how many
// it dropped: run it on a schedule so "30 days of versions" holds for
// files nobody replaces. Their objects are deleted after the commit.
func (p *Plugin) Trim(ctx context.Context, s *records.Store, scope, fileID string) (int, error) {
	var n int
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		f, err := s.File(ctx, scope, fileID)
		if err != nil {
			return err
		}
		pol, err := p.policy(ctx, f)
		if err != nil {
			return err
		}
		list, err := p.Versions(ctx, s, scope, fileID)
		if err != nil {
			return err
		}
		gone := pol.expired(list, 0, p.clock(s))
		n = len(gone)
		return p.drop(ctx, s, gone)
	})
	return n, err
}

// TrimPageSize is how many files TrimScope reads per page of the versions
// table: each file is then trimmed in its own transaction, so no lock is
// held across the scope.
const TrimPageSize = 100

// TrimScope applies the history policy to every file of scope that has
// versions, as Trim does for one: the scheduled run that makes "30 days of
// versions" hold for a whole tenant without the application walking its
// files. It pages through the versions table by file ID, TrimPageSize at
// a time, and trims each file in its own transaction, so a large scope
// never holds one long transaction; a file purged meanwhile is skipped. A
// file whose trim fails (its policy function erred, say) does not stop
// the others: TrimScope returns how many versions it dropped and the
// first failure, naming the file. Run it per scope from a job.
func (p *Plugin) TrimScope(ctx context.Context, s *records.Store, scope string) (int, error) {
	var total int
	var first error
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return total, errors.Join(first, err)
		}
		var id string
		var page []string
		q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(FileID, &id)).From(Table).
			Where(sqlb.Eq(Scope, scope), sqlb.Greater(FileID, after)).GroupBy(FileID).
			Order(sqlb.OrderBy(FileID, sqlb.SortAsc)).Limit(TrimPageSize)
		if err := s.Each(ctx, q, func() error { page = append(page, id); return nil }); err != nil {
			return total, errors.Join(first, err)
		}
		for _, fileID := range page {
			n, err := p.Trim(ctx, s, scope, fileID)
			total += n
			if err != nil && filesystem.CodeOf(err) != filesystem.CodeNotFound && first == nil {
				first = filesystem.Errorf(filesystem.CodeOf(err), "versions: trim file %q: %w", fileID, err)
			}
		}
		if len(page) < TrimPageSize {
			return total, first
		}
		after = page[len(page)-1]
	}
}

// drop deletes version rows, releases their objects (records.Store.Release)
// and deletes them after the commit.
func (p *Plugin) drop(ctx context.Context, s *records.Store, list []Version) error {
	for _, v := range list {
		if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Scope, v.Scope), sqlb.Eq(ID, v.ID))); err != nil {
			return err
		}
		if err := s.Release(ctx, v.held()); err != nil {
			return err
		}
		s.DeleteObjectAfterCommit(ctx, v.object())
	}
	return nil
}

// BeforeCreate implements records.FileHooks.
func (p *Plugin) BeforeCreate(context.Context, *records.Store, *records.File) error { return nil }

// AfterCreate implements records.FileHooks.
func (p *Plugin) AfterCreate(context.Context, *records.Store, records.File) error { return nil }

// BeforePurge implements records.FileHooks: a purged file's versions go
// with it.
func (p *Plugin) BeforePurge(ctx context.Context, s *records.Store, f records.File) error {
	list, err := p.Versions(ctx, s, f.Scope, f.ID)
	if err != nil {
		return err
	}
	return p.drop(ctx, s, list)
}

// PurgeScope implements records.ScopePurger: once Store.PurgeScope has
// purged every file of scope (whose BeforePurge dropped their versions),
// any version of the scope left behind is dropped the same way: row
// deleted, object released and deleted after the commit.
func (p *Plugin) PurgeScope(ctx context.Context, s *records.Store, scope string) error {
	var v Version
	var list []Version
	q := sqlb.NewSelect(s.Dialect()).Columns(bindings(&v)...).From(Table).Where(sqlb.Eq(Scope, scope))
	if err := s.Each(ctx, q, func() error { list = append(list, v); return nil }); err != nil {
		return err
	}
	return p.drop(ctx, s, list)
}

// Versions lists the versions of a file of scope, newest first.
func (p *Plugin) Versions(ctx context.Context, s *records.Store, scope, fileID string) ([]Version, error) {
	var v Version
	var out []Version
	q := sqlb.NewSelect(s.Dialect()).Columns(bindings(&v)...).From(Table).
		Where(sqlb.Eq(Scope, scope), sqlb.Eq(FileID, fileID)).Order(sqlb.OrderBy(Number, sqlb.SortDesc))
	if err := s.Each(ctx, q, func() error { v.ReplacedAt = v.ReplacedAt.UTC(); out = append(out, v); return nil }); err != nil {
		return nil, err
	}
	return out, nil
}

// Version returns one version of scope, or CodeNotFound.
func (p *Plugin) Version(ctx context.Context, s *records.Store, scope, id string) (Version, error) {
	var v Version
	q := sqlb.NewSelect(s.Dialect()).Columns(bindings(&v)...).From(Table).Where(sqlb.Eq(Scope, scope), sqlb.Eq(ID, id))
	if err := s.One(ctx, q); err != nil {
		if errors.Is(err, sqlb.ErrNoRows) {
			return Version{}, filesystem.Errorf(filesystem.CodeNotFound, "versions: no version %q", id)
		}
		return Version{}, err
	}
	v.ReplacedAt = v.ReplacedAt.UTC()
	return v, nil
}

// Restore makes version id the content of its file again, by copying its
// object, and returns the file. The current content becomes the newest
// version, so restoring can itself be undone. A version whose file is in
// the trash is CodeNotFound.
func (p *Plugin) Restore(ctx context.Context, s *records.Store, scope, id string) (records.File, error) {
	v, err := p.Version(ctx, s, scope, id)
	if err != nil {
		return records.File{}, err
	}
	f, err := s.File(ctx, scope, v.FileID)
	if err != nil {
		return records.File{}, err
	}
	// Checked before the copy, so a trashed file costs no object.
	if f.DeletedAt != nil {
		return records.File{}, filesystem.Errorf(filesystem.CodeNotFound, "versions: file %q is in the trash", v.FileID)
	}
	src, err := s.Disks().Disk(v.Disk)
	if err != nil {
		return records.File{}, err
	}
	// The copy goes to the file's own disk, which can differ from the
	// version's when the file moved (records.WithVisibilityDisks).
	disk, err := s.Disks().Disk(f.Disk)
	if err != nil {
		return records.File{}, err
	}
	key, err := p.restoreKeyFor(ctx, disk, f, v)
	if err != nil {
		return records.File{}, err
	}
	if _, err := filesystem.CopyAcross(ctx, src, v.Key, disk, key); err != nil {
		return records.File{}, err
	}
	// The copy goes again if the replacement fails, or if an enclosing
	// transaction rolls back, as with records.Store.Upload.
	discard := func(ctx context.Context) { _ = disk.Delete(context.WithoutCancel(ctx), key) }
	f, err = s.ReplaceObject(ctx, scope, v.FileID, records.NewContent{
		Key: key, Filename: v.Filename, ContentType: v.ContentType, Size: v.Size, SHA256: v.SHA256,
	})
	if err != nil {
		discard(ctx)
		return records.File{}, err
	}
	s.OnRollback(ctx, discard)
	return f, nil
}

// restoreKeyFor names the key a restore of v into f copies to on disk:
// WithRestoreKey's, checked, or a random one under restoreDir. A custom
// key must be new, since the copy would overwrite whatever it names.
func (p *Plugin) restoreKeyFor(ctx context.Context, disk *filesystem.Disk, f records.File, v Version) (string, error) {
	if p.restoreKey == nil {
		return randomRestoreKey(), nil
	}
	key, err := p.restoreKey(ctx, RestoreKeyRequest{File: f, Version: v})
	if err != nil {
		return "", err
	}
	if key == "" || key == f.Key || (key == v.Key && v.Disk == f.Disk) {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "versions: restore key %q is empty or names an object in use", key)
	}
	_, err = disk.Stat(ctx, key)
	switch {
	case filesystem.CodeOf(err) == filesystem.CodeNotFound:
		return key, nil
	case err == nil:
		return "", filesystem.Errorf(filesystem.CodeExists, "versions: restore key %q already holds an object", key)
	}
	return "", err
}

// OnEvent implements records.EventHooks: when a file is made public or
// private, trashed or restored, its kept versions follow its object, so an
// old version never stays public after the file was made private or
// trashed. A trashed file's versions are made private (the visibility
// its own object has in the trash); a restored file's get its visibility
// back. A version on another disk than the file's (records.WithVisibilityDisks,
// where trashing moves a public file to the private disk) is copied over,
// its old object deleted after the commit; one on the same disk has its
// visibility set where the disk can. A version whose object is gone is
// skipped rather than blocking the file's change. Every change is undone
// if the transaction rolls back.
func (p *Plugin) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	switch e.Kind {
	case records.EventFileVisibilityChanged, records.EventFileTrashed, records.EventFileRestored:
	default:
		return nil
	}
	f, err := s.File(ctx, e.Scope, e.FileID)
	if err != nil {
		return err
	}
	list, err := p.Versions(ctx, s, e.Scope, e.FileID)
	if err != nil {
		return err
	}
	dst, err := s.Disks().Disk(f.Disk)
	if err != nil {
		return err
	}
	want := f.Visibility
	if f.DeletedAt != nil {
		want = filesystem.VisibilityPrivate
	}
	for _, v := range list {
		src, err := s.Disks().Disk(v.Disk)
		if err != nil {
			return err
		}
		if v.Disk == f.Disk {
			if !dst.Capabilities().SetVisibility {
				continue // the disk cannot hide one object; neither could the file's
			}
			before, err := dst.Stat(ctx, v.Key)
			if filesystem.CodeOf(err) == filesystem.CodeNotFound {
				continue // a lost object has nothing to show or hide
			}
			if err != nil {
				return err
			}
			if before.Visibility == want {
				continue
			}
			if err := dst.SetVisibility(ctx, v.Key, want); err != nil {
				return err
			}
			s.OnRollback(ctx, func(ctx context.Context) { _ = dst.SetVisibility(context.WithoutCancel(ctx), v.Key, before.Visibility) })
			continue
		}
		if err := moveVersion(ctx, s, v, src, dst, want); err != nil {
			return err
		}
	}
	return nil
}

// moveVersion moves a kept version's object from src to dst under the same
// key, inside the caller's transaction: the copy gets visibility want
// where dst can set it and is deleted again on rollback, the row names
// dst, and the old object is deleted after the commit. A version whose
// object was lost is skipped (nothing to move).
func moveVersion(ctx context.Context, s *records.Store, v Version, src, dst *filesystem.Disk, want filesystem.Visibility) error {
	if _, err := filesystem.CopyAcross(ctx, src, v.Key, dst, v.Key); filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return nil
	} else if err != nil {
		return err
	}
	s.OnRollback(ctx, func(ctx context.Context) { _ = dst.Delete(context.WithoutCancel(ctx), v.Key) })
	if dst.Capabilities().SetVisibility {
		if err := dst.SetVisibility(ctx, v.Key, want); err != nil {
			return err
		}
	}
	upd := sqlb.NewUpdate(s.Dialect()).Table(Table).Set(sqlb.Set(Disk, dst.Name())).Where(sqlb.Eq(Scope, v.Scope), sqlb.Eq(ID, v.ID))
	if _, err := s.Exec(ctx, upd); err != nil {
		return err
	}
	s.DeleteObjectAfterCommit(ctx, v.object())
	return nil
}

// The plugin moves its objects with their file.
var _ records.FileDiskMover = (*Plugin)(nil)

// MoveFileDisk implements records.FileDiskMover: when records.Store.MoveFile
// moves a file from f.Disk to the disk named to, every kept version on
// f.Disk moves with it (copied, given the file's visibility, its row
// updated, the old object deleted after the commit; copies deleted again
// if the move rolls back). Versions on another disk than the file's, and
// versions whose object was lost, are left as they are.
func (p *Plugin) MoveFileDisk(ctx context.Context, s *records.Store, f records.File, to string) error {
	list, err := p.Versions(ctx, s, f.Scope, f.ID)
	if err != nil {
		return err
	}
	src, err := s.Disks().Disk(f.Disk)
	if err != nil {
		return err
	}
	dst, err := s.Disks().Disk(to)
	if err != nil {
		return err
	}
	for _, v := range list {
		if v.Disk != f.Disk {
			continue
		}
		if err := moveVersion(ctx, s, v, src, dst, f.Visibility); err != nil {
			return err
		}
	}
	return nil
}

// OwnedObjects implements records.ObjectOwner: a kept version's object
// is the plugin's, so records.Store.SweepOrphans leaves it alone.
func (p *Plugin) OwnedObjects(ctx context.Context, s *records.Store, disk string, keys []string) (map[string]bool, error) {
	owned := map[string]bool{}
	var key string
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(Key, &key)).From(Table).Where(sqlb.Eq(Disk, disk), sqlb.In(Key, keys...))
	if err := s.Each(ctx, q, func() error { owned[key] = true; return nil }); err != nil {
		return nil, err
	}
	return owned, nil
}

// The plugin reports what it holds to usage plugins.
var _ records.HeldCounter = (*Plugin)(nil)

// HeldBytes implements records.HeldCounter: the total size of the kept
// versions of scope, so a usage recount (quota.Recount) includes them.
func (p *Plugin) HeldBytes(ctx context.Context, s *records.Store, scope string) (int64, error) {
	var n int64
	sum := sqlb.Coalesce(sqlb.Sum(Size), sqlb.Param[int64](0))
	if err := s.One(ctx, sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(sum, &n)).From(Table).Where(sqlb.Eq(Scope, scope))); err != nil {
		return 0, err
	}
	return n, nil
}

// The plugin stores disk names of its own.
var _ records.DiskRenamer = (*Plugin)(nil)

// RenameDisk implements records.DiskRenamer: every kept version on the
// disk named from now names to, in every scope, inside the transaction of
// records.Store.RenameDisk, so a failure rolls the whole rename back. Rows
// only: the objects must already be reachable on to under the same keys.
func (p *Plugin) RenameDisk(ctx context.Context, s *records.Store, from, to string) error {
	_, err := s.Exec(ctx, sqlb.NewUpdate(s.Dialect()).Table(Table).Set(sqlb.Set(Disk, to)).Where(sqlb.Eq(Disk, from)))
	return err
}

// Delete removes one version, its object after the commit.
func (p *Plugin) Delete(ctx context.Context, s *records.Store, scope, id string) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		v, err := p.Version(ctx, s, scope, id)
		if err != nil {
			return err
		}
		return p.drop(ctx, s, []Version{v})
	})
}
