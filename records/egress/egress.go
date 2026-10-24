// Package egress is a records plugin that meters the bytes each scope
// serves, in daily buckets, from the reads handlers report with
// records.Store.Served (recordshttp's file route, share links, image
// variants), so a plan's bandwidth can be shown, limited by the
// application and billed.
//
// A bucket is one row per scope and UTC day (or WithBucket's bucket: an
// hour, a day in the tenant's time zone), charged with one UPDATE per
// read (an INSERT for the day's first), so concurrent reads never lose a
// count. Report turns finished days into egress.daily_total events
// (records.Store.Emit) for a billing pipeline: each carries what the day
// gained since its last report and a deterministic event ID, so a consumer
// that drops IDs it has seen counts every byte exactly once, even when a
// read lands after its day was first reported.
//
// WithPerSubject adds a breakdown by what was served: the bytes of each
// share link (records.ServeShare, whose Subject is the link's ID), or of
// each image variant, per bucket, read with SubjectDays. It feeds a
// "this link used 4 GB" panel or a per-link cap the application enforces;
// billing stays on the per-scope buckets.
package egress

import (
	"context"
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

// PluginName is the plugin's name in the migration ledger and the prefix
// of its event kinds.
const PluginName = "egress"

// TableNameDaily is the table the plugin owns: one row per scope and day.
const TableNameDaily = "fs_egress_daily"

// Column names of fs_egress_daily.
const (
	ColScope            = "scope"
	ColDay              = "day"
	ColBytes            = "bytes"
	ColRequests         = "requests"
	ColReportedBytes    = "reported_bytes"
	ColReportedRequests = "reported_requests"
	ColUpdatedAt        = "updated_at"
)

// Table and typed columns of fs_egress_daily. Day is the UTC date as
// DayLayout text, so it sorts and compares the same on every engine.
var (
	Table            = sqlb.Table{Name: TableNameDaily, Alias: "eg"}
	Scope            = sqlb.Col[string](Table, ColScope)
	Day              = sqlb.Col[string](Table, ColDay)
	Bytes            = sqlb.Col[int64](Table, ColBytes)
	Requests         = sqlb.Col[int64](Table, ColRequests)
	ReportedBytes    = sqlb.Col[int64](Table, ColReportedBytes)
	ReportedRequests = sqlb.Col[int64](Table, ColReportedRequests)
	UpdatedAt        = sqlb.Col[time.Time](Table, ColUpdatedAt)
)

// DayLayout is how a bucket's day is written: a UTC date.
const DayLayout = time.DateOnly

// EventDailyTotal is the kind of the events Report emits.
const EventDailyTotal records.EventKind = PluginName + ".daily_total"

// Detail keys of an egress.daily_total event. Values are decimal integers
// except DetailDay, a DayLayout date.
const (
	DetailDay = "day"
	// DetailBytes and DetailRequests are what the day gained since its
	// previous report: the amounts a billing consumer adds.
	DetailBytes    = "bytes"
	DetailRequests = "requests"
	// DetailTotalBytes and DetailTotalRequests are the day's totals so far.
	DetailTotalBytes    = "total_bytes"
	DetailTotalRequests = "total_requests"
)

// eventIDPrefix starts the deterministic IDs of daily_total events.
const eventIDPrefix = "egr_"

// eventIDHashBytes is how much of the bucket's SHA-256 an event ID keeps:
// 128 bits make a collision between two buckets negligible while the ID
// (prefix plus 32 hex digits) stays well under records.MaxEventIDBytes.
const eventIDHashBytes = 16

// MaxBucketBytes is the longest bucket key, the indexed width of the day
// column (SQLTypes.Short).
const MaxBucketBytes = 64

// BucketFunc names the bucket a read of scope at time at is metered in:
// a UTC day by default (UTCDay), or an hour, or a day in the tenant's own
// time zone. Its keys are stored in the day column and sent as
// DetailDay, so for one scope they must sort, as text, in time order
// (Report treats every bucket before the current one as finished, and
// Days, Total and Prune compare keys), depend only on scope and at, and
// be 1 to MaxBucketBytes bytes.
type BucketFunc func(scope string, at time.Time) string

// UTCDay is the default BucketFunc: at's UTC date as DayLayout text.
func UTCDay(_ string, at time.Time) string { return at.UTC().Format(DayLayout) }

// BucketContextFunc is BucketFunc with the read's context and an error,
// for a bucket that needs a lookup: a tenant's time zone kept in the
// application's database. The keys follow BucketFunc's rules. An error
// fails what asked for the key (the read's metering, Days, Report,
// Prune) rather than metering in a guessed bucket.
type BucketContextFunc func(ctx context.Context, scope string, at time.Time) (string, error)

// Plugin meters reads. Construct it with New and install it with
// records.WithPlugins.
type Plugin struct {
	// bucket is WithBucket's or WithBucketContext's function; nil is
	// UTCDay, and lets Report and Prune select by one key for every scope.
	bucket BucketContextFunc
	// subject is WithSubjectFunc's function; nil meters Served.Subject for
	// the kinds WithPerSubject lists.
	subject SubjectFunc
	// perSubject is WithPerSubject's kinds; empty meters per scope only.
	perSubject map[records.ServeKind]bool
	// err is the first invalid option: every read's metering returns it,
	// so a misconfigured plugin refuses reads rather than miscounting.
	err error
}

// Option configures a Plugin.
type Option func(*Plugin)

// WithBucket meters reads in fn's buckets instead of UTC days: hourly
// metering, or billing days in each tenant's time zone. Report then
// treats a bucket as finished once fn(scope, now) names a later one. A
// key fn returns that is empty or longer than MaxBucketBytes fails the
// read's metering (records.Store.Served returns the error) rather than
// being cut short. Keep one fn for the life of the data: buckets already
// stored keep the keys they were written with.
func WithBucket(fn BucketFunc) Option {
	if fn == nil {
		return WithBucketContext(nil)
	}
	return WithBucketContext(func(_ context.Context, scope string, at time.Time) (string, error) { return fn(scope, at), nil })
}

// WithBucketContext is WithBucket with a function that gets the context
// and may fail: billing days in a tenant's time zone looked up per read
// from the application's database, with no preloaded cache. An error
// from fn fails the read's metering (records.Store.Served returns it, so
// the handler refuses the read), and Days, Total, Report and Prune
// return it: nothing is metered in, or reported from, a bucket fn did
// not name. The last of WithBucket and WithBucketContext wins; nil keeps
// UTC days.
func WithBucketContext(fn BucketContextFunc) Option { return func(p *Plugin) { p.bucket = fn } }

// New returns a Plugin.
func New(opts ...Option) *Plugin {
	p := &Plugin{}
	for _, o := range opts {
		o(p)
	}
	return p
}

// keyOf is the bucket a read of scope at at falls in, checked.
func (p *Plugin) keyOf(ctx context.Context, scope string, at time.Time) (string, error) {
	if p.bucket == nil {
		return UTCDay(scope, at), nil
	}
	key, err := p.bucket(ctx, scope, at)
	if err != nil {
		return "", err
	}
	if key == "" || len(key) > MaxBucketBytes {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "egress: bucket key %q is not 1 to %d bytes", key, MaxBucketBytes)
	}
	return key, nil
}

