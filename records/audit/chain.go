package audit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// Column names fs_audit gains for the chain (migration 0003_audit_chain).
// Entries written without WithHashChain hold 0 and "".
const (
	ColSeq      = "seq"
	ColPrevHash = "prev_hash"
	ColHash     = "hash"
)

// Typed chain columns of fs_audit.
var (
	Seq      = sqlb.Col[int64](Table, ColSeq)
	PrevHash = sqlb.Col[string](Table, ColPrevHash)
	Hash     = sqlb.Col[string](Table, ColHash)
)

// TableNameChain is the table of chain heads: one row per scope, holding
// the scope's last sequence number and hash, the anchor Prune leaves (the
// sequence number, hash and time of the last entry it deleted), and a MAC
// over all of it under the chain key, so the row cannot be rewritten (a
// cut-off tail, a faked prune) without the key.
const TableNameChain = "fs_audit_chain"

// Column names of fs_audit_chain.
const (
	ColChainScope      = "scope"
	ColChainSeq        = "seq"
	ColChainHead       = "head"
	ColChainAnchorSeq  = "anchor_seq"
	ColChainAnchorHash = "anchor_hash"
	ColChainSince      = "since"
	ColChainAnchorAt   = "anchor_at"
	ColChainMAC        = "mac"
	// ColChainKeyID is the ID of the key the row's MAC is under
	// (migration 0004_audit_keys_source); "" for WithHashChain's key.
	ColChainKeyID = "key_id"
)

// Table and typed columns of fs_audit_chain. Since is when the scope's
// chain began; Verify refuses an unchained entry written after it.
var (
	ChainTable      = sqlb.Table{Name: TableNameChain, Alias: "auc"}
	ChainScope      = sqlb.Col[string](ChainTable, ColChainScope)
	ChainSeq        = sqlb.Col[int64](ChainTable, ColChainSeq)
	ChainHead       = sqlb.Col[string](ChainTable, ColChainHead)
	ChainAnchorSeq  = sqlb.Col[int64](ChainTable, ColChainAnchorSeq)
	ChainAnchorHash = sqlb.Col[string](ChainTable, ColChainAnchorHash)
	ChainSince      = sqlb.Col[time.Time](ChainTable, ColChainSince)
	// ChainAnchorAt is the time of the last entry Prune deleted (since,
	// before any prune); ChainMAC authenticates the whole row.
	ChainAnchorAt = sqlb.Col[time.Time](ChainTable, ColChainAnchorAt)
	ChainMAC      = sqlb.Col[string](ChainTable, ColChainMAC)
	ChainKeyID    = sqlb.Col[string](ChainTable, ColChainKeyID)
)

// MaxKeyIDBytes is the longest chain key ID, the width of its columns.
const MaxKeyIDBytes = 64

// Keyring holds the chain's keys by ID, so the key can rotate (a KMS
// rotation, a compromised key replaced) while Verify still checks every
// entry under the key it was hashed with. Each entry and each chain head
// records the ID of its key.
type Keyring interface {
	// Current returns the ID and key that new entries, and the chain heads
	// they move, are hashed with. ID "" is WithHashChain's single key and
	// keeps the original hash format; any other ID is at most
	// MaxKeyIDBytes and is part of what it hashes.
	Current(ctx context.Context) (id string, key []byte, err error)
	// Key returns the key with id, for an entry or head hashed under it:
	// "" is the key entries were chained with before key IDs (the key
	// WithHashChain was given). An unknown id is an error.
	Key(ctx context.Context, id string) ([]byte, error)
}

// StaticKeyring is a Keyring held in memory: Keys by ID, CurrentID the
// one new entries use. To move a WithHashChain(key) log to key IDs, put
// key under "" and the new key under its ID, and make that ID current:
// the old entries still verify, the new ones use the new key.
type StaticKeyring struct {
	CurrentID string
	Keys      map[string][]byte
}

// Current implements Keyring.
func (k StaticKeyring) Current(ctx context.Context) (string, []byte, error) {
	key, err := k.Key(ctx, k.CurrentID)
	return k.CurrentID, key, err
}

