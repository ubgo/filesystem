package audit

import (
	"context"
	"strconv"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// ChangeQuery selects the next changes of one scope's feed.
type ChangeQuery struct {
	Scope string
	// Cursor is ChangePage.NextCursor of the previous call; "" starts at
	// the oldest entry still kept.
	Cursor string
	// Limit caps the page; 0 means DefaultPageSize, at most MaxPageSize.
	Limit int
}

// ChangePage is the changes after a cursor, oldest first. NextCursor is
// where the next call continues: the last entry's place, or the query's
// own cursor when nothing new has committed yet (poll again with it).
type ChangePage struct {
	Entries    []Entry
	NextCursor string
}

// accessPattern matches the kinds of reads (records.ServeKind), which the
// change feed leaves out.
const accessPattern = "access.%"

// Changes is a change feed over the log: every change to scope's files,
// attachments and plugin rows (reads logged by WithAccessLog left out),
// in commit order, after a cursor. It needs WithHashChain
// (CodeUnsupported otherwise).
//
// The cursor is an entry's sequence number, stored with the entry, so it
// means the same after a restart, on another process, and after a
// deploy. It never skips a change: sequence numbers are taken under the
// scope's chain lock and that lock is held until the change commits, so
// a change numbered N commits before any change can be numbered N+1, and
// a reader that has seen N has seen everything before it. A change that
// rolls back gives its number back, leaving no gap. A cursor before the
// oldest entry Prune kept is refused with CodeNotFound: the changes after
// it are gone, and the consumer must resynchronise instead of silently
// missing them.
func (p *Plugin) Changes(ctx context.Context, s *records.Store, q ChangeQuery) (ChangePage, error) {
	if !p.chain {
		return ChangePage{}, filesystem.Errorf(filesystem.CodeUnsupported, "audit: Changes needs WithHashChain")
	}
	limit := q.Limit
	switch {
	case limit < 0 || limit > MaxPageSize:
		return ChangePage{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "audit: limit %d is not in [0, %d]", limit, MaxPageSize)
	case limit == 0:
		limit = DefaultPageSize
	}
	var after int64
	if q.Cursor != "" {
		n, err := strconv.ParseInt(q.Cursor, 10, 64)
		if err != nil || n < 0 {
			return ChangePage{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "audit: malformed change cursor %q", q.Cursor)
		}
		after = n
	}
	c, err := head(ctx, s, q.Scope)
	switch {
	case filesystem.CodeOf(err) == filesystem.CodeNotFound:
		return ChangePage{NextCursor: q.Cursor}, nil
	case err != nil:
		return ChangePage{}, err
	case q.Cursor != "" && after < c.anchorSeq:
		return ChangePage{}, filesystem.Errorf(filesystem.CodeNotFound, "audit: cursor %d of scope %q is before the oldest kept entry %d", after, q.Scope, c.anchorSeq+1)
	}
	var en Entry
	var kind, detail string
	sel := sqlb.NewSelect(s.Dialect()).Columns(entryBindings(&en, &kind, &detail)...).From(Table).
		Where(sqlb.Eq(Scope, q.Scope), sqlb.Greater(Seq, max(after, c.anchorSeq)), sqlb.NotLike(Kind, accessPattern)).
		Order(sqlb.OrderBy(Seq, sqlb.SortAsc)).Limit(limit)
	page := ChangePage{NextCursor: q.Cursor}
	err = s.Each(ctx, sel, func() error {
		page.Entries = append(page.Entries, entryOf(en, kind, detail))
		return nil
	})
	if err != nil {
		return ChangePage{}, err
	}
	if n := len(page.Entries); n > 0 {
		page.NextCursor = strconv.FormatInt(page.Entries[n-1].Seq, 10)
	}
	return page, nil
}
