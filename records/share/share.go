// Package share is a records plugin for share links: a URL that lets
// anyone holding it download one file, until it expires, is revoked, or has
// been used a set number of times. The "send a link" feature of drives,
// chat apps and client portals.
//
// Tokens are shown once, when the link is created, and stored only as
// SHA-256 hashes, so a leaked database cannot be turned into working links.
// Every failure a visitor can see is the same not-found (expired, revoked,
// exhausted, unknown and trashed all look alike), so links cannot be
// probed. A link's downloads are counted with one conditional UPDATE, so a
// limit holds under concurrent requests. Links are deleted with their file.
package share

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// PluginName is the plugin's name in the migration ledger.
const PluginName = "share"

// TableNameShares is the table the plugin owns.
const TableNameShares = "fs_shares"

// Column names of fs_shares.
const (
	ColID           = "id"
	ColScope        = "scope"
	ColFileID       = "file_id"
	ColTokenHash    = "token_hash"
	ColExpiresAt    = "expires_at"
	ColMaxDownloads = "max_downloads"
	ColDownloads    = "downloads"
	ColRevokedAt    = "revoked_at"
	ColCreatedAt    = "created_at"
	// ColPasswordHash, ColTargetKind, ColTargetRef and ColMode came with
	// migration 0002 (passwords, collections, request links).
	ColPasswordHash = "password_hash"
	ColTargetKind   = "target_kind"
	ColTargetRef    = "target_ref"
	ColMode         = "mode"
)

// Table and typed columns.
var (
	Table        = sqlb.Table{Name: TableNameShares, Alias: "s"}
	ID           = sqlb.Col[string](Table, ColID)
	Scope        = sqlb.Col[string](Table, ColScope)
	FileID       = sqlb.Col[string](Table, ColFileID)
	TokenHash    = sqlb.Col[string](Table, ColTokenHash)
	ExpiresAt    = sqlb.Col[time.Time](Table, ColExpiresAt)
	MaxDownloads = sqlb.Col[int64](Table, ColMaxDownloads)
	Downloads    = sqlb.Col[int64](Table, ColDownloads)
	// RevokedAt is nil for a live link.
	RevokedAt = sqlb.Col[*time.Time](Table, ColRevokedAt)
	CreatedAt = sqlb.Col[time.Time](Table, ColCreatedAt)
	// PasswordHash is "" for a link without a password, else the PBKDF2
	// hash (see hashPassword); never the password.
	PasswordHash = sqlb.Col[string](Table, ColPasswordHash)
	// TargetKind is KindFile for a one-file link, else a Collection's
	// Kind; TargetRef is the collection's reference ("" for a file).
	TargetKind = sqlb.Col[string](Table, ColTargetKind)
	TargetRef  = sqlb.Col[string](Table, ColTargetRef)
	// Mode is ModeDownload or ModeRequest.
	Mode = sqlb.Col[string](Table, ColMode)
)

// KindFile is the TargetKind of a link to one file.
const KindFile = "file"

// Link modes: what a link lets its holder do.
const (
	// ModeDownload links serve their file, or their collection's files.
	ModeDownload = "download"
	// ModeRequest links accept uploads into their collection and serve
	// nothing (file request links).
	ModeRequest = "request"
)

// Limits.
const (
	// IDPrefix starts every share ID.
	IDPrefix = "shr_"
	// MaxLifetime caps a link's lifetime; a permanent public link should be
	// a public file instead. It is a floor of the plugin: WithMaxLifetime
	// can lower the cap, never raise it.
	MaxLifetime = 365 * 24 * time.Hour
	// MinTokenBytes is the token's least entropy, 256 bits, and the
	// default: a token is the whole secret of a link without a password,
	// so WithTokenBytes can lengthen it, never shorten it.
	MinTokenBytes = 32
	// MaxTokenBytes bounds WithTokenBytes: 512 bits, a 86-character token,
	// already past any brute-force horizon and short enough for a URL.
	MaxTokenBytes = 64
)

