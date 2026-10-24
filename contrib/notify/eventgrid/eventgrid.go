// Package eventgrid receives Azure Blob Storage events delivered by Event
// Grid to a webhook (the Event Grid event schema) and passes each
// BlobCreated and BlobDeleted event to a notify.Handler. It answers Event
// Grid's subscription validation handshake itself, and refuses every
// request its Authorize function does not accept, the handshake included.
//
// Event Grid cannot sign a webhook delivery, so authenticity comes from a
// secret only the subscription knows: put it in the endpoint URL's query
// (QuerySecret, sent with the handshake and every delivery) or in a
// delivery header (HeaderSecret), or verify a Microsoft Entra token with
// a function of your own. It imports only the standard library.
package eventgrid

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ubgo/filesystem/contrib/notify"
)

// Event Grid event types this package acts on.
const (
	TypeValidation  = "Microsoft.EventGrid.SubscriptionValidationEvent"
	TypeBlobCreated = "Microsoft.Storage.BlobCreated"
	TypeBlobDeleted = "Microsoft.Storage.BlobDeleted"
)

// DefaultMaxBodyBytes caps a delivery's body: Event Grid delivers at most
// 1 MB per batch.
const DefaultMaxBodyBytes = 1 << 20

// accountSegment precedes the storage account's name in a blob event's
// topic.
const accountSegment = "/providers/Microsoft.Storage/storageAccounts/"

// blobSubject prefixes the subject of a blob event, followed by
// "<container>/blobs/<blob name>".
const blobSubject = "/blobServices/default/containers/"

// ErrUnauthorized is what an Authorize function returns to refuse a
// request; any non-nil error refuses it with 401.
var ErrUnauthorized = errors.New("eventgrid: unauthorized")

// Options configure Handler.
type Options struct {
	// Authorize accepts or refuses each request before its body is read.
	// Required: an unauthenticated webhook would let anyone forge events.
	Authorize func(*http.Request) error
	// Buckets are the containers whose events are handled, each written
	// "<storage account>/<container>": container names are unique only
	// inside an account, so a name alone would accept another account's
	// container of the same name. Required.
	Buckets notify.Buckets
	// Handler receives each blob event. Required. An error answers the
	// delivery with 500, so Event Grid retries the whole batch later:
	// handlers must be idempotent.
	Handler notify.Handler
	// OnRejected, when set, receives each event from a container not in
	// Buckets (or whose topic names no storage account); it is
	// acknowledged and not handled.
	OnRejected func(notify.Event)
	// MaxBodyBytes caps a delivery; 0 means DefaultMaxBodyBytes.
	MaxBodyBytes int64
	// CloudEvents reads deliveries in the CloudEvents 1.0 schema (an
	// event subscription created with that schema) instead of the Event
	// Grid schema: a single event or a batch, and the abuse-protection
	// handshake (an OPTIONS request with WebHook-Request-Origin) in
	// place of the validation event. The handshake is answered only
	// after Authorize accepts it. Off by default.
	CloudEvents bool
	// PassOther hands the Handler every other event of a storage
	// account's topic (a tier change, a blob renamed) as
	// notify.KindOther, its type in Event.Name, instead of acknowledging
	// it unhandled. Only events whose subject names a blob pass. Off by
	// default.
	PassOther bool
}

// cloudEvent is one event of the CloudEvents 1.0 JSON format.
type cloudEvent struct {
	ID      string          `json:"id"`
	Source  string          `json:"source"`
	Type    string          `json:"type"`
	Subject string          `json:"subject"`
	Time    time.Time       `json:"time"`
	Data    json.RawMessage `json:"data"`
}

// gridEvent maps a CloudEvent onto the Event Grid schema's fields: the
// source is the topic the storage account is read from.
func (c cloudEvent) gridEvent() gridEvent {
	return gridEvent{ID: c.ID, Topic: c.Source, EventType: c.Type, Subject: c.Subject, EventTime: c.Time, Data: c.Data}
}

// CloudEvents abuse-protection handshake headers.
const (
	headerRequestOrigin = "WebHook-Request-Origin"
	headerAllowedOrigin = "WebHook-Allowed-Origin"
)

// gridEvent is one event of the Event Grid schema.
type gridEvent struct {
	ID        string          `json:"id"`
	Topic     string          `json:"topic"`
	EventType string          `json:"eventType"`
	Subject   string          `json:"subject"`
	EventTime time.Time       `json:"eventTime"`
	Data      json.RawMessage `json:"data"`
}

// blobData is the data of a blob event.
type blobData struct {
	ContentLength int64  `json:"contentLength"`
	ETag          string `json:"eTag"`
}

// validationData is the data of the validation event.
type validationData struct {
	ValidationCode string `json:"validationCode"`
}