// Key implements Keyring.
func (k StaticKeyring) Key(_ context.Context, id string) ([]byte, error) {
	key, ok := k.Keys[id]
	if !ok {
		return nil, filesystem.Errorf(filesystem.CodeNotFound, "audit: no chain key %q", id)
	}
	return key, nil
}

// WithHashChainKeyring is WithHashChain with keys by ID from ring: entries
// are hashed with ring.Current and verified with ring.Key of the ID each
// recorded, so the key rotates without breaking Verify. Entries and heads
// written with WithHashChain(key) carry the ID "", so ring.Key("") must
// return that key for them to verify. An error from ring fails the write
// (the change rolls back) or the Verify, never skips the chain. A nil ring
// is WithHashChain(nil): the empty key under "".
func WithHashChainKeyring(ring Keyring) Option {
	if ring == nil {
		ring = StaticKeyring{Keys: map[string][]byte{"": nil}}
	}
	return WithHashChainMAC(KeyringMAC(ring))
}

// Bounds of a Signer's MAC. The floor keeps a signer that returns a
// truncated or empty tag from turning the chain into a checksum anyone can
// recompute; the ceiling is what fits, in hex, in the hash columns (191
// characters on MySQL), and admits HMAC-SHA512.
const (
	MinMACBytes = 16
	MaxMACBytes = 64
)

// Signer computes the chain's MACs by key ID without handing the key to
// the process: the seam for a key that never leaves an HSM or a KMS (AWS
// KMS GenerateMac, Azure Key Vault, Vault transit hmac). A Keyring is a
// Signer through KeyringMAC, which computes HMAC-SHA256 in process; a
// remote signer that computes HMAC-SHA256 under the same key yields the
// same hashes, so a log can move between the two without breaking Verify.
type Signer interface {
	// CurrentID is the ID of the key new entries, and the chain heads they
	// move, are signed with. "" keeps the original hash format; any other
	// ID is at most MaxKeyIDBytes and is part of what it signs.
	CurrentID(ctx context.Context) (string, error)
	// MAC returns the tag of msg under the key with id, MinMACBytes to
	// MaxMACBytes long. It must be deterministic (Verify recomputes it and
	// compares) and must fail, never answer, for an unknown id.
	MAC(ctx context.Context, id string, msg []byte) ([]byte, error)
}

// KeyringMAC is the Signer of ring: HMAC-SHA256 in process under the key
// ring returns for each ID. It is what WithHashChain and
// WithHashChainKeyring use.
func KeyringMAC(ring Keyring) Signer { return keyringSigner{ring} }

// keyringSigner adapts a Keyring to Signer.
type keyringSigner struct{ ring Keyring }

// CurrentID implements Signer.
func (k keyringSigner) CurrentID(ctx context.Context) (string, error) {
	id, _, err := k.ring.Current(ctx)
	return id, err
}

// MAC implements Signer.
func (k keyringSigner) MAC(ctx context.Context, id string, msg []byte) ([]byte, error) {
	key, err := k.ring.Key(ctx, id)
	if err != nil {
		return nil, err
	}
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	return h.Sum(nil), nil
}

// WithHashChainMAC is WithHashChain with every MAC computed by signer, so
// the chain key can stay inside an HSM or a KMS. Entries are signed under
// signer.CurrentID and verified under the ID each recorded, as with
// WithHashChainKeyring. An error from signer, or a tag outside MinMACBytes
// to MaxMACBytes, fails the write (the change rolls back) or the Verify;
// it never skips the chain. A nil signer is WithHashChain(nil).
func WithHashChainMAC(signer Signer) Option {
	if signer == nil {
		signer = KeyringMAC(StaticKeyring{Keys: map[string][]byte{"": nil}})
	}
	return func(p *Plugin) { p.chain, p.signer = true, signer }
}

