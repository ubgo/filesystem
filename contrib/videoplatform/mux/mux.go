// Package mux is the videoplatform.Platform for Mux Video
// (https://www.mux.com/docs/api-reference/video), over its REST API with
// net/http: assets created from a URL, read, deleted, and webhooks
// verified with the Mux-Signature header.
//
// What needs a real Mux account, and so is not covered by this module's
// tests (which run against an in-process fake of the documented API): the
// credentials themselves, Mux fetching the source URL, and the delivery
// of webhooks to a public URL.
package mux

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/videoplatform"
	"github.com/ubgo/filesystem/contrib/videoplatform/internal/rest"
)

// Name is the platform's name in records.
const Name = "mux"

// Defaults.
const (
	// DefaultBaseURL is Mux's API.
	DefaultBaseURL = "https://api.mux.com"
	// StreamBaseURL and ImageBaseURL build playback and thumbnail URLs.
	StreamBaseURL = "https://stream.mux.com"
	ImageBaseURL  = "https://image.mux.com"
)

// Playback policies: who may play an asset.
const (
	// PolicyPublic plays with the bare playback URL.
	PolicyPublic = "public"
	// PolicySigned needs a token the app signs with a Mux signing key
	// (the HLS and Thumbnail URLs then need ?token=).
	PolicySigned = "signed"
)

// MaxPassthroughBytes is Mux's limit on an asset's passthrough.
const MaxPassthroughBytes = 255

// signatureHeader carries Mux's webhook signature: "t=<unix>,v1=<hex>".
const signatureHeader = "Mux-Signature"

// Config configures a Client. TokenID, TokenSecret and WebhookSecret come
// from the Mux dashboard (Settings, Access Tokens and Webhooks); the
// secrets are videoplatform.Secret so a logged Config or Client does not
// show them.
type Config struct {
	TokenID     string
	TokenSecret videoplatform.Secret
	// WebhookSecret signs the webhooks Mux sends; without it every webhook
	// is refused.
	WebhookSecret videoplatform.Secret
	// Policy is PolicyPublic or PolicySigned; "" means PolicyPublic.
	Policy string
	// BaseURL of the API; "" means DefaultBaseURL (a fake in tests).
	BaseURL string
	// HTTP is the client; nil means one with a 30-second timeout.
	HTTP *http.Client
	// Now is the clock webhook timestamps are checked against; nil means
	// time.Now.
	Now func() time.Time
	// CreateOptions are extra fields of the create-asset request, as raw
	// JSON by Mux's own field names: {"video_quality": "basic"},
	// {"mp4_support": "capped-1080p"}, {"max_resolution_tier": "2160p"}.
	// The adapter's own fields (inputs, playback_policies, passthrough)
	// cannot be set here: New refuses them with CodeInvalidArgument, as
	// it does a value that is not valid JSON. Nil sends today's request.
	CreateOptions map[string]json.RawMessage
}

// reservedCreateFields are the create-request fields the adapter sets.
var reservedCreateFields = []string{"inputs", "playback_policies", "passthrough"}

// httpTimeout bounds a call when Config.HTTP is nil.
const httpTimeout = 30 * time.Second

// Client is a videoplatform.Platform for Mux.
type Client struct {
	cfg Config
}

var _ videoplatform.Platform = (*Client)(nil)

// New returns a Client. Missing credentials or an unknown policy are
// CodeInvalidArgument.
func New(cfg Config) (*Client, error) {
	if cfg.TokenID == "" || cfg.TokenSecret.IsZero() {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "mux: TokenID and TokenSecret are required")
	}
	if cfg.Policy == "" {
		cfg.Policy = PolicyPublic
	}
	if cfg.Policy != PolicyPublic && cfg.Policy != PolicySigned {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "mux: unknown playback policy %q", cfg.Policy)
	}
	if err := rest.CheckExtra(Name, cfg.CreateOptions, reservedCreateFields...); err != nil {
		return nil, err
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: httpTimeout}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Client{cfg: cfg}, nil
}

// Name implements videoplatform.Platform.
func (c *Client) Name() string { return Name }

// asset is Mux's asset object, the part the adapter reads.
type asset struct {
	ID          string  `json:"id"`
	Status      string  `json:"status"`
	Duration    float64 `json:"duration"`
	Passthrough string  `json:"passthrough"`
	PlaybackIDs []struct {
		ID     string `json:"id"`
		Policy string `json:"policy"`
	} `json:"playback_ids"`
	Errors struct {
		Type     string   `json:"type"`
		Messages []string `json:"messages"`
	} `json:"errors"`
}

// envelope is Mux's response and webhook wrapper. CreatedAt is a
// webhook event's creation time (RFC 3339); API responses have none.
type envelope struct {
	Type      string `json:"type"`
	CreatedAt string `json:"created_at"`
	Data      asset  `json:"data"`
}

// createRequest is the body of POST /video/v1/assets.
type createRequest struct {
	Inputs           []input  `json:"inputs"`
	PlaybackPolicies []string `json:"playback_policies"`
	Passthrough      string   `json:"passthrough,omitempty"`
}

