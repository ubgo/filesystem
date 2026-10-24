// Package audit is a records plugin that keeps an append-only log of every
// change to files and attachments, and of every change other plugins
// report with records.Store.Emit (folders, tags, share links, with their
// Subject and Detail): who did what to which file, and when.
// The activity feed of a drive, and the trail compliance asks for.
//
// Entries are written by records.EventHooks inside the change's own
// transaction, so the log never shows a change that rolled back and never
// misses one that committed. The actor comes from the context: the
// application sets it once per request with WithActor (or supplies its own
// lookup with WithActorFunc). Entries outlive the files they describe;
// Prune applies a retention period.
//
// Four opt-in options extend it. WithSource also logs where each change
// came from (records.Source: channel, client, and with WithSourceIP the
// IP address). WithAccessLog also logs reads reported
// with records.Store.Served (downloads, links handed out), sampled.
// WithHashChain numbers each scope's entries in commit order and chains
// them by hash, so Verify detects an entry edited, deleted or inserted
// behind the plugin's back, and Changes serves a change feed whose cursor
// never skips a commit. WithRetentionFloor makes Prune refuse to delete
// entries younger than a regulated minimum.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// PluginName is the plugin's name in the migration ledger.
const PluginName = "audit"

// TableNameAudit is the table the plugin owns.
const TableNameAudit = "fs_audit"

// Column names of fs_audit.
const (
	ColID           = "id"
	ColScope        = "scope"
	ColAt           = "at"
	ColActor        = "actor"
	ColKind         = "kind"
	ColFileID       = "file_id"
	ColAttachmentID = "attachment_id"
	ColOwnerType    = "owner_type"
	ColOwnerID      = "owner_id"
	ColRole         = "role"
	ColSubject      = "subject"
	ColDetail       = "detail"
)

// Table and typed columns. Columns an event kind does not set hold "".
var (
	Table        = sqlb.Table{Name: TableNameAudit, Alias: "au"}
	ID           = sqlb.Col[string](Table, ColID)
	Scope        = sqlb.Col[string](Table, ColScope)
	At           = sqlb.Col[time.Time](Table, ColAt)
	Actor        = sqlb.Col[string](Table, ColActor)
	Kind         = sqlb.Col[string](Table, ColKind)
	FileID       = sqlb.Col[string](Table, ColFileID)
	AttachmentID = sqlb.Col[string](Table, ColAttachmentID)
	OwnerType    = sqlb.Col[string](Table, ColOwnerType)
	OwnerID      = sqlb.Col[string](Table, ColOwnerID)
	Role         = sqlb.Col[string](Table, ColRole)
	Subject      = sqlb.Col[string](Table, ColSubject)
	// Detail holds the event's Detail as a JSON object, "" when it has
	// none.
	Detail = sqlb.Col[string](Table, ColDetail)
)

// Limits.
const (
	// IDPrefix starts every entry ID.
	IDPrefix = "aud_"
	// seqDigits is the fixed width, in hex digits, of the write sequence
	// that follows IDPrefix in an entry ID. Fixed width keeps the IDs'
	// text order equal to their numeric order in every collation.
	seqDigits = 16
	// MaxActorBytes is the longest actor, the indexed width of its column.
	MaxActorBytes = records.MaxActorBytes
	// DefaultPageSize is the page size when Query.Limit is 0.
	DefaultPageSize = 50
	// MaxPageSize caps Query.Limit.
	MaxPageSize = 1000
)

// Entry is one logged change: the records.Event and who made it. Actor is
// "" for changes made without one (background jobs such as SweepTrash).
type Entry struct {
	ID    string
	Actor string
	// Seq is the entry's place in its scope's chain (WithHashChain), from
	// 1 in commit order; 0 for an entry written without the chain.
	Seq int64
	// KeyID is the ID of the chain key the entry was hashed with
	// (WithHashChainKeyring); "" for WithHashChain's single key, and for
	// an entry written without the chain.
	KeyID string
	// Source is where the change came from, with WithSource (the IP only
	// with WithSourceIP); zero without it, and for entries written before
	// it was turned on.
	Source records.Source
	records.Event
}

