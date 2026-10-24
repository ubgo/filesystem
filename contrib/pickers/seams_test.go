package pickers_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/pickers"
)

const docType = "application/vnd.google-apps.document"

// keyAuth is a Drive source whose vendor takes the token in its own
// header.
type keyAuth struct{ pickers.GoogleDrive }

func (keyAuth) Authorize(h http.Header, token string) { h.Set("X-Api-Token", token) }

// TestAuthorizer pins the token seam: a source's Authorize sets the
// token its way and no Bearer header goes out; without it, Bearer.
func TestAuthorizer(t *testing.T) {
	var seen []http.Header
	api := answering(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Clone())
		if r.URL.Query().Get("alt") == "media" {
			_, _ = w.Write(pdf)
			return
		}
		_, _ = io.WriteString(w, `{"name":"x.pdf","size":"10","mimeType":"application/pdf"}`)
	})
	if err := importFrom(t, keyAuth{pickers.GoogleDrive{APIURL: api}}, "doc1"); err != nil {
		t.Fatal(err)
	}
	for _, h := range seen {
		if h.Get("X-Api-Token") != userToken || h.Get("Authorization") != "" {
			t.Fatalf("custom auth sent %v", h)
		}
	}
	seen = nil
	if err := importFrom(t, pickers.GoogleDrive{APIURL: api}, "doc1"); err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 || seen[0].Get("Authorization") != "Bearer "+userToken || seen[0].Get("X-Api-Token") != "" {
		t.Fatalf("default auth sent %v", seen)
	}
}

// TestDriveExport pins the export seam: a native Doc named in Export is
// fetched through files.export in its format and named with its
// extension; one it does not name, or any native file without Export,
// stays CodeUnsupported.
func TestDriveExport(t *testing.T) {
	var exported string
	api := answering(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/export"):
			exported = r.URL.Query().Get("mimeType")
			_, _ = w.Write(pdf)
		case r.URL.Query().Get("alt") == "media":
			t.Errorf("a native file was downloaded with alt=media")
		default:
			_, _ = io.WriteString(w, `{"name":"Plan","mimeType":"`+docType+`"}`)
		}
	})
	src := pickers.GoogleDrive{APIURL: api, Export: map[string]pickers.DriveExport{docType: {MimeType: "application/pdf", Extension: ".pdf"}}}
	res, err := pickers.Import(context.Background(), newDisk(t), src, pickers.Pick{ID: "doc1", Token: userToken}, policy(), loopback)
	if err != nil || exported != "application/pdf" || res.Filename != "Plan.pdf" {
		t.Fatalf("export: %+v %q %v", res, exported, err)
	}
	sheetsOnly := pickers.GoogleDrive{APIURL: api, Export: map[string]pickers.DriveExport{"application/vnd.google-apps.spreadsheet": {MimeType: "application/pdf", Extension: ".pdf"}}}
	for name, s := range map[string]pickers.GoogleDrive{"other type": sheetsOnly, "default": {APIURL: api}} {
		if err := importFrom(t, s, "doc1"); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestDriveOpenRechecks pins that Open, called on its own (a caller that
// skips Stat, or a file changed between the two), still refuses a native
// document Export does not name and fails when the metadata cannot be
// read.
func TestDriveOpenRechecks(t *testing.T) {
	native := answering(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("alt") == "media" {
			t.Errorf("a native file was downloaded with alt=media")
		}
		_, _ = io.WriteString(w, `{"name":"Plan","mimeType":"`+docType+`"}`)
	})
	export := map[string]pickers.DriveExport{"application/vnd.google-apps.spreadsheet": {MimeType: "application/pdf", Extension: ".pdf"}}
	pick := pickers.Pick{ID: "doc1", Token: userToken}
	if _, err := (pickers.GoogleDrive{APIURL: native, Export: export}).Open(context.Background(), http.DefaultClient, pick); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("Open of an unnamed native document = %v", err)
	}
	broken := answering(t, status(http.StatusInternalServerError))
	if rc, err := (pickers.GoogleDrive{APIURL: broken, Export: export}).Open(context.Background(), http.DefaultClient, pick); err == nil {
		_ = rc.Close()
		t.Error("Open without metadata succeeded")
	}
}
