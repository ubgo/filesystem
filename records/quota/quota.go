// Package quota is a records plugin that limits how much each scope (a
// tenant, a workspace, a user) may store, in bytes and in files, and keeps
// the usage that billing pages and admin screens show. With
// WithUploaderLimits it also limits and counts each uploader within a
// scope (a member of a workspace), beside the scope's own total.
//
// Usage is charged inside the transaction that records a file, with one
// conditional UPDATE per counter ("add the size only if the total stays
// within the limit"), so two concurrent uploads can never both pass a
// limit, and a rolled-back upload un-charges itself. Purging a file
// releases its space; a trashed file still counts, because its bytes are
// still stored.
//
// With WithHeldObjects, the objects other plugins hold beside a file's
// content (kept versions, image variants, reported through
// records.Store.Hold and Release) count toward the scope's bytes too.
//
// With WithDimensions each file is also charged to counters the
// application names (an organisation across its workspaces, an owner
// type, a content class), each with its own limit.
//
// Limits come from the application (LimitFunc, UploaderLimitFunc,
// DimensionFunc),
// typically from the scope's plan. A usage row is created on its first
// upload; Recount rebuilds a scope's rows from fs_files when they have
// drifted (after a manual repair).
package quota

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// PluginName is the plugin's name in the migration ledger.
const PluginName = "quota"

// TableNameUsage is the table of scope totals: one row per scope.
const TableNameUsage = "fs_quota_usage"

// TableNameUploaderUsage is the table of per-uploader totals: one row per
// scope and uploader (records.File.UploadedBy, "" for files recorded
// without an actor). It is filled only with WithUploaderLimits.
const TableNameUploaderUsage = "fs_quota_uploader_usage"

// Column names of fs_quota_usage, and of fs_quota_uploader_usage, which
// has the same columns plus ColUploader.
const (
	ColScope     = "scope"
	ColBytes     = "bytes"
	ColFiles     = "files"
	ColUpdatedAt = "updated_at"
	ColUploader  = "uploader"
	// ColReserved is fs_quota_usage's bytes held by open reservations
	// (Reserve); the uploader table has no such column.
	ColReserved = "reserved"
)

// Table and typed columns of fs_quota_usage.
var (
	Table     = sqlb.Table{Name: TableNameUsage, Alias: "q"}
	Scope     = sqlb.Col[string](Table, ColScope)
	Bytes     = sqlb.Col[int64](Table, ColBytes)
	Files     = sqlb.Col[int64](Table, ColFiles)
	UpdatedAt = sqlb.Col[time.Time](Table, ColUpdatedAt)
	Reserved  = sqlb.Col[int64](Table, ColReserved)
)

// Table and typed columns of fs_quota_uploader_usage.
var (
	UploaderTable     = sqlb.Table{Name: TableNameUploaderUsage, Alias: "qu"}
	UploaderScope     = sqlb.Col[string](UploaderTable, ColScope)
	Uploader          = sqlb.Col[string](UploaderTable, ColUploader)
	UploaderBytes     = sqlb.Col[int64](UploaderTable, ColBytes)
	UploaderFiles     = sqlb.Col[int64](UploaderTable, ColFiles)
	UploaderUpdatedAt = sqlb.Col[time.Time](UploaderTable, ColUpdatedAt)
)

// Limit is what a scope, or an uploader in it, may store. Zero in a field
// means unlimited.
type Limit struct {
	Bytes int64
	Files int64
}

// LimitFunc returns the limit of a scope, usually from its billing plan.
type LimitFunc func(ctx context.Context, scope string) (Limit, error)

// UploaderLimitFunc returns the limit of one uploader within a scope
// (records.File.UploadedBy; "" for files recorded without an actor), for
// example a seat's share of a team plan. It is asked on every upload and
// every growing replacement, after the scope's own limit passed.
type UploaderLimitFunc func(ctx context.Context, scope, uploader string) (Limit, error)

// Usage is what a scope, or an uploader in it, stores now.
type Usage struct {
	Bytes int64
	Files int64
	// Reserved is the bytes a scope's open reservations hold for uploads
	// in flight (Reserve); they count against the byte limit but are not
	// stored yet. Always 0 for an uploader.
	Reserved int64
}

