// Package uploadsessions is a records plugin that keeps upload sessions
// (upload.Session, UP-15) in the database: the SessionStore to use when
// sessions must survive a restart or be shared by several instances
// behind a load balancer. upload.Sessions does every check (the secret,
// the expiry by its own clock, the limits against the endpoint's policy);
// this store only keeps the rows, never a token's secret, only its
// SHA-256.
//
//	p := uploadsessions.New()
//	store, _ := records.New(db, dialect, disks, records.WithPlugins(p))
//	sessions, _ := upload.NewSessions(p.Store(store), upload.SessionsOptions{})
//
// Schedule sessions.Sweep (the library starts no background work); an
// expired session is refused whether or not it was swept.
package uploadsessions

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// PluginName is the plugin's name in the migration ledger.
const PluginName = "uploadsessions"

// TableNameSessions is the table the plugin owns: one row per session.
const TableNameSessions = "fs_upload_sessions"

// Column names of fs_upload_sessions.
const (
	ColID         = "id"
	ColSecretHash = "secret_hash"
	ColKey        = "storage_key"
	ColPrefix     = "prefix"
	ColMaxSize    = "max_size"
	ColAllowed    = "allowed"
	ColCreatedAt  = "created_at"
	ColExpiresAt  = "expires_at"
)

// Table and typed columns.
var (
	Table      = sqlb.Table{Name: TableNameSessions, Alias: "us"}
	ID         = sqlb.Col[string](Table, ColID)
	SecretHash = sqlb.Col[string](Table, ColSecretHash)
	// Key and Prefix: exactly one is set ("" for the other), as in
	// upload.Session.
	Key     = sqlb.Col[string](Table, ColKey)
	Prefix  = sqlb.Col[string](Table, ColPrefix)
	MaxSize = sqlb.Col[int64](Table, ColMaxSize)
	// Allowed is the session's type patterns as a JSON array, "[]" or
	// "null" for none (the policy's types).
	Allowed   = sqlb.Col[string](Table, ColAllowed)
	CreatedAt = sqlb.Col[time.Time](Table, ColCreatedAt)
	ExpiresAt = sqlb.Col[time.Time](Table, ColExpiresAt)
)

// Plugin owns the sessions table. Construct it with New, install it with
// records.WithPlugins, and get the SessionStore with Store.
type Plugin struct{}

// New returns a Plugin.
func New() *Plugin { return &Plugin{} }

// Name implements records.Plugin.
func (p *Plugin) Name() string { return PluginName }

// Migrations implements records.Migrator.
func (p *Plugin) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	st, err := records.NewDDL(engine).CreateTable(records.Table{
		Table: Table,
		Columns: []ddl.ColumnSpec{
			records.Column(ID, t.Key), records.Column(SecretHash, t.Short), records.Column(Key, t.Long), records.Column(Prefix, t.Long),
			records.Column(MaxSize, t.Int), records.Column(Allowed, t.Doc), records.Column(CreatedAt, t.Timestamp), records.Column(ExpiresAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{ID},
		// Sweep deletes by expiry.
		Indexes: []records.Index{{Name: "fs_upload_sessions_expires", Columns: []records.IndexColumn{records.Key(ExpiresAt)}}},
	}).Statements()
	return []records.Migration{{Owner: PluginName, Name: "0001_upload_sessions", Statements: st}}, err
}

// Store returns the upload.SessionStore over s's database. The plugin
// must be installed in s (its table exists only then).
func (p *Plugin) Store(s *records.Store) Store { return Store{s: s} }

// Store is an upload.SessionStore in the records database. Statements
// join the transaction in ctx, if any (records.Store.Tx).
type Store struct{ s *records.Store }

var _ upload.SessionStore = Store{}

// SaveSession implements upload.SessionStore. An ID already present is
// CodeExists.
func (st Store) SaveSession(ctx context.Context, sess upload.Session) error {
	allowed, _ := json.Marshal(sess.Allowed) // a []string always marshals
	ins := sqlb.NewInsert(st.s.Dialect()).Into(Table).Row(
		sqlb.Set(ID, sess.ID), sqlb.Set(SecretHash, sess.SecretHash), sqlb.Set(Key, sess.Key), sqlb.Set(Prefix, sess.Prefix),
		sqlb.Set(MaxSize, sess.MaxSize), sqlb.Set(Allowed, string(allowed)),
		sqlb.Set(CreatedAt, sess.CreatedAt.UTC()), sqlb.Set(ExpiresAt, sess.ExpiresAt.UTC()),
	)
	if _, err := st.s.Exec(ctx, ins); err != nil {
		if records.IsUniqueViolation(err) {
			return filesystem.NewError(filesystem.CodeExists, err)
		}
		return err
	}
	return nil
}

// LoadSession implements upload.SessionStore: CodeNotFound when there is
// no session with id.
func (st Store) LoadSession(ctx context.Context, id string) (upload.Session, error) {
	var sess upload.Session
	var allowed string
	q := sqlb.NewSelect(st.s.Dialect()).Columns(
		sqlb.Bind(ID, &sess.ID), sqlb.Bind(SecretHash, &sess.SecretHash), sqlb.Bind(Key, &sess.Key), sqlb.Bind(Prefix, &sess.Prefix),
		sqlb.Bind(MaxSize, &sess.MaxSize), sqlb.Bind(Allowed, &allowed), sqlb.Bind(CreatedAt, &sess.CreatedAt), sqlb.Bind(ExpiresAt, &sess.ExpiresAt),
	).From(Table).Where(sqlb.Eq(ID, id))
	if err := st.s.One(ctx, q); err != nil {
		if errors.Is(err, sqlb.ErrNoRows) {
			return upload.Session{}, filesystem.Errorf(filesystem.CodeNotFound, "uploadsessions: session %q not found", id)
		}
		return upload.Session{}, err
	}
	// A row this store did not write is the database's corruption, not a
	// session: refused rather than read as one without type limits.
	if err := json.Unmarshal([]byte(allowed), &sess.Allowed); err != nil {
		return upload.Session{}, filesystem.NewError(filesystem.CodeUnknown, err)
	}
	sess.CreatedAt, sess.ExpiresAt = sess.CreatedAt.UTC(), sess.ExpiresAt.UTC()
	return sess, nil
}

// DeleteSession implements upload.SessionStore.
func (st Store) DeleteSession(ctx context.Context, id string) error {
	_, err := st.s.Exec(ctx, sqlb.NewDelete(st.s.Dialect()).From(Table).Where(sqlb.Eq(ID, id)))
	return err
}

// SweepSessions implements upload.SessionStore: one DELETE of every
// session expired at now.
func (st Store) SweepSessions(ctx context.Context, now time.Time) (int, error) {
	n, err := st.s.Exec(ctx, sqlb.NewDelete(st.s.Dialect()).From(Table).Where(sqlb.LessEq(ExpiresAt, now.UTC())))
	return int(n), err
}
