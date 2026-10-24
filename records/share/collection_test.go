package share_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/share"
)

// TestCollectionHead: a HEAD of a file in a collection link answers 204
// for a file in the collection and 404 for any other, and spends no use,
// so probing a link never counts as a download.
func TestCollectionHead(t *testing.T) {
	ctx := t.Context()
	u := newSuite(t, 0)
	dir, err := u.folders.Create(ctx, u.store, "t", folders.RootID, "Reports")
	if err != nil {
		t.Fatal(err)
	}
	in := u.file(t, "in.txt")
	if err := u.folders.Put(ctx, u.store, "t", in.ID, dir.ID); err != nil {
		t.Fatal(err)
	}
	outside := u.file(t, "outside.txt")
	sh, err := u.plugin.CreateFor(ctx, u.store, "t", share.Target{Kind: folders.KindFolder, Ref: dir.ID}, share.Options{TTL: time.Hour, MaxDownloads: 1})
	if err != nil {
		t.Fatal(err)
	}
	h := share.Handler(u.store, u.plugin)
	for range 2 {
		if rec := serve(h, http.MethodHead, "/"+sh.Token+"/"+in.ID, nil, nil); rec.Code != http.StatusNoContent || rec.Header().Get("Location") != "" {
			t.Fatalf("HEAD of a file in the folder = %d, Location %q", rec.Code, rec.Header().Get("Location"))
		}
	}
	if rec := serve(h, http.MethodHead, "/"+sh.Token+"/"+outside.ID, nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("HEAD of a file outside the folder = %d", rec.Code)
	}
	if n := u.uses(t, sh); n != 0 {
		t.Fatalf("uses after HEADs = %d, want 0", n)
	}
	// The one allowed download is still there after the HEADs.
	if rec := serve(h, http.MethodGet, "/"+sh.Token+"/"+in.ID, nil, nil); rec.Code != http.StatusFound {
		t.Fatalf("GET after HEADs = %d", rec.Code)
	}
}

// TestCollections: Collections lists the links to one target, newest
// first and without their tokens, and no link to another target. Order is
// checked on CreatedAt alone: two links made in the same microsecond fall
// back to their random IDs.
func TestCollections(t *testing.T) {
	ctx := t.Context()
	u := newSuite(t, 0)
	var dirs []string
	for _, name := range []string{"A", "B"} {
		d, err := u.folders.Create(ctx, u.store, "t", folders.RootID, name)
		if err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, d.ID)
	}
	target := share.Target{Kind: folders.KindFolder, Ref: dirs[0]}
	var made []string
	for range 2 {
		sh, err := u.plugin.CreateFor(ctx, u.store, "t", target, share.Options{TTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		made = append(made, sh.ID)
	}
	if _, err := u.plugin.CreateFor(ctx, u.store, "t", share.Target{Kind: folders.KindFolder, Ref: dirs[1]}, share.Options{TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	list, err := u.plugin.Collections(ctx, u.store, "t", target)
	if err != nil || len(list) != len(made) {
		t.Fatalf("Collections = %+v, %v", list, err)
	}
	want := map[string]bool{made[0]: true, made[1]: true}
	for i, sh := range list {
		if !want[sh.ID] || sh.Token != "" {
			t.Errorf("Collections[%d] = %s token %q, want one of %v without a token", i, sh.ID, sh.Token, made)
		}
		if i > 0 && sh.CreatedAt.After(list[i-1].CreatedAt) {
			t.Errorf("Collections not newest first: %v after %v", sh.CreatedAt, list[i-1].CreatedAt)
		}
	}
}

// TestFolderRequestLink: an upload through a request link to a folder
// lands in that folder (folders' Collection is a Receiver), and the root
// cannot receive one.
func TestFolderRequestLink(t *testing.T) {
	ctx := t.Context()
	u := newSuite(t, 0)
	dir, err := u.folders.Create(ctx, u.store, "t", folders.RootID, "Inbox")
	if err != nil {
		t.Fatal(err)
	}
	sh, err := u.plugin.CreateRequest(ctx, u.store, "t", share.Target{Kind: folders.KindFolder, Ref: dir.ID}, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "doc.txt")
	_, _ = fw.Write([]byte("hello"))
	_ = mw.Close()
	h := share.Handler(u.store, u.plugin, share.WithUploads("media", anyText))
	rec := serve(h, http.MethodPost, "/"+sh.Token, &buf, http.Header{"Content-Type": {mw.FormDataContentType()}})
	var got struct{ ID string }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusCreated {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body)
	}
	if in, err := u.folders.FolderOf(ctx, u.store, "t", got.ID); err != nil || in != dir.ID {
		t.Fatalf("uploaded file is in %q, %v; want %s", in, err, dir.ID)
	}
	if _, err := u.plugin.CreateRequest(ctx, u.store, "t", share.Target{Kind: folders.KindFolder, Ref: folders.RootID}, share.Options{TTL: time.Hour}); err == nil {
		t.Fatal("a request link to the root was created")
	}
}
