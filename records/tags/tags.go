// Package tags is a records plugin for tagging files: free-form labels per
// scope, files listed by tag, and a tag cloud with counts. The labels of
// media libraries, document managers and asset managers.
//
// Tags are normalized before they are stored, so "Invoice" and "invoice "
// are one tag. The default normalization trims spaces and lowercases; an
// app that wants case-sensitive tags, or slugs, passes WithNormalize.
package tags

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// PluginName is the plugin's name in the migration ledger.
const PluginName = "tags"

// TableNameTags is the table the plugin owns: one row per tag of a file.
const TableNameTags = "fs_file_tags"

// Column names of fs_file_tags.
const (
	ColScope  = "scope"
	ColFileID = "file_id"
	ColTag    = "tag"
)

// Table and typed columns.
var (
	Table  = sqlb.Table{Name: TableNameTags, Alias: "ft"}
	Scope  = sqlb.Col[string](Table, ColScope)
	FileID = sqlb.Col[string](Table, ColFileID)
	Tag    = sqlb.Col[string](Table, ColTag)
)

// Limits.
const (
	// MaxTagChars is the longest tag after normalization, the width of its
	// column.
	MaxTagChars = 64
	// MaxPerFile caps the tags of one file, so one file cannot carry an
	// unbounded number of rows.
	MaxPerFile = 64
)

// Event kinds the plugin emits through records.Store.Emit, inside the
// transaction of each change, so an audit log or a webhook sees tag
// changes the way it sees the Store's. Tags are reported as stored
// (normalized). A call that changes nothing emits nothing.
const (
	// EventAdded is one tag added to a file: FileID; Detail has DetailTag.
	// Adding several tags emits one event per tag.
	EventAdded records.EventKind = PluginName + ".added"
	// EventRemoved is one tag removed from a file: FileID; Detail has
	// DetailTag.
	EventRemoved records.EventKind = PluginName + ".removed"
	// EventRenamed is a tag renamed across a scope (Rename): Detail has
	// DetailTag (new) and DetailFrom (old).
	EventRenamed records.EventKind = PluginName + ".renamed"
	// EventDeleted is a tag removed from every file of a scope (Delete):
	// Detail has DetailTag.
	EventDeleted records.EventKind = PluginName + ".deleted"
)

// EventKindValues lists every event kind the plugin emits.
var EventKindValues = []records.EventKind{EventAdded, EventRemoved, EventRenamed, EventDeleted}

// Detail keys of the plugin's events.
const (
	DetailTag  = "tag"
	DetailFrom = "from"
)

// Normalize maps a tag as typed to the tag as stored, or refuses it.
type Normalize func(string) (string, error)

// DefaultNormalize trims spaces and lowercases, then refuses a tag that is
// empty, longer than MaxTagChars, not valid UTF-8, or holds control
// characters.
func DefaultNormalize(tag string) (string, error) {
	if !utf8.ValidString(tag) {
		// Checked first: ToLower would replace invalid bytes with U+FFFD
		// and so make them valid.
		return Check(tag)
	}
	return Check(strings.ToLower(strings.TrimSpace(tag)))
}

// Check refuses a tag that cannot be stored: empty, longer than
// MaxTagChars, invalid UTF-8, or with control characters. A custom
// Normalize should end with it.
func Check(tag string) (string, error) {
	if tag == "" || !utf8.ValidString(tag) || utf8.RuneCountInString(tag) > MaxTagChars || strings.IndexFunc(tag, unicode.IsControl) >= 0 {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "tags: a tag must be 1 to %d characters of valid UTF-8 without control characters", MaxTagChars)
	}
	return tag, nil
}

// Plugin manages tags. Construct it with New and install it with
// records.WithPlugins.
type Plugin struct {
	normalize Normalize
}

// Option configures a Plugin.
type Option func(*Plugin)

// WithNormalize replaces DefaultNormalize.
func WithNormalize(n Normalize) Option { return func(p *Plugin) { p.normalize = n } }

// New returns a Plugin.
func New(opts ...Option) *Plugin {
	p := &Plugin{normalize: DefaultNormalize}
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
	st, err := records.NewDDL(engine).CreateTable(records.Table{
		Table:      Table,
		Columns:    []ddl.ColumnSpec{records.Column(Scope, t.Key), records.Column(FileID, t.Key), records.Column(Tag, t.Short)},
		PrimaryKey: []ddl.ColumnRef{FileID, Tag},
		Indexes:    []records.Index{{Name: "fs_file_tags_tag", Columns: []records.IndexColumn{records.Key(Scope), records.Key(Tag), records.Key(FileID)}}},
	}).Statements()
	return []records.Migration{{Owner: PluginName, Name: "0001_file_tags", Statements: st}}, err
}

// BeforeCreate implements records.FileHooks.
func (p *Plugin) BeforeCreate(context.Context, *records.Store, *records.File) error { return nil }

// AfterCreate implements records.FileHooks.
func (p *Plugin) AfterCreate(context.Context, *records.Store, records.File) error { return nil }

