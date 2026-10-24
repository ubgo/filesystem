package quota

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// TableNameReservations is the table of open reservations: bytes held for
// an upload in flight (a presigned PUT, a tus upload) until its file is
// recorded, released, or the reservation expires.
const TableNameReservations = "fs_quota_reservations"

// Column names of fs_quota_reservations.
const (
	ColReservationID        = "id"
	ColReservationScope     = "scope"
	ColReservationBytes     = "bytes"
	ColReservationExpiresAt = "expires_at"
	ColReservationCreatedAt = "created_at"
)

// Table and typed columns of fs_quota_reservations.
var (
	ReservationTable     = sqlb.Table{Name: TableNameReservations, Alias: "qr"}
	ReservationID        = sqlb.Col[string](ReservationTable, ColReservationID)
	ReservationScope     = sqlb.Col[string](ReservationTable, ColReservationScope)
	ReservationBytes     = sqlb.Col[int64](ReservationTable, ColReservationBytes)
	ReservationExpiresAt = sqlb.Col[time.Time](ReservationTable, ColReservationExpiresAt)
	ReservationCreatedAt = sqlb.Col[time.Time](ReservationTable, ColReservationCreatedAt)
)

// ReservationIDPrefix starts every reservation ID.
const ReservationIDPrefix = "qrs_"

// Reservation is bytes held against a scope's byte limit for one upload in
// flight.
type Reservation struct {
	ID        string
	Scope     string
	Bytes     int64
	ExpiresAt time.Time
}

// Reserve holds n bytes of scope's byte limit for an upload that has not
// arrived yet (a presigned PUT or a tus upload, which reach the backend
// before any file is recorded), so concurrent uploads cannot together
// promise more than the limit: the check and the hold are one conditional
// UPDATE, refused with CodeQuotaExceeded past the limit. Reserved bytes
// count against the limit for every other upload until the reservation
// ends, which happens one of three ways:
//
//   - the file is recorded with a context from WithReservation: its real
//     size is charged and the reservation is used up, in the file's
//     transaction (the commit);
//   - Release, when the upload is abandoned;
//   - Sweep, once ttl has passed, for uploads nobody released.
//
// Files are counted only when recorded; a reservation holds bytes alone.
// n and ttl must be positive (CodeInvalidArgument).
func (p *Plugin) Reserve(ctx context.Context, s *records.Store, scope string, n int64, ttl time.Duration) (Reservation, error) {
	if n <= 0 || ttl <= 0 {
		return Reservation{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "quota: reserve %d bytes for %s: both must be positive", n, ttl)
	}
	limit, err := p.limits(ctx, scope)
	if err != nil {
		return Reservation{}, err
	}
	// The Store's clock, which Sweep's caller (a job) shares.
	now := s.Now()
	r := Reservation{ID: s.NewID(ReservationIDPrefix), Scope: scope, Bytes: n, ExpiresAt: now.Add(ttl)}
	err = s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if err := p.hold(ctx, s, scopeCounter(scope), n, limit); err != nil {
			return err
		}
		_, err := s.Exec(ctx, sqlb.NewInsert(s.Dialect()).Into(ReservationTable).Row(
			sqlb.Set(ReservationID, r.ID), sqlb.Set(ReservationScope, scope), sqlb.Set(ReservationBytes, n),
			sqlb.Set(ReservationExpiresAt, r.ExpiresAt), sqlb.Set(ReservationCreatedAt, now),
		))
		return err
	})
	if err != nil {
		return Reservation{}, err
	}
	return r, nil
}

// hold adds n reserved bytes to c under limit, creating the row when the
// scope has none yet.
func (p *Plugin) hold(ctx context.Context, s *records.Store, c counter, n int64, limit Limit) error {
	if limit.Bytes > 0 && n > limit.Bytes {
		return exceeded(c.name, limit, Usage{}, n, 0)
	}
	where := append([]sqlb.Predicate{}, c.key...)
	if limit.Bytes > 0 {
		where = append(where, sqlb.LessEqExpr(sqlb.Add(c.held(), sqlb.Param(n)), sqlb.Param(limit.Bytes)))
	}
	changed, err := s.Exec(ctx, sqlb.NewUpdate(s.Dialect()).Table(c.table).Set(
		sqlb.SetExpr(c.reserved, sqlb.Add(c.reserved, sqlb.Param(n))), sqlb.Set(c.updated, p.stamp(s)),
	).Where(where...))
	if err != nil || changed == 1 {
		return err
	}
	if u, err := p.load(ctx, s, c); err == nil {
		u.Bytes += u.Reserved
		return exceeded(c.name, limit, u, n, 0)
	} else if filesystem.CodeOf(err) != filesystem.CodeNotFound {
		return err
	}
	if err := p.insert(ctx, s, c, Usage{Reserved: n}); err != nil {
		if records.IsUniqueViolation(err) {
			return filesystem.NewError(filesystem.CodeUnavailable, err)
		}
		return err
	}
	return nil
}

