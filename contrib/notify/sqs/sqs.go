// Package sqs consumes Amazon S3 event notifications from an SQS queue,
// sent there directly or through an SNS topic, and passes each object
// created or removed event to a notify.Handler. It also reads queues that
// S3-compatible stores fill with S3-format events.
//
// Reading the queue is authenticated by AWS IAM: the SQS client signs
// every request with the application's credentials, and only principals
// the queue policy allows can send to it. Because anyone that policy
// lets send could still write a message naming any bucket, the consumer
// handles only events from the buckets it is given.
//
// A message is deleted from the queue only after the Handler accepted
// every event in it; a Handler error leaves it to come back after the
// queue's visibility timeout, and a message that never succeeds goes to
// the queue's dead-letter queue (configure one). A message that is not an
// S3 event is reported and deleted, or with KeepMalformed left for the
// dead-letter queue. Handlers must be
// idempotent: SQS delivers at least once.
package sqs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/ubgo/filesystem/contrib/notify"
)

// Receive defaults: SQS's maxima, so an idle queue costs one request per
// 20 seconds and a busy one moves ten messages a request.
const (
	DefaultWaitSeconds = 20
	DefaultMaxMessages = 10
)

// testEvent is the message S3 sends when a notification is configured.
const testEvent = "s3:TestEvent"

// S3 event name prefixes, followed by the operation (Put, Copy, ...).
const (
	prefixCreated = "ObjectCreated:"
	prefixRemoved = "ObjectRemoved:"
)

// API is the part of *sqs.Client the consumer uses, so a test or another
// client can stand in for it.
type API interface {
	ReceiveMessage(ctx context.Context, in *awssqs.ReceiveMessageInput, opts ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *awssqs.DeleteMessageInput, opts ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error)
}

// Options configure a Consumer.
type Options struct {
	// QueueURL is the queue to read. Required.
	QueueURL string
	// Buckets are the buckets whose events are handled. Required.
	Buckets notify.Buckets
	// Handler receives each event. Required.
	Handler notify.Handler
	// OnRejected, when set, receives each event from a bucket not in
	// Buckets; it is acknowledged and not handled.
	OnRejected func(notify.Event)
	// OnError, when set, receives every failure that does not stop Run:
	// a message that is not an S3 event (wrapping notify.ErrMalformed), a
	// Handler error, a failed receive or delete.
	OnError func(error)
	// KeepMalformed leaves a message that is not an S3 event in the
	// queue, for its dead-letter queue to collect after maxReceiveCount
	// deliveries. By default it is reported to OnError and deleted, so it
	// is not received again forever on a queue without one.
	KeepMalformed bool
	// PassOther hands the Handler every S3 event of another type (an
	// "ObjectRestore:Completed", a "LifecycleTransition") as
	// notify.KindOther, its type in Event.Name, instead of deleting it
	// unhandled. Off by default.
	PassOther bool
	// WaitSeconds is the long-poll wait; 0 means DefaultWaitSeconds.
	WaitSeconds int32
	// MaxMessages is the messages per receive; 0 means DefaultMaxMessages.
	MaxMessages int32
}

// Consumer reads S3 events from one queue.
type Consumer struct {
	api  API
	opts Options
}

// New returns a Consumer reading through api (an *sqs.Client). It
// refuses incomplete Options.
func New(api API, opts Options) (*Consumer, error) {
	switch {
	case api == nil || opts.QueueURL == "":
		return nil, errors.New("sqs: a client and a queue URL are required")
	case opts.Handler == nil:
		return nil, notify.ErrNoHandler
	}
	if err := opts.Buckets.Validate(); err != nil {
		return nil, err
	}
	if opts.WaitSeconds <= 0 {
		opts.WaitSeconds = DefaultWaitSeconds
	}
	if opts.MaxMessages <= 0 {
		opts.MaxMessages = DefaultMaxMessages
	}
	if opts.OnError == nil {
		opts.OnError = func(error) {}
	}
	return &Consumer{api: api, opts: opts}, nil
}

// Run polls until ctx ends, then returns nil. A failed receive is passed
// to OnError and retried after a pause, so a network outage does not end
// the consumer.
func (c *Consumer) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		if _, err := c.Poll(ctx); err != nil && ctx.Err() == nil {
			c.opts.OnError(err)
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
	return nil
}

