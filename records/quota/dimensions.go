package quota

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// TableNameDimensionUsage is the table of dimension totals
// (WithDimensions): one row per dimension key, across every scope. It is
// created by migration 0004_dimension_usage whether or not the option is
// on, and filled only with it.
const TableNameDimensionUsage = "fs_quota_dimension_usage"

// ColDimension is fs_quota_dimension_usage's key column; its other columns
// are ColBytes, ColFiles and ColUpdatedAt.
const ColDimension = "dimension"

// Table and typed columns of fs_quota_dimension_usage.
var (
	DimensionTable     = sqlb.Table{Name: TableNameDimensionUsage, Alias: "qd"}
	DimensionKey       = sqlb.Col[string](DimensionTable, ColDimension)
	DimensionBytes     = sqlb.Col[int64](DimensionTable, ColBytes)
	DimensionFiles     = sqlb.Col[int64](DimensionTable, ColFiles)
	DimensionUpdatedAt = sqlb.Col[time.Time](DimensionTable, ColUpdatedAt)
)

// MaxDimensionBytes is the longest dimension key, the indexed width of
// its column (SQLTypes.Key).
const MaxDimensionBytes = 191

// Dimension is one more counter a file is charged to, beside its scope's
// (and its uploader's): an organisation's total across its workspaces
// ("org:acme"), an owner type's ("owner:invoice"), a content class's
// ("scope:t1/video"). Rows are shared by key across scopes, so one key
// returned for files of several scopes is one cap over all of them.
type Dimension struct {
	// Key names the counter: 1 to MaxDimensionBytes bytes.
	Key string
	// Limit is the counter's limit; a zero field is unlimited.
	Limit Limit
}

// DimensionFunc returns the dimensions file f is charged to, with their
// limits, usually from the scope's organisation and plan. It is asked on
// every upload, on both sides of every replacement (so a dimension keyed
// by content type follows the file when its type changes) and on every
// purge, so it must give the same answer for the same file contents: a
// key that moved meanwhile (a workspace moved to another organisation)
// leaves the old counter charged until RecountDimensions.
type DimensionFunc func(ctx context.Context, f records.File) ([]Dimension, error)

// WithDimensions charges every file to the dimensions fn returns as well
// as to its scope, in the same transaction and with the same conditional
// UPDATE, refusing it with CodeQuotaExceeded when any would pass its
// limit. It fails closed: an error from fn, a key that is empty or longer
// than MaxDimensionBytes, or a key returned twice, refuses the change.
// Held objects (WithHeldObjects) count toward the scope only. Off by
// default. Turning it on for a Store with files, or changing fn, needs
// RecountDimensions.
func WithDimensions(fn DimensionFunc) Option { return func(p *Plugin) { p.dimensions = fn } }

func dimensionCounter(key string) counter {
	return counter{
		table: DimensionTable, bytes: DimensionBytes, files: DimensionFiles, updated: DimensionUpdatedAt,
		key: []sqlb.Predicate{sqlb.Eq(DimensionKey, key)}, keySet: []sqlb.Assignment{sqlb.Set(DimensionKey, key)},
		name: fmt.Sprintf("dimension %q", key),
	}
}

// dimensionsOf asks WithDimensions' function for f's dimensions and checks
// them; none without the option.
func (p *Plugin) dimensionsOf(ctx context.Context, f records.File) ([]Dimension, error) {
	if p.dimensions == nil {
		return nil, nil
	}
	dims, err := p.dimensions(ctx, f)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(dims))
	for _, d := range dims {
		if d.Key == "" || len(d.Key) > MaxDimensionBytes || seen[d.Key] {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "quota: dimension key %q is empty, longer than %d bytes or repeated", d.Key, MaxDimensionBytes)
		}
		seen[d.Key] = true
	}
	return dims, nil
}

// chargeDimensions charges f (one file of f.Size bytes) to each of its
// dimensions.
func (p *Plugin) chargeDimensions(ctx context.Context, s *records.Store, dims []Dimension, size int64) error {
	for _, d := range dims {
		if err := p.charge(ctx, s, dimensionCounter(d.Key), size, d.Limit, 0); err != nil {
			return err
		}
	}
	return nil
}

// releaseDimensions releases one file of size bytes from each of dims.
func (p *Plugin) releaseDimensions(ctx context.Context, s *records.Store, dims []Dimension, size int64) error {
	for _, d := range dims {
		if err := p.shrink(ctx, s, dimensionCounter(d.Key), size, 1); err != nil {
			return err
		}
	}
	return nil
}

