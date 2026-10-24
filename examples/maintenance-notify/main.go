// Command maintenance-notify confirms direct uploads from the bucket's own
// object events, with github.com/ubgo/filesystem/contrib/notify/sqs: the
// bucket publishes an S3 event notification to an SQS queue for every
// object written under incoming/, and a consumer confirms each new object
// against the upload policy the moment it lands, instead of waiting for
// the browser to say it finished. The queue and the bucket run in this
// process (an in-memory queue behind the consumer's API interface, and a
// memory disk), so it needs no AWS account.
//
//	go run ./maintenance-notify
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"log"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/notify"
	"github.com/ubgo/filesystem/contrib/notify/sqs"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// Names of this deployment: the bucket the events come from, the queue
// they are delivered to, and the directory direct uploads land in.
const (
	bucketName  = "media"
	queueURL    = "https://sqs.us-east-1.amazonaws.com/123456789012/media-events"
	incomingDir = "incoming"
)

// avatars is the policy a direct upload is confirmed against: PNG only,
// at most 1 MiB, the type detected from the stored bytes.
var avatars = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/png"}}

// Confirmer is the application's notify.Handler. It confirms each object
// created under incomingDir with upload.Confirm, which deletes an object
// the policy refuses, and remembers the events it has handled, because a
// queue delivers at least once. A real app keeps that set in its database
// (a unique Event.ID column), not in memory.
type Confirmer struct {
	Disk *filesystem.Disk
	Out  io.Writer
	seen map[string]bool
	// fail, when set, fails the next event, as a database outage would.
	fail error
}

// Handle is the notify.Handler: return nil to acknowledge, an error to
// have the event delivered again.
// ds:def id=confirmer-handle-9z5tacrg
func (c *Confirmer) Handle(ctx context.Context, e notify.Event) error {
	if e.Kind != notify.KindCreated || !filesystem.KeyInDir(e.Key, incomingDir) {
		return nil
	}
	if c.seen[e.ID] {
		fmt.Fprintf(c.Out, "duplicate %s: skipped\n", e.Key)
		return nil
	}
	if err := c.fail; err != nil {
		c.fail = nil
		return err
	}
	res, err := upload.Confirm(ctx, c.Disk, e.Key, path.Base(e.Key), avatars)
	switch {
	case filesystem.CodeOf(err) == filesystem.CodeRejected || filesystem.CodeOf(err) == filesystem.CodeTooLarge:
		fmt.Fprintf(c.Out, "refused %s: %s, object deleted\n", e.Key, filesystem.CodeOf(err))
	case filesystem.CodeOf(err) == filesystem.CodeNotFound:
		fmt.Fprintf(c.Out, "gone %s: deleted before the event arrived\n", e.Key)
	case err != nil:
		return err // anything else is worth another delivery
	default:
		fmt.Fprintf(c.Out, "confirmed %s: %s, %d bytes\n", res.Key, res.ContentType, res.Size)
	}
	c.seen[e.ID] = true
	return nil
}

// newConsumer reads the queue for bucketName's events. Events from any
// other bucket are acknowledged without being handled, because anyone
// allowed to publish to the queue could name any bucket.
// ds:def id=newconsumer-9gz6djdp
func newConsumer(q sqs.API, c *Confirmer, out io.Writer) (*sqs.Consumer, error) {
	return sqs.New(q, sqs.Options{
		QueueURL: queueURL,
		Buckets:  notify.Buckets{bucketName},
		Handler:  c.Handle,
		OnRejected: func(e notify.Event) {
			fmt.Fprintf(out, "rejected an event from bucket %q\n", e.Bucket)
		},
		OnError: func(err error) {
			if errors.Is(err, notify.ErrMalformed) {
				fmt.Fprintln(out, "dropped a message that is not an S3 event")
				return
			}
			fmt.Fprintf(out, "left for redelivery: %v\n", err)
		},
	})
}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run uploads two files straight to the bucket, lets the bucket publish
// its events, and polls the queue three times.
func run(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk(bucketName, memory.New())
	if err != nil {
		return err
	}
	q := &queue{}
	c := &Confirmer{Disk: disk, Out: out, seen: map[string]bool{}, fail: errors.New("database unavailable")}
	consumer, err := newConsumer(q, c, out)
	if err != nil {
		return err
	}
	// Two direct uploads: a real PNG, and HTML named .png.
	photo, err := pngBytes()
	if err != nil {
		return err
	}
	first, err := bucketPut(ctx, disk, q, bucketName, "incoming/avatar 1.png", photo, "1")
	if err != nil {
		return err
	}
	if _, err := bucketPut(ctx, disk, q, bucketName, "incoming/avatar-2.png", []byte("<html><script>alert(1)</script>"), "2"); err != nil {
		return err
	}
	q.send(s3Event("other-bucket", "incoming/x.png", 1, "3"))
	q.send("hello")
	if err := poll(ctx, out, consumer, q, 1); err != nil {
		return err
	}
	if err := poll(ctx, out, consumer, q, 2); err != nil {
		return err
	}
	q.send(first) // the queue delivers the first event again
	if err := poll(ctx, out, consumer, q, 3); err != nil {
		return err
	}
	keys, err := listKeys(ctx, disk)
	fmt.Fprintf(out, "stored: %v\n", keys)
	return err
}