// mac is the hex tag of msg under the key with id, its length checked.
func (p *Plugin) mac(ctx context.Context, id string, msg []byte) (string, error) {
	tag, err := p.signer.MAC(ctx, id, msg)
	if err != nil {
		return "", err
	}
	if len(tag) < MinMACBytes || len(tag) > MaxMACBytes {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "audit: chain signer returned a %d-byte MAC, want %d to %d", len(tag), MinMACBytes, MaxMACBytes)
	}
	return hex.EncodeToString(tag), nil
}

// chainDDL is migration 0003: the chain columns, their index and the
// chain table. Columns get literal defaults (no TEXT column, so MySQL
// takes them), and every statement can run again after a crash.
func chainDDL(engine string, t records.SQLTypes) *records.DDL {
	return records.NewDDL(engine).
		AddColumn(Table, records.Column(Seq, t.Int).Default(0)).
		AddColumn(Table, records.Column(PrevHash, t.Key).Default("")).
		AddColumn(Table, records.Column(Hash, t.Key).Default("")).
		CreateIndex(Table, records.Index{Name: "fs_audit_seq", Columns: []records.IndexColumn{records.Key(Scope), records.Key(Seq)}}).
		CreateTable(records.Table{
			Table: ChainTable,
			Columns: []ddl.ColumnSpec{
				records.Column(ChainScope, t.Key), records.Column(ChainSeq, t.Int), records.Column(ChainHead, t.Key),
				records.Column(ChainAnchorSeq, t.Int), records.Column(ChainAnchorHash, t.Key), records.Column(ChainSince, t.Timestamp),
				records.Column(ChainAnchorAt, t.Timestamp), records.Column(ChainMAC, t.Key),
			},
			PrimaryKey: []ddl.ColumnRef{ChainScope},
		})
}

// WithHashChain numbers every entry of a scope in commit order (Entry.Seq)
// and chains it to the one before by an HMAC-SHA256 of its fields and the
// previous hash, keyed with key. Verify then detects an entry edited,
// deleted, reordered or inserted by anything but this plugin, and Changes
// serves the log as a change feed. Without a key (nil) the chain still
// catches edits made without recomputing it; with a key kept outside the
// database it also catches someone who can rewrite every row, since only
// the key's holder can compute a valid chain.
//
// Each scope's entries are written one transaction at a time: the
// scope's chain row is locked from the entry's write until its
// transaction commits. Off by default for that reason. The lock is taken
// mid-transaction, when the change emits its event, after the change's
// own row locks; on Postgres and MySQL two transactions that take those
// locks in opposite orders can deadlock. The database then aborts one,
// which surfaces as CodeUnavailable (records.IsLockConflict): run the
// whole transaction again. The chain row itself carries a MAC under the
// key, so its head and prune anchor cannot be rewritten without it; a
// write that finds the row altered is refused with ErrChainBroken. Keep it on once
// on; entries written while it is off are not chained, and Verify
// refuses unchained entries written after a scope's chain began.
func WithHashChain(key []byte) Option {
	return WithHashChainKeyring(StaticKeyring{Keys: map[string][]byte{"": append([]byte(nil), key...)}})
}

// currentID is the signer's current key ID, checked.
func (p *Plugin) currentID(ctx context.Context) (string, error) {
	id, err := p.signer.CurrentID(ctx)
	if err != nil {
		return "", err
	}
	if len(id) > MaxKeyIDBytes {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "audit: chain key ID is longer than %d bytes", MaxKeyIDBytes)
	}
	return id, nil
}

// WithRetentionFloor makes Prune and PruneScope refuse (CodeInvalidArgument,
// deleting nothing) a cutoff later than the Store's clock minus d, so no
// misconfigured job can delete entries a regulation requires to be kept.
// It bounds this plugin's own deletes only.
func WithRetentionFloor(d time.Duration) Option { return func(p *Plugin) { p.floor = d } }

// checkFloor refuses a cutoff inside the retention floor.
func (p *Plugin) checkFloor(s *records.Store, cutoff time.Time) error {
	if p.floor <= 0 {
		return nil
	}
	if limit := s.Now().Add(-p.floor); cutoff.After(limit) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "audit: cutoff %s crosses the retention floor of %s (entries after %s must be kept)",
			cutoff.UTC().Format(time.RFC3339), p.floor, limit.Format(time.RFC3339))
	}
	return nil
}

