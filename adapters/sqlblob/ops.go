package sqlblob

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/mysql"
	"github.com/ubgo/sqlb/pg"
	"github.com/ubgo/sqlb/sqlite"

	"github.com/ubgo/filesystem"
)

// deleteKeysBatch caps the keys of one DELETE ... IN, well under every
// engine's bound-parameter limit (SQLite's is the lowest, 32766).
const deleteKeysBatch = 500

// incoming names, per Go type, the value an upsert tried to insert into a
// column: EXCLUDED.col on Postgres and SQLite, new.col on MySQL.
type incoming struct {
	str   func(sqlb.Expr[string]) sqlb.Expr[string]
	num   func(sqlb.Expr[int64]) sqlb.Expr[int64]
	bytes func(sqlb.Expr[[]byte]) sqlb.Expr[[]byte]
}

// replaceAll sets every column but the key to the incoming row's value.
func (d *Driver) replaceAll(in incoming) []sqlb.Assignment {
	c := d.cols
	return []sqlb.Assignment{
		sqlb.SetExpr(c.data, in.bytes(c.data)), sqlb.SetExpr(c.size, in.num(c.size)), sqlb.SetExpr(c.sha256, in.str(c.sha256)),
		sqlb.SetExpr(c.contentType, in.str(c.contentType)), sqlb.SetExpr(c.visibility, in.str(c.visibility)),
		sqlb.SetExpr(c.metadata, in.str(c.metadata)), sqlb.SetExpr(c.cacheControl, in.str(c.cacheControl)),
		sqlb.SetExpr(c.contentDisposition, in.str(c.contentDisposition)),
		sqlb.SetExpr(c.contentEncoding, in.str(c.contentEncoding)), sqlb.SetExpr(c.modified, in.num(c.modified)),
	}
}

// upsertStatement is the one-statement insert-or-replace of r for the
// driver's engine.
func (d *Driver) upsertStatement(r row) builder {
	as := d.assignments(r)
	switch d.engine {
	case sqlb.DialectPostgres:
		set := d.replaceAll(incoming{str: pg.Excluded[string], num: pg.Excluded[int64], bytes: pg.Excluded[[]byte]})
		return pg.Insert(d.cols.table).Row(as...).Clause(pg.OnConflict(d.cols.key).DoUpdate(set...).Clause())
	case sqlb.DialectMySQL:
		set := d.replaceAll(incoming{str: mysql.New[string], num: mysql.New[int64], bytes: mysql.New[[]byte]})
		return mysql.Insert(d.cols.table).Row(as...).Clause(mysql.OnDuplicateKeyUpdate(set[0], set[1:]...))
	}
	set := d.replaceAll(incoming{str: sqlite.Excluded[string], num: sqlite.Excluded[int64], bytes: sqlite.Excluded[[]byte]})
	return sqlite.Insert(d.cols.table).Row(as...).Clause(sqlite.OnConflict(d.cols.key).DoUpdate(set...).Clause())
}

// Delete implements filesystem.Driver.
func (d *Driver) Delete(ctx context.Context, key string) error {
	_, err := exec(ctx, d.db, sqlb.NewDelete(d.dialect).From(d.cols.table).Where(sqlb.Eq(d.cols.key, key)))
	return err
}

// DeleteKeys implements filesystem.KeysDeleter, deleteKeysBatch keys per
// statement.
func (d *Driver) DeleteKeys(ctx context.Context, keys []string) error {
	for len(keys) > 0 {
		n := min(len(keys), deleteKeysBatch)
		if _, err := exec(ctx, d.db, sqlb.NewDelete(d.dialect).From(d.cols.table).Where(sqlb.In(d.cols.key, keys[:n]...))); err != nil {
			return err
		}
		keys = keys[n:]
	}
	return nil
}

// inDir is the predicate for keys inside dir: a range on the byte-ordered
// key, [dir+"/", dir+"0"), since '0' is the byte after '/'. It uses the
// primary key's index and needs no LIKE escaping. The root has no bound.
func (d *Driver) inDir(dir string) []sqlb.Predicate {
	if dir == "" {
		return nil
	}
	return []sqlb.Predicate{sqlb.GreaterEq(d.cols.key, dir+"/"), sqlb.Less(d.cols.key, dir+"0")}
}