// BeforePurge implements records.FileHooks: a purged file's tags go too.
func (p *Plugin) BeforePurge(ctx context.Context, s *records.Store, f records.File) error {
	_, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Scope, f.Scope), sqlb.Eq(FileID, f.ID)))
	return err
}

// PurgeScope implements records.ScopePurger: once Store.PurgeScope has
// purged every file of scope, any tag row of the scope left behind goes
// too (BeforePurge already removed each file's tags).
func (p *Plugin) PurgeScope(ctx context.Context, s *records.Store, scope string) error {
	_, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Scope, scope)))
	return err
}

// normalizeAll normalizes tags, dropping duplicates and keeping order.
func (p *Plugin) normalizeAll(tags []string) ([]string, error) {
	out := make([]string, 0, len(tags))
	for _, raw := range tags {
		tag, err := p.normalize(raw)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, tag) {
			out = append(out, tag)
		}
	}
	return out, nil
}

// Tags returns the tags of a file of scope, sorted.
func (p *Plugin) Tags(ctx context.Context, s *records.Store, scope, fileID string) ([]string, error) {
	if _, err := s.File(ctx, scope, fileID); err != nil {
		return nil, err
	}
	return p.tagsOf(ctx, s, scope, fileID)
}

func (p *Plugin) tagsOf(ctx context.Context, s *records.Store, scope, fileID string) ([]string, error) {
	var tag string
	var out []string
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(Tag, &tag)).From(Table).
		Where(sqlb.Eq(Scope, scope), sqlb.Eq(FileID, fileID)).Order(sqlb.OrderBy(Tag, sqlb.SortAsc))
	if err := s.Each(ctx, q, func() error { out = append(out, tag); return nil }); err != nil {
		return nil, err
	}
	return out, nil
}

// Set replaces the tags of a file of scope with tags (none clears them)
// and returns them as stored, sorted. More than MaxPerFile tags is
// CodeInvalidArgument.
func (p *Plugin) Set(ctx context.Context, s *records.Store, scope, fileID string, tags ...string) ([]string, error) {
	return p.change(ctx, s, scope, fileID, func([]string) ([]string, error) { return p.normalizeAll(tags) })
}

// Add adds tags to a file of scope; tags it already has are kept once.
func (p *Plugin) Add(ctx context.Context, s *records.Store, scope, fileID string, tags ...string) ([]string, error) {
	return p.change(ctx, s, scope, fileID, func(current []string) ([]string, error) {
		add, err := p.normalizeAll(tags)
		if err != nil {
			return nil, err
		}
		for _, tag := range add {
			if !slices.Contains(current, tag) {
				current = append(current, tag)
			}
		}
		return current, nil
	})
}

// Remove removes tags from a file of scope; tags it does not have are
// ignored.
func (p *Plugin) Remove(ctx context.Context, s *records.Store, scope, fileID string, tags ...string) ([]string, error) {
	return p.change(ctx, s, scope, fileID, func(current []string) ([]string, error) {
		drop, err := p.normalizeAll(tags)
		if err != nil {
			return nil, err
		}
		return slices.DeleteFunc(current, func(tag string) bool { return slices.Contains(drop, tag) }), nil
	})
}

// change computes a file's new tags from its current ones and writes only
// the difference, in one transaction.
func (p *Plugin) change(ctx context.Context, s *records.Store, scope, fileID string, next func(current []string) ([]string, error)) ([]string, error) {
	var out []string
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := s.File(ctx, scope, fileID); err != nil {
			return err
		}
		current, err := p.tagsOf(ctx, s, scope, fileID)
		if err != nil {
			return err
		}
		want, err := next(slices.Clone(current))
		if err != nil {
			return err
		}
		if len(want) > MaxPerFile {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "tags: a file has at most %d tags", MaxPerFile)
		}
		var gone []string
		for _, tag := range current {
			if !slices.Contains(want, tag) {
				gone = append(gone, tag)
			}
		}
		if len(gone) > 0 {
			del := sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Scope, scope), sqlb.Eq(FileID, fileID), sqlb.In(Tag, gone...))
			if _, err := s.Exec(ctx, del); err != nil {
				return err
			}
		}
		var added []string
		for _, tag := range want {
			if !slices.Contains(current, tag) {
				added = append(added, tag)
			}
		}
		if len(added) > 0 {
			if _, err := s.Exec(ctx, insertRows(s, scope, fileID, added)); err != nil {
				return err
			}
		}
		for _, c := range []struct {
			kind records.EventKind
			tags []string
		}{{EventRemoved, gone}, {EventAdded, added}} {
			for _, tag := range c.tags {
				if err := s.Emit(ctx, p, records.Event{Kind: c.kind, Scope: scope, FileID: fileID, Detail: map[string]string{DetailTag: tag}}); err != nil {
					return err
				}
			}
		}
		slices.Sort(want)
		out = want
		return nil
	})
	return out, err
}