// link gives r the next sequence number of its scope and its hash. The
// UPDATE that takes the number locks the scope's chain row until the
// transaction commits, so the next entry of the scope waits for this one:
// numbers follow commit order, and a rolled-back entry gives its number
// back. The scope's first entry creates the row; two first entries racing
// make one INSERT fail, surfaced as CodeUnavailable for the caller to
// retry (as the quota plugin does).
func (p *Plugin) link(ctx context.Context, s *records.Store, r *row) error {
	id, err := p.currentID(ctx)
	if err != nil {
		return err
	}
	n, err := s.Exec(ctx, sqlb.NewUpdate(s.Dialect()).Table(ChainTable).
		Set(sqlb.SetExpr(ChainSeq, sqlb.Add(ChainSeq, sqlb.Param[int64](1)))).Where(sqlb.Eq(ChainScope, r.scope)))
	if err != nil {
		return retryable(err)
	}
	if n == 0 {
		r.seq, r.prev, r.keyID = 1, "", id
		if r.hash, err = p.mac(ctx, id, digestMsg(*r)); err != nil {
			return err
		}
		c := chainHead{scope: r.scope, seq: 1, head: r.hash, anchorAt: r.at, since: r.at, keyID: id}
		mac, err := p.mac(ctx, id, sealMsg(c))
		if err != nil {
			return err
		}
		_, err = s.Exec(ctx, sqlb.NewInsert(s.Dialect()).Into(ChainTable).Row(
			sqlb.Set(ChainScope, r.scope), sqlb.Set(ChainSeq, c.seq), sqlb.Set(ChainHead, c.head),
			sqlb.Set(ChainAnchorSeq, int64(0)), sqlb.Set(ChainAnchorHash, ""), sqlb.Set(ChainSince, c.since),
			sqlb.Set(ChainAnchorAt, c.anchorAt), sqlb.Set(ChainMAC, mac), sqlb.Set(ChainKeyID, c.keyID),
		))
		if records.IsUniqueViolation(err) {
			return filesystem.NewError(filesystem.CodeUnavailable, err)
		}
		return retryable(err)
	}
	c, err := head(ctx, s, r.scope)
	if err != nil {
		return retryable(err)
	}
	// The row was sealed before this entry's increment. A row rewritten
	// behind the plugin is refused, never extended: extending it would
	// seal the forgery.
	before := c
	before.seq--
	if err := p.checkHead(ctx, before, c.mac); err != nil {
		return err
	}
	// The entry and the head move to the current key: a rotation takes
	// effect on each scope's next entry.
	r.seq, r.prev, r.keyID = c.seq, c.head, id
	if r.hash, err = p.mac(ctx, id, digestMsg(*r)); err != nil {
		return err
	}
	c.head, c.keyID = r.hash, id
	mac, err := p.mac(ctx, id, sealMsg(c))
	if err != nil {
		return err
	}
	_, err = s.Exec(ctx, sqlb.NewUpdate(s.Dialect()).Table(ChainTable).
		Set(sqlb.Set(ChainHead, c.head), sqlb.Set(ChainKeyID, c.keyID), sqlb.Set(ChainMAC, mac)).Where(sqlb.Eq(ChainScope, r.scope)))
	return retryable(err)
}

// checkHead refuses a chain row whose stored MAC is not c's under the key
// the row names: a row rewritten behind the plugin.
func (p *Plugin) checkHead(ctx context.Context, c chainHead, stored string) error {
	mac, err := p.mac(ctx, c.keyID, sealMsg(c))
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(mac), []byte(stored)) {
		return broken(c.scope, "the chain head was altered")
	}
	return nil
}

// retryable turns the database aborting the transaction over a lock
// conflict into CodeUnavailable: the whole transaction rolled back and
// can be run again.
func retryable(err error) error {
	if records.IsLockConflict(err) {
		return filesystem.NewError(filesystem.CodeUnavailable, err)
	}
	return err
}