// poll receives and handles one batch, as Consumer.Run does in a loop.
func poll(ctx context.Context, out io.Writer, consumer *sqs.Consumer, q *queue, n int) error {
	deleted, err := consumer.Poll(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "poll %d: %d acknowledged, %d left in the queue\n", n, deleted, q.depth())
	return nil
}

// bucketPut stands in for a client's direct upload: the object is written
// to the bucket, and the bucket publishes its event to the queue. It
// returns the event, so the demo can deliver it again.
func bucketPut(ctx context.Context, disk *filesystem.Disk, q *queue, bucket, key string, body []byte, sequencer string) (string, error) {
	if _, err := disk.WriteBytes(ctx, key, body, filesystem.WriteOptions{}); err != nil {
		return "", err
	}
	event := s3Event(bucket, key, int64(len(body)), sequencer)
	q.send(event)
	return event, nil
}

// s3Event formats the notification S3 sends for a created object, with
// the key URL-encoded as S3 encodes it (a space becomes "+").
func s3Event(bucket, key string, size int64, sequencer string) string {
	type object struct {
		Key       string `json:"key"`
		Size      int64  `json:"size"`
		ETag      string `json:"eTag"`
		Sequencer string `json:"sequencer"`
	}
	type record struct {
		EventName string    `json:"eventName"`
		EventTime time.Time `json:"eventTime"`
		S3        struct {
			Bucket struct {
				Name string `json:"name"`
			} `json:"bucket"`
			Object object `json:"object"`
		} `json:"s3"`
	}
	var r record
	r.EventName = "ObjectCreated:Put"
	r.EventTime = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	r.S3.Bucket.Name = bucket
	r.S3.Object = object{Key: url.QueryEscape(key), Size: size, ETag: "etag-" + sequencer, Sequencer: sequencer}
	body, err := json.Marshal(map[string][]record{"Records": {r}})
	if err != nil {
		panic(err) // the types above always marshal
	}
	return string(body)
}

// pngBytes encodes a 1x1 PNG.
func pngBytes() ([]byte, error) {
	var buf bytes.Buffer
	err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 1, 1)))
	return buf.Bytes(), err
}

// listKeys lists every key on the disk.
func listKeys(ctx context.Context, disk *filesystem.Disk) ([]string, error) {
	var keys []string
	for e, err := range disk.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return nil, err
		}
		keys = append(keys, e.Key)
	}
	return keys, nil
}

// queue is an in-memory SQS queue implementing sqs.API: a received message
// stays until it is deleted, so one whose handling failed is received again
// by the next poll, as SQS redelivers it after its visibility timeout.
type queue struct {
	mu   sync.Mutex
	msgs []types.Message
	n    int
}

func (q *queue) send(body string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.n++
	id := strconv.Itoa(q.n)
	q.msgs = append(q.msgs, types.Message{MessageId: aws.String("m" + id), ReceiptHandle: aws.String("r" + id), Body: aws.String(body)})
}

func (q *queue) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.msgs)
}

func (q *queue) ReceiveMessage(_ context.Context, in *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := min(int(in.MaxNumberOfMessages), len(q.msgs))
	return &awssqs.ReceiveMessageOutput{Messages: slices.Clone(q.msgs[:n])}, nil
}

func (q *queue) DeleteMessage(_ context.Context, in *awssqs.DeleteMessageInput, _ ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.msgs = slices.DeleteFunc(q.msgs, func(m types.Message) bool { return aws.ToString(m.ReceiptHandle) == aws.ToString(in.ReceiptHandle) })
	return &awssqs.DeleteMessageOutput{}, nil
}