// Event kinds the plugin emits through records.Store.Emit, inside the
// transaction of each change, so an audit log or a webhook sees share
// links the way it sees the Store's changes. Each has FileID (the linked
// file) and Subject (the link's ID); tokens never appear in events.
// Revoking a revoked link emits nothing; Prune is housekeeping on links
// that can no longer work, and emits nothing either.
const (
	// EventCreated: Detail has DetailExpiresAt (RFC 3339, UTC) and
	// DetailMaxDownloads (decimal, "0" for unlimited).
	EventCreated records.EventKind = PluginName + ".created"
	// EventRevoked: no Detail.
	EventRevoked records.EventKind = PluginName + ".revoked"
	// EventDownloaded is one download spent (Resolve): Detail has
	// DetailDownloads, the link's count including this one.
	EventDownloaded records.EventKind = PluginName + ".downloaded"
)

// EventKindValues lists every event kind the plugin emits.
var EventKindValues = []records.EventKind{EventCreated, EventRevoked, EventDownloaded}

// Detail keys of the plugin's events.
const (
	DetailExpiresAt    = "expires_at"
	DetailMaxDownloads = "max_downloads"
	DetailDownloads    = "downloads"
	// DetailTarget is the link's Target.Kind and DetailMode its Mode, on
	// EventCreated.
	DetailTarget = "target"
	DetailMode   = "mode"
)

// Share is a link as stored. Token is set only in the value Create returns.
// A link to one file has FileID and Target.Kind KindFile; a link to a
// collection (a folder, an owner's role) has Target and no FileID. Mode
// says whether it serves files or accepts uploads; MaxDownloads and
// Downloads count uses of either kind.
type Share struct {
	ID           string
	Scope        string
	FileID       string
	Target       Target
	Mode         string
	Token        string
	ExpiresAt    time.Time
	MaxDownloads int64
	Downloads    int64
	RevokedAt    *time.Time
	CreatedAt    time.Time
	// HasPassword says the link needs a password (WithPassword).
	HasPassword bool
}

// row is Share as stored, with the password hash kept out of Share.
type row struct {
	Share
	passwordHash string
}

func bindings(r *row) []sqlb.Binding {
	sh := &r.Share
	return []sqlb.Binding{
		sqlb.Bind(ID, &sh.ID), sqlb.Bind(Scope, &sh.Scope), sqlb.Bind(FileID, &sh.FileID),
		sqlb.Bind(ExpiresAt, &sh.ExpiresAt), sqlb.Bind(MaxDownloads, &sh.MaxDownloads),
		sqlb.Bind(Downloads, &sh.Downloads), sqlb.Bind(RevokedAt, &sh.RevokedAt), sqlb.Bind(CreatedAt, &sh.CreatedAt),
		sqlb.Bind(PasswordHash, &r.passwordHash), sqlb.Bind(TargetKind, &sh.Target.Kind), sqlb.Bind(TargetRef, &sh.Target.Ref),
		sqlb.Bind(Mode, &sh.Mode),
	}
}

// share returns the row as a Share, without its hash.
func (r row) share() Share {
	sh := r.Share
	sh.HasPassword = r.passwordHash != ""
	return sh
}

// Options describe a new link.
type Options struct {
	// TTL is the link's lifetime: required, at most MaxLifetime (or the
	// lower cap of WithMaxLifetime).
	TTL time.Duration
	// MaxDownloads limits how often the link works (downloads, or uploads
	// for a request link); 0 is unlimited.
	MaxDownloads int64
	// Password, when set, must be given to use the link (WithPassword).
	// Only its PBKDF2 hash is stored.
	Password string
}

// Plugin manages share links. Construct it with New and install it with
// records.WithPlugins.
type Plugin struct {
	now func() time.Time
	// tokenBytes is WithTokenBytes', MinTokenBytes by default.
	tokenBytes int
	// maxLifetime is WithMaxLifetime's cap, MaxLifetime by default.
	maxLifetime time.Duration
	// err is the first invalid option: Migrations and every link creation
	// return it, so a misconfigured plugin fails at startup, closed.
	err error
	// collections are WithCollections', by Kind.
	collections map[string]Collection
	// quarantineDeposits is WithQuarantinedDeposits.
	quarantineDeposits bool
}