// hashTimeLayout writes an entry's time for its hash: UTC at the
// microsecond precision every engine stores.
const hashTimeLayout = "2006-01-02T15:04:05.000000Z"

// Format markers of the second hash format, which an entry with a key ID
// or a source, and a head with a key ID, use. Each is written first, and
// no first field of the original format can equal it (an entry's starts
// with its decimal sequence number, a head's with "chain"), so the two
// formats never encode alike.
const (
	digestV2 = "v2"
	sealV2   = "chain2"
)

// digest is r's hash under key in process: HMAC-SHA256 of digestMsg(r),
// in hex, what KeyringMAC signs.
func digest(r row, key []byte) string { return hmacHex(key, digestMsg(r)) }

// hmacHex is HMAC-SHA256 of msg under key, in hex.
func hmacHex(key, msg []byte) string {
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	return hex.EncodeToString(h.Sum(nil))
}

// digestMsg is what r's hash signs: every field of r, each written with
// its length so no two rows encode alike, and the previous hash. A row
// with no key ID and no source encodes exactly as it did before either
// existed, so chains written then still verify; one with either is
// encoded with digestV2, its key ID first and its source last, so neither
// can be changed or removed without breaking the hash.
func digestMsg(r row) []byte {
	fields := []string{
		strconv.FormatInt(r.seq, 10), r.prev, r.id, r.scope, r.at.UTC().Format(hashTimeLayout), r.actor, r.kind,
		r.fileID, r.attachmentID, r.ownerType, r.ownerID, r.role, r.subject, r.detail,
	}
	if r.keyID != "" || r.source != (records.Source{}) {
		fields = append(append([]string{digestV2, r.keyID}, fields...), r.source.Channel, r.source.Client, r.source.IP)
	}
	return encodeFields(fields)
}

// encodeFields writes each field as its length, a colon, the field and a
// semicolon, so no two field lists encode alike.
func encodeFields(fields []string) []byte {
	var b []byte
	for _, f := range fields {
		b = fmt.Appendf(b, "%d:%s;", len(f), f)
	}
	return b
}

// chainHead is one fs_audit_chain row.
type chainHead struct {
	scope                 string
	seq, anchorSeq        int64
	head, anchorHash, mac string
	since, anchorAt       time.Time
	// keyID is the ID of the key mac is under; "" for WithHashChain's.
	keyID string
}

// seal is c's MAC under key in process: HMAC-SHA256 of sealMsg(c), in
// hex, what KeyringMAC signs.
func seal(c chainHead, key []byte) string { return hmacHex(key, sealMsg(c)) }

// sealMsg is what c's MAC signs: every field but the MAC itself. A head
// with no key ID encodes exactly as before key IDs existed; one with an
// ID is encoded with sealV2 and the ID first.
func sealMsg(c chainHead) []byte {
	fields := []string{
		"chain", c.scope, strconv.FormatInt(c.seq, 10), c.head, strconv.FormatInt(c.anchorSeq, 10), c.anchorHash,
		c.anchorAt.UTC().Format(hashTimeLayout), c.since.UTC().Format(hashTimeLayout),
	}
	if c.keyID != "" {
		fields = append([]string{sealV2, c.keyID}, fields...)
	}
	return encodeFields(fields)
}

// head reads scope's chain row, CodeNotFound when it has none.
func head(ctx context.Context, s *records.Store, scope string) (chainHead, error) {
	c := chainHead{scope: scope}
	err := s.One(ctx, sqlb.NewSelect(s.Dialect()).Columns(
		sqlb.Bind(ChainSeq, &c.seq), sqlb.Bind(ChainHead, &c.head), sqlb.Bind(ChainAnchorSeq, &c.anchorSeq),
		sqlb.Bind(ChainAnchorHash, &c.anchorHash), sqlb.Bind(ChainSince, &c.since), sqlb.Bind(ChainAnchorAt, &c.anchorAt), sqlb.Bind(ChainMAC, &c.mac),
		sqlb.Bind(ChainKeyID, &c.keyID),
	).From(ChainTable).Where(sqlb.Eq(ChainScope, scope)))
	if errors.Is(err, sqlb.ErrNoRows) {
		return c, filesystem.Errorf(filesystem.CodeNotFound, "audit: scope %q has no chain", scope)
	}
	return c, err
}

