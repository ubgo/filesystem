// Package pubsub consumes Google Cloud Storage object notifications from
// a Pub/Sub subscription and passes each finalize (created) and delete
// event to a notify.Handler.
//
// It is pull-only, by design: the subscription is read with the
// application's Google credentials (IAM), so no endpoint is exposed and
// no push token has to be verified. Because any publisher allowed on the
// topic could still publish a message naming any bucket, the consumer
// handles only events from the buckets it is given.
//
// A message is acknowledged only after the Handler accepted it; a Handler
// error nacks it, and Pub/Sub delivers it again (configure a dead-letter
// topic for messages that never succeed). A message that is not a GCS
// notification is reported and acknowledged, or with KeepMalformed
// nacked for the dead-letter topic. Handlers must be idempotent:
// Pub/Sub delivers at least once.
package pubsub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"github.com/ubgo/filesystem/contrib/notify"
)

// GCS notification attributes and event types.
const (
	attrEventType   = "eventType"
	attrBucket      = "bucketId"
	attrObject      = "objectId"
	attrEventTime   = "eventTime"
	attrOverwritten = "overwrittenByGeneration"
	attrGeneration  = "objectGeneration"

	eventFinalize = "OBJECT_FINALIZE"
	eventDelete   = "OBJECT_DELETE"
)

// Receiver is the part of *pubsub.Subscriber the consumer uses, so a
// test or a wrapper can stand in for it.
type Receiver interface {
	Receive(ctx context.Context, f func(context.Context, *pubsub.Message)) error
}

// Options configure a Consumer.
type Options struct {
	// Buckets are the buckets whose events are handled. Required.
	Buckets notify.Buckets
	// Handler receives each event. Required.
	Handler notify.Handler
	// OnRejected, when set, receives each event from a bucket not in
	// Buckets; it is acknowledged and not handled.
	OnRejected func(notify.Event)
	// OnError, when set, receives every message that is not a GCS
	// notification (wrapping notify.ErrMalformed) and every Handler
	// failure.
	OnError func(error)
	// KeepMalformed nacks a message that is not a GCS notification, for
	// the subscription's dead-letter topic to collect. By default it is
	// reported to OnError and acknowledged, so it is not delivered again
	// forever on a subscription without one.
	KeepMalformed bool
	// PassOther hands the Handler every GCS notification of another
	// type (OBJECT_METADATA_UPDATE, OBJECT_ARCHIVE) as notify.KindOther,
	// its type in Event.Name, instead of acknowledging it unhandled. The
	// delete GCS sends for an object a newer write replaced stays
	// skipped: the finalize of that write already reports it. Off by
	// default.
	PassOther bool
}

// Consumer reads GCS notifications from one subscription.
type Consumer struct {
	sub  Receiver
	opts Options
}

// New returns a Consumer reading through sub (a *pubsub.Subscriber). It
// refuses incomplete Options.
func New(sub Receiver, opts Options) (*Consumer, error) {
	switch {
	case sub == nil:
		return nil, errors.New("pubsub: a subscriber is required")
	case opts.Handler == nil:
		return nil, notify.ErrNoHandler
	}
	if err := opts.Buckets.Validate(); err != nil {
		return nil, err
	}
	if opts.OnError == nil {
		opts.OnError = func(error) {}
	}
	return &Consumer{sub: sub, opts: opts}, nil
}

// Run receives until ctx ends, then returns nil, or returns the error
// that stopped the subscription (a missing subscription, denied
// credentials).
func (c *Consumer) Run(ctx context.Context) error {
	err := c.sub.Receive(ctx, c.receive)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// receive handles one message and acknowledges or nacks it.
func (c *Consumer) receive(ctx context.Context, m *pubsub.Message) {
	e, ok, err := decode(m.ID, m.Attributes, m.Data, c.opts.PassOther)
	switch {
	case err != nil:
		c.opts.OnError(fmt.Errorf("pubsub: message %s: %w", m.ID, err))
		if c.opts.KeepMalformed {
			m.Nack()
		} else {
			m.Ack()
		}
		return
	case !ok:
		m.Ack()
		return
	case !c.opts.Buckets.Allows(e.Bucket):
		if c.opts.OnRejected != nil {
			c.opts.OnRejected(e)
		}
		m.Ack()
		return
	}
	if err := c.opts.Handler(ctx, e); err != nil {
		c.opts.OnError(fmt.Errorf("pubsub: message %s: %w", m.ID, err))
		m.Nack()
		return
	}
	m.Ack()
}

// objectResource is the part of the JSON_API_V1 payload read here.
type objectResource struct {
	Size string `json:"size"`
	ETag string `json:"etag"`
}

// decode turns a GCS notification into an Event. Other event types
// (KindOther with passOther), and the delete GCS sends for an object
// replaced by a newer write, are skipped (ok false).
func decode(id string, attrs map[string]string, data []byte, passOther bool) (notify.Event, bool, error) {
	var kind notify.Kind
	switch attrs[attrEventType] {
	case eventFinalize:
		kind = notify.KindCreated
	case eventDelete:
		if attrs[attrOverwritten] != "" {
			return notify.Event{}, false, nil
		}
		kind = notify.KindDeleted
	case "":
		return notify.Event{}, false, fmt.Errorf("%w: not a GCS notification: no eventType", notify.ErrMalformed)
	default:
		if !passOther {
			return notify.Event{}, false, nil
		}
		kind = notify.KindOther
	}
	e := notify.Event{Provider: notify.ProviderGCS, Kind: kind, Bucket: attrs[attrBucket], Key: attrs[attrObject], Name: attrs[attrEventType], VersionID: attrs[attrGeneration]}
	e.ID = notify.EventID("", e.Bucket, e.Key, id)
	if e.Bucket == "" || e.Key == "" {
		return notify.Event{}, false, fmt.Errorf("%w: not a GCS notification: no bucket or object", notify.ErrMalformed)
	}
	e.Time, _ = time.Parse(time.RFC3339Nano, attrs[attrEventTime])
	var obj objectResource
	if len(data) > 0 && json.Unmarshal(data, &obj) == nil {
		e.ETag = obj.ETag
		e.Size, _ = strconv.ParseInt(obj.Size, 10, 64)
	}
	return e, true, nil
}
