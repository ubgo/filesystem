package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"changelog: file.created attachment.created attachments.reordered file.trashed file.restored file.replaced file.status_changed file.status_changed file.released file.visibility_changed file.moved attachment.deleted file.purged <nil>",
		"sent: 13",
		"rolled back: handler failed; rows 13, sent 13",
		"purge under hold: permission; metadata map[classification:internal legal_hold:true]",
		"replace text with PNG: rejected",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestEveryKindIsProduced checks the demo covers the closed set.
func TestEveryKindIsProduced(t *testing.T) {
	db, err := openSQLite(filepath.Join(t.TempDir(), "records.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	log := &Changelog{}
	s, err := newStore(t.Context(), db, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := everyKind(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	got, err := log.Kinds(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range records.EventKindValues {
		if !strings.Contains(strings.Join(got, " "), string(k)) {
			t.Errorf("no %s event", k)
		}
	}
	// The PNG refusal comes from TypeLock, not from the upload policy.
	g, err := s.Upload(t.Context(), tenant, diskName, strings.NewReader("text"), "b.txt", textPolicy)
	if err != nil {
		t.Fatal(err)
	}
	png := upload.Policy{MaxSize: maxUpload, Allowed: []string{"image/png"}}
	_, err = s.Replace(t.Context(), tenant, g.ID, bytes.NewReader([]byte("\x89PNG\r\n\x1a\n")), "", png)
	if err == nil || !strings.Contains(err.Error(), "cannot become") {
		t.Fatalf("Replace = %v, want the TypeLock refusal", err)
	}
}