// Plugin enforces limits. Construct it with New and install it with
// records.WithPlugins.
type Plugin struct {
	limits LimitFunc
	now    func() time.Time
	// held is WithHeldObjects: charge objects other plugins hold.
	held bool
	// uploaders is WithUploaderLimits, and uploaderLimits its function.
	uploaders      bool
	uploaderLimits UploaderLimitFunc
	// events is WithUsageEvents.
	events bool
	// dimensions is WithDimensions' function; nil charges no dimension.
	dimensions DimensionFunc
}

// Option configures a Plugin.
type Option func(*Plugin)

// WithHeldObjects counts the objects other plugins hold beside a file's
// content (kept versions, image variants) toward a scope's bytes, so the
// byte limit bounds everything the scope stores. Holders report them with
// records.Store.Hold and Release, inside their own transaction; holding
// one past the byte limit is refused with CodeQuotaExceeded, which aborts
// the holder's change (a replacement that would keep a version, a variant
// being generated). Held objects never count as files, and never count
// toward an uploader's usage (a HeldObject names no uploader).
//
// Off by default, so usage rows built without it keep their meaning. A
// Store that already holds versions or variants when it is turned on
// must Recount every scope, or releasing those uncharged objects leaves
// usage below the truth.
func WithHeldObjects() Option { return func(p *Plugin) { p.held = true } }

// WithUploaderLimits keeps usage per uploader as well as per scope, and
// limits each uploader with fn: every file is charged to its scope and to
// its uploader (records.File.UploadedBy) in the same transaction, and is
// refused with CodeQuotaExceeded when either would pass its limit. A
// replacement is charged to the file's uploader, not to whoever replaced
// it, because the bytes stay the uploader's. Return Limit{} from fn to
// count uploaders without limiting them.
//
// Off by default. Turning it on for a Store with files needs a Recount of
// every scope, which builds the uploader rows from fs_files.
func WithUploaderLimits(fn UploaderLimitFunc) Option {
	return func(p *Plugin) { p.uploaders, p.uploaderLimits = true, fn }
}

// WithUsageEvents makes every change to a scope's usage emit a
// quota.usage_changed event (EventUsageChanged, through
// records.Store.Emit) inside the change's transaction, so billing, audit
// logs and webhooks see usage move with the change that moved it, and
// never for one that rolled back. Each event carries the Store's random
// event ID, kept by every redelivery, for at-least-once consumers. Off by
// default: it adds an event and a read to every upload, replacement and
// purge.
func WithUsageEvents() Option { return func(p *Plugin) { p.events = true } }

// WithClock gives this plugin its own clock, for tests: the time usage
// rows are stamped with. Without it the plugin reads records.Store.Now,
// so records.WithClock rules the plugin and the Store alike.
func WithClock(now func() time.Time) Option { return func(p *Plugin) { p.now = now } }

// New returns a Plugin taking limits from fn. A nil fn, or a nil
// UploaderLimitFunc given to WithUploaderLimits, is CodeInvalidArgument.
func New(fn LimitFunc, opts ...Option) (*Plugin, error) {
	if fn == nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "quota: nil LimitFunc")
	}
	p := &Plugin{limits: fn}
	for _, o := range opts {
		o(p)
	}
	if p.uploaders && p.uploaderLimits == nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "quota: nil UploaderLimitFunc")
	}
	return p, nil
}

// Name implements records.Plugin.
func (p *Plugin) Name() string { return PluginName }

