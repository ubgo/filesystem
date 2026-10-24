package sqs_test

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/ubgo/filesystem/contrib/notify"
	"github.com/ubgo/filesystem/contrib/notify/sqs"
)

const queue = "https://sqs.us-east-1.amazonaws.com/123456789012/uploads"

const s3Event = `{"Records":[
 {"eventName":"ObjectCreated:Put","eventTime":"2026-10-07T09:00:00Z","s3":{"bucket":{"name":"media"},"object":{"key":"pending/a+b%2B.png","size":42,"eTag":"abc","sequencer":"0055"}}},
 {"eventName":"ObjectRemoved:Delete","eventTime":"2026-10-07T09:00:01Z","s3":{"bucket":{"name":"media"},"object":{"key":"old.txt","sequencer":"0056"}}},
 {"eventName":"ObjectRestore:Completed","eventTime":"2026-10-07T09:00:02Z","s3":{"bucket":{"name":"media"},"object":{"key":"x"}}}
]}`

// fakeQueue is an in-process SQS: it hands out its messages once and
// records which were deleted.
type fakeQueue struct {
	mu       sync.Mutex
	messages []types.Message
	deleted  []string
}

func (q *fakeQueue) ReceiveMessage(_ context.Context, in *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := &awssqs.ReceiveMessageOutput{Messages: q.messages}
	q.messages = nil
	return out, nil
}

func (q *fakeQueue) DeleteMessage(_ context.Context, in *awssqs.DeleteMessageInput, _ ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.deleted = append(q.deleted, aws.ToString(in.ReceiptHandle))
	return &awssqs.DeleteMessageOutput{}, nil
}

func msg(id, body string) types.Message {
	return types.Message{MessageId: aws.String(id), ReceiptHandle: aws.String("rh-" + id), Body: aws.String(body)}
}

