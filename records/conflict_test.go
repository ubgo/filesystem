package records_test

import (
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// TestIfUnchanged: a replacement made against the content the caller read
// goes through; one made against content replaced since is refused with
// CodeConflict, uploads nothing and changes nothing.
func TestIfUnchanged(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f, err := e.store.Upload(ctx, "t", "media", reader(pngData), "doc.png", images)
	if err != nil {
		t.Fatal(err)
	}
	read := f.SHA256
	alice, err := e.store.Replace(ctx, "t", f.ID, reader(pngData+"-alice"), "", images, records.IfUnchanged(strings.ToUpper(read)))
	if err != nil {
		t.Fatalf("a replacement of the content read: %v", err)
	}
	before := e.objectCount(t)
	_, err = e.store.Replace(ctx, "t", f.ID, reader(pngData+"-bob"), "", images, records.IfUnchanged(read))
	wantCode(t, err, filesystem.CodeConflict)
	if e.objectCount(t) != before {
		t.Fatal("a refused replacement stored its body")
	}
	if cur, _ := e.store.File(ctx, "t", f.ID); cur.SHA256 != alice.SHA256 {
		t.Fatal("the refused replacement changed the file")
	}
	// ReplaceObject checks under the row lock too.
	if _, err := e.disk.WriteBytes(ctx, "next.png", []byte(pngData+"-next"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = e.store.ReplaceObject(ctx, "t", f.ID, records.NewContent{Key: "next.png"}, records.IfUnchanged(read))
	wantCode(t, err, filesystem.CodeConflict)
	if _, err := e.store.ReplaceObject(ctx, "t", f.ID, records.NewContent{Key: "next.png"}, records.IfUnchanged(alice.SHA256)); err != nil {
		t.Fatal(err)
	}
}
