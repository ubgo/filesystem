// Package locks is a records plugin for check-out: a subject locks a file
// for a while, and until it unlocks it or the lock expires, nobody else
// may replace its content (records.Store.Replace and ReplaceObject), trash
// it or purge it: each is refused with ErrLocked, and an administrator
// Breaks the lock first. Changing its visibility, status or metadata stays
// allowed. The "check out to edit" of document managers, and the guard an
// editor takes before a long upload. A purge with no actor (SweepTrash,
// SweepExpired) is refused too while the lock holds, and succeeds on a
// later run once it expired. Store.PurgeScope, tenant offboarding, is
// never refused.
//
// Who holds a lock is the actor of the request (records.WithActor, or the
// function given to WithOwnerFunc, which may name a device as well as a
// user), so the check needs no new argument on the Store's calls. Every
// lock carries a random token as well; WithHolderFunc decides who passes
// a lock (TokenHolder asks for the token too, as WebDAV's If header does).
// Expired locks stop counting at once and are deleted by Sweep, on the
// application's schedule.
//
// A lock is exclusive: one holder. WithSharedLocks adds shared locks,
// WebDAV's shared write lock: several holders at once (LockShared), each
// with its own token, and any of them may change the file while nobody
// else may. An exclusive lock and shared locks never hold together: Lock
// is refused while another subject holds a shared lock, and LockShared
// while anyone holds an exclusive one.
package locks

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// PluginName is the plugin's name in the migration ledger.
const PluginName = "locks"

// TableNameLocks is the table the plugin owns: one row per locked file.
const TableNameLocks = "fs_locks"

// Column names of fs_locks.
const (
	ColScope     = "scope"
	ColFileID    = "file_id"
	ColOwner     = "owner"
	ColExpiresAt = "expires_at"
	ColLockedAt  = "locked_at"
	// ColToken is the lock's token (migration 0002_lock_tokens); "" for a
	// lock taken before that migration.
	ColToken = "token"
)

// TableNameShared is the table of shared locks (WithSharedLocks): one row
// per holder of a file's shared lock.
const TableNameShared = "fs_shared_locks"

// Table and typed columns.
var (
	Table     = sqlb.Table{Name: TableNameLocks, Alias: "lk"}
	Scope     = sqlb.Col[string](Table, ColScope)
	FileID    = sqlb.Col[string](Table, ColFileID)
	Owner     = sqlb.Col[string](Table, ColOwner)
	ExpiresAt = sqlb.Col[time.Time](Table, ColExpiresAt)
	LockedAt  = sqlb.Col[time.Time](Table, ColLockedAt)
	Token     = sqlb.Col[string](Table, ColToken)

	// SharedTable has fs_locks' columns, keyed by (scope, file, owner).
	SharedTable     = sqlb.Table{Name: TableNameShared, Alias: "lks"}
	SharedScope     = sqlb.Col[string](SharedTable, ColScope)
	SharedFileID    = sqlb.Col[string](SharedTable, ColFileID)
	SharedOwner     = sqlb.Col[string](SharedTable, ColOwner)
	SharedExpiresAt = sqlb.Col[time.Time](SharedTable, ColExpiresAt)
	SharedLockedAt  = sqlb.Col[time.Time](SharedTable, ColLockedAt)
	SharedToken     = sqlb.Col[string](SharedTable, ColToken)
)

// Mode is how a lock is held.
type Mode string

// The closed set of lock modes.
const (
	// Exclusive is one holder; Lock takes it.
	Exclusive Mode = "exclusive"
	// Shared is one of several holders; LockShared takes it
	// (WithSharedLocks).
	Shared Mode = "shared"
)

// ModeValues lists every Mode.
var ModeValues = []Mode{Exclusive, Shared}

// TokenPrefix starts every lock token, followed by 128 random bits.
const TokenPrefix = "lkt_"

// MaxOwnerBytes is the longest owner, the indexed width of its column.
const MaxOwnerBytes = records.MaxActorBytes

// ErrLocked is the error of a change refused because another owner holds
// the file's lock (CodeConflict).
var ErrLocked = filesystem.Errorf(filesystem.CodeConflict, "locks: the file is checked out by someone else")

// Event kinds the plugin emits through records.Store.Emit: FileID is the
// file, Subject the lock's owner; EventLocked has DetailExpiresAt.
const (
	EventLocked   records.EventKind = PluginName + ".locked"
	EventUnlocked records.EventKind = PluginName + ".unlocked"
)