// WithActor is records.WithActor: the one actor of a request, which the
// Store records as File.UploadedBy and this plugin logs.
func WithActor(ctx context.Context, actor string) context.Context {
	return records.WithActor(ctx, actor)
}

// ActorFrom is records.ActorFrom, the default actor lookup.
func ActorFrom(ctx context.Context) string { return records.ActorFrom(ctx) }

// Plugin writes and reads the log. Construct it with New and install it
// with records.WithPlugins.
type Plugin struct {
	actor func(context.Context) string
	// access is WithAccessLog's sampler; nil logs no reads.
	access func(records.Served) bool
	// chain is WithHashChain, WithHashChainKeyring or WithHashChainMAC,
	// and signer what computes its MACs (a single key under ID "" for
	// WithHashChain).
	chain  bool
	signer Signer
	// source is WithSource, sourceIP WithSourceIP.
	source, sourceIP bool
	// entryDetail is WithEntryDetail's hook; nil adds nothing.
	entryDetail func(context.Context, records.Event) map[string]string
	// floor is WithRetentionFloor; 0 for none.
	floor time.Duration
	// seq numbers the entries this Plugin writes, so entries stamped with
	// the same microsecond still list in the order they were written.
	seq atomic.Uint64
}

// Option configures a Plugin.
type Option func(*Plugin)

// Column names fs_audit gains in migration 0004_audit_keys_source, all ""
// in rows written before it or without the options that fill them.
const (
	// ColKeyID is the chain key's ID (WithHashChainKeyring).
	ColKeyID = "key_id"
	// ColSourceChannel, ColSourceClient and ColSourceIP are the change's
	// records.Source (WithSource; the IP with WithSourceIP).
	ColSourceChannel = "source_channel"
	ColSourceClient  = "source_client"
	ColSourceIP      = "source_ip"
)

// Typed columns of migration 0004_audit_keys_source.
var (
	KeyID         = sqlb.Col[string](Table, ColKeyID)
	SourceChannel = sqlb.Col[string](Table, ColSourceChannel)
	SourceClient  = sqlb.Col[string](Table, ColSourceClient)
	SourceIP      = sqlb.Col[string](Table, ColSourceIP)
)

// keysSourceDDL is migration 0004: the key ID and source columns of
// fs_audit and the key ID of fs_audit_chain, with literal defaults (no
// TEXT column, so MySQL takes them); every statement can run again after
// a crash. It runs whether or not the options are on, so turning one on
// later needs no migration.
func keysSourceDDL(engine string, t records.SQLTypes) *records.DDL {
	return records.NewDDL(engine).
		AddColumn(Table, records.Column(KeyID, t.Short).Default("")).
		AddColumn(Table, records.Column(SourceChannel, t.Short).Default("")).
		AddColumn(Table, records.Column(SourceClient, t.Text).Default("")).
		AddColumn(Table, records.Column(SourceIP, t.Short).Default("")).
		AddColumn(ChainTable, records.Column(ChainKeyID, t.Short).Default(""))
}

// WithSource logs, with every entry, where the change came from: the
// channel and client of records.SourceFrom (set by the application with
// records.WithSource, once per request), the "from where" a compliance
// trail asks for. The IP address is personal data and is kept only with
// WithSourceIP as well. With the hash chain on, the source is part of the
// entry's hash. A source wider than its columns fails the change rather
// than being cut short. Off by default.
func WithSource() Option { return func(p *Plugin) { p.source = true } }

// WithSourceIP logs the source's IP address too, and turns WithSource
// on. Storing addresses is the application's explicit choice, as with
// records.WithSourceIP (which this plugin does not read: an address the
// Store drops from files may still be logged here, and the reverse).
func WithSourceIP() Option { return func(p *Plugin) { p.source, p.sourceIP = true, true } }