// replaceDimensions moves a replacement's sizes between the dimensions of
// the file before (cur) and after (next): a dimension in both is charged
// the difference, one only before is released, one only after is
// charged the whole new file.
func (p *Plugin) replaceDimensions(ctx context.Context, s *records.Store, cur records.File, next records.File) error {
	if p.dimensions == nil {
		return nil
	}
	before, err := p.dimensionsOf(ctx, cur)
	if err != nil {
		return err
	}
	after, err := p.dimensionsOf(ctx, next)
	if err != nil {
		return err
	}
	kept := make(map[string]bool, len(after))
	for _, d := range after {
		kept[d.Key] = true
	}
	var gone []Dimension
	was := make(map[string]bool, len(before))
	for _, d := range before {
		was[d.Key] = true
		if !kept[d.Key] {
			gone = append(gone, d)
		}
	}
	if err := p.releaseDimensions(ctx, s, gone, cur.Size); err != nil {
		return err
	}
	delta := next.Size - cur.Size
	for _, d := range after {
		c := dimensionCounter(d.Key)
		var err error
		switch {
		case !was[d.Key]:
			err = p.charge(ctx, s, c, next.Size, d.Limit, 0)
		case delta > 0:
			err = p.growCounter(ctx, s, c, delta, d.Limit)
		case delta < 0:
			err = p.shrink(ctx, s, c, -delta, 0)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// DimensionUsage returns what dimension key stores; a key never charged
// uses nothing.
func (p *Plugin) DimensionUsage(ctx context.Context, s *records.Store, key string) (Usage, error) {
	return p.usage(ctx, s, dimensionCounter(key))
}

// RecountDimensions rebuilds every dimension row from the files of every
// scope (trashed ones included), asking WithDimensions' function for each
// file, in one transaction. Run it after turning WithDimensions on or
// changing its function. It reads every file, so schedule it off-peak on a
// large Store, or repair a part with RecountDimensionsFor. Without
// WithDimensions it is CodeInvalidArgument.
func (p *Plugin) RecountDimensions(ctx context.Context, s *records.Store) error {
	return p.RecountDimensionsFor(ctx, s, RecountFilter{})
}

// RecountFilter narrows RecountDimensionsFor to part of a Store.
type RecountFilter struct {
	// Keys are the dimension rows rebuilt; each is deleted and rebuilt
	// from the files read, so a key no file maps to any more is removed.
	// Empty rebuilds every row, which needs Scopes empty too.
	Keys []string
	// Scopes are the scopes whose files are read; empty reads every
	// scope. Every file charged to one of Keys must be in one of Scopes
	// (an organisation's key with all of its workspaces), or the row is
	// rebuilt short.
	Scopes []string
}

// RecountDimensionsFor rebuilds the dimension rows f.Keys from the files of
// f.Scopes, in one transaction holding only those rows and reading only
// those scopes: the repair after one organisation changes plan or a
// workspace moves to another one (list the old key and the new), without
// a full-Store recount. Contributions of the files read to other keys are
// left alone. Scopes without Keys is CodeInvalidArgument, since rebuilding
// every key from some scopes would drop the others' charges; so is a call
// without WithDimensions. The zero filter is RecountDimensions.
func (p *Plugin) RecountDimensionsFor(ctx context.Context, s *records.Store, f RecountFilter) error {
	if p.dimensions == nil {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "quota: RecountDimensions needs WithDimensions")
	}
	if len(f.Keys) == 0 && len(f.Scopes) > 0 {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "quota: RecountDimensionsFor with Scopes needs Keys")
	}
	only := map[string]bool{}
	for _, k := range f.Keys {
		only[k] = true
	}
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		del := sqlb.NewDelete(s.Dialect()).From(DimensionTable).All()
		if len(f.Keys) > 0 {
			del = sqlb.NewDelete(s.Dialect()).From(DimensionTable).Where(sqlb.In(DimensionKey, f.Keys...))
		}
		if _, err := s.Exec(ctx, del); err != nil {
			return err
		}
		var row records.File
		var files []records.File
		q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(records.FileID, &row.ID), sqlb.Bind(records.FileScope, &row.Scope)).From(records.TableFiles)
		if len(f.Scopes) > 0 {
			q = q.Where(sqlb.In(records.FileScope, f.Scopes...))
		}
		// Collected first: a statement cannot run on the transaction while
		// the rows are still being read.
		if err := s.Each(ctx, q, func() error { files = append(files, row); return nil }); err != nil {
			return err
		}
		totals := map[string]*Usage{}
		var order []string
		for _, file := range files {
			full, err := s.File(ctx, file.Scope, file.ID)
			if err != nil {
				return err
			}
			dims, err := p.dimensionsOf(ctx, full)
			if err != nil {
				return err
			}
			for _, d := range dims {
				if len(only) > 0 && !only[d.Key] {
					continue
				}
				u, ok := totals[d.Key]
				if !ok {
					u = &Usage{}
					totals[d.Key] = u
					order = append(order, d.Key)
				}
				u.Bytes += full.Size
				u.Files++
			}
		}
		for _, key := range order {
			if err := p.insert(ctx, s, dimensionCounter(key), *totals[key]); err != nil {
				return err
			}
		}
		return nil
	})
}
