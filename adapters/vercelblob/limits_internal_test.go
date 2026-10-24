package vercelblob

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	srv := hugeJSON(`{"pathname":"x","size":1,`)
	defer srv.Close()
	d, err := New(Config{Token: "vercel_blob_rw_s_secret", Access: AccessPrivate, APIURL: srv.URL, BlobURL: srv.URL})
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
