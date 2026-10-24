package cloudinary

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
)

// hugeJSON answers with a valid JSON object whose padding runs past
// maxJSONBody after the fields given in head (which ends with a comma).
func hugeJSON(head string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, head, `"pad":"`, strings.Repeat("a", maxJSONBody), `"}`)
	}))
}

// TestJSONBodyIsCapped: an answer past maxJSONBody is refused rather than
// buffered, though it is valid JSON the driver would otherwise accept.
func TestJSONBodyIsCapped(t *testing.T) {
	srv := hugeJSON(`{"public_id":"x","bytes":1,"version":1,`)
	defer srv.Close()
	d, err := New(Config{CloudName: "c", APIKey: "k", APISecret: "s", APIURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Stat(context.Background(), "x"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("stat of an oversized answer: %v, want CodeUnavailable", err)
	}
}

// TestBodyErrorKeepsItsCause pins that the marker put on a failing body's
// error still unwraps to it, so a caller inspecting the transport's error
// chain finds the body's own error.
func TestBodyErrorKeepsItsCause(t *testing.T) {
	cause := errors.New("disk read failed")
	if err := (bodyError{cause}); !errors.Is(err, cause) || err.Error() != cause.Error() {
		t.Fatalf("bodyError %v does not carry its cause", err)
	}
}

// TestRedactLeavesOtherErrors pins that redactURLError changes only a
// *url.Error: any other error, which carries no URL, comes back as is.
func TestRedactLeavesOtherErrors(t *testing.T) {
	plain := errors.New("plain")
	if got := redactURLError(plain, func(string) bool { return true }); got != plain {
		t.Fatalf("got %v, want the error unchanged", got)
	}
}

// TestUnsignedParameters pins Cloudinary's signing rule: file,
// cloud_name, resource_type, api_key and signature are left out of the
// signature, so adding them does not change it.
func TestUnsignedParameters(t *testing.T) {
	base := url.Values{"public_id": {"a"}, "timestamp": {"1"}}
	want := signature(base, "s", sha1.New)
	with := url.Values{"public_id": {"a"}, "timestamp": {"1"}, "file": {"f"}, "cloud_name": {"c"}, "resource_type": {"raw"}, "api_key": {"k"}, "signature": {"x"}}
	if got := signature(with, "s", sha1.New); got != want {
		t.Fatalf("signature with unsigned parameters %s, want %s", got, want)
	}
}
