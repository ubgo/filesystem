package sqs_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/ubgo/filesystem/contrib/notify"
	"github.com/ubgo/filesystem/contrib/notify/sqs"
)

const versioned = `{"Records":[
 {"eventName":"ObjectRemoved:DeleteMarkerCreated","eventTime":"2026-10-07T09:00:00Z","s3":{"bucket":{"name":"media"},"object":{"key":"a.png","versionId":"v2","sequencer":"01"}}},
 {"eventName":"ObjectRestore:Completed","eventTime":"2026-10-07T09:00:02Z","s3":{"bucket":{"name":"media"},"object":{"key":"x","versionId":"v1","sequencer":"02"}}}
]}`

// TestPassOther pins the event-kind seams: every event carries the
// provider's name and version ID (a delete marker is told apart from a
// delete), an unknown type is dropped by default and arrives as
// KindOther with PassOther.
func TestPassOther(t *testing.T) {
	run := func(pass bool) []notify.Event {
		t.Helper()
		var got []notify.Event
		c, err := sqs.New(&fakeQueue{messages: []types.Message{msg("m", versioned)}}, sqs.Options{
			QueueURL: queue, Buckets: notify.Buckets{"media"}, PassOther: pass,
			Handler: func(_ context.Context, e notify.Event) error { got = append(got, e); return nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Poll(t.Context()); err != nil {
			t.Fatal(err)
		}
		return got
	}
	def := run(false)
	if len(def) != 1 || def[0].Kind != notify.KindDeleted || def[0].Name != "ObjectRemoved:DeleteMarkerCreated" || def[0].VersionID != "v2" {
		t.Fatalf("default %+v", def)
	}
	all := run(true)
	if len(all) != 2 || all[1].Kind != notify.KindOther || all[1].Name != "ObjectRestore:Completed" || all[1].VersionID != "v1" || all[1].Key != "x" {
		t.Fatalf("PassOther %+v", all)
	}
}
