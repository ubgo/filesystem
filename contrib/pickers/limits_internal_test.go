package pickers

import (
	"context"
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

// TestJSONBodyIsCapped: a vendor answer past maxJSONBody is refused rather
// than buffered, for each source's metadata request.
func TestJSONBodyIsCapped(t *testing.T) {
	ctx := context.Background()
	drive := hugeJSON(`{"name":"x.pdf","size":"1","mimeType":"application/pdf",`)
	defer drive.Close()
	if _, err := (GoogleDrive{APIURL: drive.URL}).Stat(ctx, http.DefaultClient, Pick{ID: "a"}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("drive: %v, want CodeUnavailable", err)
	}
	dbx := hugeJSON(`{".tag":"file","name":"x.pdf","size":1,`)
	defer dbx.Close()
	if _, err := (Dropbox{APIURL: dbx.URL}).Stat(ctx, http.DefaultClient, Pick{ID: "id:a"}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("dropbox: %v, want CodeUnavailable", err)
	}
}

// TestRedactURLErrorContract pins the helper's contract beyond what send
// uses: an error that is not a *url.Error comes back as is, and keepPath
// accepting an origin keeps that URL's path while still dropping its
// query.
func TestRedactURLErrorContract(t *testing.T) {
	plain := errors.New("plain")
	if got := redactURLError(plain, func(string) bool { return false }); got != plain {
		t.Fatalf("got %v, want the error unchanged", got)
	}
	ue := &url.Error{Op: "Get", URL: "https://api.example.com/files/x?sig=SECRET", Err: errors.New("down")}
	got := redactURLError(ue, func(string) bool { return true }).Error()
	if !strings.Contains(got, "/files/x") || strings.Contains(got, "SECRET") {
		t.Fatalf("got %q, want the path kept and the query dropped", got)
	}
}