// Handler returns the webhook. It refuses incomplete Options.
func Handler(opts Options) (http.Handler, error) {
	switch {
	case opts.Authorize == nil:
		return nil, errors.New("eventgrid: Authorize is required (QuerySecret, HeaderSecret or your own)")
	case opts.Handler == nil:
		return nil, notify.ErrNoHandler
	}
	if err := opts.Buckets.Validate(); err != nil {
		return nil, err
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serve(w, r, opts) }), nil
}

func serve(w http.ResponseWriter, r *http.Request, opts Options) {
	if opts.CloudEvents && r.Method == http.MethodOptions {
		if opts.Authorize(r) != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		origin := r.Header.Get(headerRequestOrigin)
		if origin == "" {
			http.Error(w, "missing "+headerRequestOrigin, http.StatusBadRequest)
			return
		}
		w.Header().Set(headerAllowedOrigin, origin)
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if opts.Authorize(r) != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, opts.MaxBodyBytes))
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	var events []gridEvent
	if opts.CloudEvents {
		if events, err = cloudEvents(body); err != nil {
			http.Error(w, "not a CloudEvents event or batch", http.StatusBadRequest)
			return
		}
	} else if err := json.Unmarshal(body, &events); err != nil {
		http.Error(w, "not an Event Grid event array", http.StatusBadRequest)
		return
	}
	for _, ge := range events {
		// CloudEvents subscriptions validate with the OPTIONS handshake;
		// a validation event in that schema is not trusted.
		if opts.CloudEvents || ge.EventType != TypeValidation {
			continue
		}
		var v validationData
		if err := json.Unmarshal(ge.Data, &v); err != nil || v.ValidationCode == "" {
			http.Error(w, "bad validation event", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"validationResponse": v.ValidationCode})
		return
	}
	for _, ge := range events {
		e, ok := decode(ge, opts.PassOther)
		if !ok {
			continue
		}
		if e.Account == "" || !opts.Buckets.Allows(e.Account+"/"+e.Bucket) {
			if opts.OnRejected != nil {
				opts.OnRejected(e)
			}
			continue
		}
		if err := opts.Handler(r.Context(), e); err != nil {
			http.Error(w, "handler failed", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// cloudEvents parses a CloudEvents delivery: a batch (a JSON array) or a
// single event (an object).
func cloudEvents(body []byte) ([]gridEvent, error) {
	var batch []cloudEvent
	if err := json.Unmarshal(body, &batch); err != nil {
		var one cloudEvent
		if err := json.Unmarshal(body, &one); err != nil {
			return nil, err
		}
		batch = []cloudEvent{one}
	}
	out := make([]gridEvent, 0, len(batch))
	for _, c := range batch {
		out = append(out, c.gridEvent())
	}
	return out, nil
}

// decode turns a blob event into a notify.Event; other events (unless
// passOther, as KindOther) and malformed subjects are skipped.
func decode(ge gridEvent, passOther bool) (notify.Event, bool) {
	var kind notify.Kind
	switch ge.EventType {
	case TypeBlobCreated:
		kind = notify.KindCreated
	case TypeBlobDeleted:
		kind = notify.KindDeleted
	case TypeValidation:
		return notify.Event{}, false
	default:
		if !passOther || ge.EventType == "" {
			return notify.Event{}, false
		}
		kind = notify.KindOther
	}
	rest, ok := strings.CutPrefix(ge.Subject, blobSubject)
	if !ok {
		return notify.Event{}, false
	}
	container, blob, ok := strings.Cut(rest, "/blobs/")
	if !ok || container == "" || blob == "" {
		return notify.Event{}, false
	}
	var d blobData
	_ = json.Unmarshal(ge.Data, &d) // size and ETag are informative only
	account := accountOf(ge.Topic)
	return notify.Event{
		Provider: notify.ProviderAzure, ID: notify.EventID(account, container, blob, ge.ID), Kind: kind,
		Account: account, Bucket: container, Key: blob, Size: d.ContentLength, ETag: d.ETag, Time: ge.EventTime,
		Name: ge.EventType,
	}, true
}

// accountOf returns the storage account a blob event's topic names
// (".../providers/Microsoft.Storage/storageAccounts/<account>"), or "".
func accountOf(topic string) string {
	_, account, ok := strings.Cut(topic, accountSegment)
	if !ok || account == "" || strings.Contains(account, "/") {
		return ""
	}
	return account
}

// QuerySecret accepts a request whose query parameter param equals
// secret, compared in constant time: subscribe Event Grid to
// https://host/path?param=secret.
func QuerySecret(param, secret string) func(*http.Request) error {
	return func(r *http.Request) error { return match(r.URL.Query().Get(param), secret) }
}

// HeaderSecret accepts a request whose header name equals secret,
// compared in constant time: set it as a delivery attribute (marked
// secret) of the subscription.
func HeaderSecret(name, secret string) func(*http.Request) error {
	return func(r *http.Request) error { return match(r.Header.Get(name), secret) }
}

// match compares got with an always non-empty want in constant time.
func match(got, want string) error {
	if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return ErrUnauthorized
	}
	return nil
}