// EventKindValues lists every event kind the plugin emits.
var EventKindValues = []records.EventKind{EventLocked, EventUnlocked}

// DetailExpiresAt is EventLocked's expiry, RFC 3339 in UTC.
const DetailExpiresAt = "expires_at"

// DetailMode is Shared on the events of a shared lock; absent for an
// exclusive one, so those read as they always have.
const DetailMode = "mode"

// Lock is a file's check-out.
type Lock struct {
	Scope     string
	FileID    string
	Owner     string
	ExpiresAt time.Time
	LockedAt  time.Time
	// Token is the lock's random token (TokenPrefix and 128 random bits,
	// or WithTokenFunc's rendering of 128 random bits),
	// made when the lock is taken and kept while its owner extends it:
	// WebDAV's opaquelocktoken, handed to the client that locked. It is
	// never put in an event. "" for a lock taken before tokens existed.
	// Each holder of a shared lock has its own.
	Token string
	// Mode is Exclusive or Shared; "" reads as Exclusive.
	Mode Mode
}

func bindings(l *Lock) []sqlb.Binding {
	return []sqlb.Binding{
		sqlb.Bind(Scope, &l.Scope), sqlb.Bind(FileID, &l.FileID), sqlb.Bind(Owner, &l.Owner),
		sqlb.Bind(ExpiresAt, &l.ExpiresAt), sqlb.Bind(LockedAt, &l.LockedAt), sqlb.Bind(Token, &l.Token),
	}
}

func sharedBindings(l *Lock) []sqlb.Binding {
	return []sqlb.Binding{
		sqlb.Bind(SharedScope, &l.Scope), sqlb.Bind(SharedFileID, &l.FileID), sqlb.Bind(SharedOwner, &l.Owner),
		sqlb.Bind(SharedExpiresAt, &l.ExpiresAt), sqlb.Bind(SharedLockedAt, &l.LockedAt), sqlb.Bind(SharedToken, &l.Token),
	}
}

// newToken is a new lock token: WithTokenFunc's rendering of
// TokenRandomBytes random bytes when set, else TokenPrefix and
// rand.Text's 128-plus random bits.
func (p *Plugin) newToken() (string, error) {
	if p.tokenErr != nil {
		return "", p.tokenErr
	}
	if p.tokenFunc == nil {
		return TokenPrefix + strings.ToLower(rand.Text()), nil
	}
	var raw [TokenRandomBytes]byte
	_, _ = rand.Read(raw[:]) // crypto/rand.Read never fails
	return p.renderToken(raw)
}

// TokenRandomBytes is how many random bytes a TokenFunc renders: 128
// bits, the floor today's tokens set.
const TokenRandomBytes = 16

// MaxTokenBytes is the longest token, the indexed width of its column.
const MaxTokenBytes = records.MaxActorBytes

// TokenFunc renders a lock token from TokenRandomBytes random bytes, in a
// shape a front end needs: WebDAV's "opaquelocktoken:" or "urn:uuid:"
// URIs. It must encode every bit of random (a UUID's version bits would
// drop six of them, so write all 128 as hex in the UUID's layout), and
// return 1 to MaxTokenBytes bytes.
type TokenFunc func(random [TokenRandomBytes]byte) string

// WithTokenFunc makes every new lock token with fn instead of TokenPrefix
// and random text. The randomness stays the plugin's: fn only renders
// the bytes it is given, so the token keeps today's 128-bit floor. New
// checks that fn's output changes when any one of the 128 bits does and
// that it fits MaxTokenBytes; a fn that drops a bit, or answers empty or
// too long, makes every Lock and LockShared that needs a new token refuse
// (CodeInvalidArgument), so no weaker token is ever handed out. Tokens already stored keep their form.
func WithTokenFunc(fn TokenFunc) Option { return func(p *Plugin) { p.tokenFunc = fn } }

// renderToken is fn's token for raw, its length checked.
func (p *Plugin) renderToken(raw [TokenRandomBytes]byte) (string, error) {
	tok := p.tokenFunc(raw)
	if tok == "" || len(tok) > MaxTokenBytes {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "locks: a token is 1 to %d bytes", MaxTokenBytes)
	}
	return tok, nil
}

