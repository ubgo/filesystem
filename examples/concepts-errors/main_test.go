package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun checks each way of handling an error the errors page shows.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"avatar: default.png",
		"errors.Is ErrNotFound: true",
		"wrapped, still not_found",
		"code=not_found op=stat disk=media key=avatars/nobody.png",
		"local: not_found, native *fs.PathError: true",
		`batch failure: "../escape" invalid_key`,
		"batch errors.Is ErrInvalidKey: true",
		"tmp/a still there: false",
		"not_found -> 404 Not Found",
		"exists -> 409 Conflict",
		"unsupported -> 501 Not Implemented",
		"too_large -> 413 Request Entity Too Large",
		"rejected -> 415 Unsupported Media Type",
		"quota_exceeded -> 507 Insufficient Storage",
		"unavailable -> 503 Service Unavailable",
		"unknown -> 500 Internal Server Error",
		`upload over the plan, default: 507 application/json {"error":"the free plan holds 1 GB","code":"quota_exceeded"}`,
		`upload over the plan, problem: 402 application/problem+json {"code":"quota_exceeded","status":402,"title":"the free plan holds 1 GB"}`,
		`unsigned download, problem: 403 application/problem+json {"code":"permission","status":403,"title":"Forbidden"}`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
