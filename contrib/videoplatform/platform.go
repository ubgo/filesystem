// Package videoplatform hands videos to a hosted video platform (Mux,
// Cloudflare Stream, or any other behind the Platform interface), which
// transcodes them into adaptive streams (HLS) with thumbnails, and keeps
// each file's asset in the records layer: the platform's asset ID, its
// state, and the playback and thumbnail URLs a player needs. Building an
// HLS ladder in-house is a video team's work; delegating it is what most
// products do.
//
// The flow: Plugin.Publish asks the platform to fetch the file's object
// through a short-lived signed URL and records the asset as preparing
// (the file's records status becomes processing). The platform reports
// progress to Plugin.Webhook, which verifies the request's signature,
// records the asset's new state and sets the file's status to ready or
// failed, emitting EventReady or EventFailed. Plugin.Sync asks the
// platform directly, for a webhook that was missed. Purging or replacing
// the file deletes its asset on the platform after the change commits.
//
// The adapters live in sub-packages (mux, cfstream) and speak each
// platform's documented REST API with net/http; this module imports
// nothing but the standard library and the library's own packages.
package videoplatform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"
)

// Status is where an asset stands on its platform.
type Status string

// The closed set of statuses.
const (
	// StatusPreparing: the platform is fetching or transcoding the video.
	StatusPreparing Status = "preparing"
	// StatusReady: the video can be played.
	StatusReady Status = "ready"
	// StatusErrored: the platform gave up; Asset.Detail says why.
	StatusErrored Status = "errored"
	// StatusDeleted: the asset was deleted on the platform. It is final:
	// the plugin never moves an asset out of it.
	StatusDeleted Status = "deleted"
)

// StatusValues lists every Status, in the order a state may only advance
// through when the platform does not date it: preparing, ready, then the
// terminal errored and deleted.
var StatusValues = []Status{StatusPreparing, StatusReady, StatusErrored, StatusDeleted}

// rank is s's position in StatusValues, -1 when unknown.
func (s Status) rank() int { return slices.Index(StatusValues, s) }

// terminal reports whether s is final on its platform (errored, deleted).
func (s Status) terminal() bool { return s == StatusErrored || s == StatusDeleted }

// Asset is a video on a platform, as the platform reports it.
type Asset struct {
	// ID is the platform's asset ID.
	ID     string
	Status Status
	// PlaybackID is the platform's playback ID where it has one apart from
	// the asset ID (Mux); "" otherwise.
	PlaybackID string
	// HLS is the adaptive stream's URL, set once the platform knows it.
	HLS string
	// Thumbnail is a poster image's URL.
	Thumbnail string
	// Duration of the video, 0 until the platform has measured it.
	Duration time.Duration
	// Detail is the platform's reason for StatusErrored, for the app and
	// its users; never a credential.
	Detail string
	// Passthrough is the reference the asset was created with (Source).
	Passthrough string
	// At is when the platform reached this state, as it dates it (a
	// webhook event's creation time); the zero time when it gives none.
	// The plugin applies a dated state only when it is later than the
	// recorded one, so a replayed or late webhook changes nothing.
	At time.Time
}

// Source is what Platform.Ingest fetches.
type Source struct {
	// URL the platform downloads the video from: a signed URL that
	// expires once the platform has had time to fetch it.
	URL string
	// Passthrough is an opaque reference the platform stores with the
	// asset and returns with it ("<scope>/<file ID>").
	Passthrough string
	// Options are extra fields of this one create request, as raw JSON by
	// the platform's own field names, merged over the adapter's
	// Config.CreateOptions (a field set in both takes this value): a
	// per-plan {"max_resolution_tier": "1080p"}, a per-creator watermark,
	// per-tenant {"allowedOrigins": [...]}. The Plugin fills it through
	// WithCreateOptions. The adapter's reserved fields (the source URL,
	// the playback policy, the passthrough) cannot be set here either:
	// Ingest refuses them, and a value that is not valid JSON, with
	// CodeInvalidArgument before calling the platform. Nil sends the
	// Config's request.
	Options map[string]json.RawMessage
}

// Platform is a hosted video service. Implementations are safe for
// concurrent use. Errors use the library's codes: CodeNotFound for an
// asset the platform does not know, CodeUnavailable for a platform that
// is down or rate limiting, CodePermission for credentials it refuses.
type Platform interface {
	// Name identifies the platform in records ("mux"); it must not change
	// once assets are recorded.
	Name() string
	// Ingest creates an asset from the video at src.URL.
	Ingest(ctx context.Context, src Source) (Asset, error)
	// Asset returns the asset's current state.
	Asset(ctx context.Context, id string) (Asset, error)
	// Delete removes the asset; an asset already gone is not an error.
	Delete(ctx context.Context, id string) error
	// Webhook verifies a webhook request (its body already read, at most
	// MaxWebhookBytes) against the platform's signing secret and returns
	// the asset it reports. A bad or stale signature is CodePermission;
	// a correctly signed body it cannot read is CodeInvalidArgument; an
	// event about something other than an asset is ErrIgnoredEvent.
	Webhook(r *http.Request, body []byte) (Asset, error)
}

// ErrIgnoredEvent is returned by Platform.Webhook for an authentic event
// that is not about an asset's state (an upload, a live stream): the
// handler acknowledges it and changes nothing.
var ErrIgnoredEvent = errors.New("videoplatform: event is not about an asset")

// MaxWebhookBytes bounds a webhook body: platforms send a few KiB.
const MaxWebhookBytes = 1 << 20

// WebhookTolerance is how far a webhook's signed timestamp may be from
// now, in either direction, before it is refused as a replay.
const WebhookTolerance = 5 * time.Minute