// List implements filesystem.Driver. A recursive listing is paged by the
// database (key > cursor, ORDER BY key, LIMIT), so each page costs one
// indexed query whatever the table's size. A non-recursive listing reads
// the attributes (never the bytes) of every key under dir and folds
// sub-directories with filesystem.PageOf, O(n) in the keys under dir like
// the local driver.
func (d *Driver) List(ctx context.Context, dir string, opts filesystem.ListOptions) (filesystem.Page, error) {
	where := d.inDir(dir)
	limit := 0
	if opts.Recursive {
		if opts.Cursor != "" {
			where = append(where, sqlb.Greater(d.cols.key, opts.Cursor))
		}
		limit = opts.Limit
		if limit <= 0 {
			limit = filesystem.DefaultPageSize
		}
	}
	var o object
	sel := sqlb.NewSelect(d.dialect).Columns(d.bindings(&o)...).From(d.cols.table).Order(sqlb.OrderBy(d.cols.key, sqlb.SortAsc))
	if len(where) > 0 {
		sel = sel.Where(where...)
	}
	if limit > 0 {
		sel = sel.Limit(limit + 1) // one more than the page tells whether another follows
	}
	var all []filesystem.Entry
	err := each(ctx, d.db, sel, func() error {
		e, err := o.entry()
		if err != nil {
			return err
		}
		all = append(all, e)
		return nil
	})
	if err != nil {
		return filesystem.Page{}, err
	}
	if !opts.Recursive {
		return filesystem.PageOf(all, dir, opts), nil
	}
	page := filesystem.Page{Entries: all}
	if len(all) > limit {
		page.Entries = all[:limit]
		page.NextCursor = all[limit-1].Key
	}
	return page, nil
}

// inTx runs fn in a transaction, committing when it succeeds.
func (d *Driver) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// load reads key's full row, bytes included.
func (d *Driver) load(ctx context.Context, db sqlb.DBTX, key string) (row, error) {
	var r row
	bs := append(d.bindings(&r.object), sqlb.Bind(d.cols.data, &r.data))
	if err := one(ctx, db, sqlb.NewSelect(d.dialect).Columns(bs...).From(d.cols.table).Where(sqlb.Eq(d.cols.key, key))); err != nil {
		if errors.Is(err, sqlb.ErrNoRows) {
			return row{}, notFound(key)
		}
		return row{}, err
	}
	if r.data == nil {
		r.data = []byte{}
	}
	return r, nil
}

// Copy implements filesystem.Copier inside the database: the source row is
// read and written under dst in one transaction, with a new modification
// time. The bytes pass through the application's process (no engine copies
// a row under a new key portably), but never through the caller.
func (d *Driver) Copy(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	var out filesystem.Entry
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		r, err := d.load(ctx, tx, src)
		if err != nil {
			return err
		}
		r.key, r.modified = dst, d.nowNanos()
		if err := d.upsert(ctx, tx, r); err != nil {
			return err
		}
		out, err = r.entry()
		return err
	})
	return out, err
}

// Move implements filesystem.Mover atomically: dst is removed and src's row
// renamed to dst in one transaction, so no reader ever sees both or
// neither.
func (d *Driver) Move(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	var out filesystem.Entry
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		if out, err = d.stat(ctx, tx, src); err != nil || src == dst {
			return err
		}
		del := sqlb.NewDelete(d.dialect).From(d.cols.table).Where(sqlb.Eq(d.cols.key, dst))
		if _, err := exec(ctx, tx, del); err != nil {
			return err
		}
		ren := sqlb.NewUpdate(d.dialect).Table(d.cols.table).Set(sqlb.Set(d.cols.key, dst)).Where(sqlb.Eq(d.cols.key, src))
		if _, err := exec(ctx, tx, ren); err != nil {
			return err
		}
		out.Key = dst
		return nil
	})
	return out, err
}

// SetVisibility implements filesystem.VisibilitySetter.
func (d *Driver) SetVisibility(ctx context.Context, key string, v filesystem.Visibility) error {
	q := sqlb.NewUpdate(d.dialect).Table(d.cols.table).Set(sqlb.Set(d.cols.visibility, string(v))).Where(sqlb.Eq(d.cols.key, key))
	n, err := exec(ctx, d.db, q)
	if err != nil || n > 0 {
		return err
	}
	// No row changed: the key is missing, or (on MySQL, which does not
	// count an unchanged row) it already had v.
	_, err = d.stat(ctx, d.db, key)
	return err
}

// DeletePrefix implements filesystem.PrefixDeleter in one statement.
func (d *Driver) DeletePrefix(ctx context.Context, dir string) error {
	del := sqlb.NewDelete(d.dialect).From(d.cols.table)
	var st builder
	if where := d.inDir(dir); len(where) > 0 {
		st = del.Where(where...)
	} else {
		st = del.All()
	}
	_, err := exec(ctx, d.db, st)
	return err
}