// WithActorFunc replaces the Store's actor lookup (records.Store.Actor,
// which records.WithActorFunc sets for the Store and every plugin) for
// this plugin alone, for applications that keep the signed-in user in
// their own context key. Without it the plugin logs s.Actor(ctx).
func WithActorFunc(fn func(context.Context) string) Option { return func(p *Plugin) { p.actor = fn } }

// WithEntryDetail adds fn's fields to every entry's Detail, for what the
// fixed Source columns do not hold: a request or trace ID, a user agent,
// the admin acting "on behalf of" a user. fn runs inside the change, with
// its context and event; the fields are stored with the event's own
// Detail, so with the hash chain on they are part of the entry's hash. A
// key the event already carries keeps the event's value: the hook adds
// context, it cannot rewrite what the change reported. A nil or empty map
// adds nothing. Without the option entries hold exactly the event's
// Detail.
func WithEntryDetail(fn func(context.Context, records.Event) map[string]string) Option {
	return func(p *Plugin) { p.entryDetail = fn }
}

// detailOf is e's Detail with WithEntryDetail's fields added under keys
// the event does not use.
func (p *Plugin) detailOf(ctx context.Context, e records.Event) map[string]string {
	if p.entryDetail == nil {
		return e.Detail
	}
	extra := p.entryDetail(ctx, e)
	if len(extra) == 0 {
		return e.Detail
	}
	out := make(map[string]string, len(e.Detail)+len(extra))
	for k, v := range extra {
		out[k] = v
	}
	for k, v := range e.Detail {
		out[k] = v
	}
	return out
}

// New returns a Plugin.
func New(opts ...Option) *Plugin {
	// Without a chain option the ring holds the empty key under "", which
	// is what Verify used before keyrings: a plugin that never chained
	// still reads a chained scope as broken rather than panicking.
	p := &Plugin{signer: KeyringMAC(StaticKeyring{Keys: map[string][]byte{"": nil}})}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Name implements records.Plugin.
func (p *Plugin) Name() string { return PluginName }

// Migrations implements records.Migrator.
func (p *Plugin) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	byScope := []records.IndexColumn{records.Key(Scope), records.Key(At), records.Key(ID)}
	byFile := []records.IndexColumn{records.Key(Scope), records.Key(FileID), records.Key(At), records.Key(ID)}
	byActor := []records.IndexColumn{records.Key(Scope), records.Key(Actor), records.Key(At), records.Key(ID)}
	table, err1 := records.NewDDL(engine).CreateTable(records.Table{
		Table: Table,
		Columns: []ddl.ColumnSpec{
			records.Column(ID, t.Key), records.Column(Scope, t.Key), records.Column(At, t.Timestamp), records.Column(Actor, t.Key),
			records.Column(Kind, t.Short), records.Column(FileID, t.Key), records.Column(AttachmentID, t.Key),
			records.Column(OwnerType, t.Short), records.Column(OwnerID, t.Key), records.Column(Role, t.Short),
		},
		PrimaryKey: []ddl.ColumnRef{ID},
		Indexes: []records.Index{
			{Name: "fs_audit_scope", Columns: byScope},
			{Name: "fs_audit_file", Columns: byFile},
			{Name: "fs_audit_actor", Columns: byActor},
		},
	}).Statements()
	subjectDetail, err2 := subjectDetail(engine, t).Statements()
	chain, err3 := chainDDL(engine, t).Statements()
	keysSource, err4 := keysSourceDDL(engine, t).Statements()
	return []records.Migration{
		{Owner: PluginName, Name: "0001_audit", Statements: table},
		{Owner: PluginName, Name: "0002_audit_subject_detail", Statements: subjectDetail},
		{Owner: PluginName, Name: "0003_audit_chain", Statements: chain},
		{Owner: PluginName, Name: "0004_audit_keys_source", Statements: keysSource},
	}, errors.Join(err1, err2, err3, err4)
}