// checkTokenFunc refuses a TokenFunc whose output does not change when one
// bit of its input does: one that drops part of the randomness.
func (p *Plugin) checkTokenFunc() error {
	var base [TokenRandomBytes]byte
	ref, err := p.renderToken(base)
	if err != nil {
		return err
	}
	for i := range TokenRandomBytes * 8 {
		flipped := base
		flipped[i/8] ^= 1 << (i % 8)
		tok, err := p.renderToken(flipped)
		if err != nil {
			return err
		}
		if tok == ref {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "locks: the token function ignores bit %d of its random input", i)
		}
	}
	return nil
}

// Action is the change a HolderFunc is asked about.
type Action string

// The closed set of Actions.
const (
	ActionReplace Action = "replace"
	ActionTrash   Action = "trash"
	ActionPurge   Action = "purge"
)

// HolderRequest is what a HolderFunc decides: whether the request may
// change a file under Lock. It is a struct so fields can be added without
// breaking implementations.
type HolderRequest struct {
	// Lock is the file's live lock.
	Lock Lock
	// Owner is who the request acts for (records.ActorFrom, or
	// WithOwnerFunc's).
	Owner string
	// Action is the change asked: replace, trash or purge. A rule such as
	// "an admin may purge a locked file but not overwrite it" decides on
	// it without Break.
	Action Action
	// File is the file as the change found it (for a trash, as the trash
	// left it).
	File records.File
}

// HolderFunc reports whether the request in ctx holds req.Lock, and so may
// make req.Action to req.File. DefaultHolder is the default.
type HolderFunc func(ctx context.Context, req HolderRequest) (bool, error)

// DefaultHolder holds a lock when the request's owner is the lock's.
func DefaultHolder(_ context.Context, req HolderRequest) (bool, error) {
	return req.Owner == req.Lock.Owner, nil
}

type tokenKey struct{}

// WithLockToken returns ctx carrying the lock token a request presents
// (WebDAV's If header), for TokenHolder.
func WithLockToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, tokenKey{}, token)
}

// LockTokenFrom returns the token set with WithLockToken, or "".
func LockTokenFrom(ctx context.Context) string {
	t, _ := ctx.Value(tokenKey{}).(string)
	return t
}

// TokenHolder is a HolderFunc that asks for the lock's owner and its token
// (WithLockToken), compared in constant time: WebDAV's rule, where a
// client must present the token it got when it locked, so one user's two
// clients (or tabs) do not overwrite each other. A lock with no token
// (taken before tokens existed) is held by its owner alone.
func TokenHolder(ctx context.Context, req HolderRequest) (bool, error) {
	if req.Owner != req.Lock.Owner {
		return false, nil
	}
	if req.Lock.Token == "" {
		return true, nil
	}
	return subtle.ConstantTimeCompare([]byte(LockTokenFrom(ctx)), []byte(req.Lock.Token)) == 1, nil
}

// Plugin keeps locks. Construct it with New and install it with
// records.WithPlugins.
type Plugin struct {
	now    func() time.Time
	owner  func(context.Context) string
	maxTTL time.Duration
	// holder is WithHolderFunc's; DefaultHolder otherwise.
	holder HolderFunc
	// shared is WithSharedLocks: LockShared is allowed. Shared locks
	// already stored are honoured either way.
	shared bool
	// tokenFunc is WithTokenFunc's; nil makes TokenPrefix tokens.
	// tokenErr is its check's failure, returned by every lock taken.
	tokenFunc TokenFunc
	tokenErr  error
}

// DefaultMaxTTL is the longest lock Lock takes unless WithMaxTTL says
// otherwise: a working day, so a forgotten check-out frees itself
// overnight instead of blocking trash and purges until someone Breaks it.
const DefaultMaxTTL = 8 * time.Hour

// Option configures a Plugin.
type Option func(*Plugin)

// WithOwnerFunc replaces the Store's actor lookup (records.Store.Actor,
// which records.WithActorFunc sets for the Store and every plugin) as the
// way a request names who it acts for, for this plugin alone: to name a
// device as well as a user, say. Without it the owner is s.Actor(ctx).
func WithOwnerFunc(fn func(context.Context) string) Option { return func(p *Plugin) { p.owner = fn } }

// WithHolderFunc decides with fn, instead of DefaultHolder, whether a
// request holds a file's live lock and may change the file: TokenHolder
// for WebDAV-style tokens, or a rule of the application's (a team lead
// passing a member's lock). It fails closed: an error from fn refuses the
// change with that error, as a false refuses it with ErrLocked. Locking
// and unlocking still go by owner.
func WithHolderFunc(fn HolderFunc) Option { return func(p *Plugin) { p.holder = fn } }