// Name implements records.Plugin.
func (p *Plugin) Name() string { return PluginName }

var _ records.ServeHooks = (*Plugin)(nil)

// Migrations implements records.Migrator.
func (p *Plugin) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	subjects, err2 := subjectMigration(engine)
	st, err := records.NewDDL(engine).CreateTable(records.Table{
		Table: Table,
		Columns: []ddl.ColumnSpec{
			records.Column(Scope, t.Key), records.Column(Day, t.Short), records.Column(Bytes, t.Int), records.Column(Requests, t.Int),
			records.Column(ReportedBytes, t.Int), records.Column(ReportedRequests, t.Int), records.Column(UpdatedAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{Scope, Day},
		Indexes:    []records.Index{{Name: "fs_egress_daily_day", Columns: []records.IndexColumn{records.Key(Day), records.Key(Scope)}}},
	}).Statements()
	return []records.Migration{
		{Owner: PluginName, Name: "0001_egress_daily", Statements: st},
		{Owner: PluginName, Name: "0002_egress_subjects", Statements: subjects},
	}, errors.Join(err, err2)
}

// OnServed implements records.ServeHooks: it adds the read's bytes and one
// request to its scope's bucket for sv.At (the UTC day, or WithBucket's).
// Two first reads of a bucket racing each other make one INSERT fail; that
// read then charges the row the other created.
func (p *Plugin) OnServed(ctx context.Context, s *records.Store, sv records.Served) error {
	if p.err != nil {
		return p.err
	}
	day, err := p.keyOf(ctx, sv.Scope, sv.At)
	if err != nil {
		return err
	}
	if err := p.chargeScope(ctx, s, sv, day); err != nil {
		return err
	}
	// Not one transaction with the scope's charge: a racing first read
	// is retried after a unique violation, which aborts a Postgres
	// transaction. A subject charge that fails leaves the read counted for
	// its scope, so billing errs on the side of the bytes served.
	return p.chargeSubject(ctx, s, sv, day)
}

// chargeScope adds a read to its scope's bucket day.
func (p *Plugin) chargeScope(ctx context.Context, s *records.Store, sv records.Served, day string) error {
	if n, err := p.add(ctx, s, sv.Scope, day, sv.Bytes); err != nil || n == 1 {
		return err
	}
	ins := sqlb.NewInsert(s.Dialect()).Into(Table).Row(
		sqlb.Set(Scope, sv.Scope), sqlb.Set(Day, day), sqlb.Set(Bytes, sv.Bytes), sqlb.Set(Requests, int64(1)),
		sqlb.Set(ReportedBytes, int64(0)), sqlb.Set(ReportedRequests, int64(0)), sqlb.Set(UpdatedAt, sv.At),
	)
	_, err := s.Exec(ctx, ins)
	if !records.IsUniqueViolation(err) {
		return err
	}
	_, err = p.add(ctx, s, sv.Scope, day, sv.Bytes)
	return err
}

// add charges an existing bucket and reports how many rows it changed.
func (p *Plugin) add(ctx context.Context, s *records.Store, scope, day string, n int64) (int64, error) {
	return s.Exec(ctx, sqlb.NewUpdate(s.Dialect()).Table(Table).Set(
		sqlb.SetExpr(Bytes, sqlb.Add(Bytes, sqlb.Param(n))),
		sqlb.SetExpr(Requests, sqlb.Add(Requests, sqlb.Param[int64](1))),
		sqlb.Set(UpdatedAt, s.Now()),
	).Where(sqlb.Eq(Scope, scope), sqlb.Eq(Day, day)))
}

// Bucket is one scope's reads on one UTC day (or in one WithBucket
// bucket, whose key Day then holds).
type Bucket struct {
	Scope    string
	Day      string
	Bytes    int64
	Requests int64
}

// Days returns scope's buckets for the UTC days from from's day up to,
// but not including, to's day, oldest first. Days without reads have no
// bucket.
func (p *Plugin) Days(ctx context.Context, s *records.Store, scope string, from, to time.Time) ([]Bucket, error) {
	var b Bucket
	var out []Bucket
	where, err := p.period(ctx, scope, from, to)
	if err != nil {
		return nil, err
	}
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(Scope, &b.Scope), sqlb.Bind(Day, &b.Day), sqlb.Bind(Bytes, &b.Bytes), sqlb.Bind(Requests, &b.Requests)).
		From(Table).Where(where...).Order(sqlb.OrderBy(Day, sqlb.SortAsc))
	err = s.Each(ctx, q, func() error { out = append(out, b); return nil })
	return out, err
}

