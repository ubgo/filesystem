package records

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/sqlb"
)

// Store records files and attachments in a database and reaches their bytes
// through a filesystem.Manager. It is safe for concurrent use.
type Store struct {
	db         *sql.DB
	dialect    sqlb.Dialect
	disks      *filesystem.Manager
	now        func() time.Time
	ids        func(prefix string) string
	ownerTypes map[string]bool
	// singleRoles hold at most one file per owner (WithSingleRoles).
	singleRoles map[string]bool
	// roleLimits cap the files per owner of a role (WithRoleLimit).
	roleLimits map[string]int
	plugins    []Plugin
	// objectErrors receives object deletions that failed after an outer
	// transaction committed, when no caller is left to return them to.
	objectErrors func(ctx context.Context, f File, err error)
	// lockWait bounds how long Migrate waits for another instance's
	// migration lock on MySQL.
	lockWait time.Duration
	// visibilityDisks maps a visibility to the disk its files live on,
	// when WithVisibilityDisks is set; nil otherwise.
	visibilityDisks map[filesystem.Visibility]string
	// keepSourceIP is WithSourceIP: store Source.IP, which is personal
	// data, only when the application asked.
	keepSourceIP bool
	// retention is WithRetention's rules, applied by SweepRetention.
	retention []Retention
	// scanFailure is WithScanFailure: what FailScan does with a file
	// whose scan failed; "" is ScanFailureKeep.
	scanFailure ScanFailure
	// actor is WithActorFunc's lookup; nil reads ActorFrom (see Actor).
	actor func(context.Context) string
	// names is WithTableNames' mapping; nil keeps the default names.
	names TableNamer
	// orders are WithFileOrder's, by name; nil without any.
	orders map[FileOrder]fileOrdering
}

// Option configures a Store.
type Option func(*Store) error

// WithClock replaces time.Now, for tests.
func WithClock(now func() time.Time) Option {
	return func(s *Store) error { s.now = now; return nil }
}

// WithIDs replaces the ID generator. It receives a prefix (IDPrefixFile,
// IDPrefixAttachment, or a plugin's own such as "shr_" or "acl_", through
// Store.NewID) and must return a unique string starting with it, at most
// 191 bytes (the indexed width on MySQL). Use it to plug in
// github.com/ubgo/publicid or a UUID scheme: every row a plugin creates
// carries the same scheme as the files. Event IDs (IDPrefixEvent) and the
// audit plugin's sequence-prefixed IDs stay random by design.
func WithIDs(fn func(prefix string) string) Option {
	return func(s *Store) error {
		if fn == nil {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: nil ID generator")
		}
		s.ids = fn
		return nil
	}
}

// WithObjectErrorHandler receives the failures of object deletions that
// run after an enclosing Tx commits (see Purge), when there is no caller to
// return them to; log them, or queue the key for a retry. Without it they
// are dropped and the object is left for a sweep: a purged row never comes
// back, so the failure cannot corrupt the records.
func WithObjectErrorHandler(fn func(ctx context.Context, f File, err error)) Option {
	return func(s *Store) error {
		if fn == nil {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: nil object error handler")
		}
		s.objectErrors = fn
		return nil
	}
}

// DefaultMigrationLockWait is how long Migrate waits on MySQL for another
// instance to finish migrating: long enough for a normal migration, short
// enough that a stuck lock fails the deploy instead of hanging it.
const DefaultMigrationLockWait = 60 * time.Second

// WithMigrationLockWait sets how long Migrate waits on MySQL for another
// instance's migration lock (whole seconds; 0 fails at once if the lock is
// held). Postgres waits for its advisory lock without a bound; SQLite has
// one writer and needs no lock.
func WithMigrationLockWait(d time.Duration) Option {
	return func(s *Store) error {
		if d < 0 {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: negative migration lock wait")
		}
		s.lockWait = d
		return nil
	}
}

// ownerTypePattern constrains owner types: they are stored and compared as
// plain strings, so they are kept to identifier characters.
var ownerTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)