// WithSharedLocks enables shared locks (LockShared, UnlockShared,
// SharedLocksOf): several subjects hold a file's lock together, each with
// its own token, as WebDAV's shared write lock. While any holds one, a
// request may change the file when the HolderFunc says it holds one of
// them (WithHolderFunc applies to each shared lock in turn, so
// TokenHolder asks for that holder's own token), and nobody else may. An
// exclusive lock and shared locks exclude each other: Lock is ErrLocked
// while another subject holds a live shared lock (a subject's own shared
// lock is given up when it takes the exclusive one), and LockShared is
// ErrLocked while anyone holds a live exclusive lock. Without it
// LockShared is CodeUnsupported; shared locks already stored still guard
// their files, so turning it off never unlocks a file.
func WithSharedLocks() Option { return func(p *Plugin) { p.shared = true } }

// WithMaxTTL sets the longest lock Lock takes (DefaultMaxTTL otherwise);
// a longer ttl is refused, not shortened, so a caller never holds a lock
// shorter than it believes. An editor that needs longer extends its own
// lock by locking again. max must be positive.
func WithMaxTTL(max time.Duration) Option { return func(p *Plugin) { p.maxTTL = max } }

// WithClock gives this plugin its own clock, for tests. Without it the
// plugin reads records.Store.Now, so records.WithClock rules the plugin
// and the Store alike; times are kept in UTC at microsecond precision
// either way.
func WithClock(now func() time.Time) Option { return func(p *Plugin) { p.now = now } }