// subjectDetail adds the subject and detail columns plugin events fill
// (records.Event.Subject and Detail). Every statement can run again after
// a crash before the ledger write (records.DDL). MySQL gives a TEXT column
// no literal default, so there detail is added nullable, back-filled with
// "" and then made NOT NULL; rows written before the migration hold "".
func subjectDetail(engine string, t records.SQLTypes) *records.DDL {
	d := records.NewDDL(engine).AddColumn(Table, records.Column(Subject, t.Key).Default(""))
	if engine != records.EngineMySQL {
		return d.AddColumn(Table, records.Column(Detail, t.Doc).Default(""))
	}
	return d.AddColumn(Table, records.Column(Detail, t.Doc).Nullable()).
		// A data backfill, not DDL: written by hand, and re-runnable.
		Raw("UPDATE "+TableNameAudit+" SET "+ColDetail+" = '' WHERE "+ColDetail+" IS NULL").
		ModifyColumn(Table, records.Column(Detail, t.Doc))
}

// encodeDetail stores an event's Detail as a JSON object, "" for none. A
// map of strings cannot fail to marshal.
func encodeDetail(d map[string]string) string {
	if len(d) == 0 {
		return ""
	}
	b, _ := json.Marshal(d)
	return string(b)
}

// decodeDetail reads a stored Detail. Rows this package wrote hold "" or
// a JSON object; anything else (a hand-edited row) reads as nil rather
// than failing the page.
func decodeDetail(s string) map[string]string {
	if s == "" {
		return nil
	}
	var d map[string]string
	if json.Unmarshal([]byte(s), &d) != nil {
		return nil
	}
	return d
}

// newID returns an entry ID: IDPrefix, then this Plugin's write sequence
// in seqDigits hex digits, then random text so IDs from different
// processes never collide. Entries are listed by (at, id), and the store
// stamps times at microsecond precision, so several changes in one
// transaction (or one fast request) often share an at; the sequence makes
// them list in the order they were written instead of in random order.
// The order of same-microsecond entries written by different processes is
// not defined: they share no clock finer than the microsecond.
func (p *Plugin) newID() string {
	return fmt.Sprintf("%s%0*x%s", IDPrefix, seqDigits, p.seq.Add(1), strings.ToLower(rand.Text()))
}

// OnEvent implements records.EventHooks. An actor longer than
// MaxActorBytes fails the change: an audit trail that silently cut actors
// short could not be trusted.
func (p *Plugin) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	r, err := p.row(ctx, s, e)
	if err != nil {
		return err
	}
	return p.write(ctx, s, &r)
}

// row is one fs_audit row as written and as Verify hashes it.
type row struct {
	id, scope, actor, kind, fileID, attachmentID, ownerType, ownerID, role, subject, detail string
	at                                                                                      time.Time
	seq                                                                                     int64
	prev, hash                                                                              string
	// keyID is the chain key's ID (WithHashChainKeyring), source the
	// change's (WithSource): both "" or zero in rows of the original shape.
	keyID  string
	source records.Source
}

// actorOf is who ctx acts for: WithActorFunc's lookup when set, else the
// Store's (records.Store.Actor), so records.WithActorFunc rules this plugin
// like every other.
func (p *Plugin) actorOf(ctx context.Context, s *records.Store) string {
	if p.actor != nil {
		return p.actor(ctx)
	}
	return s.Actor(ctx)
}