// Migrations implements records.Migrator. 0002 adds the uploader table,
// whether or not WithUploaderLimits is on, so turning it on later needs
// no migration.
func (p *Plugin) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	usage, err1 := records.NewDDL(engine).CreateTable(records.Table{
		Table: Table,
		Columns: []ddl.ColumnSpec{
			records.Column(Scope, t.Key), records.Column(Bytes, t.Int), records.Column(Files, t.Int), records.Column(UpdatedAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{Scope},
	}).Statements()
	uploaders, err2 := records.NewDDL(engine).CreateTable(records.Table{
		Table: UploaderTable,
		Columns: []ddl.ColumnSpec{
			records.Column(UploaderScope, t.Key), records.Column(Uploader, t.Key), records.Column(UploaderBytes, t.Int),
			records.Column(UploaderFiles, t.Int), records.Column(UploaderUpdatedAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{UploaderScope, Uploader},
	}).Statements()
	reservations, err3 := records.NewDDL(engine).AddColumn(Table, records.Column(Reserved, t.Int).Default(0)).CreateTable(records.Table{
		Table: ReservationTable,
		Columns: []ddl.ColumnSpec{
			records.Column(ReservationID, t.Key), records.Column(ReservationScope, t.Key), records.Column(ReservationBytes, t.Int),
			records.Column(ReservationExpiresAt, t.Timestamp), records.Column(ReservationCreatedAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{ReservationID},
		Indexes: []records.Index{
			{Name: "fs_quota_reservations_scope", Columns: []records.IndexColumn{records.Key(ReservationScope)}},
			{Name: "fs_quota_reservations_expires", Columns: []records.IndexColumn{records.Key(ReservationExpiresAt)}},
		},
	}).Statements()
	dimensions, err4 := records.NewDDL(engine).CreateTable(records.Table{
		Table: DimensionTable,
		Columns: []ddl.ColumnSpec{
			records.Column(DimensionKey, t.Key), records.Column(DimensionBytes, t.Int), records.Column(DimensionFiles, t.Int),
			records.Column(DimensionUpdatedAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{DimensionKey},
	}).Statements()
	return []records.Migration{
		{Owner: PluginName, Name: "0001_usage", Statements: usage},
		{Owner: PluginName, Name: "0002_uploader_usage", Statements: uploaders},
		{Owner: PluginName, Name: "0003_reservations", Statements: reservations},
		{Owner: PluginName, Name: "0004_dimension_usage", Statements: dimensions},
	}, errors.Join(err1, err2, err3, err4)
}

// The plugin hears of objects other plugins hold.
var _ records.UsageHooks = (*Plugin)(nil)

// stamp is WithClock's time when set, else the Store's (records.WithClock),
// in UTC at microsecond precision, the precision every engine keeps.
func (p *Plugin) stamp(s *records.Store) time.Time {
	if p.now == nil {
		return s.Now()
	}
	return p.now().UTC().Truncate(time.Microsecond)
}

// counter is one usage row: a scope's in fs_quota_usage, or an uploader's
// in fs_quota_uploader_usage. Every charge, release and read goes through
// it, so both kinds of row follow the same rules.
type counter struct {
	table   sqlb.Table
	bytes   sqlb.Expr[int64]
	files   sqlb.Expr[int64]
	updated sqlb.Expr[time.Time]
	// reserved is the scope row's reserved bytes, which count against the
	// byte limit; unset for an uploader row, which has none.
	reserved sqlb.Expr[int64]
	// scope is the scope the row belongs to. scopeRow marks the scope's
	// own row, the only one with reserved bytes and usage events.
	scope    string
	scopeRow bool
	// key selects the row; keySet writes its key columns on insert.
	key    []sqlb.Predicate
	keySet []sqlb.Assignment
	// name is how errors name the row: `scope "t1"`.
	name string
}

func scopeCounter(scope string) counter {
	return counter{
		table: Table, bytes: Bytes, files: Files, updated: UpdatedAt, reserved: Reserved, scope: scope, scopeRow: true,
		key: []sqlb.Predicate{sqlb.Eq(Scope, scope)}, keySet: []sqlb.Assignment{sqlb.Set(Scope, scope)},
		name: fmt.Sprintf("scope %q", scope),
	}
}

func uploaderCounter(scope, uploader string) counter {
	return counter{
		table: UploaderTable, bytes: UploaderBytes, files: UploaderFiles, updated: UploaderUpdatedAt, scope: scope,
		key:    []sqlb.Predicate{sqlb.Eq(UploaderScope, scope), sqlb.Eq(Uploader, uploader)},
		keySet: []sqlb.Assignment{sqlb.Set(UploaderScope, scope), sqlb.Set(Uploader, uploader)},
		name:   fmt.Sprintf("uploader %q in scope %q", uploader, scope),
	}
}

// BeforeCreate implements records.FileHooks: it charges the file to its
// scope and, with WithUploaderLimits, to its uploader, and with
// WithDimensions to each of its dimensions, or refuses it with
// CodeQuotaExceeded naming the limit it passes.
func (p *Plugin) BeforeCreate(ctx context.Context, s *records.Store, f *records.File) error {
	// Asked first, so a failing DimensionFunc refuses the file before any
	// counter moves.
	dims, err := p.dimensionsOf(ctx, *f)
	if err != nil {
		return err
	}
	if err := p.chargeOwners(ctx, s, f); err != nil {
		return err
	}
	return p.chargeDimensions(ctx, s, dims, f.Size)
}

// chargeOwners charges f to its scope and, with WithUploaderLimits, its
// uploader.
func (p *Plugin) chargeOwners(ctx context.Context, s *records.Store, f *records.File) error {
	limit, err := p.limits(ctx, f.Scope)
	if err != nil {
		return err
	}
	consumed, err := p.consume(ctx, s, f.Scope)
	if err != nil {
		return err
	}
	if err := p.charge(ctx, s, scopeCounter(f.Scope), f.Size, limit, consumed); err != nil {
		return err
	}
	if !p.uploaders {
		return nil
	}
	if limit, err = p.uploaderLimits(ctx, f.Scope, f.UploadedBy); err != nil {
		return err
	}
	return p.charge(ctx, s, uploaderCounter(f.Scope, f.UploadedBy), f.Size, limit, 0)
}

// charge adds size bytes and one file to c, creating the row on its first
// file, or refuses with CodeQuotaExceeded when that passes limit. consumed
// is the bytes of the reservation this file uses up (Reserve), moved from
// the row's reserved bytes to its stored ones in the same UPDATE, so the
// file is never counted twice and the limit check sees the reservation
// gone; 0 without one.
func (p *Plugin) charge(ctx context.Context, s *records.Store, c counter, size int64, limit Limit, consumed int64) error {
	if exceeds(Usage{}, size, limit) {
		return exceeded(c.name, limit, Usage{}, size, 1)
	}
	where := append([]sqlb.Predicate{}, c.key...)
	if limit.Bytes > 0 {
		where = append(where, sqlb.LessEqExpr(sqlb.Add(c.held(), sqlb.Param(size-consumed)), sqlb.Param(limit.Bytes)))
	}
	if limit.Files > 0 {
		where = append(where, sqlb.LessEqExpr(sqlb.Add(c.files, sqlb.Param[int64](1)), sqlb.Param(limit.Files)))
	}
	set := []sqlb.Assignment{
		sqlb.SetExpr(c.bytes, sqlb.Add(c.bytes, sqlb.Param(size))),
		sqlb.SetExpr(c.files, sqlb.Add(c.files, sqlb.Param[int64](1))),
		sqlb.Set(c.updated, p.stamp(s)),
	}
	if consumed > 0 {
		set = append(set, sqlb.SetExpr(c.reserved, floorSub(c.reserved, consumed)))
	}
	n, err := s.Exec(ctx, sqlb.NewUpdate(s.Dialect()).Table(c.table).Set(set...).Where(where...))
	if err != nil {
		return err
	}
	if n == 1 {
		return p.changed(ctx, s, c, size, 1)
	}
	// No row changed: either the row does not exist yet, or the charge
	// would pass a limit.
	if u, err := p.load(ctx, s, c); err == nil {
		u.Bytes += u.Reserved - consumed
		return exceeded(c.name, limit, u, size, 1)
	} else if filesystem.CodeOf(err) != filesystem.CodeNotFound {
		return err
	}
	// First file of the row. Two first uploads racing each other make one
	// INSERT fail; it surfaces as CodeUnavailable for the caller to retry
	// (a failed statement also aborts a Postgres transaction, so it cannot
	// be retried in place).
	if err := p.insert(ctx, s, c, Usage{Bytes: size, Files: 1}); err != nil {
		if records.IsUniqueViolation(err) {
			return filesystem.NewError(filesystem.CodeUnavailable, err)
		}
		return err
	}
	return p.changed(ctx, s, c, size, 1)
}

// held is what counts against c's byte limit: its stored bytes plus, for
// a scope, its reserved ones.
func (c counter) held() sqlb.Expr[int64] {
	if !c.scopeRow {
		return c.bytes
	}
	return sqlb.Add(c.bytes, c.reserved)
}

func (p *Plugin) insert(ctx context.Context, s *records.Store, c counter, u Usage) error {
	row := append(append([]sqlb.Assignment{}, c.keySet...),
		sqlb.Set(c.bytes, u.Bytes), sqlb.Set(c.files, u.Files), sqlb.Set(c.updated, p.stamp(s)))
	if c.scopeRow {
		row = append(row, sqlb.Set(c.reserved, u.Reserved))
	}
	_, err := s.Exec(ctx, sqlb.NewInsert(s.Dialect()).Into(c.table).Row(row...))
	return err
}

// EventUsageChanged is the kind of the events WithUsageEvents emits.
const EventUsageChanged records.EventKind = PluginName + ".usage_changed"

// Detail keys of a quota.usage_changed event, all decimal integers.
const (
	// DetailBytes and DetailFiles are the scope's usage after the change.
	DetailBytes = "bytes"
	DetailFiles = "files"
	// DetailDeltaBytes and DetailDeltaFiles are what the change added
	// (negative when it released), for a consumer that sums changes.
	DetailDeltaBytes = "delta_bytes"
	DetailDeltaFiles = "delta_files"
)

// changed emits quota.usage_changed for a charge of c by the given bytes
// and files (which always changes the row), with WithUsageEvents and for a
// scope row only. The new totals are read inside the change's transaction,
// so they are the ones it left. A release goes through shrink, which
// compares the row before and after instead.
func (p *Plugin) changed(ctx context.Context, s *records.Store, c counter, bytes, files int64) error {
	if !p.events || !c.scopeRow {
		return nil
	}
	u, err := p.load(ctx, s, c)
	if err != nil {
		return err
	}
	return p.emitUsage(ctx, s, c, u, bytes, files)
}

// emitUsage emits quota.usage_changed for c, now at u, after a change of
// the given bytes and files.
func (p *Plugin) emitUsage(ctx context.Context, s *records.Store, c counter, u Usage, bytes, files int64) error {
	return s.Emit(ctx, p, records.Event{Kind: EventUsageChanged, Scope: c.scope, Detail: map[string]string{
		DetailBytes: strconv.FormatInt(u.Bytes, 10), DetailFiles: strconv.FormatInt(u.Files, 10),
		DetailDeltaBytes: strconv.FormatInt(bytes, 10), DetailDeltaFiles: strconv.FormatInt(files, 10),
	}})
}

// PurgeScope implements records.ScopePurger: once Store.PurgeScope has
// purged every file of scope, the scope's usage rows go (its total and
// every uploader's), so an offboarded tenant leaves no counter behind (a
// new upload starts fresh ones).
func (p *Plugin) PurgeScope(ctx context.Context, s *records.Store, scope string) error {
	if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Scope, scope))); err != nil {
		return err
	}
	if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(ReservationTable).Where(sqlb.Eq(ReservationScope, scope))); err != nil {
		return err
	}
	_, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(UploaderTable).Where(sqlb.Eq(UploaderScope, scope)))
	return err
}

// exceeds reports whether adding size bytes and one file to u passes limit.
func exceeds(u Usage, size int64, limit Limit) bool {
	return (limit.Bytes > 0 && u.Bytes+size > limit.Bytes) || (limit.Files > 0 && u.Files+1 > limit.Files)
}

// exceeded is the refusal of adding size bytes and files files to u, the
// usage of the row named name, naming the limit that charge passes:
// bytes, files, or both. When usage changed between the refused update
// and the read of u (a concurrent purge) so that neither seems passed, it
// names both.
func exceeded(name string, limit Limit, u Usage, size, files int64) error {
	overBytes := limit.Bytes > 0 && u.Bytes+size > limit.Bytes
	overFiles := limit.Files > 0 && u.Files+files > limit.Files
	switch {
	case overBytes && !overFiles:
		return filesystem.Errorf(filesystem.CodeQuotaExceeded, "quota: %s would exceed its limit of %d bytes", name, limit.Bytes)
	case overFiles && !overBytes:
		return filesystem.Errorf(filesystem.CodeQuotaExceeded, "quota: %s would exceed its limit of %d files", name, limit.Files)
	}
	return filesystem.Errorf(filesystem.CodeQuotaExceeded, "quota: %s would exceed its limits of %d bytes and %d files (0 is unlimited)", name, limit.Bytes, limit.Files)
}

// BeforeReplace implements records.ReplaceHooks: it charges the size
// difference of the new content to the scope and, with
// WithUploaderLimits, to the file's uploader, refusing growth past either
// byte limit with CodeQuotaExceeded and crediting a shrink. The previous
// content, if a versions plugin keeps it, is charged again through OnHold
// with WithHeldObjects, and not counted without it. With WithDimensions
// the file's dimensions are moved too (see replaceDimensions).
func (p *Plugin) BeforeReplace(ctx context.Context, s *records.Store, cur records.File, next *records.File) error {
	if err := p.replaceOwners(ctx, s, cur, next); err != nil {
		return err
	}
	return p.replaceDimensions(ctx, s, cur, *next)
}

// replaceOwners charges a replacement's size difference to the scope and,
// with WithUploaderLimits, the file's uploader.
func (p *Plugin) replaceOwners(ctx context.Context, s *records.Store, cur records.File, next *records.File) error {
	delta := next.Size - cur.Size
	switch {
	case delta > 0:
		if err := p.grow(ctx, s, cur.Scope, delta); err != nil {
			return err
		}
		if p.uploaders {
			limit, err := p.uploaderLimits(ctx, cur.Scope, cur.UploadedBy)
			if err != nil {
				return err
			}
			return p.growCounter(ctx, s, uploaderCounter(cur.Scope, cur.UploadedBy), delta, limit)
		}
	case delta < 0:
		if err := p.shrink(ctx, s, scopeCounter(cur.Scope), -delta, 0); err != nil {
			return err
		}
		if p.uploaders {
			return p.shrink(ctx, s, uploaderCounter(cur.Scope, cur.UploadedBy), -delta, 0)
		}
	}
	return nil
}

// grow charges n more bytes (no file) to scope under its limit.
func (p *Plugin) grow(ctx context.Context, s *records.Store, scope string, n int64) error {
	limit, err := p.limits(ctx, scope)
	if err != nil {
		return err
	}
	return p.growCounter(ctx, s, scopeCounter(scope), n, limit)
}

// growCounter charges n more bytes (no file) to c, refusing to pass the
// byte limit with CodeQuotaExceeded. A row that does not exist (quota, or
// WithUploaderLimits, was installed after the files; Recount builds it)
// is not charged.
func (p *Plugin) growCounter(ctx context.Context, s *records.Store, c counter, n int64, limit Limit) error {
	where := append([]sqlb.Predicate{}, c.key...)
	if limit.Bytes > 0 {
		where = append(where, sqlb.LessEqExpr(sqlb.Add(c.held(), sqlb.Param(n)), sqlb.Param(limit.Bytes)))
	}
	upd := sqlb.NewUpdate(s.Dialect()).Table(c.table).Set(
		sqlb.SetExpr(c.bytes, sqlb.Add(c.bytes, sqlb.Param(n))),
		sqlb.Set(c.updated, p.stamp(s)),
	).Where(where...)
	changed, err := s.Exec(ctx, upd)
	if err != nil {
		return err
	}
	if changed == 1 {
		return p.changed(ctx, s, c, n, 0)
	}
	// No row changed: the growth passes the limit, or there is no row.
	if u, err := p.load(ctx, s, c); err == nil {
		u.Bytes += u.Reserved
		return exceeded(c.name, limit, u, n, 0)
	} else if filesystem.CodeOf(err) != filesystem.CodeNotFound {
		return err
	}
	return nil
}

// shrink releases n bytes and files files of c. Usage never goes below
// zero: a file recorded before quota was installed was never charged, so
// releasing it must not leave a negative balance that would let the row
// store more than its limit.
func (p *Plugin) shrink(ctx context.Context, s *records.Store, c counter, n, files int64) error {
	set := []sqlb.Assignment{sqlb.SetExpr(c.bytes, floorSub(c.bytes, n)), sqlb.Set(c.updated, p.stamp(s))}
	if files > 0 {
		set = append(set, sqlb.SetExpr(c.files, floorSub(c.files, files)))
	}
	// Whether usage moved is read from the row before and after, never
	// from the UPDATE's row count: MySQL counts only rows whose values
	// changed, so the count cannot tell a missing row from a release that
	// changed nothing, or one floored at zero from a real one.
	report := p.events && c.scopeRow
	var before Usage
	if report {
		var err error
		if before, err = p.load(ctx, s, c); filesystem.CodeOf(err) == filesystem.CodeNotFound {
			return nil // no row: nothing was charged, nothing to release
		} else if err != nil {
			return err
		}
	}
	if _, err := s.Exec(ctx, sqlb.NewUpdate(s.Dialect()).Table(c.table).Set(set...).Where(c.key...)); err != nil || !report {
		return err
	}
	after, err := p.load(ctx, s, c)
	if err != nil || (after.Bytes == before.Bytes && after.Files == before.Files) {
		return err
	}
	return p.emitUsage(ctx, s, c, after, after.Bytes-before.Bytes, after.Files-before.Files)
}

// OnHold implements records.UsageHooks: with WithHeldObjects it charges a
// held object's size to its scope, refusing it past the byte limit with
// CodeQuotaExceeded; without, it does nothing. Held objects never count
// toward an uploader.
func (p *Plugin) OnHold(ctx context.Context, s *records.Store, o records.HeldObject) error {
	if !p.held || o.Size <= 0 {
		return nil
	}
	return p.grow(ctx, s, o.Scope, o.Size)
}

// OnRelease implements records.UsageHooks: with WithHeldObjects it
// releases a held object's size, never below zero; without, it does
// nothing.
func (p *Plugin) OnRelease(ctx context.Context, s *records.Store, o records.HeldObject) error {
	if !p.held || o.Size <= 0 {
		return nil
	}
	return p.shrink(ctx, s, scopeCounter(o.Scope), o.Size, 0)
}

// AfterCreate implements records.FileHooks.
func (p *Plugin) AfterCreate(context.Context, *records.Store, records.File) error { return nil }

// BeforePurge implements records.FileHooks: it releases the file's space
// and count from its scope and, with WithUploaderLimits, its uploader, and
// with WithDimensions its dimensions, never below zero.
func (p *Plugin) BeforePurge(ctx context.Context, s *records.Store, f records.File) error {
	dims, err := p.dimensionsOf(ctx, f)
	if err != nil {
		return err
	}
	if err := p.shrink(ctx, s, scopeCounter(f.Scope), f.Size, 1); err != nil {
		return err
	}
	if p.uploaders {
		if err := p.shrink(ctx, s, uploaderCounter(f.Scope, f.UploadedBy), f.Size, 1); err != nil {
			return err
		}
	}
	return p.releaseDimensions(ctx, s, dims, f.Size)
}

// floorSub renders col - n, or 0 where that would be negative.
func floorSub(col sqlb.Expr[int64], n int64) sqlb.Expr[int64] {
	return sqlb.Case[int64]().When(sqlb.GreaterExpr(col, sqlb.Param(n)), sqlb.Sub(col, sqlb.Param(n))).Else(sqlb.Param[int64](0))
}

func (p *Plugin) load(ctx context.Context, s *records.Store, c counter) (Usage, error) {
	var u Usage
	cols := []sqlb.Binding{sqlb.Bind(c.bytes, &u.Bytes), sqlb.Bind(c.files, &u.Files)}
	if c.scopeRow {
		cols = append(cols, sqlb.Bind(c.reserved, &u.Reserved))
	}
	q := sqlb.NewSelect(s.Dialect()).Columns(cols...).From(c.table).Where(c.key...)
	if err := s.One(ctx, q); err != nil {
		if errors.Is(err, sqlb.ErrNoRows) {
			return Usage{}, filesystem.Errorf(filesystem.CodeNotFound, "quota: no usage for %s", c.name)
		}
		return Usage{}, err
	}
	return u, nil
}

// Usage returns what scope stores; a scope that never uploaded uses
// nothing.
func (p *Plugin) Usage(ctx context.Context, s *records.Store, scope string) (Usage, error) {
	return p.usage(ctx, s, scopeCounter(scope))
}

// UploaderUsage returns what uploader stores in scope (files' own bytes
// and count; held objects are the scope's only). An uploader with no
// files, or a Plugin without WithUploaderLimits, uses nothing.
func (p *Plugin) UploaderUsage(ctx context.Context, s *records.Store, scope, uploader string) (Usage, error) {
	return p.usage(ctx, s, uploaderCounter(scope, uploader))
}

func (p *Plugin) usage(ctx context.Context, s *records.Store, c counter) (Usage, error) {
	u, err := p.load(ctx, s, c)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return Usage{}, nil
	}
	return u, err
}

// Recount rebuilds scope's usage from its files (trashed ones included),
// plus, with WithHeldObjects, the bytes every records.HeldCounter plugin
// holds in it (Store.HeldBytes), and returns it. With WithUploaderLimits
// it rebuilds every uploader row of the scope too. Run it after repairing
// fs_files by hand, and for every scope after turning WithHeldObjects or
// WithUploaderLimits on.
func (p *Plugin) Recount(ctx context.Context, s *records.Store, scope string) (Usage, error) {
	var u Usage
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		// Each row is deleted before it is summed: on Postgres and MySQL
		// that locks it, so an upload charging the scope waits for the
		// recount instead of landing between the sums and the new row, and
		// the sums are read inside the same transaction as the write.
		c := scopeCounter(scope)
		if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(c.table).Where(c.key...)); err != nil {
			return err
		}
		var err error
		if u, err = sumFiles(ctx, s, sqlb.Eq(records.FileScope, scope)); err != nil {
			return err
		}
		if p.held {
			held, err := s.HeldBytes(ctx, scope)
			if err != nil {
				return err
			}
			u.Bytes += held
		}
		if u.Reserved, err = p.reservedIn(ctx, s, scope); err != nil {
			return err
		}
		if err := p.insert(ctx, s, c, u); err != nil {
			return err
		}
		if p.uploaders {
			return p.recountUploaders(ctx, s, scope)
		}
		return nil
	})
	if err != nil {
		return Usage{}, err
	}
	return u, nil
}

