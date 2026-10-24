// Package google is a vision.Labeler for Google Cloud Vision's label
// detection, over its plain REST API (images:annotate with the
// LABEL_DETECTION feature), so no Google SDK is pulled in. It needs an
// API key of a Google Cloud project with the Cloud Vision API enabled;
// Google bills each image (see the guide).
//
// The key travels in the X-Goog-Api-Key header, never in the URL, so it
// is in no error message or proxy log, and Config holds it as a
// vision.Secret, so a logged Config or Client does not show it. The image
// size is checked before anything is sent, and the response read is
// bounded.
package google

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/vision"
)

// DefaultEndpoint is Cloud Vision's annotate method.
const DefaultEndpoint = "https://vision.googleapis.com/v1/images:annotate"

// Defaults used when a Config leaves a field zero.
const (
	// DefaultMaxResults is how many labels Google is asked for.
	DefaultMaxResults = 10
	// DefaultMaxImage bounds an image: Google refuses a request over
	// 10 MB, and base64 makes 7 MiB about 9.3 MiB.
	DefaultMaxImage int64 = 7 << 20
	// httpTimeout bounds a call when Config.HTTP is nil.
	httpTimeout = 30 * time.Second
)

// maxResponse bounds the response read: a label is about 100 bytes.
const maxResponse = 1 << 20

// maxErrorMessage bounds the part of Google's error message kept in an
// error.
const maxErrorMessage = 200

// apiKeyHeader carries the key.
const apiKeyHeader = "X-Goog-Api-Key"

// featureLabels is the label detection feature.
const featureLabels = "LABEL_DETECTION"

// Config configures a Client.
type Config struct {
	// APIKey is the Google Cloud API key, sent in the X-Goog-Api-Key
	// header. Required unless HTTP is set: an organisation that bans API
	// keys passes an HTTP client that authenticates itself (an OAuth2
	// transport from golang.org/x/oauth2/google with a service account
	// or workload identity) and leaves APIKey empty.
	APIKey vision.Secret
	// Endpoint is the annotate URL; "" means DefaultEndpoint (a fake in
	// tests, a regional endpoint in production).
	Endpoint string
	// HTTP is the client; nil means one with a 30-second timeout. The
	// Client uses a copy that refuses redirects (see refuseRedirect).
	HTTP *http.Client
	// MaxResults is how many labels to ask for; 0 means
	// DefaultMaxResults.
	MaxResults int
	// MaxImage bounds an image's bytes; 0 means DefaultMaxImage. A larger
	// image is CodeTooLarge and is not sent.
	MaxImage int64
}

// Client labels images with Cloud Vision. It is safe for concurrent use.
type Client struct {
	cfg Config
}

var _ vision.Labeler = (*Client)(nil)

// New returns a Client. A missing key without an HTTP client to
// authenticate instead is CodeInvalidArgument.
func New(cfg Config) (*Client, error) {
	if cfg.APIKey.IsZero() && cfg.HTTP == nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "google: APIKey is required unless HTTP authenticates the requests")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultEndpoint
	}
	client := http.Client{Timeout: httpTimeout}
	if cfg.HTTP != nil {
		client = *cfg.HTTP // a copy: the caller's client is not changed
	}
	client.CheckRedirect = refuseRedirect
	cfg.HTTP = &client
	if cfg.MaxResults <= 0 {
		cfg.MaxResults = DefaultMaxResults
	}
	if cfg.MaxImage <= 0 {
		cfg.MaxImage = DefaultMaxImage
	}
	return &Client{cfg: cfg}, nil
}

// The request and response of images:annotate, as far as they are used.
type (
	annotateRequest struct {
		Requests []imageRequest `json:"requests"`
	}
	imageRequest struct {
		Image    imageContent `json:"image"`
		Features []feature    `json:"features"`
	}
	imageContent struct {
		Content string `json:"content"`
	}
	feature struct {
		Type       string `json:"type"`
		MaxResults int    `json:"maxResults"`
	}
	annotateResponse struct {
		Responses []imageResponse `json:"responses"`
		Error     *apiError       `json:"error"`
	}
	imageResponse struct {
		LabelAnnotations []labelAnnotation `json:"labelAnnotations"`
		Error            *apiError         `json:"error"`
	}
	labelAnnotation struct {
		Description string  `json:"description"`
		Score       float64 `json:"score"`
	}
	apiError struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
)

// Labels implements vision.Labeler. An image Google cannot read is
// CodeRejected; a refused key, an exhausted quota or an outage is
// CodeUnavailable.
func (c *Client) Labels(ctx context.Context, image io.Reader) ([]vision.Label, error) {
	raw, err := io.ReadAll(io.LimitReader(image, c.cfg.MaxImage+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > c.cfg.MaxImage {
		return nil, filesystem.Errorf(filesystem.CodeTooLarge, "google: image larger than %d bytes", c.cfg.MaxImage)
	}
	body, err := json.Marshal(annotateRequest{Requests: []imageRequest{{
		Image:    imageContent{Content: base64.StdEncoding.EncodeToString(raw)},
		Features: []feature{{Type: featureLabels, MaxResults: c.cfg.MaxResults}},
	}}})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if !c.cfg.APIKey.IsZero() {
		req.Header.Set(apiKeyHeader, c.cfg.APIKey.Reveal())
	}
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("google: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	var out annotateResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&out); err != nil && resp.StatusCode == http.StatusOK {
		return nil, filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("google: unreadable response: %w", err))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp.StatusCode, out.Error)
	}
	if len(out.Responses) != 1 {
		return nil, filesystem.Errorf(filesystem.CodeUnavailable, "google: %d responses to one image", len(out.Responses))
	}
	r := out.Responses[0]
	if r.Error != nil {
		return nil, filesystem.Errorf(filesystem.CodeRejected, "google: %s", message(r.Error))
	}
	labels := make([]vision.Label, 0, len(r.LabelAnnotations))
	for _, a := range r.LabelAnnotations {
		labels = append(labels, vision.Label{Name: a.Description, Score: a.Score})
	}
	return labels, nil
}

// errRedirect is the cause of a call answered with a redirect.
var errRedirect = errors.New("google: the endpoint answered with a redirect, which is not followed")

// refuseRedirect stops every redirect. net/http copies custom headers,
// X-Goog-Api-Key among them, onto a redirect to another host, so
// following one could hand the key to whoever the response names.
// images:annotate never redirects.
func refuseRedirect(*http.Request, []*http.Request) error { return errRedirect }

// statusError is a failed call: a refused key (Google answers 400 for
// an invalid one), an exhausted quota or an outage, all CodeUnavailable,
// a problem of the deployment rather than of the image. An image Google
// cannot decode is reported inside a 200 response instead.
func statusError(status int, e *apiError) error {
	return filesystem.Errorf(filesystem.CodeUnavailable, "google: HTTP %d: %s", status, message(e))
}

// message is Google's error message, cut short and on one line.
func message(e *apiError) string {
	if e == nil || e.Message == "" {
		return "no message"
	}
	m := strings.Join(strings.Fields(e.Message), " ")
	if len(m) > maxErrorMessage {
		m = strings.ToValidUTF8(m[:maxErrorMessage], "")
	}
	return m
}