// row builds the row of e, checking the context's actor.
func (p *Plugin) row(ctx context.Context, s *records.Store, e records.Event) (row, error) {
	actor := p.actorOf(ctx, s)
	if len(actor) > MaxActorBytes {
		return row{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "audit: actor is longer than %d bytes", MaxActorBytes)
	}
	r := row{
		id: p.newID(), scope: e.Scope, at: e.At, actor: actor, kind: string(e.Kind), fileID: e.FileID, attachmentID: e.AttachmentID,
		ownerType: e.Owner.Type, ownerID: e.Owner.ID, role: e.Role, subject: e.Subject, detail: encodeDetail(p.detailOf(ctx, e)),
	}
	if p.source {
		src := records.SourceFrom(ctx)
		r.source = records.Source{Channel: src.Channel, Client: src.Client}
		if p.sourceIP {
			r.source.IP = src.IP
		}
		if len(src.Channel) > records.MaxSourceChannelBytes || len(src.Client) > records.MaxSourceClientBytes || len(r.source.IP) > records.MaxSourceIPBytes {
			return row{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "audit: a source is at most %d bytes of channel, %d of client and %d of IP",
				records.MaxSourceChannelBytes, records.MaxSourceClientBytes, records.MaxSourceIPBytes)
		}
	}
	return r, nil
}

// write inserts r, first linking it into its scope's chain with
// WithHashChain. It must run inside a transaction (the change's, or one
// of its own), which holds the chain's row lock until it commits.
func (p *Plugin) write(ctx context.Context, s *records.Store, r *row) error {
	if p.chain {
		if err := p.link(ctx, s, r); err != nil {
			return err
		}
	}
	ins := sqlb.NewInsert(s.Dialect()).Into(Table).Row(
		sqlb.Set(ID, r.id), sqlb.Set(Scope, r.scope), sqlb.Set(At, r.at),
		sqlb.Set(Actor, r.actor), sqlb.Set(Kind, r.kind), sqlb.Set(FileID, r.fileID),
		sqlb.Set(AttachmentID, r.attachmentID), sqlb.Set(OwnerType, r.ownerType), sqlb.Set(OwnerID, r.ownerID), sqlb.Set(Role, r.role),
		sqlb.Set(Subject, r.subject), sqlb.Set(Detail, r.detail), sqlb.Set(Seq, r.seq), sqlb.Set(PrevHash, r.prev), sqlb.Set(Hash, r.hash),
		sqlb.Set(KeyID, r.keyID), sqlb.Set(SourceChannel, r.source.Channel), sqlb.Set(SourceClient, r.source.Client), sqlb.Set(SourceIP, r.source.IP),
	)
	_, err := s.Exec(ctx, ins)
	return err
}

// Query selects entries of one scope, newest first.
type Query struct {
	Scope string
	// FileID keeps the entries of one file (its history); "" keeps all.
	FileID string
	// Actor keeps the entries of one actor (their activity); "" keeps all.
	Actor string
	// Limit caps the page; 0 means DefaultPageSize.
	Limit int
	// Cursor continues from Page.NextCursor of the same query.
	Cursor string
}

// Page is one page of entries, newest first.
type Page struct {
	Entries    []Entry
	NextCursor string
}

// Entries returns one page of the log, with keyset pagination on (at, id).
func (p *Plugin) Entries(ctx context.Context, s *records.Store, q Query) (Page, error) {
	limit := q.Limit
	switch {
	case limit < 0 || limit > MaxPageSize:
		return Page{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "audit: limit %d is not in [0, %d]", limit, MaxPageSize)
	case limit == 0:
		limit = DefaultPageSize
	}
	where := []sqlb.Predicate{sqlb.Eq(Scope, q.Scope)}
	if q.FileID != "" {
		where = append(where, sqlb.Eq(FileID, q.FileID))
	}
	if q.Actor != "" {
		where = append(where, sqlb.Eq(Actor, q.Actor))
	}
	if q.Cursor != "" {
		at, id, err := records.DecodeCursor(q.Cursor)
		if err != nil {
			return Page{}, err
		}
		where = append(where, sqlb.RowLess(sqlb.Row(At, ID), sqlb.Row(sqlb.Param(at), sqlb.Param(id))))
	}
	var en Entry
	var kind, detail string
	sel := sqlb.NewSelect(s.Dialect()).Columns(entryBindings(&en, &kind, &detail)...).
		From(Table).Where(where...).Order(sqlb.OrderBy(At, sqlb.SortDesc), sqlb.OrderBy(ID, sqlb.SortDesc)).Limit(limit + 1)
	var page Page
	err := s.Each(ctx, sel, func() error {
		page.Entries = append(page.Entries, entryOf(en, kind, detail))
		return nil
	})
	if err != nil {
		return Page{}, err
	}
	if len(page.Entries) > limit {
		page.Entries = page.Entries[:limit]
		last := page.Entries[limit-1]
		page.NextCursor = records.EncodeCursor(last.At, last.ID)
	}
	return page, nil
}