// ErrChainBroken is the cause of every Verify failure (errors.Is), whose
// message names the scope and the first entry that does not check out.
var ErrChainBroken = errors.New("audit: chain broken")

// Verify checks scope's chain from its anchor to its head: every entry's
// sequence number follows the last, its previous hash is the last one's
// hash, its own hash matches its fields under the plugin's key, the last
// entry is the chain's head, no unchained entry was written after the
// chain began, the chain row's MAC holds (its head and prune anchor were
// not rewritten), and, with WithRetentionFloor, no entry was pruned
// inside the floor. It returns how many entries it checked, or an error
// wrapping ErrChainBroken (CodeConflict) at the first that fails. A scope
// with no chain checks nothing. Verify reads; it repairs nothing.
func (p *Plugin) Verify(ctx context.Context, s *records.Store, scope string) (int64, error) {
	c, err := head(ctx, s, scope)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		var n int64
		if err := s.One(ctx, sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(sqlb.Count(), &n)).From(Table).
			Where(sqlb.Eq(Scope, scope), sqlb.Greater(Seq, 0))); err != nil {
			return 0, err
		}
		if n > 0 {
			return 0, broken(scope, "has %d chained entries but no chain head", n)
		}
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if err := p.checkHead(ctx, c, c.mac); err != nil {
		return 0, err
	}
	if p.floor > 0 && c.anchorSeq > 0 && c.anchorAt.After(s.Now().Add(-p.floor)) {
		return 0, broken(scope, "entries up to %d, written %s, were pruned inside the retention floor of %s", c.anchorSeq, c.anchorAt.UTC().Format(time.RFC3339), p.floor)
	}
	var r row
	q := sqlb.NewSelect(s.Dialect()).Columns(
		sqlb.Bind(ID, &r.id), sqlb.Bind(Scope, &r.scope), sqlb.Bind(At, &r.at), sqlb.Bind(Actor, &r.actor), sqlb.Bind(Kind, &r.kind),
		sqlb.Bind(FileID, &r.fileID), sqlb.Bind(AttachmentID, &r.attachmentID), sqlb.Bind(OwnerType, &r.ownerType), sqlb.Bind(OwnerID, &r.ownerID),
		sqlb.Bind(Role, &r.role), sqlb.Bind(Subject, &r.subject), sqlb.Bind(Detail, &r.detail), sqlb.Bind(Seq, &r.seq),
		sqlb.Bind(PrevHash, &r.prev), sqlb.Bind(Hash, &r.hash), sqlb.Bind(KeyID, &r.keyID),
		sqlb.Bind(SourceChannel, &r.source.Channel), sqlb.Bind(SourceClient, &r.source.Client), sqlb.Bind(SourceIP, &r.source.IP),
	).From(Table).Where(sqlb.Eq(Scope, scope), sqlb.Greater(Seq, 0)).Order(sqlb.OrderBy(Seq, sqlb.SortAsc))
	next, prev := c.anchorSeq+1, c.anchorHash
	var checked int64
	err = s.Each(ctx, q, func() error {
		switch {
		case r.seq != next:
			return broken(scope, "expected entry %d, found %d", next, r.seq)
		case r.prev != prev:
			return broken(scope, "entry %d does not follow entry %d", r.seq, r.seq-1)
		}
		want, err := p.mac(ctx, r.keyID, digestMsg(r))
		if err != nil {
			return err
		}
		if !hmac.Equal([]byte(want), []byte(r.hash)) {
			return broken(scope, "entry %d (%s) was altered", r.seq, r.id)
		}
		next, prev = r.seq+1, r.hash
		checked++
		return nil
	})
	if err != nil {
		return checked, err
	}
	if next-1 != c.seq || prev != c.head {
		return checked, broken(scope, "the log ends at entry %d but the chain's head is entry %d", next-1, c.seq)
	}
	var loose int64
	if err := s.One(ctx, sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(sqlb.Count(), &loose)).From(Table).
		Where(sqlb.Eq(Scope, scope), sqlb.Eq(Seq, 0), sqlb.GreaterEq(At, c.since))); err != nil {
		return checked, err
	}
	if loose > 0 {
		return checked, broken(scope, "%d unchained entries were written after the chain began", loose)
	}
	return checked, nil
}

