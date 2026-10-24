// Command pickers-import imports a file a user chose in the Google Drive
// Picker: the browser posts the picked file's ID and the access token the
// picker used, and the server fetches the file from Drive and stores it
// through the same upload policy as a direct upload.
//
// It serves a stand-in for the Drive API in-process, so it needs no Google
// account.
//
//	go run .            (from examples/pickers-import)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/pickers"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// pickedID and pickerToken are what the Drive Picker hands the browser.
const (
	pickedID    = "1aBcDeFgHiJ"
	pickerToken = "ya29.picker-token"
)

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, out io.Writer) error {
	drive := fakeDrive()
	defer drive.Close()
	disk, err := filesystem.NewDisk("uploads", memory.New())
	if err != nil {
		return err
	}
	src := pickers.GoogleDrive{APIURL: drive.URL + "/drive/v3"}
	// Only for this in-process stand-in: the default refuses every
	// address that is not on the public internet.
	fetch := upload.FetchOptions{AllowAddr: func(a netip.Addr) bool { return a.IsLoopback() }}
	app := httptest.NewServer(importHandler(disk, src, fetch))
	defer app.Close()

	body := fmt.Sprintf(`{"id": %q, "token": %q}`, pickedID, pickerToken)
	resp, err := http.Post(app.URL+"/import/drive", "application/json", strings.NewReader(body))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "POST /import/drive -> %d %s", resp.StatusCode, answer)
	if err := importFromBox(ctx, out, disk, fetch, boxToken); err != nil {
		return err
	}
	return importFromBox(ctx, out, disk, fetch, "expired-token")
}

// pdfPolicy accepts PDFs up to 10 MiB, the same policy a direct upload
// would use.
var pdfPolicy = upload.Policy{MaxSize: 10 << 20, Allowed: []string{"application/pdf"}}

// maxPickBody caps the JSON the browser posts (an ID and a token), so a
// client cannot make the handler buffer an arbitrary body.
const maxPickBody = 4 << 10

// importHandler takes the pick the browser posts and imports it under
// pdfPolicy.
// ds:def id=importhandler-nvdxbkbu
func importHandler(disk *filesystem.Disk, src pickers.Source, fetch upload.FetchOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, maxPickBody)).Decode(&in); err != nil {
			http.Error(w, "bad pick", http.StatusBadRequest)
			return
		}
		res, err := pickers.Import(r.Context(), disk, src, pickers.Pick{ID: in.ID, Token: in.Token}, pdfPolicy, fetch)
		if err != nil {
			http.Error(w, string(filesystem.CodeOf(err)), http.StatusUnprocessableEntity)
			return
		}
		_ = json.NewEncoder(w).Encode(imported{Key: res.Key, Filename: res.Filename, Type: res.ContentType, Size: res.Size})
	})
}

// imported is the import endpoint's answer: where the picked file was
// stored and what the server found it to be (Type comes from its bytes,
// not from the vendor).
type imported struct {
	Key      string `json:"key"`
	Filename string `json:"filename"`
	Type     string `json:"type"`
	Size     int64  `json:"size"`
}

// fakeDrive answers files.get and alt=media for one PDF, and only with
// the picker's token.
func fakeDrive() *httptest.Server {
	pdf := []byte("%PDF-1.7\n% a picked invoice\n")
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+pickerToken || r.URL.Path != "/drive/v3/files/"+pickedID {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("alt") == "media" {
			_, _ = w.Write(pdf)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"name": "invoice-2026-10.pdf", "size": fmt.Sprint(len(pdf)), "mimeType": "application/pdf"})
	}))
}