type input struct {
	URL string `json:"url"`
}

// Ingest implements videoplatform.Platform: POST /video/v1/assets.
func (c *Client) Ingest(ctx context.Context, src videoplatform.Source) (videoplatform.Asset, error) {
	if len(src.Passthrough) > MaxPassthroughBytes {
		return videoplatform.Asset{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "mux: passthrough longer than %d bytes", MaxPassthroughBytes)
	}
	// A file's own options are checked like the Config's, here rather
	// than at New, since they arrive with each call.
	if err := rest.CheckExtra(Name, src.Options, reservedCreateFields...); err != nil {
		return videoplatform.Asset{}, err
	}
	in, err := rest.Merge(createRequest{Inputs: []input{{URL: src.URL}}, PlaybackPolicies: []string{c.cfg.Policy}, Passthrough: src.Passthrough}, rest.Overlay(c.cfg.CreateOptions, src.Options))
	if err != nil {
		return videoplatform.Asset{}, err
	}
	var out envelope
	if err := rest.Call(ctx, c.cfg.HTTP, Name, http.MethodPost, c.cfg.BaseURL+"/video/v1/assets", c.authorize, &in, &out); err != nil {
		return videoplatform.Asset{}, err
	}
	return convert(out.Data), nil
}

// Asset implements videoplatform.Platform: GET /video/v1/assets/{id}.
func (c *Client) Asset(ctx context.Context, id string) (videoplatform.Asset, error) {
	var out envelope
	if err := rest.Call[struct{}](ctx, c.cfg.HTTP, Name, http.MethodGet, c.assetURL(id), c.authorize, nil, &out); err != nil {
		return videoplatform.Asset{}, err
	}
	return convert(out.Data), nil
}

// Delete implements videoplatform.Platform: DELETE /video/v1/assets/{id};
// an asset already gone is done.
func (c *Client) Delete(ctx context.Context, id string) error {
	err := rest.Call[struct{}, struct{}](ctx, c.cfg.HTTP, Name, http.MethodDelete, c.assetURL(id), c.authorize, nil, nil)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return nil
	}
	return err
}

// assetURL is an asset's API URL, the ID escaped so it cannot change the
// path.
func (c *Client) assetURL(id string) string {
	return c.cfg.BaseURL + "/video/v1/assets/" + url.PathEscape(id)
}

func (c *Client) authorize(r *http.Request) {
	r.SetBasicAuth(c.cfg.TokenID, string(c.cfg.TokenSecret.Reveal()))
}

// Asset event types; any other is ignored.
const (
	eventCreated = "video.asset.created"
	eventReady   = "video.asset.ready"
	eventErrored = "video.asset.errored"
	eventUpdated = "video.asset.updated"
	eventDeleted = "video.asset.deleted"
)

// Webhook implements videoplatform.Platform: the Mux-Signature header is
// "t=<unix seconds>,v1=<hex HMAC-SHA256 of t.body>".
func (c *Client) Webhook(r *http.Request, body []byte) (videoplatform.Asset, error) {
	var ts, sig string
	for part := range strings.SplitSeq(r.Header.Get(signatureHeader), ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	if err := videoplatform.VerifySignature(c.cfg.WebhookSecret.Reveal(), ts, body, sig, c.cfg.Now()); err != nil {
		return videoplatform.Asset{}, err
	}
	var e envelope
	if err := json.Unmarshal(body, &e); err != nil {
		return videoplatform.Asset{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "mux: unreadable webhook")
	}
	a := convert(e.Data)
	// An unparseable time leaves the event undated rather than refused:
	// the plugin then orders it by status alone.
	a.At, _ = time.Parse(time.RFC3339Nano, e.CreatedAt)
	switch e.Type {
	case eventCreated, eventReady, eventErrored, eventUpdated:
		return a, nil
	case eventDeleted:
		a.Status, a.Detail = videoplatform.StatusDeleted, "deleted on Mux"
		return a, nil
	}
	return videoplatform.Asset{}, videoplatform.ErrIgnoredEvent
}

// convert maps Mux's asset to the platform-neutral one.
func convert(a asset) videoplatform.Asset {
	out := videoplatform.Asset{ID: a.ID, Passthrough: a.Passthrough, Duration: videoplatform.DurationOf(a.Duration)}
	switch a.Status {
	case "ready":
		out.Status = videoplatform.StatusReady
	case "errored":
		out.Status = videoplatform.StatusErrored
		out.Detail = strings.Join(append([]string{a.Errors.Type}, a.Errors.Messages...), ": ")
	default:
		out.Status = videoplatform.StatusPreparing
	}
	if len(a.PlaybackIDs) > 0 {
		pid := a.PlaybackIDs[0].ID
		out.PlaybackID = pid
		out.HLS = StreamBaseURL + "/" + url.PathEscape(pid) + ".m3u8"
		out.Thumbnail = ImageBaseURL + "/" + url.PathEscape(pid) + "/thumbnail.jpg"
	}
	return out
}