func broken(scope, format string, args ...any) error {
	return filesystem.Errorf(filesystem.CodeConflict, "%w: scope %q: %s", ErrChainBroken, scope, fmt.Sprintf(format, args...))
}

// chainedScopes lists the scopes with a chain, for Prune.
func (p *Plugin) chainedScopes(ctx context.Context, s *records.Store) ([]string, error) {
	var scope string
	var out []string
	err := s.Each(ctx, sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(ChainScope, &scope)).From(ChainTable),
		func() error { out = append(out, scope); return nil })
	return out, err
}

// pruneChained deletes scope's chained entries in sequence order up to,
// not including, the first written at or after cutoff, and moves the
// chain's anchor to the last one deleted, in one transaction that holds
// the chain row's lock, so no entry is written meanwhile.
func (p *Plugin) pruneChained(ctx context.Context, s *records.Store, scope string, cutoff time.Time) (int64, error) {
	var deleted int64
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		// A no-op write takes the row lock before anything is read. Its
		// row count says nothing: MySQL counts changed rows, not matched
		// ones, so a write that changes nothing reports 0. The read after
		// it tells whether the scope has a chain.
		if _, err := s.Exec(ctx, sqlb.NewUpdate(s.Dialect()).Table(ChainTable).
			Set(sqlb.SetExpr(ChainSeq, ChainSeq)).Where(sqlb.Eq(ChainScope, scope))); err != nil {
			return retryable(err)
		}
		c, err := head(ctx, s, scope)
		if filesystem.CodeOf(err) == filesystem.CodeNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		if err := p.checkHead(ctx, c, c.mac); err != nil {
			return err
		}
		var first int64
		if err := s.One(ctx, sqlb.NewSelect(s.Dialect()).Columns(
			sqlb.Bind(sqlb.Coalesce(sqlb.Min(Seq), sqlb.Param(c.seq+1)), &first),
		).From(Table).Where(sqlb.Eq(Scope, scope), sqlb.Greater(Seq, c.anchorSeq), sqlb.GreaterEq(At, cutoff.UTC()))); err != nil {
			return err
		}
		last := first - 1
		if last <= c.anchorSeq {
			return nil
		}
		var anchor string
		var anchorAt time.Time
		if err := s.One(ctx, sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(Hash, &anchor), sqlb.Bind(At, &anchorAt)).From(Table).
			Where(sqlb.Eq(Scope, scope), sqlb.Eq(Seq, last))); errors.Is(err, sqlb.ErrNoRows) {
			return fmt.Errorf("audit: prune scope %q: %w", scope, broken(scope, "entry %d is missing", last))
		} else if err != nil {
			// A failed read is not a missing entry: reporting it as one
			// would call a lost connection tampering.
			return err
		}
		if deleted, err = s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).
			Where(sqlb.Eq(Scope, scope), sqlb.Greater(Seq, 0), sqlb.LessEq(Seq, last))); err != nil {
			return err
		}
		c.anchorSeq, c.anchorHash, c.anchorAt = last, anchor, anchorAt.UTC()
		mac, err := p.mac(ctx, c.keyID, sealMsg(c))
		if err != nil {
			return err
		}
		_, err = s.Exec(ctx, sqlb.NewUpdate(s.Dialect()).Table(ChainTable).
			Set(sqlb.Set(ChainAnchorSeq, c.anchorSeq), sqlb.Set(ChainAnchorHash, c.anchorHash), sqlb.Set(ChainAnchorAt, c.anchorAt), sqlb.Set(ChainMAC, mac)).
			Where(sqlb.Eq(ChainScope, scope)))
		return retryable(err)
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}