// foreignOwnerTypePattern is the wider set WithForeignOwnerTypes accepts:
// the type names other frameworks already store in polymorphic columns
// ("User", "Admin::User", "App\Models\Post"). It still
// excludes "/", spaces, quotes and control characters, so a type is one
// URL path segment once escaped, and fits the 64-byte column.
var foreignOwnerTypePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:\\-]{0,63}$`)

// validOwnerType reports whether t can be an owner type: either pattern.
// Which types a Store accepts is decided by registration, not by this.
func validOwnerType(t string) bool {
	return ownerTypePattern.MatchString(t) || foreignOwnerTypePattern.MatchString(t)
}

// WithOwnerTypes registers the owner types files can be attached to
// ("user", "post", "product"). Attach refuses any other type, so a typo is
// an error instead of an attachment to nothing.
func WithOwnerTypes(types ...string) Option {
	return func(s *Store) error {
		for _, t := range types {
			if !ownerTypePattern.MatchString(t) {
				return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: owner type %q must match %s", t, ownerTypePattern)
			}
			s.ownerTypes[t] = true
		}
		return nil
	}
}

// WithForeignOwnerTypes registers owner types spelled the way another
// framework already stores them, so an application sharing a database
// with another framework's app can attach files to "User", "Admin::User"
// or "App\Models\Post" and JOIN its polymorphic columns without a mapping
// table. Types match ^[A-Za-z][A-Za-z0-9_.:\\-]{0,63}$ (letters of either
// case, digits, "_", ".", ":", a backslash and "-"); anything else is
// CodeInvalidArgument. WithOwnerTypes keeps its stricter lowercase rule,
// the default for new applications. Registration is still the allowlist:
// Attach refuses any type neither option registered.
func WithForeignOwnerTypes(types ...string) Option {
	return func(s *Store) error {
		for _, t := range types {
			if !foreignOwnerTypePattern.MatchString(t) {
				return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: foreign owner type %q must match %s", t, foreignOwnerTypePattern)
			}
			s.ownerTypes[t] = true
		}
		return nil
	}
}

// WithSingleRoles declares roles that hold at most one file per owner:
// "avatar", "cover", "logo". The database enforces it, even under
// concurrent requests (see SetAttachment), so a second Attach to an
// occupied single role is CodeExists; SetAttachment replaces the file.
// Their attachment IDs are derived from scope, owner and role, which is
// how the database enforces it; WithIDs does not apply to them.
func WithSingleRoles(roles ...string) Option {
	return func(s *Store) error {
		for _, r := range roles {
			if !rolePattern.MatchString(r) {
				return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: role %q must match %s", r, rolePattern)
			}
			s.singleRoles[r] = true
		}
		return nil
	}
}

// MaxRoleLimit is the largest WithRoleLimit: an Attach reads every slot
// of the owner's role, so the bound keeps that one short query.
const MaxRoleLimit = 1000

// WithRoleLimit caps how many files one owner may have under role (a
// gallery of at most 20 photos, 5 attachments per message), counting
// attachments of trashed files, which still take their place. Attach
// past the cap is CodeQuotaExceeded naming the role and the cap. The
// database enforces it under concurrent requests: the role's attachment
// IDs are derived from scope, owner, role and a slot number 1..max, so
// at most max rows can exist, and two Attach calls racing for the last
// slot make one fail with CodeUnavailable, to retry. WithIDs does not
// apply to such roles. max must be in [2, MaxRoleLimit] (one file is
// WithSingleRoles), and a role cannot be both.
func WithRoleLimit(role string, max int) Option {
	return func(s *Store) error {
		if !rolePattern.MatchString(role) {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: role %q must match %s", role, rolePattern)
		}
		if max < 2 || max > MaxRoleLimit {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: a role limit must be in [2, %d] (one file per owner is WithSingleRoles)", MaxRoleLimit)
		}
		s.roleLimits[role] = max
		return nil
	}
}

// WithPlugins installs plugins. Their names must be unique.
func WithPlugins(plugins ...Plugin) Option {
	return func(s *Store) error {
		for _, p := range plugins {
			if p == nil {
				return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: nil plugin")
			}
			name := p.Name()
			if name == CoreOwner || !ownerTypePattern.MatchString(name) {
				return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: invalid plugin name %q", name)
			}
			for _, q := range s.plugins {
				if q.Name() == name {
					return filesystem.Errorf(filesystem.CodeExists, "records: plugin %q installed twice", name)
				}
			}
			s.plugins = append(s.plugins, p)
		}
		return nil
	}
}

// ID prefixes, so an ID says what it identifies in logs and URLs.
const (
	IDPrefixFile       = "fil_"
	IDPrefixAttachment = "att_"
)

// defaultID is a prefix plus 128 random bits in lowercase base32.
func defaultID(prefix string) string { return prefix + strings.ToLower(rand.Text()) }

// New returns a Store over db, rendering SQL for dialect (one of
// sqlb.PostgresDialect{}, sqlb.SQLiteDialect{}, sqlb.MySQLDialect{}) and
// reaching bytes through disks. Run Migrate before first use.
func New(db *sql.DB, dialect sqlb.Dialect, disks *filesystem.Manager, opts ...Option) (*Store, error) {
	if db == nil || dialect == nil || disks == nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: db, dialect and disks are required")
	}
	if err := supported(engineOf(dialect)); err != nil {
		return nil, err
	}
	s := &Store{
		db: db, dialect: dialect, disks: disks, now: time.Now, ids: defaultID, ownerTypes: map[string]bool{}, singleRoles: map[string]bool{}, roleLimits: map[string]int{},
		objectErrors: func(context.Context, File, error) {},
		lockWait:     DefaultMigrationLockWait,
	}
	for _, o := range opts {
		if err := o(s); err != nil {
			return nil, err
		}
	}
	for role := range s.roleLimits {
		if s.singleRoles[role] {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: role %q is both a single role and a limited one", role)
		}
	}
	for _, r := range s.retention {
		if !s.ownerTypes[r.OwnerType] {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: retention rule for owner type %q, which WithOwnerTypes did not register", r.OwnerType)
		}
	}
	for _, name := range s.visibilityDisks {
		if _, err := disks.Disk(name); err != nil {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: WithVisibilityDisks names disk %q, which the manager does not have", name)
		}
	}
	return s, nil
}

// clock returns the current time in UTC at microsecond precision, the
// finest every engine stores, so a value read back equals the value
// written.
func (s *Store) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// Now is the Store's clock (WithClock) in UTC at microsecond precision,
// the precision every engine keeps: plugins stamp and compare times with
// it, so one clock rules a deployment and its tests.
func (s *Store) Now() time.Time { return s.clock() }

// NewID returns a new row ID starting with prefix from the Store's ID
// generator (WithIDs, else prefix plus 128 random bits in lowercase
// base32). Plugins create their rows' IDs with it, so an application's ID
// scheme (UUIDv7, a publicid) covers every table and not only files and
// attachments. The generator's contract applies: unique, starting with
// prefix, at most 191 bytes.
func (s *Store) NewID(prefix string) string { return s.ids(prefix) }

// txState is the transaction attached to a context, plus the work to run
// once it commits.
type txState struct {
	tx          *sql.Tx
	mu          sync.Mutex
	afterCommit []func(context.Context)
	onRollback  []func(context.Context)
}

type txKey struct{}

func txFrom(ctx context.Context) *txState {
	st, _ := ctx.Value(txKey{}).(*txState)
	return st
}

// Tx runs fn in one transaction and commits it when fn returns nil. Every
// Store call made with the ctx fn receives joins the transaction, and so
// can the application's own statements on tx. A Tx inside a Tx joins the
// outer one; only the outermost commits. Object deletions scheduled by
// Purge run after the commit, never before.
func (s *Store) Tx(ctx context.Context, fn func(ctx context.Context, tx *sql.Tx) error) error {
	if st := txFrom(ctx); st != nil {
		return fn(ctx, st.tx)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("records: begin: %w", err)
	}
	st := &txState{tx: tx}
	if err := fn(context.WithValue(ctx, txKey{}, st), tx); err != nil {
		_ = tx.Rollback() // fn's error is the one worth reporting
		st.run(ctx, st.onRollback)
		return err
	}
	if err := tx.Commit(); err != nil {
		st.run(ctx, st.onRollback)
		return fmt.Errorf("records: commit: %w", err)
	}
	st.run(ctx, st.afterCommit)
	return nil
}

// run runs the scheduled work with ctx's values but never its
// cancellation: this is cleanup of a transaction that already ended (an
// object to delete after a commit or a rollback), and a client that went
// away mid-request must not leave that object behind.
func (st *txState) run(ctx context.Context, work []func(context.Context)) {
	ctx = context.WithoutCancel(ctx)
	for _, w := range work {
		w(ctx)
	}
}

// OnRollback runs fn if the transaction in ctx rolls back, and never when
// there is no transaction. Upload uses it to delete the object of a file
// whose row a rollback undid, so a failed request leaves no orphan bytes.
func (s *Store) OnRollback(ctx context.Context, fn func(context.Context)) {
	st := txFrom(ctx)
	if st == nil {
		return
	}
	st.mu.Lock()
	st.onRollback = append(st.onRollback, fn)
	st.mu.Unlock()
}

// AfterCommit runs fn once the transaction in ctx commits, or at once when
// there is none. A rolled-back transaction runs nothing. fn gets ctx's
// values but never its cancellation, so cleanup survives a client that
// went away.
func (s *Store) AfterCommit(ctx context.Context, fn func(context.Context)) {
	st := txFrom(ctx)
	if st == nil {
		fn(context.WithoutCancel(ctx)) // cleanup, as in txState.run
		return
	}
	st.mu.Lock()
	st.afterCommit = append(st.afterCommit, fn)
	st.mu.Unlock()
}

// DeleteObjectAfterCommit deletes the object of f (its Disk and Key) once
// the transaction in ctx commits, at once when there is none, and never
// when it rolls back, unless a file row or an ObjectOwner plugin still
// names it (a shared object, see References), which is then kept. Call it
// after removing your own reference (your row). A failure, of the lookup
// or the deletion, keeps the object and goes to WithObjectErrorHandler.
// Plugins that own objects (kept versions, image variants) use it, so
// their deletions follow the object-last rule and report like the
// Store's own.
func (s *Store) DeleteObjectAfterCommit(ctx context.Context, f File) {
	if err := s.releaseObject(ctx, f); err != nil {
		s.AfterCommit(ctx, func(ctx context.Context) { s.objectErrors(ctx, f, err) })
	}
}

// Conn returns what to run statements on for ctx: the open transaction, or
// the database. Plugins use it for their own tables so their writes join
// the transaction.
func (s *Store) Conn(ctx context.Context) sqlb.DBTX {
	if st := txFrom(ctx); st != nil {
		return st.tx
	}
	return s.db
}

// Dialect returns the dialect statements must be built for.
func (s *Store) Dialect() sqlb.Dialect { return s.dialect }

// Disks returns the disk manager, for plugins that work on bytes.
func (s *Store) Disks() *filesystem.Manager { return s.disks }

// Statement is a sqlb builder ready to render.
type Statement interface {
	Build() (sqlb.Query, error)
}

// Exec runs a write on Conn(ctx) and returns the rows affected.
func (s *Store) Exec(ctx context.Context, st Statement) (int64, error) {
	q, err := s.Build(st)
	if err != nil {
		return 0, fmt.Errorf("records: build: %w", err)
	}
	res, err := s.Conn(ctx).ExecContext(ctx, q.SQL, q.Args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// One runs a read expecting one row and scans it into the statement's
// bindings; sqlb.ErrNoRows when there is none.
func (s *Store) One(ctx context.Context, st Statement) error {
	q, err := s.Build(st)
	if err != nil {
		return fmt.Errorf("records: build: %w", err)
	}
	return sqlb.One(ctx, s.Conn(ctx), q)
}

// Each runs a read and calls fn after scanning each row.
func (s *Store) Each(ctx context.Context, st Statement, fn func() error) error {
	q, err := s.Build(st)
	if err != nil {
		return fmt.Errorf("records: build: %w", err)
	}
	return sqlb.Each(ctx, s.Conn(ctx), q, fn)
}

// Unique-violation signatures of the supported drivers, recognised without
// importing them (see ubgo/auth's dberr.go, which this mirrors): Postgres
// through SQLSTATE, MySQL and SQLite by their stable message prefixes.
const (
	pgUniqueViolation = "23505"
	mysqlDuplicate    = "Error 1062"
	sqliteUnique      = "UNIQUE constraint failed"
)

// IsUniqueViolation reports whether err is a unique-constraint failure on
// any supported engine.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) && state.SQLState() == pgUniqueViolation {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, mysqlDuplicate) || strings.Contains(msg, sqliteUnique)
}

// Signatures of a transaction the database aborted to break a lock
// conflict: Postgres's deadlock_detected and serialization_failure, MySQL's
// deadlock and lock-wait timeout, SQLite's busy database.
var (
	pgAborted    = []string{"40P01", "40001"}
	mysqlAborted = []string{"Error 1213", "Error 1205"}
	sqliteBusy   = "database is locked"
)

// IsLockConflict reports whether err is the database aborting a
// transaction over a lock conflict (a deadlock, a serialization failure,
// a lock wait that timed out) on any supported engine: the transaction
// rolled back as a whole and is safe to run again.
func IsLockConflict(err error) bool {
	if err == nil {
		return false
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) && slices.Contains(pgAborted, state.SQLState()) {
		return true
	}
	msg := err.Error()
	return slices.ContainsFunc(mysqlAborted, func(sig string) bool { return strings.Contains(msg, sig) }) || strings.Contains(msg, sqliteBusy)
}

// notFound maps sqlb.ErrNoRows to CodeNotFound and passes other errors on.
func notFound(err error, what, id string) error {
	if errors.Is(err, sqlb.ErrNoRows) {
		return filesystem.Errorf(filesystem.CodeNotFound, "records: no %s %q", what, id)
	}
	return err
}