// Option configures a Plugin.
type Option func(*Plugin)

// WithQuarantinedDeposits records every file uploaded through a request
// link (Deposit) as quarantined (records.Quarantine): bytes from whoever
// holds the link stay unusable until a worker scans them
// (records.Store.Scan). The deposit policy must not be public.
func WithQuarantinedDeposits() Option { return func(p *Plugin) { p.quarantineDeposits = true } }

// WithTokenBytes makes new tokens n random bytes long (base64url, so
// about 4n/3 characters), for an organisation whose policy asks for more
// than 256 bits. n must be in [MinTokenBytes, MaxTokenBytes]: anything
// shorter is refused, since short codes and vanity slugs would let links
// be guessed. Tokens stay crypto/rand bytes stored only as SHA-256; links
// made before keep working whatever their length.
func WithTokenBytes(n int) Option {
	return func(p *Plugin) {
		if n < MinTokenBytes || n > MaxTokenBytes {
			p.fail(filesystem.Errorf(filesystem.CodeInvalidArgument, "share: token bytes %d not in [%d, %d]", n, MinTokenBytes, MaxTokenBytes))
			return
		}
		p.tokenBytes = n
	}
}

// WithMaxLifetime lowers the longest TTL a link may have, for a policy
// such as "shared links last at most 30 days": Create, CreateCollection
// and CreateRequest refuse a longer TTL with CodeInvalidArgument. d must
// be in (0, MaxLifetime]; the cap cannot be raised, since a link that
// never expires should be a public file instead.
func WithMaxLifetime(d time.Duration) Option {
	return func(p *Plugin) {
		if d <= 0 || d > MaxLifetime {
			p.fail(filesystem.Errorf(filesystem.CodeInvalidArgument, "share: max lifetime %s not in (0, %s]", d, MaxLifetime))
			return
		}
		p.maxLifetime = d
	}
}

// fail keeps the first invalid option's error.
func (p *Plugin) fail(err error) {
	if p.err == nil {
		p.err = err
	}
}

// WithClock gives this plugin its own clock, for tests. Without it the
// plugin reads records.Store.Now, so records.WithClock rules the plugin
// and the Store alike; times are kept in UTC at microsecond precision
// either way.
func WithClock(now func() time.Time) Option { return func(p *Plugin) { p.now = now } }

