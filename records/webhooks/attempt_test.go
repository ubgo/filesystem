package webhooks_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestSlowBatchSignsAndLeasesPerAttempt: the first delivery of a batch
// takes six minutes. The second must still be signed with the time it is
// sent (the receiver refuses a timestamp five minutes old), and its claim
// must run from its own start, so a second run starting meanwhile cannot
// claim it and send it twice.
func TestSlowBatchSignsAndLeasesPerAttempt(t *testing.T) {
	e := newEnv(t, nil)
	var requests atomic.Int32
	var nested int
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.rc.ServeHTTP(w, r)
		switch requests.Add(1) {
		case 1:
			e.advance(6 * time.Minute) // the first delivery is slow to answer
		case 2:
			// A second run while the second delivery is in flight.
			n, err := e.p.Deliver(r.Context(), e.store, e.clock())
			if err != nil {
				t.Error(err)
			}
			nested = n
		}
	}))
	defer slow.Close()
	ep, err := e.p.AddEndpoint(t.Context(), e.store, scope, slow.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.p.RemoveEndpoint(t.Context(), e.store, scope, e.ep.ID); err != nil {
		t.Fatal(err)
	}
	e.rc.secret = ep.Secret
	e.file(t, scope, "a")
	e.file(t, scope, "b")

	n, err := e.p.Deliver(t.Context(), e.store, e.clock())
	if err != nil || n != 2 {
		t.Fatalf("Deliver = %d, %v; want both delivered (received %d, ids %v)", n, err, requests.Load(), e.rc.ids)
	}
	if requests.Load() != 2 || nested != 0 {
		t.Fatalf("%d requests, the concurrent run delivered %d: a delivery was sent twice", requests.Load(), nested)
	}
}

// TestRacingRunsSendOnce: a second run starts while the first is sending
// its first delivery and takes the second one, which the first run had
// already read as due; that attempt fails, so the delivery stays pending
// with one attempt. The first run's claim of it must then fail on the
// attempt count rather than send it again before its backoff.
func TestRacingRunsSendOnce(t *testing.T) {
	e := newEnv(t, nil)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			e.rc.ServeHTTP(w, r)
			if _, err := e.p.Deliver(r.Context(), e.store, e.clock()); err != nil {
				t.Error(err)
			}
		default:
			http.Error(w, "busy", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	ep, err := e.p.AddEndpoint(t.Context(), e.store, scope, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.p.RemoveEndpoint(t.Context(), e.store, scope, e.ep.ID); err != nil {
		t.Fatal(err)
	}
	e.rc.secret = ep.Secret
	e.file(t, scope, "a")
	e.file(t, scope, "b")
	if _, err := e.p.Deliver(t.Context(), e.store, e.clock()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("%d requests; the second delivery was sent again by the run that lost the race", requests.Load())
	}
}

// TestStaleRunnerCannotRecord: run A's attempt outlives its lease (the
// clock jumps past it while A waits for the endpoint); run B reclaims the
// delivery and fails. When A's late success comes back, its claim (the
// old attempt count) is no longer the delivery's, so A must not record
// it: the delivery stays as B left it.
func TestStaleRunnerCannotRecord(t *testing.T) {
	e := newEnv(t, nil)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			e.advance(10 * time.Minute) // past A's lease
			if _, err := e.p.Deliver(r.Context(), e.store, e.clock()); err != nil {
				t.Error(err)
			}
			return // A's attempt succeeds, late
		}
		http.Error(w, "busy", http.StatusServiceUnavailable) // B's attempt fails
	}))
	defer srv.Close()
	ep, err := e.p.AddEndpoint(t.Context(), e.store, scope, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.p.RemoveEndpoint(t.Context(), e.store, scope, e.ep.ID); err != nil {
		t.Fatal(err)
	}
	e.rc.secret = ep.Secret
	e.file(t, scope, "a")
	if _, err := e.p.Deliver(t.Context(), e.store, e.clock()); err != nil {
		t.Fatal(err)
	}
	d := e.pending(t)
	if requests.Load() != 2 || len(d) != 1 || d[0].Attempts != 2 || d[0].LastError == "" {
		t.Fatalf("after %d requests the delivery is %+v; want B's failed second attempt, not A's stale success", requests.Load(), d)
	}
}
