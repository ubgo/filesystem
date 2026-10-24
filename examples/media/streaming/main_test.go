package main

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/videoplatform"
)

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"published a1b2c3 on mux: preparing, fetched from a signed URL true", "file status: processing",
		"webhook: 204", "forged webhook: 403", "asset ready, file ready, 12.5s",
		"hls https://stream.mux.com/pb9x8y7.m3u8", "thumbnail https://image.mux.com/pb9x8y7/thumbnail.jpg",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestPublishTwice pins that a file is published once: a second Publish
// is exists, not a second asset billed on the platform.
func TestPublishTwice(t *testing.T) {
	srv := newFakeServer(t)
	platform, err := newPlatform(srv)
	if err != nil {
		t.Fatal(err)
	}
	s, p, err := newStore(t.Context(), platform)
	if err != nil {
		t.Fatal(err)
	}
	f, _, err := publish(t.Context(), s, p, bytes.NewReader(mp4))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Publish(t.Context(), s, scope, f.ID); filesystem.CodeOf(err) != filesystem.CodeExists {
		t.Fatalf("second Publish = %v, want exists", err)
	}
}

// TestSecretNotPrinted pins that a Secret never reaches a log line.
func TestSecretNotPrinted(t *testing.T) {
	sec := videoplatform.NewSecret("hunter2")
	if got := fmt.Sprintf("%v %s %+v %#v", sec, sec, sec, sec); strings.Contains(got, "hunter2") {
		t.Fatalf("secret printed: %q", got)
	}
}

// newFakeServer serves a fake Mux API for one test and returns its URL.
func newFakeServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(newFakeMux())
	t.Cleanup(srv.Close)
	return srv.URL
}