// New returns a Plugin.
func New(opts ...Option) *Plugin {
	p := &Plugin{collections: map[string]Collection{}, tokenBytes: MinTokenBytes, maxLifetime: MaxLifetime}
	for _, o := range opts {
		o(p)
	}
	return p
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
// ds:def id=plugin-migrations-8x5tnuwt
func (p *Plugin) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	st, err := records.NewDDL(engine).CreateTable(records.Table{
		Table: Table,
		Columns: []ddl.ColumnSpec{
			records.Column(ID, t.Key), records.Column(Scope, t.Key), records.Column(FileID, t.Key), records.Column(TokenHash, t.Short),
			records.Column(ExpiresAt, t.Timestamp), records.Column(MaxDownloads, t.Int), records.Column(Downloads, t.Int),
			records.Column(RevokedAt, t.Timestamp), records.Column(CreatedAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{ID},
		Indexes: []records.Index{
			{Name: "fs_shares_token", Unique: true, Columns: []records.IndexColumn{records.Key(TokenHash)}},
			{Name: "fs_shares_file", Columns: []records.IndexColumn{records.Key(FileID)}},
		},
	}).Statements()
	more, err2 := records.NewDDL(engine).
		AddColumn(Table, records.Column(PasswordHash, t.Text).Default("")).
		AddColumn(Table, records.Column(TargetKind, t.Short).Default(KindFile)).
		AddColumn(Table, records.Column(TargetRef, t.Key).Default("")).
		AddColumn(Table, records.Column(Mode, t.Short).Default(ModeDownload)).
		CreateTable(records.Table{
			Table: EventsTable,
			Columns: []ddl.ColumnSpec{
				records.Column(EventID, t.Key), records.Column(EventScope, t.Key), records.Column(EventShareID, t.Key),
				records.Column(EventAt, t.Timestamp), records.Column(EventViewer, t.Key),
			},
			PrimaryKey: []ddl.ColumnRef{EventID},
			Indexes: []records.Index{
				{Name: "fs_share_events_share", Columns: []records.IndexColumn{records.Key(EventShareID), records.Key(EventAt), records.Key(EventID)}},
			},
		}).Statements()
	return []records.Migration{
		{Owner: PluginName, Name: "0001_shares", Statements: st},
		{Owner: PluginName, Name: "0002_shares_access", Statements: more},
	}, errors.Join(p.err, err, err2)
}

// BeforeCreate implements records.FileHooks.
func (p *Plugin) BeforeCreate(context.Context, *records.Store, *records.File) error { return nil }

// AfterCreate implements records.FileHooks: the file a Deposit records
// is placed in its link's target and spends the link here, in the
// transaction that records it (see Deposit).
func (p *Plugin) AfterCreate(ctx context.Context, s *records.Store, f records.File) error {
	d, _ := ctx.Value(depositKey{}).(*pendingDeposit)
	if d == nil || d.placed || f.Scope != d.link.Scope {
		return nil
	}
	d.placed = true
	if err := d.recv.Accept(ctx, s, d.link.Scope, d.link.Target.Ref, f); err != nil {
		return err
	}
	return p.spend(ctx, s, d.link, f.ID)
}

// BeforePurge implements records.FileHooks: a purged file's links go too,
// with their download records.
func (p *Plugin) BeforePurge(ctx context.Context, s *records.Store, f records.File) error {
	links := sqlb.NewSelect(s.Dialect()).Exprs(ID).From(Table).Where(sqlb.Eq(Scope, f.Scope), sqlb.Eq(FileID, f.ID))
	if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(EventsTable).Where(sqlb.InSubquery(EventShareID, links))); err != nil {
		return err
	}
	_, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Scope, f.Scope), sqlb.Eq(FileID, f.ID)))
	return err
}

// PurgeScope implements records.ScopePurger: once Store.PurgeScope has
// purged every file of scope, any link of the scope left behind goes too
// (BeforePurge already removed each file's links). It emits no event.
func (p *Plugin) PurgeScope(ctx context.Context, s *records.Store, scope string) error {
	if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(EventsTable).Where(sqlb.Eq(EventScope, scope))); err != nil {
		return err
	}
	_, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Scope, scope)))
	return err
}

func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Create makes a link to a live file of scope. The returned Share carries
// the token; it cannot be recovered later. A quarantined file cannot be
// shared until its scan passes (records.ErrQuarantined); a file
// quarantined after its link was made is refused to the link's visitors
// as not found.
func (p *Plugin) Create(ctx context.Context, s *records.Store, scope, fileID string, o Options) (Share, error) {
	f, err := s.File(ctx, scope, fileID)
	if err != nil {
		return Share{}, err
	}
	if f.DeletedAt != nil {
		return Share{}, filesystem.Errorf(filesystem.CodeNotFound, "share: file %q is in the trash", fileID)
	}
	if f.Quarantined() {
		return Share{}, records.ErrQuarantined
	}
	return p.create(ctx, s, scope, fileID, Target{Kind: KindFile}, ModeDownload, o)
}

