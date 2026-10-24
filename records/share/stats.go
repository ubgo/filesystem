package share

import (
	"context"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// TableNameEvents holds one row per use of a link (a download, or an
// upload through a request link), for Stats. Rows go with their link
// (Prune, the file's purge, PurgeScope); Revoke keeps them.
const TableNameEvents = "fs_share_events"

// Column names of fs_share_events.
const (
	ColEventID      = "id"
	ColEventScope   = "scope"
	ColEventShareID = "share_id"
	ColEventAt      = "at"
	ColEventViewer  = "viewer"
)

// Table and typed columns of fs_share_events.
var (
	EventsTable  = sqlb.Table{Name: TableNameEvents, Alias: "se"}
	EventID      = sqlb.Col[string](EventsTable, ColEventID)
	EventScope   = sqlb.Col[string](EventsTable, ColEventScope)
	EventShareID = sqlb.Col[string](EventsTable, ColEventShareID)
	EventAt      = sqlb.Col[time.Time](EventsTable, ColEventAt)
	// EventViewer is the actor of the request (records.WithActor), "" for
	// an anonymous visitor.
	EventViewer = sqlb.Col[string](EventsTable, ColEventViewer)
)

// eventIDPrefix starts every use record's ID.
const eventIDPrefix = "sev_"

// Use is one use of a link: when, and by whom when the visitor was known.
type Use struct {
	At     time.Time
	Viewer string
}

// Stats is what a link was used for.
type Stats struct {
	// Uses counts every use; Recent are the newest, up to the limit
	// asked, newest first.
	Uses   int64
	Recent []Use
}

// record writes one use of sh at, with the request's actor as viewer. An
// actor wider than its column fails the use rather than being cut short.
func (p *Plugin) record(ctx context.Context, s *records.Store, sh Share, at time.Time) error {
	viewer := s.Actor(ctx)
	if len(viewer) > records.MaxActorBytes {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "share: viewer is longer than %d bytes", records.MaxActorBytes)
	}
	ins := sqlb.NewInsert(s.Dialect()).Into(EventsTable).Row(
		sqlb.Set(EventID, s.NewID(eventIDPrefix)), sqlb.Set(EventScope, sh.Scope), sqlb.Set(EventShareID, sh.ID),
		sqlb.Set(EventAt, at), sqlb.Set(EventViewer, viewer),
	)
	_, err := s.Exec(ctx, ins)
	return err
}

// MaxRecentUses caps Stats' recent uses.
const MaxRecentUses = 1000

// Stats returns how often the link shareID of scope was used and its
// recent uses, up to recent (at most MaxRecentUses), newest first. An
// unknown link has none.
func (p *Plugin) Stats(ctx context.Context, s *records.Store, scope, shareID string, recent int) (Stats, error) {
	if recent < 0 || recent > MaxRecentUses {
		return Stats{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "share: recent %d is not in [0, %d]", recent, MaxRecentUses)
	}
	where := []sqlb.Predicate{sqlb.Eq(EventScope, scope), sqlb.Eq(EventShareID, shareID)}
	var st Stats
	if err := s.One(ctx, sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(sqlb.Count(), &st.Uses)).From(EventsTable).Where(where...)); err != nil {
		return Stats{}, err
	}
	if recent == 0 {
		return st, nil
	}
	var u Use
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(EventAt, &u.At), sqlb.Bind(EventViewer, &u.Viewer)).From(EventsTable).Where(where...).
		Order(sqlb.OrderBy(EventAt, sqlb.SortDesc), sqlb.OrderBy(EventID, sqlb.SortDesc)).Limit(recent)
	err := s.Each(ctx, q, func() error {
		u.At = u.At.UTC()
		st.Recent = append(st.Recent, u)
		return nil
	})
	return st, err
}
