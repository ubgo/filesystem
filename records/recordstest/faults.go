package recordstest

import (
	"context"
	"database/sql/driver"
	"errors"
	"sync"
)

// ErrInjected is the error of a step Faults made fail.
var ErrInjected = errors.New("recordstest: injected fault")

// Faults counts the database steps of a connection pool (begin, exec,
// query, commit) and can make one of them fail with ErrInjected. A test
// measures an operation with Arm(0) and Steps, then reruns it with Arm(n)
// for every n up to that count: each run must fail with ErrInjected and
// leave nothing half done.
type Faults struct {
	mu     sync.Mutex
	steps  int
	failAt int
}

// Arm resets the count and makes step n (counting from 1) fail; 0 only
// counts.
func (f *Faults) Arm(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps, f.failAt = 0, n
}

// Steps returns the steps counted since Arm.
func (f *Faults) Steps() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.steps
}

func (f *Faults) step() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps++
	if f.steps == f.failAt {
		return ErrInjected
	}
	return nil
}

// connector opens connections of the inner driver wrapped in Faults.
type connector struct {
	dsn    string
	inner  driver.Driver
	faults *Faults
}

func (c connector) Connect(context.Context) (driver.Conn, error) {
	inner, err := c.inner.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &faultyConn{Conn: inner, faults: c.faults}, nil
}

func (c connector) Driver() driver.Driver { return c.inner }

// faultyConn counts and fails steps, forwarding everything to the inner
// connection. Every driver the tests use implements the context
// interfaces; one that does not falls back to database/sql's defaults
// through driver.ErrSkip.
type faultyConn struct {
	driver.Conn
	faults *Faults
}

func (c *faultyConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := c.faults.step(); err != nil {
		return nil, err
	}
	b, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, errors.New("recordstest: driver without BeginTx")
	}
	tx, err := b.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return faultyTx{Tx: tx, faults: c.faults}, nil
}

func (c *faultyConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	if err := c.faults.step(); err != nil {
		return nil, err
	}
	return e.ExecContext(ctx, query, args)
}

func (c *faultyConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	if err := c.faults.step(); err != nil {
		return nil, err
	}
	return q.QueryContext(ctx, query, args)
}

func (c *faultyConn) CheckNamedValue(v *driver.NamedValue) error {
	if n, ok := c.Conn.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(v)
	}
	return driver.ErrSkip
}

func (c *faultyConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *faultyConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

// faultyTx counts and fails the commit.
type faultyTx struct {
	driver.Tx
	faults *Faults
}

func (t faultyTx) Commit() error {
	if err := t.faults.step(); err != nil {
		_ = t.Tx.Rollback()
		return err
	}
	return t.Tx.Commit()
}