// reservationKey carries the reservation a file being recorded uses up.
type reservationKey struct{}

// WithReservation returns ctx carrying reservation id, so the file
// recorded with it (records.Store.CreateFile, Upload, an upload Confirm
// that records the file) uses the reservation up in its own transaction:
// the reserved bytes are let go and the file's real size charged, under
// the limit, in one UPDATE. A file larger than its reservation passes only
// if the scope has room for the difference; a smaller one frees the rest.
// A reservation already released or swept, or of another scope, is
// ignored and the file is charged as any other.
func WithReservation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, reservationKey{}, id)
}

// consume deletes the reservation ctx carries for scope, inside the
// file's transaction, and returns its bytes; 0 when there is none. Only
// the transaction that deletes the row gets its bytes, so a reservation
// is used up once even if two files name it.
func (p *Plugin) consume(ctx context.Context, s *records.Store, scope string) (int64, error) {
	id, _ := ctx.Value(reservationKey{}).(string)
	if id == "" {
		return 0, nil
	}
	return p.take(ctx, s, scope, id)
}

// take deletes reservation id of scope and returns its bytes, or 0 when
// no such reservation exists.
func (p *Plugin) take(ctx context.Context, s *records.Store, scope, id string) (int64, error) {
	var n int64
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(ReservationBytes, &n)).From(ReservationTable).
		Where(sqlb.Eq(ReservationID, id), sqlb.Eq(ReservationScope, scope))
	if err := s.One(ctx, q); err != nil {
		if errors.Is(err, sqlb.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	deleted, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(ReservationTable).Where(sqlb.Eq(ReservationID, id), sqlb.Eq(ReservationScope, scope)))
	if err != nil || deleted == 0 {
		return 0, err
	}
	return n, nil
}

// Release ends reservation id of scope without a file, giving its bytes
// back, and reports whether it was still open. Call it when an upload is
// abandoned or refused after Reserve; releasing twice is harmless.
func (p *Plugin) Release(ctx context.Context, s *records.Store, scope, id string) (bool, error) {
	var open bool
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		n, err := p.take(ctx, s, scope, id)
		if err != nil || n == 0 {
			return err
		}
		open = true
		c := scopeCounter(scope)
		_, err = s.Exec(ctx, sqlb.NewUpdate(s.Dialect()).Table(c.table).Set(
			sqlb.SetExpr(c.reserved, floorSub(c.reserved, n)), sqlb.Set(c.updated, p.stamp(s)),
		).Where(c.key...))
		return err
	})
	return open, err
}

// Sweep releases every reservation that expired at or before now and
// returns how many it released, so an upload that never finished and was
// never released does not hold its scope's space for ever. Each is
// released in its own transaction; one already used up or released
// meanwhile is skipped. Schedule it (jobs.QuotaSweepReservations).
func (p *Plugin) Sweep(ctx context.Context, s *records.Store, now time.Time) (int64, error) {
	type ref struct{ scope, id string }
	var r ref
	var due []ref
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(ReservationScope, &r.scope), sqlb.Bind(ReservationID, &r.id)).
		From(ReservationTable).Where(sqlb.LessEq(ReservationExpiresAt, now.UTC()))
	if err := s.Each(ctx, q, func() error { due = append(due, r); return nil }); err != nil {
		return 0, err
	}
	var n int64
	for _, d := range due {
		ok, err := p.Release(ctx, s, d.scope, d.id)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// reservedIn sums scope's open reservations, for Recount.
func (p *Plugin) reservedIn(ctx context.Context, s *records.Store, scope string) (int64, error) {
	var n int64
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(sqlb.Coalesce(sqlb.Sum(ReservationBytes), sqlb.Param[int64](0)), &n)).
		From(ReservationTable).Where(sqlb.Eq(ReservationScope, scope))
	return n, s.One(ctx, q)
}
