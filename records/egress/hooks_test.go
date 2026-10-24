package egress_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/egress"
)

type zoneKey struct{}

var errZoneLookup = errors.New("tenant zone lookup failed")

// zoneFromDB stands in for a tenant time zone read from the application's
// database per read: scope "jp" meters in Tokyo days, "down" fails the
// lookup, and the context must carry the request's marker.
func zoneFromDB(ctx context.Context, scope string, at time.Time) (string, error) {
	if ctx.Value(zoneKey{}) == nil {
		return "", errors.New("no request context")
	}
	switch scope {
	case "down":
		return "", errZoneLookup
	case "jp":
		return at.In(tokyo).Format(egress.DayLayout), nil
	}
	return egress.UTCDay(scope, at), nil
}

// TestBucketContext: WithBucketContext's function gets the read's context
// and its keys are the buckets; its error fails the read's metering and
// every query, so nothing lands in a guessed bucket.
func TestBucketContext(t *testing.T) {
	e := newBucketEnv(t, egress.WithBucketContext(zoneFromDB))
	ctx := context.WithValue(t.Context(), zoneKey{}, true)
	e.at(day1.Add(20 * time.Hour)) // 05:00 on day 2 in Tokyo
	if err := e.store.Served(ctx, records.Served{Kind: records.ServeLink, Scope: "jp", FileID: "fil_1", Bytes: 10}); err != nil {
		t.Fatal(err)
	}
	days, err := e.p.Days(ctx, e.store, "jp", day2, day3)
	if err != nil || fmt.Sprint(days) != "[{jp 2026-10-04 10 1}]" {
		t.Fatalf("days = %v, %v", days, err)
	}
	if err := e.store.Served(ctx, records.Served{Kind: records.ServeLink, Scope: "down", FileID: "fil_1", Bytes: 10}); !errors.Is(err, errZoneLookup) {
		t.Fatalf("Served with a failing lookup = %v", err)
	}
	if _, err := e.p.Days(ctx, e.store, "down", day1, day2); !errors.Is(err, errZoneLookup) {
		t.Fatalf("Days with a failing lookup = %v", err)
	}
	if _, err := e.p.Report(ctx, e.store, day3); err != nil {
		t.Fatal(err)
	}
	if len(e.sink.events) != 1 || e.sink.events[0].Detail[egress.DetailDay] != "2026-10-04" {
		t.Fatalf("reported %v", e.sink.events)
	}
	if n := e.count(t, "down"); n != 0 {
		t.Fatalf("a failed lookup still metered %d bucket(s)", n)
	}
}

// count is how many daily buckets scope has.
func (e *env) count(t *testing.T, scope string) int {
	t.Helper()
	days, err := e.p.Days(context.WithValue(t.Context(), zoneKey{}, true), e.store, scope, time.Time{}, day3.AddDate(1, 0, 0))
	if err != nil && !errors.Is(err, errZoneLookup) {
		t.Fatal(err)
	}
	return len(days)
}

type apiKeyKey struct{}

// byFileOrKey breaks plain links down by file, reads carrying an API key
// down by key, and fails on a marker file.
func byFileOrKey(ctx context.Context, sv records.Served) (string, error) {
	if sv.FileID == "fil_bad" {
		return "", errZoneLookup
	}
	if k, ok := ctx.Value(apiKeyKey{}).(string); ok {
		return k, nil
	}
	if sv.Kind == records.ServeLink {
		return sv.FileID, nil
	}
	return "", nil
}

// TestSubjectFunc: WithSubjectFunc's subject breaks down reads the default
// never does (plain links by file, any read by API key), it replaces
// Served.Subject, a "" subject meters the scope only, and its error fails
// the read's metering.
func TestSubjectFunc(t *testing.T) {
	e := newSubjectEnv(t, egress.WithPerSubject(records.ServeShare), egress.WithSubjectFunc(byFileOrKey))
	ctx := t.Context()
	serve := func(ctx context.Context, kind records.ServeKind, file, subject string, n int64) error {
		return e.store.Served(ctx, records.Served{Kind: kind, Scope: "a", FileID: file, Subject: subject, Bytes: n})
	}
	for _, r := range []struct {
		kind          records.ServeKind
		file, subject string
		n             int64
	}{{records.ServeLink, "fil_1", "", 10}, {records.ServeLink, "fil_1", "", 5}, {records.ServeLink, "fil_2", "", 3}, {records.ServeShare, "fil_1", "shr_1", 7}} {
		if err := serve(ctx, r.kind, r.file, r.subject, r.n); err != nil {
			t.Fatal(err)
		}
	}
	if err := serve(context.WithValue(ctx, apiKeyKey{}, "key_9"), records.ServeLink, "fil_1", "", 4); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		kind    records.ServeKind
		subject string
		want    string
	}{
		{records.ServeLink, "fil_1", "[{a access.link fil_1 2026-10-03 15 2}]"},
		{records.ServeLink, "fil_2", "[{a access.link fil_2 2026-10-03 3 1}]"},
		{records.ServeLink, "key_9", "[{a access.link key_9 2026-10-03 4 1}]"},
		{records.ServeShare, "shr_1", "[]"}, // the function replaced Served.Subject
	} {
		got, err := e.p.SubjectDays(ctx, e.store, "a", c.kind, c.subject, day1, day2)
		if err != nil || fmt.Sprint(got) != c.want && !(c.want == "[]" && got == nil) {
			t.Fatalf("SubjectDays(%s, %s) = %v, %v; want %s", c.kind, c.subject, got, err, c.want)
		}
	}
	if err := serve(ctx, records.ServeLink, "fil_bad", "", 1); !errors.Is(err, errZoneLookup) {
		t.Fatalf("Served with a failing subject func = %v", err)
	}
	total, err := e.p.Total(ctx, e.store, "a", day1, day2)
	if err != nil || total.Bytes != 30 || total.Requests != 6 {
		t.Fatalf("scope total = %+v, %v (the failed breakdown still charges the scope)", total, err)
	}
}

// TestSubjectDefaultUnchanged: without WithSubjectFunc plain links are
// never broken down, and listed kinds use Served.Subject.
func TestSubjectDefaultUnchanged(t *testing.T) {
	e := newSubjectEnv(t, egress.WithPerSubject(records.ServeShare))
	e.serveAs(t, records.ServeLink, "", 10)
	e.serveAs(t, records.ServeShare, "shr_1", 7)
	got, err := e.p.SubjectDays(t.Context(), e.store, "a", records.ServeLink, "fil_1", day1, day2)
	if err != nil || len(got) != 0 {
		t.Fatalf("a plain link was broken down: %v, %v", got, err)
	}
	got, err = e.p.SubjectDays(t.Context(), e.store, "a", records.ServeShare, "shr_1", day1, day2)
	if err != nil || fmt.Sprint(got) != "[{a access.share shr_1 2026-10-03 7 1}]" {
		t.Fatalf("share breakdown = %v, %v", got, err)
	}
}