// insertRows is one multi-row INSERT of tags (at least one) for a file.
func insertRows(s *records.Store, scope, fileID string, tags []string) records.Statement {
	row := func(tag string) []sqlb.Assignment {
		return []sqlb.Assignment{sqlb.Set(Scope, scope), sqlb.Set(FileID, fileID), sqlb.Set(Tag, tag)}
	}
	ins := sqlb.NewInsert(s.Dialect()).Into(Table).Row(row(tags[0])...)
	for _, tag := range tags[1:] {
		ins = ins.Row(row(tag)...)
	}
	return ins
}

// Filter is the condition on records.TableFiles that keeps the files of
// scope carrying every one of tags (normalized), for records.FileQuery
// Filters or a query of the app's own.
func (p *Plugin) Filter(s *records.Store, scope string, tags ...string) (sqlb.Predicate, error) {
	want, err := p.normalizeAll(tags)
	if err != nil {
		return nil, err
	}
	if len(want) == 0 {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "tags: no tag to filter by")
	}
	preds := make([]sqlb.Predicate, 0, len(want))
	for _, tag := range want {
		tagged := sqlb.NewSelect(s.Dialect()).Exprs(FileID).From(Table).Where(sqlb.Eq(Scope, scope), sqlb.Eq(Tag, tag))
		preds = append(preds, sqlb.InSubquery(records.FileID, tagged))
	}
	return sqlb.And(preds...), nil
}

// Files lists the files of fq.Scope that carry every one of tags, with
// the order, pagination and trash filter of records.Store.Files.
func (p *Plugin) Files(ctx context.Context, s *records.Store, fq records.FileQuery, tags ...string) (records.FilePage, error) {
	f, err := p.Filter(s, fq.Scope, tags...)
	if err != nil {
		return records.FilePage{}, err
	}
	fq.Filters = append(fq.Filters[:len(fq.Filters):len(fq.Filters)], f)
	return s.Files(ctx, fq)
}

// Count is a tag and the number of live files carrying it.
type Count struct {
	Tag   string
	Files int64
}

// Counts returns every tag of scope with its number of live files (files
// in the trash do not count), most used first, then by tag.
func (p *Plugin) Counts(ctx context.Context, s *records.Store, scope string) ([]Count, error) {
	var c Count
	var out []Count
	n := sqlb.Count()
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(Tag, &c.Tag), sqlb.Bind(n, &c.Files)).From(Table).
		Join(sqlb.Join{Type: sqlb.JoinInner, Table: records.TableFiles, On: []sqlb.Predicate{sqlb.EqExpr(records.FileID, FileID), sqlb.EqExpr(records.FileScope, Scope)}}).
		Where(sqlb.Eq(Scope, scope), sqlb.IsNull(records.FileDeletedAt)).
		GroupBy(Tag).Order(sqlb.OrderBy(n, sqlb.SortDesc), sqlb.OrderBy(Tag, sqlb.SortAsc))
	if err := s.Each(ctx, q, func() error { out = append(out, c); return nil }); err != nil {
		return nil, err
	}
	return out, nil
}

// Rename renames a tag across every file of scope. A file that already
// carries the new tag keeps it once.
func (p *Plugin) Rename(ctx context.Context, s *records.Store, scope, from, to string) error {
	old, err := p.normalize(from)
	if err != nil {
		return err
	}
	tag, err := p.normalize(to)
	if err != nil || tag == old {
		return err
	}
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var id string
		var both []string
		q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(FileID, &id)).From(Table).Where(sqlb.Eq(Scope, scope), sqlb.Eq(Tag, tag))
		if err := s.Each(ctx, q, func() error { both = append(both, id); return nil }); err != nil {
			return err
		}
		var changed int64
		if len(both) > 0 {
			del := sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Scope, scope), sqlb.Eq(Tag, old), sqlb.In(FileID, both...))
			n, err := s.Exec(ctx, del)
			if err != nil {
				return err
			}
			changed += n
		}
		upd := sqlb.NewUpdate(s.Dialect()).Table(Table).Set(sqlb.Set(Tag, tag)).Where(sqlb.Eq(Scope, scope), sqlb.Eq(Tag, old))
		n, err := s.Exec(ctx, upd)
		if err != nil || changed+n == 0 {
			return err
		}
		return s.Emit(ctx, p, records.Event{Kind: EventRenamed, Scope: scope, Detail: map[string]string{DetailTag: tag, DetailFrom: old}})
	})
}

// Delete removes a tag from every file of scope. Deleting a tag no file
// carries does nothing.
func (p *Plugin) Delete(ctx context.Context, s *records.Store, scope, tag string) error {
	t, err := p.normalize(tag)
	if err != nil {
		return err
	}
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		n, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Scope, scope), sqlb.Eq(Tag, t)))
		if err != nil || n == 0 {
			return err
		}
		return s.Emit(ctx, p, records.Event{Kind: EventDeleted, Scope: scope, Detail: map[string]string{DetailTag: t}})
	})
}