// New returns a Plugin.
func New(opts ...Option) *Plugin {
	p := &Plugin{maxTTL: DefaultMaxTTL, holder: DefaultHolder}
	for _, o := range opts {
		o(p)
	}
	if p.tokenFunc != nil {
		p.tokenErr = p.checkTokenFunc()
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
func (p *Plugin) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	st, err := records.NewDDL(engine).CreateTable(records.Table{
		Table: Table,
		Columns: []ddl.ColumnSpec{
			records.Column(Scope, t.Key), records.Column(FileID, t.Key), records.Column(Owner, t.Key),
			records.Column(ExpiresAt, t.Timestamp), records.Column(LockedAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{Scope, FileID},
		Indexes:    []records.Index{{Name: "fs_locks_expires", Columns: []records.IndexColumn{records.Key(ExpiresAt)}}},
	}).Statements()
	tokens, err2 := records.NewDDL(engine).AddColumn(Table, records.Column(Token, t.Key).Default("")).Statements()
	shared, err3 := records.NewDDL(engine).CreateTable(records.Table{
		Table: SharedTable,
		Columns: []ddl.ColumnSpec{
			records.Column(SharedScope, t.Key), records.Column(SharedFileID, t.Key), records.Column(SharedOwner, t.Key),
			records.Column(SharedExpiresAt, t.Timestamp), records.Column(SharedLockedAt, t.Timestamp), records.Column(SharedToken, t.Key),
		},
		PrimaryKey: []ddl.ColumnRef{SharedScope, SharedFileID, SharedOwner},
		Indexes:    []records.Index{{Name: "fs_shared_locks_expires", Columns: []records.IndexColumn{records.Key(SharedExpiresAt)}}},
	}).Statements()
	return []records.Migration{
		{Owner: PluginName, Name: "0001_locks", Statements: st},
		{Owner: PluginName, Name: "0002_lock_tokens", Statements: tokens},
		{Owner: PluginName, Name: "0003_shared_locks", Statements: shared},
	}, errors.Join(err, err2, err3)
}

// Lock checks a live file of scope out to owner for ttl. A file unlocked,
// whose lock expired, or locked by owner already (which extends it) is
// locked, with a new token (Lock.Token) unless owner is extending its own
// live lock, which keeps its token; one held by another owner is
// ErrLocked. Two owners racing for a
// free file get one lock: the other is ErrLocked or CodeUnavailable, to
// retry. A ttl past the plugin's maximum (WithMaxTTL) is
// CodeInvalidArgument.
func (p *Plugin) Lock(ctx context.Context, s *records.Store, scope, fileID, owner string, ttl time.Duration) (Lock, error) {
	if owner == "" || len(owner) > MaxOwnerBytes || ttl <= 0 || ttl > p.maxTTL {
		return Lock{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "locks: a lock needs an owner of 1 to %d bytes and a ttl in (0, %s]", MaxOwnerBytes, p.maxTTL)
	}
	now := p.clock(s)
	l := Lock{Scope: scope, FileID: fileID, Owner: owner, ExpiresAt: now.Add(ttl), LockedAt: now}
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		// The file's row stays locked to the commit, as the guards lock it,
		// so a change racing this lock either commits first or waits and
		// then sees it.
		trashed, found, err := lockFileRow(ctx, s, scope, fileID)
		if err != nil {
			return err
		}
		if !found {
			return filesystem.Errorf(filesystem.CodeNotFound, "locks: file %q not found", fileID)
		}
		if trashed {
			return filesystem.Errorf(filesystem.CodeNotFound, "locks: file %q is in the trash", fileID)
		}
		// Shared holders other than owner keep the exclusive lock out;
		// owner's own shared lock is given up for it.
		others, err := p.sharedHolders(ctx, s, scope, fileID, now)
		if err != nil {
			return err
		}
		for _, o := range others {
			if o.Owner != owner {
				return ErrLocked
			}
		}
		if len(others) > 0 {
			if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(SharedTable).Where(
				sqlb.Eq(SharedScope, scope), sqlb.Eq(SharedFileID, fileID), sqlb.Eq(SharedOwner, owner))); err != nil {
				return err
			}
		}
		// An owner extending its live lock keeps the token its client
		// holds; any other taking gets a new one. The file's row lock
		// above keeps this read current until the commit.
		cur, held, err := p.current(ctx, s, scope, fileID, now)
		if err != nil {
			return err
		}
		if held && cur.Owner == owner && cur.Token != "" {
			l.Token = cur.Token
		} else if l.Token, err = p.newToken(); err != nil {
			return err
		}
		// Take it if it is free or ours: one conditional UPDATE, then an
		// INSERT when there was no row at all.
		upd := sqlb.NewUpdate(s.Dialect()).Table(Table).Set(sqlb.Set(Owner, owner), sqlb.Set(ExpiresAt, l.ExpiresAt), sqlb.Set(LockedAt, now), sqlb.Set(Token, l.Token)).
			Where(sqlb.Eq(Scope, scope), sqlb.Eq(FileID, fileID), sqlb.Or(sqlb.Eq(Owner, owner), sqlb.LessEq(ExpiresAt, now)))
		n, err := s.Exec(ctx, upd)
		if err != nil {
			return err
		}
		if n == 0 {
			// No row counted: none exists, another owner holds it, or (on
			// MySQL, which counts only rows whose values changed) owner
			// re-took it with the same times, which changed nothing.
			cur, held, err := p.current(ctx, s, scope, fileID, now)
			if err != nil {
				return err
			}
			if held && cur.Owner != owner {
				return ErrLocked
			}
			if !held {
				if err := p.insert(ctx, s, l); err != nil {
					return err
				}
			}
		}
		return s.Emit(ctx, p, records.Event{Kind: EventLocked, Scope: scope, FileID: fileID, Subject: owner,
			Detail: map[string]string{DetailExpiresAt: l.ExpiresAt.Format(time.RFC3339Nano)}})
	})
	if err != nil {
		return Lock{}, err
	}
	return l, nil
}

// insert writes l as a file's first lock row. Two first locks racing make
// one INSERT fail, surfaced as CodeUnavailable for the caller to retry.
func (p *Plugin) insert(ctx context.Context, s *records.Store, l Lock) error {
	ins := sqlb.NewInsert(s.Dialect()).Into(Table).Row(sqlb.Set(Scope, l.Scope), sqlb.Set(FileID, l.FileID), sqlb.Set(Owner, l.Owner),
		sqlb.Set(ExpiresAt, l.ExpiresAt), sqlb.Set(LockedAt, l.LockedAt), sqlb.Set(Token, l.Token))
	_, err := s.Exec(ctx, ins)
	if records.IsUniqueViolation(err) {
		return filesystem.NewError(filesystem.CodeUnavailable, err)
	}
	return err
}

// Unlock checks a file back in. Only owner's lock is removed; unlocking a
// file owner does not hold (unlocked, expired, or someone else's) does
// nothing.
func (p *Plugin) Unlock(ctx context.Context, s *records.Store, scope, fileID, owner string) error {
	return p.remove(ctx, s, scope, fileID, sqlb.Eq(Owner, owner))
}