// entryBindings binds an entry's columns, the kind and detail as stored.
func entryBindings(en *Entry, kind, detail *string) []sqlb.Binding {
	return []sqlb.Binding{
		sqlb.Bind(ID, &en.ID), sqlb.Bind(Scope, &en.Scope), sqlb.Bind(At, &en.At), sqlb.Bind(Actor, &en.Actor),
		sqlb.Bind(Kind, kind), sqlb.Bind(FileID, &en.FileID), sqlb.Bind(AttachmentID, &en.AttachmentID),
		sqlb.Bind(OwnerType, &en.Owner.Type), sqlb.Bind(OwnerID, &en.Owner.ID), sqlb.Bind(Role, &en.Role),
		sqlb.Bind(Subject, &en.Subject), sqlb.Bind(Detail, detail), sqlb.Bind(Seq, &en.Seq), sqlb.Bind(KeyID, &en.KeyID),
		sqlb.Bind(SourceChannel, &en.Source.Channel), sqlb.Bind(SourceClient, &en.Source.Client), sqlb.Bind(SourceIP, &en.Source.IP),
	}
}

// entryOf finishes an entry read with entryBindings.
func entryOf(en Entry, kind, detail string) Entry {
	en.Kind = records.EventKind(kind)
	en.Detail = decodeDetail(detail)
	en.At = en.At.UTC()
	return en
}

// Prune deletes the entries, in every scope, older than cutoff and returns
// how many it deleted. Schedule it to apply a retention period. With
// WithRetentionFloor it refuses a cutoff inside the floor
// (CodeInvalidArgument) and deletes nothing; with WithHashChain it keeps
// each chain verifiable (see PruneScope).
func (p *Plugin) Prune(ctx context.Context, s *records.Store, cutoff time.Time) (int64, error) {
	if err := p.checkFloor(s, cutoff); err != nil {
		return 0, err
	}
	if !p.chain {
		return s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Less(At, cutoff.UTC())))
	}
	scopes, err := p.chainedScopes(ctx, s)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, scope := range scopes {
		n, err := p.pruneChained(ctx, s, scope, cutoff)
		total += n
		if err != nil {
			return total, err
		}
	}
	n, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Seq, 0), sqlb.Less(At, cutoff.UTC())))
	return total + n, err
}

// PruneScope deletes the entries of one scope older than cutoff and
// returns how many it deleted: a retention period per tenant, or, with a
// cutoff after the last entry, the whole log of a tenant being offboarded.
// Other scopes are never touched. Like Prune, it runs inside the caller's
// transaction when ctx carries one (Store.Tx). WithRetentionFloor
// refuses a cutoff inside the floor. With WithHashChain it deletes the
// scope's oldest entries up to the first one at or after cutoff, in chain
// order, and records the last deleted entry's hash as the chain's new
// anchor, so Verify still checks what remains and still catches an old
// entry deleted by anything else.
func (p *Plugin) PruneScope(ctx context.Context, s *records.Store, scope string, cutoff time.Time) (int64, error) {
	if err := p.checkFloor(s, cutoff); err != nil {
		return 0, err
	}
	if !p.chain {
		return s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Scope, scope), sqlb.Less(At, cutoff.UTC())))
	}
	n, err := p.pruneChained(ctx, s, scope, cutoff)
	if err != nil {
		return n, err
	}
	m, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Scope, scope), sqlb.Eq(Seq, 0), sqlb.Less(At, cutoff.UTC())))
	return n + m, err
}