func snsWrap(t *testing.T, inner string) string {
	b, err := json.Marshal(map[string]string{"Type": "Notification", "Message": inner})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestConsumer(t *testing.T) {
	q := &fakeQueue{messages: []types.Message{
		msg("direct", s3Event),
		msg("via-sns", snsWrap(t, strings.ReplaceAll(s3Event, `"media"`, `"other"`))),
		msg("test", `{"Event":"s3:TestEvent","Bucket":"media"}`),
		msg("junk", `not json`),
	}}
	var got, rejected []notify.Event
	var errs []error
	c, err := sqs.New(q, sqs.Options{
		QueueURL: queue, Buckets: notify.Buckets{"media"},
		Handler:    func(_ context.Context, e notify.Event) error { got = append(got, e); return nil },
		OnRejected: func(e notify.Event) { rejected = append(rejected, e) },
		OnError:    func(err error) { errs = append(errs, err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	n, err := c.Poll(t.Context())
	if err != nil || n != 4 {
		t.Fatalf("Poll = %d, %v", n, err)
	}
	if len(got) != 2 || got[0].Key != "pending/a b+.png" || got[0].Kind != notify.KindCreated || got[0].Size != 42 || got[0].ID != notify.EventID("", "media", "pending/a b+.png", "0055") ||
		got[1].Key != "old.txt" || got[1].Kind != notify.KindDeleted {
		t.Fatalf("events %+v", got)
	}
	if len(rejected) != 2 || rejected[0].Bucket != "other" {
		t.Fatalf("rejected %+v", rejected)
	}
	// The junk message is reported, then deleted so it does not come
	// back forever.
	if strings.Join(q.deleted, ",") != "rh-direct,rh-via-sns,rh-test,rh-junk" || len(errs) != 1 || !errors.Is(errs[0], notify.ErrMalformed) {
		t.Fatalf("deleted %v, errors %v", q.deleted, errs)
	}
}

func TestHandlerErrorKeepsTheMessage(t *testing.T) {
	q := &fakeQueue{messages: []types.Message{msg("m", s3Event)}}
	c, err := sqs.New(q, sqs.Options{QueueURL: queue, Buckets: notify.Buckets{"media"}, Handler: func(context.Context, notify.Event) error { return errors.New("database down") }})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := c.Poll(t.Context()); err != nil || n != 0 || len(q.deleted) != 0 {
		t.Fatalf("Poll = %d, %v; deleted %v", n, err, q.deleted)
	}
}

func TestOptionsRequired(t *testing.T) {
	h := func(context.Context, notify.Event) error { return nil }
	q := &fakeQueue{}
	for name, o := range map[string]sqs.Options{
		"no queue":   {Buckets: notify.Buckets{"b"}, Handler: h},
		"no buckets": {QueueURL: queue, Handler: h},
		"no handler": {QueueURL: queue, Buckets: notify.Buckets{"b"}},
	} {
		if _, err := sqs.New(q, o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestSignedOnTheWire runs the real SQS client against an in-process
// SQS endpoint, which refuses requests not signed with AWS Signature
// Version 4 by the expected access key, as SQS does.
func TestSignedOnTheWire(t *testing.T) {
	var mu sync.Mutex
	var targets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDTEST/") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"__type":"com.amazon.coral.service#InvalidClientTokenId","message":"unsigned"}`)
			return
		}
		mu.Lock()
		targets = append(targets, r.Header.Get("X-Amz-Target"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		switch r.Header.Get("X-Amz-Target") {
		case "AmazonSQS.ReceiveMessage":
			sum := md5.Sum([]byte(s3Event))
			_ = json.NewEncoder(w).Encode(map[string]any{"Messages": []map[string]string{{"MessageId": "m1", "ReceiptHandle": "rh-1", "Body": s3Event, "MD5OfBody": hex.EncodeToString(sum[:])}}})
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer srv.Close()
	client := func(key string) *awssqs.Client {
		return awssqs.New(awssqs.Options{
			Region: "us-east-1", BaseEndpoint: aws.String(srv.URL),
			Credentials: credentials.NewStaticCredentialsProvider(key, "secret", ""),
		})
	}
	var got []notify.Event
	opts := sqs.Options{QueueURL: srv.URL + "/123456789012/uploads", Buckets: notify.Buckets{"media"}, Handler: func(_ context.Context, e notify.Event) error { got = append(got, e); return nil }}
	c, err := sqs.New(client("AKIDTEST"), opts)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := c.Poll(t.Context()); err != nil || n != 1 || len(got) != 2 {
		t.Fatalf("signed Poll = %d, %v, %d events", n, err, len(got))
	}
	if strings.Join(targets, ",") != "AmazonSQS.ReceiveMessage,AmazonSQS.DeleteMessage" {
		t.Fatalf("calls %v", targets)
	}
	// Other credentials are refused by the endpoint, and the consumer
	// reports the failure instead of handling anything.
	got = nil
	c, _ = sqs.New(client("AKIDOTHER"), opts)
	if _, err := c.Poll(t.Context()); err == nil || len(got) != 0 {
		t.Fatalf("Poll with wrong credentials: %v, %d events", err, len(got))
	}
}

func TestRunStopsWithContext(t *testing.T) {
	q := &fakeQueue{messages: []types.Message{msg("m", s3Event)}}
	ctx, cancel := context.WithCancel(t.Context())
	c, _ := sqs.New(q, sqs.Options{QueueURL: queue, Buckets: notify.Buckets{"media"}, Handler: func(context.Context, notify.Event) error { cancel(); return nil }})
	if err := c.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(q.deleted) != 1 {
		t.Fatalf("deleted %v", q.deleted)
	}
}

// TestKeepMalformed leaves a malformed message for the dead-letter queue.
func TestKeepMalformed(t *testing.T) {
	q := &fakeQueue{messages: []types.Message{msg("junk", "not json")}}
	var errs []error
	c, err := sqs.New(q, sqs.Options{QueueURL: queue, Buckets: notify.Buckets{"media"}, KeepMalformed: true,
		Handler: func(context.Context, notify.Event) error { return nil }, OnError: func(err error) { errs = append(errs, err) }})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := c.Poll(t.Context()); err != nil || n != 0 || len(q.deleted) != 0 || len(errs) != 1 {
		t.Fatalf("Poll = %d, %v; deleted %v, errors %v", n, err, q.deleted, errs)
	}
}

// TestEventIDsDifferAcrossKeys: S3 sequencers repeat across keys, so
// the event ID must not be the sequencer alone.
func TestEventIDsDifferAcrossKeys(t *testing.T) {
	body := `{"Records":[
 {"eventName":"ObjectCreated:Put","s3":{"bucket":{"name":"media"},"object":{"key":"a","sequencer":"01"}}},
 {"eventName":"ObjectCreated:Put","s3":{"bucket":{"name":"media"},"object":{"key":"b","sequencer":"01"}}}]}`
	q := &fakeQueue{messages: []types.Message{msg("m", body)}}
	var ids []string
	c, _ := sqs.New(q, sqs.Options{QueueURL: queue, Buckets: notify.Buckets{"media"}, Handler: func(_ context.Context, e notify.Event) error { ids = append(ids, e.ID); return nil }})
	if _, err := c.Poll(t.Context()); err != nil || len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("ids %v, %v", ids, err)
	}
}

// brokenQueue fails every receive with errReceive and every delete with
// errDelete, while handing out its messages once.
type brokenQueue struct {
	fakeQueue
	errReceive, errDelete error
}

func (q *brokenQueue) ReceiveMessage(ctx context.Context, in *awssqs.ReceiveMessageInput, o ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
	if q.errReceive != nil {
		return nil, q.errReceive
	}
	return q.fakeQueue.ReceiveMessage(ctx, in, o...)
}

func (q *brokenQueue) DeleteMessage(context.Context, *awssqs.DeleteMessageInput, ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
	return nil, q.errDelete
}

// TestRunSurvivesFailedReceives pins that a failed receive does not end
// Run: it is reported to OnError, Run pauses and polls again, and it
// returns nil once the context ends, even mid-pause.
func TestRunSurvivesFailedReceives(t *testing.T) {
	outage := errors.New("network unreachable")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var errs []error
	c, err := sqs.New(&brokenQueue{errReceive: outage}, sqs.Options{
		QueueURL: queue, Buckets: notify.Buckets{"media"}, Handler: func(context.Context, notify.Event) error { return nil },
		OnError: func(err error) {
			if errs = append(errs, err); len(errs) == 2 {
				cancel() // during the second pause
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(errs) != 2 || !errors.Is(errs[0], outage) {
		t.Fatalf("errors %v, want the outage reported once per poll", errs)
	}
}

// TestFailedDeleteIsReported pins that a handled message whose delete
// fails is reported to OnError and not counted as done.
func TestFailedDeleteIsReported(t *testing.T) {
	denied := errors.New("access denied")
	q := &brokenQueue{fakeQueue: fakeQueue{messages: []types.Message{msg("m", s3Event)}}, errDelete: denied}
	var errs []error
	c, err := sqs.New(q, sqs.Options{QueueURL: queue, Buckets: notify.Buckets{"media"},
		Handler: func(context.Context, notify.Event) error { return nil }, OnError: func(err error) { errs = append(errs, err) }})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := c.Poll(t.Context()); err != nil || n != 0 || len(errs) != 1 || !errors.Is(errs[0], denied) {
		t.Fatalf("Poll = %d, %v; errors %v", n, err, errs)
	}
}

// TestMalformedBodies pins that an event with no records and one whose
// object key does not URL-decode are malformed: reported, deleted, never
// handled.
func TestMalformedBodies(t *testing.T) {
	q := &fakeQueue{messages: []types.Message{
		msg("empty", `{"Records":[]}`),
		msg("badkey", `{"Records":[{"eventName":"ObjectCreated:Put","s3":{"bucket":{"name":"media"},"object":{"key":"bad%zz"}}}]}`),
	}}
	var errs []error
	handled := 0
	c, err := sqs.New(q, sqs.Options{QueueURL: queue, Buckets: notify.Buckets{"media"},
		Handler: func(context.Context, notify.Event) error { handled++; return nil }, OnError: func(err error) { errs = append(errs, err) }})
	if err != nil {
		t.Fatal(err)
	}
	n, err := c.Poll(t.Context())
	if err != nil || n != 2 || handled != 0 || len(errs) != 2 {
		t.Fatalf("Poll = %d, %v; handled %d, errors %v", n, err, handled, errs)
	}
	for _, e := range errs {
		if !errors.Is(e, notify.ErrMalformed) {
			t.Fatalf("error %v does not wrap notify.ErrMalformed", e)
		}
	}
}
