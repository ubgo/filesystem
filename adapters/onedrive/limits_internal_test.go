package onedrive

import (
	"context"
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
	srv := hugeJSON(`{"name":"x","size":1,"file":{},`)
	defer srv.Close()
	d, err := New(Config{Token: TokenFunc(func(context.Context) (string, error) { return "t", nil }), BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Stat(context.Background(), "x"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("stat of an oversized answer: %v, want CodeUnavailable", err)
	}
}
