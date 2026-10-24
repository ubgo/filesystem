package notify_test

import (
	"errors"
	"testing"

	"github.com/ubgo/filesystem/contrib/notify"
)

func TestBuckets(t *testing.T) {
	if err := (notify.Buckets{}).Validate(); !errors.Is(err, notify.ErrNoBuckets) {
		t.Fatalf("empty Buckets: %v", err)
	}
	b := notify.Buckets{"media"}
	if b.Validate() != nil || !b.Allows("media") || b.Allows("media2") || b.Allows("") {
		t.Fatal("Buckets.Allows")
	}
}

func TestEventID(t *testing.T) {
	a := notify.EventID("", "media", "a.png", "0055")
	if len(a) != 32 || a != notify.EventID("", "media", "a.png", "0055") {
		t.Fatalf("EventID not stable: %q", a)
	}
	for _, other := range []string{
		notify.EventID("", "media", "b.png", "0055"),  // same sequencer, other key
		notify.EventID("", "media2", "a.png", "0055"), // other bucket
		notify.EventID("acct", "media", "a.png", "0055"),
		notify.EventID("", "media", "a.png0", "055"), // parts shifted across the boundary
	} {
		if other == a {
			t.Fatalf("EventID collides: %q", other)
		}
	}
}
