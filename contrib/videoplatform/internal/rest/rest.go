// Package rest sends the JSON requests of the platform adapters and maps
// HTTP statuses to the library's error codes.
package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/ubgo/filesystem"
)

// MaxResponseBytes bounds a response body: an asset is a few KiB.
const MaxResponseBytes = 1 << 20

// Call sends method url with *in as a JSON body (nil for none), lets
// authorize set credentials, and decodes a 2xx response into *out (nil
// to ignore it). The platform's name prefixes errors; a response body is
// never quoted in an error, since it may echo a request.
func Call[In, Out any](ctx context.Context, client *http.Client, platform, method, url string, authorize func(*http.Request), in *In, out *Out) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	authorize(req)
	res, err := client.Do(req)
	if err != nil {
		return filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("%s: %s: %w", platform, method, err))
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, MaxResponseBytes+1))
	if err != nil {
		return filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("%s: reading the response: %w", platform, err))
	}
	if len(data) > MaxResponseBytes {
		return filesystem.Errorf(filesystem.CodeTooLarge, "%s: response larger than %d bytes", platform, MaxResponseBytes)
	}
	if code, ok := codeOf(res.StatusCode); !ok {
		return filesystem.Errorf(code, "%s: %s answered %d", platform, method, res.StatusCode)
	}
	if out == nil || res.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("%s: unreadable response: %w", platform, err))
	}
	return nil
}

// codeOf maps an HTTP status to an error code; ok is true for 2xx.
func codeOf(status int) (filesystem.Code, bool) {
	switch {
	case status >= 200 && status < 300:
		return "", true
	case status == http.StatusNotFound:
		return filesystem.CodeNotFound, false
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return filesystem.CodePermission, false
	case status == http.StatusTooManyRequests || status >= 500:
		return filesystem.CodeUnavailable, false
	}
	return filesystem.CodeInvalidArgument, false
}
