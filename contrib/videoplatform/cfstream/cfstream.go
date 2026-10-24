// Package cfstream is the videoplatform.Platform for Cloudflare Stream
// (https://developers.cloudflare.com/stream/), over its REST API with
// net/http: videos copied from a URL, read, deleted, and webhooks
// verified with the Webhook-Signature header.
//
// What needs a real Cloudflare account, and so is not covered by this
// module's tests (which run against an in-process fake of the documented
// API): the API token, Stream fetching the source URL, and the delivery
// of webhooks to a public URL.
package cfstream

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
const Name = "cfstream"

// DefaultBaseURL is Cloudflare's API.
const DefaultBaseURL = "https://api.cloudflare.com/client/v4"

// MetaPassthrough is the key of a video's meta that carries
// videoplatform.Source.Passthrough.
const MetaPassthrough = "passthrough"

// signatureHeader carries the webhook signature: "time=<unix>,sig1=<hex>".
const signatureHeader = "Webhook-Signature"

// httpTimeout bounds a call when Config.HTTP is nil.
const httpTimeout = 30 * time.Second

// Config configures a Client.
type Config struct {
	// AccountID and APIToken (with Stream edit permission) come from the
	// Cloudflare dashboard; the token is a videoplatform.Secret so a
	// logged Config or Client does not show it.
	AccountID string
	APIToken  videoplatform.Secret
	// WebhookSecret is the secret returned when the webhook URL was set
	// (PUT /accounts/{id}/stream/webhook); without it every webhook is
	// refused.
	WebhookSecret videoplatform.Secret
	// RequireSignedURLs makes videos play only with a signed token.
	RequireSignedURLs bool
	// BaseURL of the API; "" means DefaultBaseURL (a fake in tests).
	BaseURL string
	// HTTP is the client; nil means one with a 30-second timeout.
	HTTP *http.Client
	// Now is the clock webhook timestamps are checked against; nil means
	// time.Now.
	Now func() time.Time
	// CreateOptions are extra fields of the copy request, as raw JSON by
	// Stream's own field names: {"maxDurationSeconds": 600},
	// {"allowedOrigins": ["app.example.com"]}, {"watermark": {"uid":
	// "…"}}, {"thumbnailTimestampPct": 0.5}. The adapter's own fields
	// (url, meta, requireSignedURLs) cannot be set here: New refuses them
	// with CodeInvalidArgument, as it does a value that is not valid
	// JSON. Nil sends today's request.
	CreateOptions map[string]json.RawMessage
}

// reservedCreateFields are the copy-request fields the adapter sets.
var reservedCreateFields = []string{"url", "meta", "requireSignedURLs"}

// Client is a videoplatform.Platform for Cloudflare Stream.
type Client struct {
	cfg Config
}

var _ videoplatform.Platform = (*Client)(nil)

