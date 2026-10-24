package pubsub_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	gpubsub "cloud.google.com/go/pubsub/v2"
	pb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"github.com/ubgo/filesystem/contrib/notify"
	"github.com/ubgo/filesystem/contrib/notify/pubsub"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	project = "proj"
	topic   = "projects/proj/topics/gcs"
	subName = "projects/proj/subscriptions/gcs-sub"
)

// fake starts an in-process Pub/Sub (pstest) with one topic and one
// subscription, and returns a client connected to it.
func fake(t *testing.T) (*pstest.Server, *gpubsub.Client) {
	t.Helper()
	srv := pstest.NewServer()
	t.Cleanup(func() { _ = srv.Close() })
	conn, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client, err := gpubsub.NewClient(t.Context(), project, option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.TopicAdminClient.CreateTopic(t.Context(), &pb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SubscriptionAdminClient.CreateSubscription(t.Context(), &pb.Subscription{Name: subName, Topic: topic, AckDeadlineSeconds: 10}); err != nil {
		t.Fatal(err)
	}
	return srv, client
}

func attrs(eventType, bucket, object string) map[string]string {
	return map[string]string{"eventType": eventType, "bucketId": bucket, "objectId": object, "eventTime": "2026-10-07T09:00:00.123Z", "payloadFormat": "JSON_API_V1"}
}

func TestConsumer(t *testing.T) {
	srv, client := fake(t)
	pub := client.Publisher(topic)
	overwritten := attrs("OBJECT_DELETE", "media", "replaced.png")
	overwritten["overwrittenByGeneration"] = "2"
	for _, m := range []*gpubsub.Message{
		{Attributes: attrs("OBJECT_FINALIZE", "media", "pending/a b.png"), Data: []byte(`{"size":"42","etag":"CJ"}`)},
		{Attributes: attrs("OBJECT_DELETE", "media", "old.txt")},
		{Attributes: overwritten},
		{Attributes: attrs("OBJECT_METADATA_UPDATE", "media", "x")},
		{Attributes: attrs("OBJECT_FINALIZE", "other", "y")},
		{Data: []byte("not a notification")},
	} {
		if _, err := pub.Publish(t.Context(), m).Get(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	pub.Stop()

	var mu sync.Mutex
	var got, rejected []notify.Event
	var errs []error
	failedOnce := false
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	c, err := pubsub.New(client.Subscriber(subName), pubsub.Options{
		Buckets: notify.Buckets{"media"},
		Handler: func(_ context.Context, e notify.Event) error {
			mu.Lock()
			defer mu.Unlock()
			if e.Key == "old.txt" && !failedOnce {
				failedOnce = true // nacked: Pub/Sub must deliver it again
				return errors.New("database down")
			}
			got = append(got, e)
			if len(got) == 2 {
				cancel()
			}
			return nil
		},
		OnRejected: func(e notify.Event) { mu.Lock(); rejected = append(rejected, e); mu.Unlock() },
		OnError:    func(err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("timed out before both events were handled")
	}
	mu.Lock()
	defer mu.Unlock()
	byKey := map[string]notify.Event{}
	for _, e := range got {
		byKey[e.Key] = e
	}
	created, deleted := byKey["pending/a b.png"], byKey["old.txt"]
	if len(got) != 2 || created.Kind != notify.KindCreated || created.Size != 42 || created.ETag != "CJ" || created.Provider != notify.ProviderGCS || created.Time.IsZero() || deleted.Kind != notify.KindDeleted {
		t.Fatalf("events %+v", got)
	}
	if !failedOnce || len(rejected) != 1 || rejected[0].Bucket != "other" || len(errs) < 2 {
		t.Fatalf("redelivered %v, rejected %+v, errors %v", failedOnce, rejected, errs)
	}
	// Every handled, skipped or rejected message was acknowledged.
	acked := 0
	for _, m := range srv.Messages() {
		if m.Acks > 0 {
			acked++
		}
	}
	if acked != 6 {
		t.Fatalf("%d messages acknowledged, want all 6 (the malformed one too)", acked)
	}
	if len(created.ID) != 32 || created.ID == deleted.ID {
		t.Fatalf("event IDs %q and %q", created.ID, deleted.ID)
	}
}

func TestOptionsRequired(t *testing.T) {
	_, client := fake(t)
	sub := client.Subscriber(subName)
	h := func(context.Context, notify.Event) error { return nil }
	for name, tc := range map[string]struct {
		r    pubsub.Receiver
		opts pubsub.Options
	}{
		"no subscriber": {nil, pubsub.Options{Buckets: notify.Buckets{"b"}, Handler: h}},
		"no buckets":    {sub, pubsub.Options{Handler: h}},
		"no handler":    {sub, pubsub.Options{Buckets: notify.Buckets{"b"}}},
	} {
		if _, err := pubsub.New(tc.r, tc.opts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestKeepMalformed nacks a malformed message, so Pub/Sub delivers it
// again (for a dead-letter topic), instead of acknowledging it.
func TestKeepMalformed(t *testing.T) {
	srv, client := fake(t)
	pub := client.Publisher(topic)
	if _, err := pub.Publish(t.Context(), &gpubsub.Message{Data: []byte("junk")}).Get(t.Context()); err != nil {
		t.Fatal(err)
	}
	pub.Stop()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var mu sync.Mutex
	seen := 0
	c, err := pubsub.New(client.Subscriber(subName), pubsub.Options{
		Buckets: notify.Buckets{"media"}, KeepMalformed: true,
		Handler: func(context.Context, notify.Event) error { return nil },
		OnError: func(err error) {
			mu.Lock()
			defer mu.Unlock()
			if errors.Is(err, notify.ErrMalformed) {
				if seen++; seen == 2 {
					cancel()
				}
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Run(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen < 2 || srv.Messages()[0].Acks != 0 {
		t.Fatalf("malformed message seen %d times, acked %d", seen, srv.Messages()[0].Acks)
	}
}

// stoppedReceiver is a subscription that fails at once, as a missing one
// or one with denied credentials does.
type stoppedReceiver struct{ err error }

func (r stoppedReceiver) Receive(context.Context, func(context.Context, *gpubsub.Message)) error {
	return r.err
}

// TestRunReturnsTheSubscriptionError pins that Run reports the error that
// stopped the subscription while its context is still live.
func TestRunReturnsTheSubscriptionError(t *testing.T) {
	stop := errors.New("subscription not found")
	c, err := pubsub.New(stoppedReceiver{stop}, pubsub.Options{Buckets: notify.Buckets{"b"}, Handler: func(context.Context, notify.Event) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Run(t.Context()); !errors.Is(err, stop) {
		t.Fatalf("Run = %v, want the subscription's error", err)
	}
}

// TestNotificationWithoutBucketOrObject pins that a GCS notification
// naming no bucket or no object is malformed: with no OnError set each is
// still acknowledged (dropped, not redelivered forever) and reaches
// neither the handler nor OnRejected.
func TestNotificationWithoutBucketOrObject(t *testing.T) {
	srv, client := fake(t)
	pub := client.Publisher(topic)
	for _, a := range []map[string]string{attrs("OBJECT_FINALIZE", "", "x"), attrs("OBJECT_FINALIZE", "media", "")} {
		if _, err := pub.Publish(t.Context(), &gpubsub.Message{Attributes: a}).Get(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	pub.Stop()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var mu sync.Mutex
	var reached []notify.Event
	record := func(e notify.Event) { mu.Lock(); reached = append(reached, e); mu.Unlock() }
	c, err := pubsub.New(client.Subscriber(subName), pubsub.Options{
		Buckets:    notify.Buckets{"media"},
		Handler:    func(_ context.Context, e notify.Event) error { record(e); return nil },
		OnRejected: record,
	})
	if err != nil {
		t.Fatal(err)
	}
	acked := func() bool {
		for _, m := range srv.Messages() {
			if m.Acks == 0 {
				return false
			}
		}
		return true
	}
	go func() {
		for ctx.Err() == nil {
			if acked() {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	if err := c.Run(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reached) != 0 || !acked() {
		t.Fatalf("reached %+v, all acked %v; want both dropped with an ack", reached, acked())
	}
}