// create inserts a link and emits EventCreated.
func (p *Plugin) create(ctx context.Context, s *records.Store, scope, fileID string, target Target, mode string, o Options) (Share, error) {
	if p.err != nil {
		return Share{}, p.err
	}
	if o.TTL <= 0 || o.TTL > p.maxLifetime || o.MaxDownloads < 0 {
		return Share{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "share: TTL must be in (0, %s] and MaxDownloads not negative", p.maxLifetime)
	}
	pwHash := ""
	if o.Password != "" {
		var err error
		if pwHash, err = hashPassword(o.Password); err != nil {
			return Share{}, err
		}
	}
	raw := make([]byte, p.tokenBytes)
	_, _ = rand.Read(raw) // crypto/rand.Read never fails (it aborts the process instead)
	now := p.clock(s)
	sh := Share{
		ID: s.NewID(IDPrefix), Scope: scope, FileID: fileID, Target: target, Mode: mode,
		Token: base64.RawURLEncoding.EncodeToString(raw), ExpiresAt: now.Add(o.TTL), MaxDownloads: o.MaxDownloads, CreatedAt: now,
		HasPassword: pwHash != "",
	}
	ins := sqlb.NewInsert(s.Dialect()).Into(Table).Row(
		sqlb.Set(ID, sh.ID), sqlb.Set(Scope, sh.Scope), sqlb.Set(FileID, sh.FileID), sqlb.Set(TokenHash, hash(sh.Token)),
		sqlb.Set(ExpiresAt, sh.ExpiresAt), sqlb.Set(MaxDownloads, sh.MaxDownloads), sqlb.Set(Downloads, int64(0)),
		sqlb.Set(CreatedAt, sh.CreatedAt), sqlb.Set(PasswordHash, pwHash), sqlb.Set(TargetKind, target.Kind),
		sqlb.Set(TargetRef, target.Ref), sqlb.Set(Mode, mode),
	)
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := s.Exec(ctx, ins); err != nil {
			return err
		}
		return s.Emit(ctx, p, records.Event{Kind: EventCreated, Scope: scope, FileID: fileID, Subject: sh.ID, Detail: map[string]string{
			DetailExpiresAt: sh.ExpiresAt.Format(time.RFC3339Nano), DetailMaxDownloads: strconv.FormatInt(sh.MaxDownloads, 10),
			DetailTarget: target.Kind, DetailMode: mode,
		}})
	})
	if err != nil {
		return Share{}, err
	}
	return sh, nil
}

// errGone is the one error a visitor sees for any unusable link.
func errGone() error {
	return filesystem.Errorf(filesystem.CodeNotFound, "share: link not found or no longer valid")
}

// Resolve spends one download of the file link with token and returns its
// file. Any unusable link (unknown, expired, revoked, exhausted, its file
// trashed, expired or a view-once file already seen, or a collection or
// request link) is CodeNotFound with the same message, and spends nothing:
// the file is checked, and its view counted (records.Store.Viewed), before
// the download is counted, in one transaction. A link with a password needs WithPassword; a missing or wrong
// one is ErrPassword and spends nothing either.
func (p *Plugin) Resolve(ctx context.Context, s *records.Store, token string, opts ...AccessOption) (records.File, error) {
	sh, err := p.usable(ctx, s, token, ModeDownload, opts)
	if err != nil {
		return records.File{}, err
	}
	if sh.Target.Kind != KindFile {
		return records.File{}, errGone()
	}
	f, err := p.liveFile(ctx, s, sh.Scope, sh.FileID)
	if err != nil {
		return records.File{}, err
	}
	if err := p.take(ctx, s, sh, f); err != nil {
		return records.File{}, err
	}
	return f, nil
}

// take is one download of f through sh: in one transaction it counts the
// file's view (records.Store.Viewed) and only then spends a use, so an
// expired file, or a view-once file already seen, is errGone and spends
// nothing, and a view is never counted for a download the link refused.
func (p *Plugin) take(ctx context.Context, s *records.Store, sh Share, f records.File) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		// A file quarantined since liveFile read it is refused like an
		// expired one: the uniform not-found, nothing spent.
		ok, err := s.Viewed(ctx, f.Scope, f.ID)
		switch {
		case errors.Is(err, records.ErrQuarantined), err == nil && !ok:
			return errGone()
		case err != nil:
			return err
		}
		return p.spend(ctx, s, sh, f.ID)
	})
}

