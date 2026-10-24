package variants_test

import (
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// TestEphemeralFilesGetNoVariantLinks: URL, SrcSet and Get refuse a view-once
// file (a thumbnail would show it without spending its view) and an
// expired file, as the serving routes do; an ordinary file still links.
func TestEphemeralFilesGetNoVariantLinks(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, transforms)
	record := func(key string, in records.NewFile) records.File {
		if _, err := e.disk.WriteBytes(ctx, key, pngBytes(t, 100, 100), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		in.Scope, in.Disk, in.Key = "t", "media", key
		f, err := e.store.CreateFile(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	past := time.Now().Add(-time.Minute)
	for _, f := range []records.File{record("once.png", records.NewFile{ViewOnce: true}), record("old.png", records.NewFile{ExpiresAt: &past})} {
		_, err := e.plugin.URL(ctx, e.store, "t", f.ID, "thumb", time.Hour)
		wantCode(t, err, filesystem.CodeNotFound)
		_, _, err = e.plugin.SrcSet(ctx, e.store, "t", f.ID, time.Hour, "thumb")
		wantCode(t, err, filesystem.CodeNotFound)
		_, err = e.plugin.Get(ctx, e.store, "t", f.ID, "thumb")
		wantCode(t, err, filesystem.CodeNotFound)
	}
	plain := record("plain.png", records.NewFile{})
	if _, err := e.plugin.URL(ctx, e.store, "t", plain.ID, "thumb", time.Hour); err != nil {
		t.Fatal(err)
	}
}