// Poll receives one batch, handles it and returns how many messages it
// deleted (fully handled, or malformed and not kept). Run calls it in a loop; call it yourself to
// drive the consumer from your own scheduler.
func (c *Consumer) Poll(ctx context.Context) (int, error) {
	out, err := c.api.ReceiveMessage(ctx, &awssqs.ReceiveMessageInput{
		QueueUrl: aws.String(c.opts.QueueURL), WaitTimeSeconds: c.opts.WaitSeconds, MaxNumberOfMessages: c.opts.MaxMessages,
	})
	if err != nil {
		return 0, fmt.Errorf("sqs: receive: %w", err)
	}
	done := 0
	for _, m := range out.Messages {
		if err := c.handle(ctx, aws.ToString(m.Body)); err != nil {
			c.opts.OnError(fmt.Errorf("sqs: message %s: %w", aws.ToString(m.MessageId), err))
			if !errors.Is(err, notify.ErrMalformed) || c.opts.KeepMalformed {
				continue // left for redelivery
			}
		}
		if _, err := c.api.DeleteMessage(ctx, &awssqs.DeleteMessageInput{QueueUrl: aws.String(c.opts.QueueURL), ReceiptHandle: m.ReceiptHandle}); err != nil {
			c.opts.OnError(fmt.Errorf("sqs: delete message %s: %w", aws.ToString(m.MessageId), err))
			continue
		}
		done++
	}
	return done, nil
}

// snsEnvelope is an SNS notification wrapping the S3 event.
type snsEnvelope struct {
	Type    string `json:"Type"`
	Message string `json:"Message"`
}

// s3Message is an S3 event notification, or S3's test event.
type s3Message struct {
	Event   string     `json:"Event"`
	Records []s3Record `json:"Records"`
}

type s3Record struct {
	EventName string    `json:"eventName"`
	EventTime time.Time `json:"eventTime"`
	S3        struct {
		Bucket struct {
			Name string `json:"name"`
		} `json:"bucket"`
		Object struct {
			Key       string `json:"key"`
			Size      int64  `json:"size"`
			ETag      string `json:"eTag"`
			Sequencer string `json:"sequencer"`
			VersionID string `json:"versionId"`
		} `json:"object"`
	} `json:"s3"`
}

// handle passes every event of one message body to the Handler.
func (c *Consumer) handle(ctx context.Context, body string) error {
	var env snsEnvelope
	if json.Unmarshal([]byte(body), &env) == nil && env.Type == "Notification" {
		body = env.Message
	}
	var msg s3Message
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		return fmt.Errorf("%w: not an S3 event: %v", notify.ErrMalformed, err)
	}
	if msg.Event == testEvent {
		return nil
	}
	if len(msg.Records) == 0 {
		return fmt.Errorf("%w: not an S3 event: no records", notify.ErrMalformed)
	}
	for _, r := range msg.Records {
		e, ok, err := decode(r, c.opts.PassOther)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if !c.opts.Buckets.Allows(e.Bucket) {
			if c.opts.OnRejected != nil {
				c.opts.OnRejected(e)
			}
			continue
		}
		if err := c.opts.Handler(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// decode turns one record into an Event; records of other event types
// are skipped, or with passOther become KindOther.
func decode(r s3Record, passOther bool) (notify.Event, bool, error) {
	var kind notify.Kind
	switch {
	case strings.HasPrefix(r.EventName, prefixCreated):
		kind = notify.KindCreated
	case strings.HasPrefix(r.EventName, prefixRemoved):
		kind = notify.KindDeleted
	case passOther && r.EventName != "":
		kind = notify.KindOther
	default:
		return notify.Event{}, false, nil
	}
	// S3 sends keys URL-encoded, spaces as "+".
	key, err := url.QueryUnescape(r.S3.Object.Key)
	if err != nil {
		return notify.Event{}, false, fmt.Errorf("%w: bad object key %q: %v", notify.ErrMalformed, r.S3.Object.Key, err)
	}
	return notify.Event{
		Provider: notify.ProviderS3, ID: notify.EventID("", r.S3.Bucket.Name, key, r.S3.Object.Sequencer), Kind: kind,
		Bucket: r.S3.Bucket.Name, Key: key, Size: r.S3.Object.Size, ETag: r.S3.Object.ETag, Time: r.EventTime,
		Name: r.EventName, VersionID: r.S3.Object.VersionID,
	}, true, nil
}