// spend counts one use of the link with one conditional UPDATE, so
// MaxDownloads holds under concurrent requests, records it for Stats
// (fileID is what was downloaded, or uploaded through a request link),
// and emits EventDownloaded. It joins the caller's transaction.
func (p *Plugin) spend(ctx context.Context, s *records.Store, sh Share, fileID string) error {
	now := p.clock(s)
	where := []sqlb.Predicate{sqlb.Eq(ID, sh.ID), sqlb.IsNull(RevokedAt), sqlb.Greater(ExpiresAt, now)}
	if sh.MaxDownloads > 0 {
		where = append(where, sqlb.Less(Downloads, sh.MaxDownloads))
	}
	upd := sqlb.NewUpdate(s.Dialect()).Table(Table).Set(sqlb.SetExpr(Downloads, sqlb.Add(Downloads, sqlb.Param[int64](1)))).Where(where...)
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		n, err := s.Exec(ctx, upd)
		if err != nil {
			return err
		}
		if n != 1 {
			return errGone()
		}
		// Read the count back rather than adding one to what was read
		// before: another download may have come in between, and the
		// UPDATE's row lock, held to the commit, makes this read the count
		// this download made.
		var downloads int64
		if err := s.One(ctx, sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(Downloads, &downloads)).From(Table).Where(sqlb.Eq(ID, sh.ID))); err != nil {
			return err
		}
		if err := p.record(ctx, s, sh, now); err != nil {
			return err
		}
		return s.Emit(ctx, p, records.Event{Kind: EventDownloaded, Scope: sh.Scope, FileID: fileID, Subject: sh.ID, Detail: map[string]string{
			DetailDownloads: strconv.FormatInt(downloads, 10),
		}})
	})
}

// Check returns the file of the file link with token if the link is
// usable now, without spending a download; it fails as Resolve does. Use
// it to answer a HEAD or to show a landing page before the download.
func (p *Plugin) Check(ctx context.Context, s *records.Store, token string, opts ...AccessOption) (records.File, error) {
	sh, err := p.usable(ctx, s, token, ModeDownload, opts)
	if err != nil {
		return records.File{}, err
	}
	if sh.Target.Kind != KindFile {
		return records.File{}, errGone()
	}
	return p.liveFile(ctx, s, sh.Scope, sh.FileID)
}

// usable returns the link with token when it is in mode and usable now:
// not revoked, expired or exhausted, and its password (if any) given.
// Every other failure is errGone; a missing or wrong password is
// ErrPassword. It spends nothing.
func (p *Plugin) usable(ctx context.Context, s *records.Store, token, mode string, opts []AccessOption) (Share, error) {
	r, err := p.lookupRow(ctx, s, token)
	if err != nil {
		return Share{}, err
	}
	sh := r.share()
	if sh.Mode != mode || !p.alive(s, sh) {
		return Share{}, errGone()
	}
	if r.passwordHash != "" && !checkPassword(r.passwordHash, accessOf(opts).password) {
		return Share{}, ErrPassword
	}
	return sh, nil
}

// alive reports whether sh can still be used at the plugin's clock: not
// revoked, not expired, uses left.
func (p *Plugin) alive(s *records.Store, sh Share) bool {
	return sh.RevokedAt == nil && sh.ExpiresAt.After(p.clock(s)) && (sh.MaxDownloads == 0 || sh.Downloads < sh.MaxDownloads)
}

// peek returns the link with token, usable or not, without checking its
// password: what the Handler routes by. An unknown link is errGone.
func (p *Plugin) peek(ctx context.Context, s *records.Store, token string) (Share, error) {
	r, err := p.lookupRow(ctx, s, token)
	return r.share(), err
}

