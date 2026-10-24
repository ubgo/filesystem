package pdf_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/pdf"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// twoDiskEnv is newEnv with a second disk, "cdn", in the Manager.
func twoDiskEnv(t *testing.T, p *pdf.Plugin) (*env, *filesystem.Disk) {
	t.Helper()
	media := newDisk(t)
	cdn, _ := filesystem.NewDisk("cdn", memory.New())
	mgr, err := filesystem.NewManager(media, cdn)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := recordstest.Open(t)
	store, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &env{disk: media, store: store}, cdn
}

// keysOn lists every key on d.
func keysOn(t *testing.T, d *filesystem.Disk) []string {
	t.Helper()
	var keys []string
	for e, err := range d.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, e.Key)
	}
	return keys
}

// TestLocation_CustomDiskAndPrefix pins B10: WithLocation places each
// derived object by name (the preview on the cdn disk, the text under its
// own prefix on the file's disk), records a disk only when it is not the
// file's, and a replacement and the purge delete them where they are.
func TestLocation_CustomDiskAndPrefix(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	loc := func(f records.File, name string) (string, string, error) {
		if name == pdf.DerivedPreview {
			return "cdn", "previews/" + f.ID + "-", nil
		}
		return "", "texts/" + f.ID + "-", nil
	}
	e, cdn := twoDiskEnv(t, pdf.NewPlugin(pdf.Tool{}, pdf.WithPreview(), pdf.WithText(), pdf.WithLocation(loc)))
	f, err := e.store.Upload(t.Context(), scope, "media", bytes.NewReader(textDocument("", "Revenue grew")), "r.pdf", docPolicy)
	if err != nil {
		t.Fatal(err)
	}
	f = e.reread(t, f)
	pDisk, pKey, ok := pdf.PreviewLocation(f)
	if !ok || pDisk != "cdn" || f.Metadata[pdf.MetaPreviewDisk] != "cdn" || !strings.HasPrefix(pKey, "previews/"+f.ID+"-") || !strings.HasSuffix(pKey, pdf.PreviewSuffix) {
		t.Fatalf("preview at %s:%s (%v)", pDisk, pKey, f.Metadata)
	}
	tDisk, tKey, ok := pdf.TextLocation(f)
	if _, recorded := f.Metadata[pdf.MetaTextDisk]; !ok || recorded || tDisk != "media" || !strings.HasPrefix(tKey, "texts/"+f.ID+"-") {
		t.Fatalf("text at %s:%s (%v)", tDisk, tKey, f.Metadata)
	}
	if ok, _ := cdn.Exists(t.Context(), pKey); !ok || !e.exists(t, tKey) {
		t.Fatal("a derived object is missing")
	}
	next, err := e.store.Replace(t.Context(), scope, f.ID, bytes.NewReader(textDocument("", "Costs fell")), "", docPolicy)
	if err != nil {
		t.Fatal(err)
	}
	next = e.reread(t, next)
	if ok, _ := cdn.Exists(t.Context(), pKey); ok || e.exists(t, tKey) {
		t.Fatal("the old preview or text outlived the replacement")
	}
	if _, k, _ := pdf.PreviewLocation(next); k == pKey || k == "" {
		t.Fatalf("new preview key %q", k)
	}
	if err := e.store.Purge(t.Context(), scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if keys := append(keysOn(t, e.disk), keysOn(t, cdn)...); len(keys) != 0 {
		t.Fatalf("objects outlived the purged file: %v", keys)
	}
}

// TestLocation_ErrorFailsClosed pins B10: a Location error for the text,
// after the preview was stored, deletes the preview, records neither, and
// marks the file failed; the upload itself succeeds, as for any Process
// failure.
func TestLocation_ErrorFailsClosed(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	loc := func(f records.File, name string) (string, string, error) {
		if name == pdf.DerivedText {
			return "", "", filesystem.Errorf(filesystem.CodeUnavailable, "the search bucket is down")
		}
		return "cdn", "previews/", nil
	}
	e, cdn := twoDiskEnv(t, pdf.NewPlugin(pdf.Tool{}, pdf.WithPreview(), pdf.WithText(), pdf.WithLocation(loc)))
	f, err := e.store.Upload(t.Context(), scope, "media", bytes.NewReader(textDocument("", "Revenue grew")), "r.pdf", docPolicy)
	if err != nil {
		t.Fatal(err)
	}
	f = e.reread(t, f)
	if f.Status != records.StatusFailed || f.StatusDetail != pdf.StatusFailedPrefix+string(filesystem.CodeUnavailable) {
		t.Fatalf("status = %q %q", f.Status, f.StatusDetail)
	}
	if _, ok := pdf.PreviewKey(f); ok {
		t.Fatalf("a preview was recorded: %v", f.Metadata)
	}
	if keys := keysOn(t, cdn); len(keys) != 0 {
		t.Fatalf("the preview outlived the failed run: %v", keys)
	}
	if keys := keysOn(t, e.disk); len(keys) != 1 || keys[0] != f.Key {
		t.Fatalf("media disk holds %v, want only the document", keys)
	}
}

// TestLocation_Default: without WithLocation (and with a nil one) the
// preview and text stay beside their file on its disk, no disk recorded.
func TestLocation_Default(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	e, _ := twoDiskEnv(t, pdf.NewPlugin(pdf.Tool{}, pdf.WithPreview(), pdf.WithText(), pdf.WithLocation(nil)))
	f, err := e.store.Upload(t.Context(), scope, "media", bytes.NewReader(textDocument("", "Revenue grew")), "r.pdf", docPolicy)
	if err != nil {
		t.Fatal(err)
	}
	f = e.reread(t, f)
	pDisk, pKey, _ := pdf.PreviewLocation(f)
	tDisk, tKey, _ := pdf.TextLocation(f)
	_, pRec := f.Metadata[pdf.MetaPreviewDisk]
	_, tRec := f.Metadata[pdf.MetaTextDisk]
	if pRec || tRec || pDisk != "media" || tDisk != "media" || !strings.HasPrefix(pKey, f.Key+pdf.PreviewInfix) || !strings.HasPrefix(tKey, f.Key+pdf.TextInfix) {
		t.Fatalf("preview %s:%s, text %s:%s (%v)", pDisk, pKey, tDisk, tKey, f.Metadata)
	}
}

// TestLocation_UnknownDisk: a Location naming a disk the Store's manager
// does not have fails the run as an error of its own would: the file is
// marked failed and nothing is recorded or left behind.
func TestLocation_UnknownDisk(t *testing.T) {
	requirePrograms(t)
	loc := func(records.File, string) (string, string, error) { return "nowhere", "previews/", nil }
	e, cdn := twoDiskEnv(t, pdf.NewPlugin(pdf.Tool{}, pdf.WithPreview(), pdf.WithLocation(loc)))
	f, err := e.store.Upload(t.Context(), scope, "media", bytes.NewReader(textDocument("", "Revenue grew")), "r.pdf", docPolicy)
	if err != nil {
		t.Fatal(err)
	}
	f = e.reread(t, f)
	if f.Status != records.StatusFailed || f.StatusDetail != pdf.StatusFailedPrefix+string(filesystem.CodeNotFound) {
		t.Fatalf("status = %q %q", f.Status, f.StatusDetail)
	}
	if _, ok := pdf.PreviewKey(f); ok {
		t.Fatalf("a preview was recorded: %v", f.Metadata)
	}
	if keys := append(keysOn(t, e.disk), keysOn(t, cdn)...); len(keys) != 1 || keys[0] != f.Key {
		t.Fatalf("disks hold %v, want only the document", keys)
	}
}
