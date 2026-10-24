package pickers_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/pickers"
	"github.com/ubgo/filesystem/upload"
)

// answering starts a vendor that answers every request with h.
func answering(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// status answers with code and no body.
func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

// jsonBody answers 200 with body.
func jsonBody(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }
}

func importFrom(t *testing.T, src pickers.Source, id string) error {
	t.Helper()
	_, err := pickers.Import(context.Background(), newDisk(t), src, pickers.Pick{ID: id, Token: userToken}, policy(), loopback)
	return err
}

// TestDefaultOrigins pins that a source with no override names the
// vendor's own API origins, so the token goes there and nowhere else.
func TestDefaultOrigins(t *testing.T) {
	for name, c := range map[string]struct {
		src  pickers.Source
		want []string
	}{
		"drive":    {pickers.GoogleDrive{}, []string{pickers.DefaultDriveURL}},
		"dropbox":  {pickers.Dropbox{}, []string{pickers.DefaultDropboxAPIURL, pickers.DefaultDropboxURL}},
		"onedrive": {pickers.OneDrive{}, []string{pickers.DefaultOneDriveURL}},
	} {
		got := c.src.Origins()
		if len(got) != len(c.want) || got[0] != c.want[0] || got[len(got)-1] != c.want[len(c.want)-1] {
			t.Errorf("%s: origins %v, want %v", name, got, c.want)
		}
	}
}

// TestImportRefusesAnInvalidPolicy pins that the policy is validated
// before any request.
func TestImportRefusesAnInvalidPolicy(t *testing.T) {
	_, err := pickers.Import(context.Background(), newDisk(t), pickers.GoogleDrive{}, pickers.Pick{ID: "x", Token: userToken}, upload.Policy{}, loopback)
	if err == nil {
		t.Fatal("an invalid policy was accepted")
	}
}

// TestStatusClassification pins the vendor statuses past not-found and
// permission: 429 and 5xx are CodeUnavailable, anything else is
// CodeInvalidArgument.
func TestStatusClassification(t *testing.T) {
	for code, want := range map[int]filesystem.Code{
		http.StatusTooManyRequests:     filesystem.CodeUnavailable,
		http.StatusInternalServerError: filesystem.CodeUnavailable,
		http.StatusBadRequest:          filesystem.CodeInvalidArgument,
	} {
		api := answering(t, status(code))
		if err := importFrom(t, pickers.GoogleDrive{APIURL: api}, "doc1"); filesystem.CodeOf(err) != want {
			t.Errorf("%d: %v, want %s", code, err, want)
		}
	}
}

// TestCancelledImport pins that an import whose context has ended returns
// the context's own error, unclassified.
func TestCancelledImport(t *testing.T) {
	api := answering(t, jsonBody(`{}`))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := pickers.Import(ctx, newDisk(t), pickers.GoogleDrive{APIURL: api}, pickers.Pick{ID: "doc1", Token: userToken}, policy(), loopback)
	var fe *filesystem.Error
	if !errors.Is(err, context.Canceled) || errors.As(err, &fe) {
		t.Fatalf("got %v, want the context error itself", err)
	}
}

// TestUnknownSizeIsImported pins that a Drive file with no size (the API
// omits it for some files) is imported, its size unknown rather than
// read as zero or refused; the stream itself stays capped by Receive.
func TestUnknownSizeIsImported(t *testing.T) {
	api := answering(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("alt") == "media" {
			_, _ = w.Write(pdf)
			return
		}
		_, _ = io.WriteString(w, `{"name":"x.pdf","mimeType":"application/pdf"}`)
	})
	if err := importFrom(t, pickers.GoogleDrive{APIURL: api}, "doc1"); err != nil {
		t.Fatal(err)
	}
}

// TestFoldersAreUnsupported pins that a picked folder is CodeUnsupported
// on Dropbox and OneDrive.
func TestFoldersAreUnsupported(t *testing.T) {
	dbx := answering(t, jsonBody(`{".tag":"folder","name":"photos"}`))
	if err := importFrom(t, pickers.Dropbox{APIURL: dbx, ContentURL: dbx}, "id:abc"); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("dropbox folder: %v", err)
	}
	od := answering(t, jsonBody(`{"name":"photos","folder":{}}`))
	if err := importFrom(t, pickers.OneDrive{BaseURL: od}, "ITEM"); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("onedrive folder: %v", err)
	}
}

// TestMetadataFailures pins that a refused metadata request fails the
// import on Dropbox (garbled answer) and OneDrive (403).
func TestMetadataFailures(t *testing.T) {
	dbx := answering(t, jsonBody(`not json`))
	if err := importFrom(t, pickers.Dropbox{APIURL: dbx, ContentURL: dbx}, "id:abc"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("dropbox garbled metadata: %v", err)
	}
	od := answering(t, status(http.StatusForbidden))
	if err := importFrom(t, pickers.OneDrive{BaseURL: od}, "ITEM"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Errorf("onedrive refused: %v", err)
	}
}

// TestDropboxDownloadOfAVanishedFile pins that a file gone between its
// metadata and its download (409) is CodeNotFound.
func TestDropboxDownloadOfAVanishedFile(t *testing.T) {
	api := answering(t, jsonBody(`{".tag":"file","name":"a.pdf","size":10}`))
	content := answering(t, status(http.StatusConflict))
	if err := importFrom(t, pickers.Dropbox{APIURL: api, ContentURL: content}, "id:abc"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("got %v, want CodeNotFound", err)
	}
}

// TestOpenChecksTheID pins that Open refuses an ID that could leave its
// path even when called without Stat, on every source.
func TestOpenChecksTheID(t *testing.T) {
	c := http.DefaultClient
	for name, src := range map[string]pickers.Source{
		"drive":    pickers.GoogleDrive{},
		"dropbox":  pickers.Dropbox{},
		"onedrive": pickers.OneDrive{},
	} {
		if _, err := src.Open(context.Background(), c, pickers.Pick{ID: "../x", Token: userToken}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestControlCharacterInAnEndpoint pins that an override URL no request
// can carry (a control character) is CodeInvalidArgument, on the
// metadata and the content requests of every source.
func TestControlCharacterInAnEndpoint(t *testing.T) {
	const bad = "http://127.0.0.1/\x7f"
	c := http.DefaultClient
	ctx := context.Background()
	errs := map[string]error{}
	_, errs["drive stat"] = pickers.GoogleDrive{APIURL: bad}.Stat(ctx, c, pickers.Pick{ID: "doc1"})
	_, errs["drive open"] = pickers.GoogleDrive{APIURL: bad}.Open(ctx, c, pickers.Pick{ID: "doc1"})
	_, errs["dropbox stat"] = pickers.Dropbox{APIURL: bad}.Stat(ctx, c, pickers.Pick{ID: "id:abc"})
	_, errs["dropbox open"] = pickers.Dropbox{ContentURL: bad}.Open(ctx, c, pickers.Pick{ID: "id:abc"})
	for name, err := range errs {
		if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v, want CodeInvalidArgument", name, err)
		}
	}
}