// lookupRow reads the link with token; an unknown link is errGone.
func (p *Plugin) lookupRow(ctx context.Context, s *records.Store, token string) (row, error) {
	var r row
	q := sqlb.NewSelect(s.Dialect()).Columns(bindings(&r)...).From(Table).Where(sqlb.Eq(TokenHash, hash(token)))
	if err := s.One(ctx, q); err != nil {
		if errors.Is(err, sqlb.ErrNoRows) {
			return row{}, errGone()
		}
		return row{}, err
	}
	return r, nil
}

// liveFile is a link's file if it exists and is live; otherwise errGone.
func (p *Plugin) liveFile(ctx context.Context, s *records.Store, scope, fileID string) (records.File, error) {
	f, err := s.File(ctx, scope, fileID)
	// A quarantined file looks like any other unusable one: a visitor
	// learns nothing about a scan (see records.ErrQuarantined).
	if filesystem.CodeOf(err) == filesystem.CodeNotFound || err == nil && (f.DeletedAt != nil || f.Quarantined()) {
		return records.File{}, errGone()
	}
	return f, err
}

// Prune deletes the links that expired, or were revoked, before cutoff, in
// every scope, with their download records, and returns how many links it
// deleted. Such links can never work again; keeping them until cutoff
// lets Shares and Stats still show recent ones. Schedule it (the library
// starts no background work); a link exhausted by MaxDownloads goes once
// it expires, within MaxLifetime.
func (p *Plugin) Prune(ctx context.Context, s *records.Store, cutoff time.Time) (int64, error) {
	at := cutoff.UTC()
	dead := sqlb.Or(
		sqlb.Less(ExpiresAt, at),
		sqlb.And(sqlb.IsNotNull(RevokedAt), sqlb.Less(RevokedAt, &at)),
	)
	var n int64
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		links := sqlb.NewSelect(s.Dialect()).Exprs(ID).From(Table).Where(dead)
		if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(EventsTable).Where(sqlb.InSubquery(EventShareID, links))); err != nil {
			return err
		}
		var err error
		n, err = s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(dead))
		return err
	})
	return n, err
}

// Revoke ends a link at once. Revoking twice, or an unknown link, does
// nothing. Its download records stay until Prune.
func (p *Plugin) Revoke(ctx context.Context, s *records.Store, scope, shareID string) error {
	now := p.clock(s)
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var r row
		q := sqlb.NewSelect(s.Dialect()).Columns(bindings(&r)...).From(Table).Where(sqlb.Eq(Scope, scope), sqlb.Eq(ID, shareID))
		if err := s.One(ctx, q); err != nil {
			if errors.Is(err, sqlb.ErrNoRows) {
				return nil
			}
			return err
		}
		upd := sqlb.NewUpdate(s.Dialect()).Table(Table).Set(sqlb.Set(RevokedAt, &now)).
			Where(sqlb.Eq(Scope, scope), sqlb.Eq(ID, shareID), sqlb.IsNull(RevokedAt))
		n, err := s.Exec(ctx, upd)
		if err != nil || n == 0 {
			return err
		}
		return s.Emit(ctx, p, records.Event{Kind: EventRevoked, Scope: scope, FileID: r.FileID, Subject: shareID})
	})
}

// Shares lists the links of a file, newest first, without tokens.
func (p *Plugin) Shares(ctx context.Context, s *records.Store, scope, fileID string) ([]Share, error) {
	return p.list(ctx, s, sqlb.Eq(Scope, scope), sqlb.Eq(FileID, fileID), sqlb.Eq(TargetKind, KindFile))
}

func (p *Plugin) list(ctx context.Context, s *records.Store, where ...sqlb.Predicate) ([]Share, error) {
	var r row
	var out []Share
	q := sqlb.NewSelect(s.Dialect()).Columns(bindings(&r)...).From(Table).
		Where(where...).Order(sqlb.OrderBy(CreatedAt, sqlb.SortDesc), sqlb.OrderBy(ID, sqlb.SortDesc))
	if err := s.Each(ctx, q, func() error { out = append(out, r.share()); return nil }); err != nil {
		return nil, err
	}
	return out, nil
}