// Total sums scope's reads over the same days as Days.
func (p *Plugin) Total(ctx context.Context, s *records.Store, scope string, from, to time.Time) (Bucket, error) {
	b := Bucket{Scope: scope}
	where, err := p.period(ctx, scope, from, to)
	if err != nil {
		return b, err
	}
	zero := sqlb.Param[int64](0)
	q := sqlb.NewSelect(s.Dialect()).Columns(
		sqlb.Bind(sqlb.Coalesce(sqlb.Sum(Bytes), zero), &b.Bytes), sqlb.Bind(sqlb.Coalesce(sqlb.Sum(Requests), zero), &b.Requests),
	).From(Table).Where(where...)
	return b, s.One(ctx, q)
}

// period selects scope's buckets from from's bucket up to, not including,
// to's.
func (p *Plugin) period(ctx context.Context, scope string, from, to time.Time) ([]sqlb.Predicate, error) {
	first, err := p.keyOf(ctx, scope, from)
	if err != nil {
		return nil, err
	}
	end, err := p.keyOf(ctx, scope, to)
	if err != nil {
		return nil, err
	}
	return []sqlb.Predicate{sqlb.Eq(Scope, scope), sqlb.GreaterEq(Day, first), sqlb.Less(Day, end)}, nil
}

// Report emits an egress.daily_total event for every bucket of a UTC day
// before now's whose totals grew since its last report, and returns how
// many it emitted. Each event and the bucket's new reported mark commit in
// one transaction, so a crash between buckets loses nothing and repeats
// nothing; a read that lands on a reported day (a slow request finishing
// after midnight) is reported by the next run as a further event. Two
// runs racing each other emit each change once: the mark is written only
// if it is still what the run read. Schedule it daily or more often
// (jobs.EgressReport). The plugin must be installed.
func (p *Plugin) Report(ctx context.Context, s *records.Store, now time.Time) (int, error) {
	type due struct{ b, reported Bucket }
	var row due
	var list []due
	// Every read adds a request, so a bucket grew exactly when its
	// requests passed its reported mark.
	where := []sqlb.Predicate{sqlb.NotEqExpr(Requests, ReportedRequests)}
	if p.bucket == nil {
		// One current bucket for every scope: finished ones are selected
		// in SQL. A WithBucket function may differ per scope, so those are
		// filtered below instead.
		where = append(where, sqlb.Less(Day, UTCDay("", now)))
	}
	q := sqlb.NewSelect(s.Dialect()).Columns(
		sqlb.Bind(Scope, &row.b.Scope), sqlb.Bind(Day, &row.b.Day), sqlb.Bind(Bytes, &row.b.Bytes), sqlb.Bind(Requests, &row.b.Requests),
		sqlb.Bind(ReportedBytes, &row.reported.Bytes), sqlb.Bind(ReportedRequests, &row.reported.Requests),
	).From(Table).Where(where...).Order(sqlb.OrderBy(Day, sqlb.SortAsc), sqlb.OrderBy(Scope, sqlb.SortAsc))
	if err := s.Each(ctx, q, func() error { list = append(list, row); return nil }); err != nil {
		return 0, err
	}
	sent := 0
	for _, d := range list {
		current, err := p.keyOf(ctx, d.b.Scope, now)
		if err != nil {
			return sent, err
		}
		if d.b.Day >= current {
			continue // the bucket is still filling
		}
		ok, err := p.report(ctx, s, d.b, d.reported)
		if err != nil {
			return sent, err
		}
		if ok {
			sent++
		}
	}
	return sent, nil
}

