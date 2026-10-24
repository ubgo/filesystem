package azure_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
)

const testScope = "tenant-a-scope"

// TestEncryptionScopeSigned proves a presigned PUT's SAS carries the
// disk's encryption scope (ses), so a direct upload is encrypted with it,
// and that a GET does not, from Config and from both openers' DSNs.
func TestEncryptionScopeSigned(t *testing.T) {
	ctx := t.Context()
	cred, _ := azblob.NewSharedKeyCredential("acct", "a2V5")
	client, err := azblob.NewClientWithSharedKeyCredential("https://acct.blob.core.windows.net/", cred, nil)
	if err != nil {
		t.Fatal(err)
	}
	drv, err := azure.New(azure.Config{Client: client, Container: "c", SharedKey: cred, EncryptionScope: testScope})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("azure", drv)
	t.Setenv(azure.DefaultConnectionEnv, "AccountName=acct;AccountKey=a2V5")
	mux := filesystem.NewMux()
	_ = mux.Register(azure.Scheme, azure.Opener)
	_ = mux.Register("azentra", azure.TokenOpener(fakeToken{}))
	fromDSN, err := mux.Open(ctx, "dsn", "azblob://c?encryption_scope="+testScope)
	if err != nil {
		t.Fatal(err)
	}
	for name, disk := range map[string]*filesystem.Disk{"config": d, "dsn": fromDSN} {
		put, err := disk.SignedURL(ctx, "k", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if q := mustQuery(t, put.URL); q.Get("ses") != testScope {
			t.Errorf("%s: presigned PUT ses = %q", name, q.Get("ses"))
		}
		get, _ := disk.SignedURL(ctx, "k", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
		if strings.Contains(get.URL, "ses=") {
			t.Errorf("%s: presigned GET carries a scope", name)
		}
	}
	if _, err := mux.Open(ctx, "entra", "azentra://c?account=acct&encryption_scope="+url.QueryEscape(testScope)); err != nil {
		t.Errorf("TokenOpener with encryption_scope = %v", err)
	}
}

// TestEncryptionScopeSent proves every request that creates blob data
// names the scope: a write, a staged part and its commit, and a copy,
// which streams through Write because the server-side copy cannot carry
// a scope.
func TestEncryptionScopeSent(t *testing.T) {
	ctx := t.Context()
	d, p := proxied(t)
	scoped := scopedDisk(t, d)
	if scoped.Capabilities().Copy {
		t.Error("a scoped disk declares server-side Copy")
	}
	if _, err := scoped.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	id, err := scoped.CreateMultipart(ctx, "m.bin", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	part, err := scoped.UploadPart(ctx, "m.bin", id, 1, strings.NewReader("p"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scoped.CompleteMultipart(ctx, "m.bin", id, []filesystem.Part{part}); err != nil {
		t.Fatal(err)
	}
	if _, err := scoped.Copy(ctx, "a.txt", "b.txt"); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	sent := strings.Join(p.scopes, "\n")
	p.mu.Unlock()
	for _, want := range []string{"/a.txt?", "&comp=block ", "comp=blocklist", "/b.txt?"} {
		if !strings.Contains(sent, want) {
			t.Errorf("no scoped request matching %q in:\n%s", want, sent)
		}
	}
	for _, line := range p.scopes {
		if !strings.HasSuffix(line, " "+testScope) {
			t.Errorf("wrong scope: %s", line)
		}
	}
}

// scopedDisk is a disk on the same client and container as d, with the
// test encryption scope.
func scopedDisk(t *testing.T, d *filesystem.Disk) *filesystem.Disk {
	t.Helper()
	client, _ := filesystem.As[*azblob.Client](d)
	drv, err := azure.New(azure.Config{Client: client, Container: containerOf(t, d), EncryptionScope: testScope})
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := filesystem.NewDisk("scoped", drv)
	if err != nil {
		t.Fatal(err)
	}
	return scoped
}