// Break removes a file's lock whoever holds it, and every shared lock on
// it: an administrator's override. Breaking an unlocked file does
// nothing.
func (p *Plugin) Break(ctx context.Context, s *records.Store, scope, fileID string) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if err := p.removeShared(ctx, s, scope, fileID); err != nil {
			return err
		}
		return p.remove(ctx, s, scope, fileID)
	})
}

func (p *Plugin) remove(ctx context.Context, s *records.Store, scope, fileID string, extra ...sqlb.Predicate) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		l, held, err := p.current(ctx, s, scope, fileID, time.Time{})
		if err != nil || !held {
			return err
		}
		where := append([]sqlb.Predicate{sqlb.Eq(Scope, scope), sqlb.Eq(FileID, fileID)}, extra...)
		n, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(where...))
		if err != nil || n == 0 {
			return err
		}
		return s.Emit(ctx, p, records.Event{Kind: EventUnlocked, Scope: scope, FileID: fileID, Subject: l.Owner})
	})
}

// LockOf returns a file's lock and whether it holds now (not expired).
func (p *Plugin) LockOf(ctx context.Context, s *records.Store, scope, fileID string) (Lock, bool, error) {
	return p.current(ctx, s, scope, fileID, p.clock(s))
}

// current reads a file's lock; held is false when there is none or it
// expired at now (a zero now counts any lock as held).
func (p *Plugin) current(ctx context.Context, s *records.Store, scope, fileID string, now time.Time) (Lock, bool, error) {
	var l Lock
	err := s.One(ctx, lockQuery(s, scope, fileID, &l))
	return heldAt(l, now, err)
}

// lockQuery reads a file's lock into l.
func lockQuery(s *records.Store, scope, fileID string, l *Lock) records.Statement {
	return sqlb.NewSelect(s.Dialect()).Columns(bindings(l)...).From(Table).Where(sqlb.Eq(Scope, scope), sqlb.Eq(FileID, fileID))
}

// heldAt finishes a read of l that returned err: no row is no lock, and
// the lock holds unless it expired at now (a zero now counts any lock).
func heldAt(l Lock, now time.Time, err error) (Lock, bool, error) {
	if errors.Is(err, sqlb.ErrNoRows) {
		return Lock{}, false, nil
	}
	if err != nil {
		return Lock{}, false, err
	}
	l.ExpiresAt, l.LockedAt = l.ExpiresAt.UTC(), l.LockedAt.UTC()
	return l, now.IsZero() || l.ExpiresAt.After(now), nil
}

// guard refuses with ErrLocked a change to a file whose live lock the
// request does not hold (WithHolderFunc; by default, its owner is not the
// request's actor or WithOwnerFunc's); anyone may once the lock expired.
// An error deciding refuses the change too. It runs inside the change's
// transaction and locks the file's row first, as Lock does, then reads
// the lock with a locking read: a Lock that committed after the change's
// transaction began is seen, not missed, on every engine.
func (p *Plugin) guard(ctx context.Context, s *records.Store, f records.File, action Action) error {
	scope, fileID := f.Scope, f.ID
	if _, found, err := lockFileRow(ctx, s, scope, fileID); err != nil || !found {
		return err
	}
	var read Lock
	now := p.clock(s)
	err := lockedOne(ctx, s, lockQuery(s, scope, fileID, &read))
	l, held, err := heldAt(read, now, err)
	if err != nil {
		return err
	}
	if held {
		return p.passes(ctx, s, []Lock{l}, f, action)
	}
	shared, err := p.sharedHolders(ctx, s, scope, fileID, now)
	if err != nil || len(shared) == 0 {
		return err
	}
	return p.passes(ctx, s, shared, f, action)
}