// errRaced rolls back a report another run already made.
var errRaced = errors.New("egress: bucket reported by another run")

// report emits one bucket's growth and moves its mark, or does neither
// when another run moved the mark first.
func (p *Plugin) report(ctx context.Context, s *records.Store, b, reported Bucket) (bool, error) {
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		n, err := s.Exec(ctx, sqlb.NewUpdate(s.Dialect()).Table(Table).Set(
			sqlb.Set(ReportedBytes, b.Bytes), sqlb.Set(ReportedRequests, b.Requests), sqlb.Set(UpdatedAt, s.Now()),
		).Where(sqlb.Eq(Scope, b.Scope), sqlb.Eq(Day, b.Day), sqlb.Eq(ReportedBytes, reported.Bytes), sqlb.Eq(ReportedRequests, reported.Requests)))
		if err != nil {
			return err
		}
		if n != 1 {
			return errRaced
		}
		return s.Emit(ctx, p, records.Event{
			ID: eventID(b), Kind: EventDailyTotal, Scope: b.Scope,
			Detail: map[string]string{
				DetailDay:           b.Day,
				DetailBytes:         strconv.FormatInt(b.Bytes-reported.Bytes, 10),
				DetailRequests:      strconv.FormatInt(b.Requests-reported.Requests, 10),
				DetailTotalBytes:    strconv.FormatInt(b.Bytes, 10),
				DetailTotalRequests: strconv.FormatInt(b.Requests, 10),
			},
		})
	})
	if errors.Is(err, errRaced) {
		return false, nil
	}
	if err != nil {
		return false, filesystem.Errorf(filesystem.CodeOf(err), "egress: report scope %q day %s: %w", b.Scope, b.Day, err)
	}
	return true, nil
}

// Prune deletes the buckets before cutoff's (its UTC day, or its
// WithBucket bucket in each scope) that Report has fully reported, and
// the per-subject buckets before it (WithPerSubject: those are never
// reported), and returns how many it deleted: buckets outlive the files they metered (a
// purged scope's reads are still billed), so schedule it with a retention
// longer than billing needs. A bucket with unreported reads is kept
// whatever its age.
func (p *Plugin) Prune(ctx context.Context, s *records.Store, cutoff time.Time) (int64, error) {
	if p.bucket == nil {
		n, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(
			sqlb.Less(Day, UTCDay("", cutoff)), sqlb.EqExpr(Requests, ReportedRequests),
		))
		if err != nil {
			return n, err
		}
		m, err := p.pruneSubjects(ctx, s, cutoff)
		return n + m, err
	}
	// A WithBucket key may differ per scope, so each scope is pruned
	// against its own cutoff bucket.
	var scope string
	var scopes []string
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(Scope, &scope)).From(Table).Where(sqlb.EqExpr(Requests, ReportedRequests)).GroupBy(Scope)
	if err := s.Each(ctx, q, func() error { scopes = append(scopes, scope); return nil }); err != nil {
		return 0, err
	}
	total, err := p.pruneSubjects(ctx, s, cutoff)
	if err != nil {
		return total, err
	}
	for _, sc := range scopes {
		key, err := p.keyOf(ctx, sc, cutoff)
		if err != nil {
			return total, err
		}
		n, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(
			sqlb.Eq(Scope, sc), sqlb.Less(Day, key), sqlb.EqExpr(Requests, ReportedRequests),
		))
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// eventID is the same for the same bucket at the same totals, so a report
// repeated for any reason carries the ID of the first, and differs for
// every later growth of the day.
func eventID(b Bucket) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%d:%s|%s|%d|%d", len(b.Scope), b.Scope, b.Day, b.Bytes, b.Requests))
	return eventIDPrefix + hex.EncodeToString(sum[:eventIDHashBytes])
}
