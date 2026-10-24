package azure_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
)

// TestFindAbandoned proves the preview reports the uploads a sweep would
// end, with their keys, skips foreign blobs under the marker prefix,
// removes nothing, and agrees with the sweep that follows it.
func TestFindAbandoned(t *testing.T) {
	ctx := t.Context()
	client, cred, _ := account(t)
	d := newDisk(t, client, cred)
	if !d.Capabilities().FindAbandoned {
		t.Fatal("FindAbandoned not declared")
	}
	old, err := d.CreateMultipart(ctx, "old.bin", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.UploadPart(ctx, "old.bin", old, 1, strings.NewReader("a"), 1); err != nil {
		t.Fatal(err)
	}
	// An upload whose marker no longer decodes: reported without a key.
	damaged, err := d.CreateMultipart(ctx, "damaged.bin", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.UploadBuffer(ctx, containerOf(t, d), azure.MultipartDir+"/"+damaged+".json", []byte("{"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.UploadBuffer(ctx, containerOf(t, d), azure.MultipartDir+"/notes.txt", []byte("x"), nil); err != nil {
		t.Fatal(err)
	}
	if found, err := d.FindAbandoned(ctx, time.Now().Add(-time.Hour)); err != nil || len(found) != 0 {
		t.Fatalf("preview with an old cutoff = %+v, %v", found, err)
	}
	cutoff := time.Now().Add(time.Minute)
	found, err := d.FindAbandoned(ctx, cutoff)
	if err != nil || len(found) != 2 {
		t.Fatalf("preview = %+v, %v; want 2 uploads", found, err)
	}
	keys := map[string]string{}
	for _, a := range found {
		if a.Kind != filesystem.AbandonedMultipart || a.LastActivity.IsZero() || !a.LastActivity.Before(cutoff) {
			t.Errorf("reported %+v", a)
		}
		keys[a.ID] = a.Key
	}
	if keys[old] != "old.bin" || keys[damaged] != "" {
		t.Errorf("keys by upload = %v", keys)
	}
	if parts, err := d.ListParts(ctx, "old.bin", old); err != nil || len(parts) != 1 {
		t.Fatalf("the preview removed an upload (parts %v, err %v)", parts, err)
	}
	if n, err := d.SweepAbandoned(ctx, cutoff); err != nil || n != len(found) {
		t.Fatalf("sweep after the preview = %d, %v; want %d", n, err, len(found))
	}
	if found, err := d.FindAbandoned(ctx, cutoff); err != nil || len(found) != 0 {
		t.Errorf("preview after the sweep = %+v, %v", found, err)
	}
}

// TestFindAbandonedRequestFailures injects a failure into each request a
// preview makes: a failed listing or marker read is reported, a marker
// gone before it was read is reported without a key.
func TestFindAbandonedRequestFailures(t *testing.T) {
	ctx := t.Context()
	cutoff := time.Now().Add(time.Minute)
	for _, c := range []struct {
		name          string
		method, match string
		status        int
		code          string
		wantErr       bool
	}{
		{"list markers", http.MethodGet, "comp=list", http.StatusInternalServerError, "", true},
		{"read marker", http.MethodGet, ".json", http.StatusInternalServerError, "", true},
		{"marker gone", http.MethodGet, ".json", http.StatusNotFound, "BlobNotFound", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, p := proxied(t)
			if _, err := d.CreateMultipart(ctx, "x.bin", filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			p.failCode(c.method, c.match, c.status, c.code)
			found, err := d.FindAbandoned(ctx, cutoff)
			if (err != nil) != c.wantErr {
				t.Fatalf("preview = %+v, %v; want error %t", found, err, c.wantErr)
			}
			if !c.wantErr && (len(found) != 1 || found[0].Key != "") {
				t.Errorf("preview of an upload whose marker vanished = %+v", found)
			}
		})
	}
}
