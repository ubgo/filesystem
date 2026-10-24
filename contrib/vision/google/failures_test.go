package google_test

import (
	"errors"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/vision"
	"github.com/ubgo/filesystem/contrib/vision/google"
)

// TestLabels_InputAndEndpointFailures pins that an image whose reader
// fails is that read error, and that an endpoint no request can carry (a
// control character) fails before anything is sent.
func TestLabels_InputAndEndpointFailures(t *testing.T) {
	f := &fakeVision{t: t}
	c := newClient(t, f, google.Config{})
	broken := errors.New("upload connection reset")
	if _, err := c.Labels(t.Context(), iotest.ErrReader(broken)); !errors.Is(err, broken) {
		t.Fatalf("failing reader: %v, want the read error", err)
	}
	bad, err := google.New(google.Config{APIKey: vision.NewSecret("k"), Endpoint: "https://vision.example/\x7f"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Labels(t.Context(), strings.NewReader("an image")); err == nil || filesystem.CodeOf(err) == filesystem.CodeUnavailable {
		t.Fatalf("bad endpoint: %v, want a request error, not an outage", err)
	}
}
