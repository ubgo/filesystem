package pdf_test

import (
	"bytes"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/pdf"
	"github.com/ubgo/filesystem/records"
)

// TestPlugin_LeavesOtherTypesKeys pins B2: the plugin's keys are cleared
// only on files it describes, so an application's own "pages" and
// "encrypted" on another type survive; a replacement from a PDF to
// another type drops every key the plugin wrote and keeps the rest.
func TestPlugin_LeavesOtherTypesKeys(t *testing.T) {
	requirePrograms(t)
	e := newEnv(t, pdf.NewPlugin(pdf.Tool{}, pdf.WithPreview()))
	own := map[string]string{pdf.MetaPages: "7", pdf.MetaEncrypted: "yes", "app": "kept"}
	if _, err := e.disk.WriteBytes(t.Context(), "deck.txt", []byte("seven slides\n"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: "deck.txt", ContentType: "text/plain", Metadata: own})
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range own {
		if f.Metadata[k] != v {
			t.Fatalf("%s = %q, want %q (%v)", k, f.Metadata[k], v, f.Metadata)
		}
	}
	if _, err := e.disk.WriteBytes(t.Context(), "r.pdf", document(2, a4W, a4H), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	doc, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: "r.pdf", ContentType: pdf.ContentType, Metadata: map[string]string{"app": "kept"}})
	if err != nil {
		t.Fatal(err)
	}
	doc = e.reread(t, doc)
	preview, ok := pdf.PreviewKey(doc)
	if m, _ := pdf.Of(doc); m.Pages != 2 || !ok {
		t.Fatalf("pdf = %v", doc.Metadata)
	}
	next, err := e.store.Replace(t.Context(), scope, doc.ID, bytes.NewReader([]byte("now text\n")), "r.txt", docPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Metadata) != 1 || next.Metadata["app"] != "kept" {
		t.Fatalf("pdf replaced by text: %v", next.Metadata)
	}
	if e.exists(t, preview) {
		t.Fatal("the preview outlived the replaced PDF")
	}
}

// TestPlugin_ReplaceLeavesNoKeys: a PDF holding only the plugin's keys,
// replaced by another type, ends with no metadata at all.
func TestPlugin_ReplaceLeavesNoKeys(t *testing.T) {
	requirePrograms(t)
	e := newEnv(t, pdf.NewPlugin(pdf.Tool{}))
	if _, err := e.disk.WriteBytes(t.Context(), "r.pdf", document(1, a4W, a4H), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	doc, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: "r.pdf", ContentType: pdf.ContentType})
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := pdf.Of(doc); m.Pages != 1 {
		t.Fatalf("pdf = %v", doc.Metadata)
	}
	next, err := e.store.Replace(t.Context(), scope, doc.ID, bytes.NewReader([]byte("now text\n")), "r.txt", docPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Metadata) != 0 {
		t.Fatalf("pdf replaced by text kept %v", next.Metadata)
	}
}