// passes refuses with ErrLocked a request that holds none of locks
// (WithHolderFunc decides for each), and with the HolderFunc's error when
// one fails: an error never admits.
func (p *Plugin) passes(ctx context.Context, s *records.Store, locks []Lock, f records.File, action Action) error {
	owner := p.ownerOf(ctx, s)
	for _, l := range locks {
		ok, err := p.holder(ctx, HolderRequest{Lock: l, Owner: owner, Action: action, File: f})
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return ErrLocked
}

// ownerOf is who ctx acts for: WithOwnerFunc's lookup when set, else the
// Store's (records.Store.Actor).
func (p *Plugin) ownerOf(ctx context.Context, s *records.Store) string {
	if p.owner != nil {
		return p.owner(ctx)
	}
	return s.Actor(ctx)
}

// lockFileRow locks the file's row of the records table until the
// transaction ends and reports whether the file is trashed and whether it
// exists.
func lockFileRow(ctx context.Context, s *records.Store, scope, fileID string) (trashed, found bool, err error) {
	var deleted *time.Time
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(records.FileDeletedAt, &deleted)).From(records.TableFiles).
		Where(sqlb.Eq(records.FileScope, scope), sqlb.Eq(records.FileID, fileID))
	switch err := lockedOne(ctx, s, q); {
	case errors.Is(err, sqlb.ErrNoRows):
		return false, false, nil
	case err != nil:
		return false, false, err
	}
	return deleted != nil, true, nil
}

// lockedOne runs st as a locking read (FOR UPDATE) on s.Conn(ctx): the
// rows it returns stay locked until the transaction ends, and it reads
// the latest committed rows rather than the transaction's snapshot, which
// a plain read on MySQL (REPEATABLE READ) would return. SQLite has no row
// locks and needs none, since its write transactions exclude each other,
// so the clause is left out there.
func lockedOne(ctx context.Context, s *records.Store, st records.Statement) error {
	q, err := forUpdate(s, st)
	if err != nil {
		return err
	}
	return sqlb.One(ctx, s.Conn(ctx), q)
}

// forUpdate renders st for s (its dialect and table names) with the
// locking clause lockedOne needs.
func forUpdate(s *records.Store, st records.Statement) (sqlb.Query, error) {
	q, err := s.Build(st)
	if err != nil {
		return sqlb.Query{}, err
	}
	if sqlb.DialectName(s.Dialect()) != records.EngineSQLite {
		q.SQL += " FOR UPDATE"
	}
	return q, nil
}

// BeforeReplace implements records.ReplaceHooks: a file another owner has
// locked is not replaced (ErrLocked; see guard).
func (p *Plugin) BeforeReplace(ctx context.Context, s *records.Store, cur records.File, _ *records.File) error {
	return p.guard(ctx, s, cur, ActionReplace)
}

// OnEvent implements records.EventHooks: trashing a file another owner
// has locked is refused (ErrLocked; see guard), rolling the trash back.
func (p *Plugin) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	if e.Kind != records.EventFileTrashed {
		return nil
	}
	f := records.File{Scope: e.Scope, ID: e.FileID}
	if e.File != nil {
		f = *e.File
	}
	return p.guard(ctx, s, f, ActionTrash)
}

// BeforeCreate implements records.FileHooks.
func (p *Plugin) BeforeCreate(context.Context, *records.Store, *records.File) error { return nil }

// AfterCreate implements records.FileHooks.
func (p *Plugin) AfterCreate(context.Context, *records.Store, records.File) error { return nil }

// BeforePurge implements records.FileHooks: purging a file another owner
// has locked is refused (ErrLocked; see guard), except by PurgeScope of
// the file's scope (tenant offboarding beats check-out); otherwise the
// file's lock goes with it.
func (p *Plugin) BeforePurge(ctx context.Context, s *records.Store, f records.File) error {
	if scope, ok := records.PurgingScope(ctx); !ok || scope != f.Scope {
		if err := p.guard(ctx, s, f, ActionPurge); err != nil {
			return err
		}
	}
	if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Scope, f.Scope), sqlb.Eq(FileID, f.ID))); err != nil {
		return err
	}
	_, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(SharedTable).Where(sqlb.Eq(SharedScope, f.Scope), sqlb.Eq(SharedFileID, f.ID)))
	return err
}

// PurgeScope implements records.ScopePurger.
func (p *Plugin) PurgeScope(ctx context.Context, s *records.Store, scope string) error {
	if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Scope, scope))); err != nil {
		return err
	}
	_, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(SharedTable).Where(sqlb.Eq(SharedScope, scope)))
	return err
}

// Sweep deletes every lock, exclusive or shared, in any scope, expired at
// now, and returns how many. An expired lock already allows everything;
// Sweep only tidies. Schedule it; the library starts no background work.
func (p *Plugin) Sweep(ctx context.Context, s *records.Store, now time.Time) (int64, error) {
	at := now.UTC()
	n, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.LessEq(ExpiresAt, at)))
	if err != nil {
		return n, err
	}
	m, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(SharedTable).Where(sqlb.LessEq(SharedExpiresAt, at)))
	return n + m, err
}