// New returns a Client. A missing account or token is
// CodeInvalidArgument.
func New(cfg Config) (*Client, error) {
	if cfg.AccountID == "" || cfg.APIToken.IsZero() {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "cfstream: AccountID and APIToken are required")
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

// video is Stream's video object, the part the adapter reads.
type video struct {
	UID    string `json:"uid"`
	Status struct {
		State         string `json:"state"`
		ErrReasonCode string `json:"errReasonCode"`
		ErrReasonText string `json:"errReasonText"`
	} `json:"status"`
	Playback struct {
		HLS string `json:"hls"`
	} `json:"playback"`
	Thumbnail string `json:"thumbnail"`
	// Modified is when the video last changed (RFC 3339), which dates
	// the state a webhook reports.
	Modified string            `json:"modified"`
	Duration float64           `json:"duration"`
	Meta     map[string]string `json:"meta"`
}

// envelope is Stream's response wrapper.
type envelope struct {
	Success bool  `json:"success"`
	Result  video `json:"result"`
}

// copyRequest is the body of POST /accounts/{id}/stream/copy.
type copyRequest struct {
	URL               string            `json:"url"`
	Meta              map[string]string `json:"meta,omitempty"`
	RequireSignedURLs bool              `json:"requireSignedURLs"`
}

// Ingest implements videoplatform.Platform: POST
// /accounts/{id}/stream/copy.
func (c *Client) Ingest(ctx context.Context, src videoplatform.Source) (videoplatform.Asset, error) {
	req := copyRequest{URL: src.URL, RequireSignedURLs: c.cfg.RequireSignedURLs}
	if src.Passthrough != "" {
		req.Meta = map[string]string{MetaPassthrough: src.Passthrough}
	}
	// A file's own options are checked like the Config's, here rather
	// than at New, since they arrive with each call.
	if err := rest.CheckExtra(Name, src.Options, reservedCreateFields...); err != nil {
		return videoplatform.Asset{}, err
	}
	in, err := rest.Merge(req, rest.Overlay(c.cfg.CreateOptions, src.Options))
	if err != nil {
		return videoplatform.Asset{}, err
	}
	var out envelope
	if err := rest.Call(ctx, c.cfg.HTTP, Name, http.MethodPost, c.streamURL("copy"), c.authorize, &in, &out); err != nil {
		return videoplatform.Asset{}, err
	}
	return c.result(out)
}

// Asset implements videoplatform.Platform: GET
// /accounts/{id}/stream/{uid}.
func (c *Client) Asset(ctx context.Context, id string) (videoplatform.Asset, error) {
	var out envelope
	if err := rest.Call[struct{}](ctx, c.cfg.HTTP, Name, http.MethodGet, c.streamURL(id), c.authorize, nil, &out); err != nil {
		return videoplatform.Asset{}, err
	}
	return c.result(out)
}

// Delete implements videoplatform.Platform: DELETE
// /accounts/{id}/stream/{uid}; a video already gone is done.
func (c *Client) Delete(ctx context.Context, id string) error {
	err := rest.Call[struct{}, struct{}](ctx, c.cfg.HTTP, Name, http.MethodDelete, c.streamURL(id), c.authorize, nil, nil)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return nil
	}
	return err
}

// result unwraps a response, refusing one that reports failure.
func (c *Client) result(e envelope) (videoplatform.Asset, error) {
	if !e.Success {
		return videoplatform.Asset{}, filesystem.Errorf(filesystem.CodeUnavailable, "cfstream: the API reported failure")
	}
	return convert(e.Result), nil
}

// streamURL is an API URL under the account's stream path, the last
// segment escaped so it cannot change the path.
func (c *Client) streamURL(segment string) string {
	return c.cfg.BaseURL + "/accounts/" + url.PathEscape(c.cfg.AccountID) + "/stream/" + url.PathEscape(segment)
}

func (c *Client) authorize(r *http.Request) {
	r.Header.Set("Authorization", "Bearer "+string(c.cfg.APIToken.Reveal()))
}

// Webhook implements videoplatform.Platform: the Webhook-Signature header
// is "time=<unix seconds>,sig1=<hex HMAC-SHA256 of time.body>", and the
// body is the video. Every Stream webhook is about a video's state.
func (c *Client) Webhook(r *http.Request, body []byte) (videoplatform.Asset, error) {
	var ts, sig string
	for part := range strings.SplitSeq(r.Header.Get(signatureHeader), ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "time":
			ts = v
		case "sig1":
			sig = v
		}
	}
	if err := videoplatform.VerifySignature(c.cfg.WebhookSecret.Reveal(), ts, body, sig, c.cfg.Now()); err != nil {
		return videoplatform.Asset{}, err
	}
	var v video
	if err := json.Unmarshal(body, &v); err != nil || v.UID == "" {
		return videoplatform.Asset{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "cfstream: unreadable webhook")
	}
	return convert(v), nil
}

// convert maps Stream's video to the platform-neutral asset. Stream
// reports an unknown duration as -1, which DurationOf makes 0.
func convert(v video) videoplatform.Asset {
	out := videoplatform.Asset{ID: v.UID, HLS: v.Playback.HLS, Thumbnail: v.Thumbnail, Passthrough: v.Meta[MetaPassthrough]}
	out.Duration = videoplatform.DurationOf(v.Duration)
	// An unparseable time leaves the state undated rather than refused:
	// the plugin then orders it by status alone.
	out.At, _ = time.Parse(time.RFC3339Nano, v.Modified)
	switch v.Status.State {
	case "ready":
		out.Status = videoplatform.StatusReady
	case "error":
		out.Status = videoplatform.StatusErrored
		out.Detail = strings.TrimPrefix(v.Status.ErrReasonCode+": "+v.Status.ErrReasonText, ": ")
	default:
		out.Status = videoplatform.StatusPreparing
	}
	return out
}
