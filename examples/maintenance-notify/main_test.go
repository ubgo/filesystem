package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/notify"
	"github.com/ubgo/filesystem/memory"
)

// TestRun checks the output the maintenance guide shows.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	want := `left for redelivery: sqs: message m1: database unavailable
refused incoming/avatar-2.png: rejected, object deleted
rejected an event from bucket "other-bucket"
dropped a message that is not an S3 event
poll 1: 3 acknowledged, 1 left in the queue
confirmed incoming/avatar 1.png: image/png, 72 bytes
poll 2: 1 acknowledged, 0 left in the queue
duplicate incoming/avatar 1.png: skipped
poll 3: 1 acknowledged, 0 left in the queue
stored: [incoming/avatar 1.png]
`
	if got := out.String(); got != want {
		t.Fatalf("output:\n%s\nwant:\n%s", got, strings.TrimSpace(want))
	}
}

// TestHandleSkipsWhatItDoesNotOwn: deletions and keys outside incomingDir
// are acknowledged without a Confirm, and an object deleted before its
// event arrived is acknowledged rather than retried forever.
func TestHandleSkipsWhatItDoesNotOwn(t *testing.T) {
	disk, err := filesystem.NewDisk(bucketName, memory.New())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	c := &Confirmer{Disk: disk, Out: &out, seen: map[string]bool{}}
	for _, e := range []notify.Event{
		{ID: "1", Kind: notify.KindDeleted, Bucket: bucketName, Key: "incoming/a.png"},
		{ID: "2", Kind: notify.KindCreated, Bucket: bucketName, Key: "exports/a.zip"},
		{ID: "3", Kind: notify.KindCreated, Bucket: bucketName, Key: "incoming/gone.png"},
	} {
		if err := c.Handle(t.Context(), e); err != nil {
			t.Errorf("%s: %v", e.Key, err)
		}
	}
	if got, want := out.String(), "gone incoming/gone.png: deleted before the event arrived\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

// TestRedeliveryAfterAnOutage: a message whose handling failed stays in
// the queue until a later poll handles it.
func TestRedeliveryAfterAnOutage(t *testing.T) {
	disk, err := filesystem.NewDisk(bucketName, memory.New())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	c := &Confirmer{Disk: disk, Out: &out, seen: map[string]bool{}, fail: errOutage{}}
	q := &queue{}
	consumer, err := newConsumer(q, c, &out)
	if err != nil {
		t.Fatal(err)
	}
	photo, err := pngBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bucketPut(t.Context(), disk, q, bucketName, "incoming/p.png", photo, "9"); err != nil {
		t.Fatal(err)
	}
	for i, want := range []int{0, 1} {
		n, err := consumer.Poll(t.Context())
		if err != nil || n != want {
			t.Fatalf("poll %d = %d, %v; want %d", i+1, n, err, want)
		}
	}
	if q.depth() != 0 || !strings.Contains(out.String(), "confirmed incoming/p.png") {
		t.Errorf("depth %d, output:\n%s", q.depth(), out.String())
	}
}

type errOutage struct{}

func (errOutage) Error() string { return "outage" }
