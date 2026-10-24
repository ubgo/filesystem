package egress

import (
	"context"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// TableNameSubjects is the per-subject breakdown (WithPerSubject): one
// row per scope, serve kind, subject and bucket.
const TableNameSubjects = "fs_egress_subjects"

// Column names of fs_egress_subjects that fs_egress_daily does not have.
const (
	ColKind    = "kind"
	ColSubject = "subject"
)

// MaxSubjectBytes is the longest subject metered (a share link ID, a
// variant name), the indexed width of its column.
const MaxSubjectBytes = records.MaxActorBytes

// SubjectTable and its typed columns. Day holds the same bucket keys as
// fs_egress_daily (UTCDay, or WithBucket's).
var (
	SubjectTable     = sqlb.Table{Name: TableNameSubjects, Alias: "egs"}
	SubjectScope     = sqlb.Col[string](SubjectTable, ColScope)
	SubjectKind      = sqlb.Col[records.ServeKind](SubjectTable, ColKind)
	SubjectID        = sqlb.Col[string](SubjectTable, ColSubject)
	SubjectDay       = sqlb.Col[string](SubjectTable, ColDay)
	SubjectBytes     = sqlb.Col[int64](SubjectTable, ColBytes)
	SubjectRequests  = sqlb.Col[int64](SubjectTable, ColRequests)
	SubjectUpdatedAt = sqlb.Col[time.Time](SubjectTable, ColUpdatedAt)
)

// SubjectBucket is one subject's reads in one bucket: a share link's
// bytes on one day.
type SubjectBucket struct {
	Scope    string
	Kind     records.ServeKind
	Subject  string
	Day      string
	Bytes    int64
	Requests int64
}

// WithPerSubject meters each read of the given kinds in a bucket of its
// own subject as well as in its scope's: WithPerSubject(records.ServeShare)
// gives the bytes of each share link per day (records.Served.Subject is
// the link's ID, which the share plugin's handler reports), and
// records.ServeVariant those of each image variant. A read of such a
// kind without a Subject is metered per scope only. Kinds must be valid
// (records.ServeKind.Valid); an invalid one makes every read's metering
// fail (CodeInvalidArgument), so nothing is served unmetered. The
// breakdown is for display and per-link limits the application enforces;
// Report and billing stay per scope.
func WithPerSubject(kinds ...records.ServeKind) Option {
	return func(p *Plugin) {
		for _, k := range kinds {
			if !k.Valid() {
				if p.err == nil {
					p.err = filesystem.Errorf(filesystem.CodeInvalidArgument, "egress: serve kind %q is not valid", k)
				}
				return
			}
			if p.perSubject == nil {
				p.perSubject = map[records.ServeKind]bool{}
			}
			p.perSubject[k] = true
		}
	}
}

// SubjectFunc names the subject a read is broken down by, "" for none:
// the file of a plain link (sv.FileID with records.ServeLink, for "top
// downloaded files" or a per-file bandwidth cap), or a downloader or API
// key the application put on ctx. The row keeps sv.Kind beside the
// subject, so SubjectDays reads it back with that kind. A subject is at
// most MaxSubjectBytes. An error fails the read's metering, after its
// scope was charged, as a failed breakdown write does.
type SubjectFunc func(ctx context.Context, sv records.Served) (string, error)

// WithSubjectFunc breaks reads down by fn's subject instead of
// Served.Subject: fn sees every read, of every kind, and WithPerSubject's
// kinds are not consulted. Without it the subject is Served.Subject for
// the kinds WithPerSubject lists (a share link's ID, a variant's name),
// and plain links are never broken down. nil keeps that default.
func WithSubjectFunc(fn SubjectFunc) Option { return func(p *Plugin) { p.subject = fn } }

// subjectOf is the subject sv is broken down by, "" for none.
func (p *Plugin) subjectOf(ctx context.Context, sv records.Served) (string, error) {
	if p.subject != nil {
		return p.subject(ctx, sv)
	}
	if !p.perSubject[sv.Kind] {
		return "", nil
	}
	return sv.Subject, nil
}

// subjectMigration creates fs_egress_subjects.
func subjectMigration(engine string) ([]string, error) {
	t := records.TypesFor(engine)
	return records.NewDDL(engine).CreateTable(records.Table{
		Table: SubjectTable,
		Columns: []ddl.ColumnSpec{
			records.Column(SubjectScope, t.Key), records.Column(SubjectKind, t.Short), records.Column(SubjectID, t.Key),
			records.Column(SubjectDay, t.Short), records.Column(SubjectBytes, t.Int), records.Column(SubjectRequests, t.Int),
			records.Column(SubjectUpdatedAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{SubjectScope, SubjectKind, SubjectID, SubjectDay},
		Indexes:    []records.Index{{Name: "fs_egress_subjects_day", Columns: []records.IndexColumn{records.Key(SubjectDay)}}},
	}).Statements()
}

// chargeSubject adds a read to its subject's bucket day when it has one
// (subjectOf): one UPDATE, an INSERT for the bucket's first read, and the
// UPDATE again when a racing first read inserted it.
func (p *Plugin) chargeSubject(ctx context.Context, s *records.Store, sv records.Served, day string) error {
	subject, err := p.subjectOf(ctx, sv)
	if err != nil || subject == "" {
		return err
	}
	sv.Subject = subject
	if len(sv.Subject) > MaxSubjectBytes {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "egress: subject of %d bytes, more than %d", len(sv.Subject), MaxSubjectBytes)
	}
	where := []sqlb.Predicate{sqlb.Eq(SubjectScope, sv.Scope), sqlb.Eq(SubjectKind, sv.Kind), sqlb.Eq(SubjectID, sv.Subject), sqlb.Eq(SubjectDay, day)}
	add := sqlb.NewUpdate(s.Dialect()).Table(SubjectTable).Set(
		sqlb.SetExpr(SubjectBytes, sqlb.Add(SubjectBytes, sqlb.Param(sv.Bytes))),
		sqlb.SetExpr(SubjectRequests, sqlb.Add(SubjectRequests, sqlb.Param[int64](1))),
		sqlb.Set(SubjectUpdatedAt, s.Now()),
	).Where(where...)
	if n, err := s.Exec(ctx, add); err != nil || n == 1 {
		return err
	}
	ins := sqlb.NewInsert(s.Dialect()).Into(SubjectTable).Row(
		sqlb.Set(SubjectScope, sv.Scope), sqlb.Set(SubjectKind, sv.Kind), sqlb.Set(SubjectID, sv.Subject), sqlb.Set(SubjectDay, day),
		sqlb.Set(SubjectBytes, sv.Bytes), sqlb.Set(SubjectRequests, int64(1)), sqlb.Set(SubjectUpdatedAt, sv.At),
	)
	_, err = s.Exec(ctx, ins)
	if !records.IsUniqueViolation(err) {
		return err
	}
	_, err = s.Exec(ctx, add)
	return err
}

// SubjectDays returns one subject's buckets (a share link's days, with
// kind records.ServeShare and the link's ID) from from's bucket up to,
// not including, to's, oldest first. Buckets without reads have no row;
// without WithPerSubject for kind there are none.
func (p *Plugin) SubjectDays(ctx context.Context, s *records.Store, scope string, kind records.ServeKind, subject string, from, to time.Time) ([]SubjectBucket, error) {
	first, err := p.keyOf(ctx, scope, from)
	if err != nil {
		return nil, err
	}
	end, err := p.keyOf(ctx, scope, to)
	if err != nil {
		return nil, err
	}
	b := SubjectBucket{Scope: scope, Kind: kind, Subject: subject}
	var out []SubjectBucket
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(SubjectDay, &b.Day), sqlb.Bind(SubjectBytes, &b.Bytes), sqlb.Bind(SubjectRequests, &b.Requests)).
		From(SubjectTable).
		Where(sqlb.Eq(SubjectScope, scope), sqlb.Eq(SubjectKind, kind), sqlb.Eq(SubjectID, subject), sqlb.GreaterEq(SubjectDay, first), sqlb.Less(SubjectDay, end)).
		Order(sqlb.OrderBy(SubjectDay, sqlb.SortAsc))
	err = s.Each(ctx, q, func() error { out = append(out, b); return nil })
	return out, err
}

// pruneSubjects deletes the per-subject buckets before cutoff's bucket
// (each scope's own under WithBucket) and returns how many.
func (p *Plugin) pruneSubjects(ctx context.Context, s *records.Store, cutoff time.Time) (int64, error) {
	if p.bucket == nil {
		return s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(SubjectTable).Where(sqlb.Less(SubjectDay, UTCDay("", cutoff))))
	}
	var scope string
	var scopes []string
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(SubjectScope, &scope)).From(SubjectTable).GroupBy(SubjectScope)
	if err := s.Each(ctx, q, func() error { scopes = append(scopes, scope); return nil }); err != nil {
		return 0, err
	}
	var total int64
	for _, sc := range scopes {
		key, err := p.keyOf(ctx, sc, cutoff)
		if err != nil {
			return total, err
		}
		n, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(SubjectTable).Where(sqlb.Eq(SubjectScope, sc), sqlb.Less(SubjectDay, key)))
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
