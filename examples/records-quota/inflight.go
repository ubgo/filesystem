package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/sqlb"
)

// ticketTTL is how long a reservation holds its bytes for an upload that
// never confirms.
const ticketTTL = 15 * time.Minute

// usageLog collects quota.usage_changed events, as a billing consumer
// behind webhooks would.
type usageLog struct{ lines []string }

func (u *usageLog) Name() string { return "usage_log" }

func (u *usageLog) OnEvent(_ context.Context, _ *records.Store, e records.Event) error {
	if e.Kind == quota.EventUsageChanged {
		u.lines = append(u.lines, fmt.Sprintf("%s bytes (%s)", e.Detail[quota.DetailBytes], e.Detail[quota.DetailDeltaBytes]))
	}
	return nil
}

// newBillingStore is newStore with WithUsageEvents: every change to a
// scope's usage emits quota.usage_changed inside the change's
// transaction, so a rolled-back upload never bills.
// ds:def id=newbillingstore-c8n4w2kq
func newBillingStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager, consumer records.Plugin) (*records.Store, *quota.Plugin, error) {
	q, err := quota.New(limits, quota.WithUsageEvents())
	if err != nil {
		return nil, nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(q, consumer))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, q, nil
}

// issueTicket reserves room for an upload that reaches the backend before
// any file is recorded (a presigned PUT, a tus upload): the check and the
// hold are one conditional UPDATE, so concurrent tickets never promise
// more than the limit.
// ds:def id=issueticket-w5t9m3hr
func issueTicket(ctx context.Context, s *records.Store, q *quota.Plugin, scope string, size int64) (quota.Reservation, error) {
	return q.Reserve(ctx, s, scope, size, ticketTTL)
}

// confirm records the uploaded object under its reservation: the real
// size is charged and the reservation used up in one transaction.
// ds:def id=confirm-h2r7c9vk
func confirm(ctx context.Context, s *records.Store, r quota.Reservation, key string) (records.File, error) {
	return s.CreateFile(quota.WithReservation(ctx, r.ID), records.NewFile{Scope: r.Scope, Disk: diskName, Key: key})
}

// inflight runs reservations and usage events on a free-plan scope of its
// own, with a second Store on the same database.
func inflight(ctx context.Context, w io.Writer, db *sql.DB, disks *filesystem.Manager) error {
	log := &usageLog{}
	s, q, err := newBillingStore(ctx, db, disks, log)
	if err != nil {
		return err
	}
	scope := inflightTenant
	a, err := issueTicket(ctx, s, q, scope, freePlan.Bytes-10)
	if err != nil {
		return err
	}
	_, err = issueTicket(ctx, s, q, scope, 20)
	fmt.Fprintf(w, "second ticket while the first holds the room: %s\n", filesystem.CodeOf(err))
	u, err := q.Usage(ctx, s, scope)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "reserved %d of %d bytes\n", u.Reserved, freePlan.Bytes)
	disk, err := s.Disks().Disk(diskName)
	if err != nil {
		return err
	}
	if _, err := disk.Write(ctx, "direct/a.txt", strings.NewReader("small"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	if _, err := confirm(ctx, s, a, "direct/a.txt"); err != nil {
		return err
	}
	u, err = q.Usage(ctx, s, scope)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "confirmed: %d bytes used, %d reserved\n", u.Bytes, u.Reserved)
	b, err := issueTicket(ctx, s, q, scope, 20)
	if err != nil {
		return err
	}
	released, err := q.Release(ctx, s, scope, b.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "abandoned ticket released: %t\n", released)
	fmt.Fprintf(w, "usage events: %s\n", strings.Join(log.lines, ", "))
	return nil
}
