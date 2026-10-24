package gcs_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gcs"
)

// TestSetCORS proves SetCORS sends the rules as a bucket update, with the
// request and exposed headers merged into GCS's one header list, and
// refuses incomplete rules.
func TestSetCORS(t *testing.T) {
	ctx := t.Context()
	client, srv, p, _ := proxied(t)
	bucket := newBucket(t, srv)
	if err := gcs.SetCORS(ctx, client, bucket, gcs.DirectUploadCORS("https://app.example.com")); err != nil {
		t.Fatal(err)
	}
	// fake-gcs-server accepts the update but does not keep CORS, so the
	// request body is what is checked.
	p.mu.Lock()
	body := strings.Join(p.bodies, "\n")
	p.mu.Unlock()
	var sent struct {
		CORS []struct {
			Origin         []string `json:"origin"`
			Method         []string `json:"method"`
			ResponseHeader []string `json:"responseHeader"`
			MaxAgeSeconds  int64    `json:"maxAgeSeconds"`
		} `json:"cors"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil || len(sent.CORS) != 1 {
		t.Fatalf("bucket update = %s (%v)", body, err)
	}
	got := sent.CORS[0]
	if !slices.Equal(got.Origin, []string{"https://app.example.com"}) || !slices.Contains(got.Method, "PUT") || !slices.Contains(got.Method, "POST") ||
		!slices.Contains(got.ResponseHeader, "Content-Type") || !slices.Contains(got.ResponseHeader, "ETag") || got.MaxAgeSeconds != int64(time.Hour/time.Second) {
		t.Errorf("sent rule = %+v", got)
	}
	for _, bad := range [][]gcs.CORSRule{nil, {{AllowedMethods: []string{"GET"}}}, {{AllowedOrigins: []string{"https://x"}}}} {
		if err := gcs.SetCORS(ctx, client, bucket, bad...); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("SetCORS(%v) = %v", bad, err)
		}
	}
	if err := gcs.SetCORS(ctx, client, "no-such-bucket", gcs.DirectUploadCORS("https://x")); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("SetCORS on a missing bucket = %v", err)
	}
}
