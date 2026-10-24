// Package notify turns a storage backend's object notifications (S3
// events through SQS, GCS events through Pub/Sub, Azure Blob events
// through Event Grid) into one Event and calls the application's Handler
// with it: the place to confirm a direct upload the moment its object
// lands, instead of waiting for the client to report it, or to react to
// objects deleted outside the application.
//
// This module is the vendor-neutral seam and imports only the standard
// library. Each consumer lives in a module of its own (notify/sqs,
// notify/pubsub, notify/eventgrid), so an application downloads only the
// SDK of the cloud it uses, and a consumer for another backend needs
// nothing but this package.
//
// Every consumer is authenticated (the queue or subscription is read with
// the cloud's own credentials, or the webhook checks a secret) and keeps
// only events from the buckets it is given, because a notification is
// an outside party's claim about an object: a Handler should still check
// the object (Disk.Stat) before trusting what the event says about it.
// Deliveries are at least once, so a Handler must be idempotent.
package notify

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"slices"
	"time"
)

// Kind is what happened to the object.
type Kind string

// The closed set of event kinds. By default consumers drop every other
// event their backend sends (metadata updates, tier changes, restores);
// with a consumer's PassOther option those arrive as KindOther, the
// provider's own event type in Event.Name.
const (
	// KindCreated: the object was written (created or overwritten).
	KindCreated Kind = "created"
	// KindDeleted: the object was deleted. On a versioned S3 bucket that
	// includes a delete marker ("ObjectRemoved:DeleteMarkerCreated" in
	// Name), after which older versions remain: check Name and VersionID
	// before treating the data as gone.
	KindDeleted Kind = "deleted"
	// KindOther: any other event of the provider (a Glacier restore
	// completing, a tier or metadata change), sent only with a consumer's
	// PassOther option. Name says which.
	KindOther Kind = "other"
)

// Kinds lists every Kind.
var Kinds = []Kind{KindCreated, KindDeleted, KindOther}

// The providers that send events, in Event.Provider.
const (
	ProviderS3    = "s3"
	ProviderGCS   = "gcs"
	ProviderAzure = "azure"
)

// Event is one object notification, the same for every backend.
type Event struct {
	// Provider is the backend that sent it (ProviderS3, ...).
	Provider string
	// ID identifies the event for deduplication: EventID of its account,
	// bucket, key and the provider's own identifier (the S3 sequencer,
	// the Pub/Sub message ID, the Event Grid event ID), none of which is
	// unique on its own across keys and buckets.
	ID   string
	Kind Kind
	// Account is the storage account that holds the bucket, for
	// providers whose bucket names are unique only inside an account
	// (Azure); empty for S3 and GCS, whose bucket names are global.
	Account string
	// Bucket is the bucket, or the Azure container.
	Bucket string
	// Key is the object's key in the bucket, decoded: the key a Disk
	// over the bucket's root uses (strip a scoped disk's prefix first).
	Key string
	// Size and ETag are what the event reports for a created object;
	// zero when it reports none.
	Size int64
	ETag string
	// Time is when the provider says it happened.
	Time time.Time
	// Name is the provider's own event type, verbatim:
	// "ObjectCreated:Put", "ObjectRemoved:DeleteMarkerCreated",
	// "OBJECT_FINALIZE", "Microsoft.Storage.BlobCreated".
	Name string
	// VersionID is the object version the event is about, when the
	// provider reports one: the S3 versionId, the GCS generation; empty
	// otherwise (and for Azure, whose blob events carry none).
	VersionID string
}

// EventID derives Event.ID from the event's account, bucket and key and
// the provider's identifier for it, so two events are equal only when
// all four are: a provider ID alone (an S3 sequencer is ordered only per
// key) repeats across keys. It is a hex SHA-256 prefix, safe as a
// database key.
func EventID(account, bucket, key, providerID string) string {
	h := sha256.New()
	for _, part := range []string{account, bucket, key, providerID} {
		// Length-prefixed, so no part can run into the next.
		h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(part))))
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil)[:eventIDBytes])
}

// eventIDBytes is the length of an EventID, in bytes: 128 bits, far from
// any collision among one application's events.
const eventIDBytes = 16

// ErrMalformed marks a message that is not a notification of the
// provider it came from: consumers report it to OnError wrapped in it
// and, unless told to keep such messages, acknowledge it so it is not
// delivered forever.
var ErrMalformed = errors.New("notify: malformed notification")

// Handler receives each event. Returning nil acknowledges it; an error
// leaves it to be delivered again (each consumer documents how), so a
// transient failure is retried by the backend rather than lost.
type Handler func(ctx context.Context, e Event) error

// Buckets are the buckets (or Azure containers) a consumer accepts
// events from. Every consumer requires at least one: events from any
// other bucket are reported as rejected and acknowledged, never handled.
type Buckets []string

// ErrNoBuckets is returned by consumers configured without Buckets.
var ErrNoBuckets = errors.New("notify: no buckets are allowed; list the buckets whose events to accept")

// ErrNoHandler is returned by consumers configured without a Handler.
var ErrNoHandler = errors.New("notify: no handler")

// Allows reports whether events from bucket are accepted.
func (b Buckets) Allows(bucket string) bool { return slices.Contains(b, bucket) }

// Validate returns ErrNoBuckets when b is empty.
func (b Buckets) Validate() error {
	if len(b) == 0 {
		return ErrNoBuckets
	}
	return nil
}