// recountUploaders rebuilds every uploader row of scope from fs_files.
func (p *Plugin) recountUploaders(ctx context.Context, s *records.Store, scope string) error {
	if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(UploaderTable).Where(sqlb.Eq(UploaderScope, scope))); err != nil {
		return err
	}
	var uploader string
	var u Usage
	type row struct {
		uploader string
		u        Usage
	}
	var rows []row
	sum := sqlb.Coalesce(sqlb.Sum(records.FileSize), sqlb.Param[int64](0))
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(records.FileUploadedBy, &uploader), sqlb.Bind(sum, &u.Bytes), sqlb.Bind(sqlb.Count(), &u.Files)).
		From(records.TableFiles).Where(sqlb.Eq(records.FileScope, scope)).GroupBy(records.FileUploadedBy)
	// Collected first: a statement cannot run on the transaction while
	// the grouped rows are still being read.
	if err := s.Each(ctx, q, func() error { rows = append(rows, row{uploader, u}); return nil }); err != nil {
		return err
	}
	for _, r := range rows {
		if err := p.insert(ctx, s, uploaderCounter(scope, r.uploader), r.u); err != nil {
			return err
		}
	}
	return nil
}

// RecountUploader rebuilds one uploader's usage in scope from its files
// (trashed ones included) and returns it. Use it to repair one member
// without recounting a large scope; Recount rebuilds them all.
func (p *Plugin) RecountUploader(ctx context.Context, s *records.Store, scope, uploader string) (Usage, error) {
	var u Usage
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		c := uploaderCounter(scope, uploader)
		if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(c.table).Where(c.key...)); err != nil {
			return err
		}
		var err error
		if u, err = sumFiles(ctx, s, sqlb.Eq(records.FileScope, scope), sqlb.Eq(records.FileUploadedBy, uploader)); err != nil {
			return err
		}
		return p.insert(ctx, s, c, u)
	})
	if err != nil {
		return Usage{}, err
	}
	return u, nil
}

// sumFiles totals the size and count of the files matching where.
func sumFiles(ctx context.Context, s *records.Store, where ...sqlb.Predicate) (Usage, error) {
	var u Usage
	sum := sqlb.Coalesce(sqlb.Sum(records.FileSize), sqlb.Param[int64](0))
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(sum, &u.Bytes), sqlb.Bind(sqlb.Count(), &u.Files)).From(records.TableFiles).Where(where...)
	if err := s.One(ctx, q); err != nil {
		return Usage{}, err
	}
	return u, nil
}
