package azure_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
)

// TestReservedKeysRefused proves no operation reaches a blob under
// MultipartDir through the Disk: reading, deleting, overwriting by copy or
// signing a URL for an upload marker is CodeInvalidKey, as keys under the
// local driver's staging directory are, and Azure is never asked.
func TestReservedKeysRefused(t *testing.T) {
	ctx := t.Context()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	cred, _ := azblob.NewSharedKeyCredential("acct", "a2V5")
	client, err := azblob.NewClientWithSharedKeyCredential(srv.URL+"/acct/", cred, nil)
	if err != nil {
		t.Fatal(err)
	}
	drv, err := azure.New(azure.Config{Client: client, Container: "c", SharedKey: cred})
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("azure", drv)
	if err != nil {
		t.Fatal(err)
	}
	marker := azure.MultipartDir + "/abcdefghijklmnopqrstuvwxyz.json"
	checks := map[string]error{"delete": d.Delete(ctx, marker)}
	_, checks["stat"] = d.Stat(ctx, marker)
	_, checks["read"] = d.ReadRange(ctx, marker, filesystem.ByteRange{})
	_, checks["write"] = d.WriteBytes(ctx, marker, []byte("x"), filesystem.WriteOptions{})
	_, checks["copy onto"] = d.Copy(ctx, "a.txt", marker)
	_, checks["signed GET"] = d.SignedURL(ctx, marker, filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	_, checks["signed PUT"] = d.SignedURL(ctx, marker, filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	_, checks["create multipart"] = d.CreateMultipart(ctx, marker, filesystem.WriteOptions{})
	_, checks["write dir"] = d.Write(ctx, azure.MultipartDir, io.NopCloser(strings.NewReader("x")), filesystem.WriteOptions{})
	for name, err := range checks {
		if filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
			t.Errorf("%s of a marker = %v, want invalid_key", name, err)
		}
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("%d requests reached Azure for reserved keys", n)
	}
}
