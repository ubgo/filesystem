package sqlblob

import (
	"context"
	"fmt"

	"github.com/ubgo/sqlb"
)

// builder is any sqlb statement not yet rendered: a select, insert,
// update, delete or engine upsert. Every statement the driver runs is
// handed to exec, one or each as a builder, never as a rendered Query, so
// rendering and its error live in build alone.
type builder interface {
	Build() (sqlb.Query, error)
}

// build renders st. sqlb refuses only a malformed builder (no columns, no
// WHERE, a row of the wrong width), and every statement here has a shape
// fixed in code, so the refusal is a bug in the driver; it is still
// returned, never ignored, so such a bug fails the operation instead of
// running half a statement. It is the one place a build error is handled.
func build(st builder) (sqlb.Query, error) {
	q, err := st.Build()
	if err != nil {
		return sqlb.Query{}, fmt.Errorf("sqlblob: build: %w", err)
	}
	return q, nil
}

// exec builds and runs a write through db (the pool, or a transaction) and
// returns how many rows it changed. Nothing reaches db when the build
// fails.
func exec(ctx context.Context, db sqlb.DBTX, st builder) (int64, error) {
	q, err := build(st)
	if err != nil {
		return 0, err
	}
	res, err := db.ExecContext(ctx, q.SQL, q.Args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// one builds and runs a read of exactly one row into st's bindings
// (sqlb.ErrNoRows when there is none). Nothing reaches db when the build
// fails.
func one(ctx context.Context, db sqlb.DBTX, st builder) error {
	q, err := build(st)
	if err != nil {
		return err
	}
	return sqlb.One(ctx, db, q)
}

// each builds and runs a read, calling fn once per row after scanning it
// into st's bindings. Nothing reaches db when the build fails.
func each(ctx context.Context, db sqlb.DBTX, st builder, fn func() error) error {
	q, err := build(st)
	if err != nil {
		return err
	}
	return sqlb.Each(ctx, db, q, fn)
}
