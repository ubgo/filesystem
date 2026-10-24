package azure_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
)

// tenantScope is a Config.EncryptionScopeFor that picks a scope by tenant
// prefix.
func tenantScope(key string) string {
	if strings.HasPrefix(key, "tenants/a/") {
		return "scope-a"
	}
	return ""
}

// TestEncryptionScopeFor proves the per-key scope reaches writes and a
// presigned PUT's SAS, the disk-wide scope covers the other keys, the
// server-side copy is not declared, and a CustomerKey beside it is
// refused.
func TestEncryptionScopeFor(t *testing.T) {
	ctx := t.Context()
	cred, _ := azblob.NewSharedKeyCredential("acct", base64.StdEncoding.EncodeToString([]byte("k")))
	drv, d, s := scriptedDriver(t, azure.Config{EncryptionScope: "scope-default", EncryptionScopeFor: tenantScope, SharedKey: cred})
	for key, want := range map[string]string{"tenants/a/x": "scope-a", "shared/x": "scope-default"} {
		if _, err := d.WriteBytes(ctx, key, []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		puts := s.sent("PUT /c/" + key)
		if len(puts) == 0 {
			t.Fatalf("no PUT for %s", key)
		}
		for _, r := range puts {
			if got := r.Header.Get("x-ms-encryption-scope"); got != want {
				t.Errorf("%s sent scope %q, want %q", key, got, want)
			}
		}
	}
	if drv.Capabilities().Copy {
		t.Error("a disk with EncryptionScopeFor declares the server-side copy")
	}
	put, err := d.SignedURL(ctx, "tenants/a/direct", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(put.URL)
	if u.Query().Get("ses") != "scope-a" {
		t.Errorf("presigned PUT carries scope %q", u.Query().Get("ses"))
	}
	key, _ := azure.NewCustomerKey(make([]byte, 32))
	_, err = azure.New(azure.Config{Client: drv.Native(), Container: "c", EncryptionScopeFor: tenantScope, CustomerKey: key})
	wantCode(t, err, filesystem.CodeInvalidArgument)
}

// TestClock proves Config.Clock sets a SAS's expiry and ExpiresAt, and
// DelegateOptions.Clock a delegated SAS's.
func TestClock(t *testing.T) {
	ctx := t.Context()
	at := time.Date(2032, 3, 4, 5, 6, 7, 0, time.UTC)
	clock := func() time.Time { return at }
	cred, _ := azblob.NewSharedKeyCredential("acct", base64.StdEncoding.EncodeToString([]byte("k")))
	_, d, _ := scriptedDriver(t, azure.Config{SharedKey: cred, Clock: clock})
	get, err := d.SignedURL(ctx, "k", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(get.URL)
	if se := u.Query().Get("se"); se != at.Add(time.Minute).Format(time.RFC3339) || !get.ExpiresAt.Equal(at.Add(time.Minute)) {
		t.Errorf("se %s, ExpiresAt %v; want a minute after the clock", se, get.ExpiresAt)
	}
	sas, err := azure.DelegatePrefix(azure.DelegateOptions{ContainerURL: "https://acct.blob.core.windows.net/c", SharedKey: cred, Prefix: "p", Expires: time.Hour, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	if !sas.Expires.Equal(at.Add(time.Hour)) {
		t.Errorf("delegated SAS expires %v, want an hour after the clock", sas.Expires)
	}
}

// TestCopyPollInterval proves a pending copy is polled at
// Config.CopyPollInterval, not the default, and that a negative interval
// is refused.
func TestCopyPollInterval(t *testing.T) {
	_, d, s := scriptedDriver(t, azure.Config{CopyPollInterval: 150 * time.Millisecond})
	s.on("HEAD /c/dst", reply{status: http.StatusOK, header: propsHeaders(map[string]string{"x-ms-copy-status": "pending"})})
	s.on("HEAD /c/src", reply{status: http.StatusOK, header: propsHeaders(nil)})
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	_, err := d.Copy(ctx, "src", "dst")
	wantCode(t, err, filesystem.CodeUnavailable)
	// 400 ms at 150 ms is three polls; the 50 ms default would make eight.
	if n := len(s.sent("HEAD /c/dst")); n < 2 || n > 4 {
		t.Errorf("%d polls in 400 ms at a 150 ms interval", n)
	}
	cred, _ := azblob.NewSharedKeyCredential("acct", base64.StdEncoding.EncodeToString([]byte("k")))
	client, _ := azblob.NewClientWithSharedKeyCredential("https://acct.blob.core.windows.net/", cred, nil)
	_, err = azure.New(azure.Config{Client: client, Container: "c", CopyPollInterval: -time.Second})
	wantCode(t, err, filesystem.CodeInvalidArgument)
}

// TestOpenerSASConnectionString proves a SAS connection string opens a
// disk whose requests carry the token, that declares no Presign, and that
// one without an endpoint or account is refused.
func TestOpenerSASConnectionString(t *testing.T) {
	ctx := t.Context()
	s := &scripted{}
	srv := newScriptedServer(t, s)
	t.Setenv("AZ_SAS_CONN", "BlobEndpoint="+srv+";SharedAccessSignature=sv=2022-11-02&sp=rwdlc&sig=abc%3D")
	t.Setenv("AZ_SAS_BARE", "SharedAccessSignature=sv=2022-11-02&sig=abc")
	mux := filesystem.NewMux()
	_ = mux.Register(azure.Scheme, azure.Opener)
	d, err := mux.Open(ctx, "az", "azblob://c?connection_env=AZ_SAS_CONN")
	if err != nil {
		t.Fatal(err)
	}
	if d.Capabilities().Presign {
		t.Error("a SAS disk declares Presign")
	}
	if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	puts := s.sent("PUT /c/k")
	if len(puts) == 0 || puts[0].URL.Query().Get("sig") != "abc=" {
		t.Fatalf("write did not carry the SAS: %v", puts)
	}
	if _, err := mux.Open(ctx, "az", "azblob://c?connection_env=AZ_SAS_BARE"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("SAS without an endpoint: %v, want CodeInvalidArgument", err)
	}
	t.Setenv("AZ_SAS_MALFORMED", "BlobEndpoint="+srv+";SharedAccessSignature=sv=2022-11-02&sig=%zz")
	if _, err := mux.Open(ctx, "az", "azblob://c?connection_env=AZ_SAS_MALFORMED"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("a malformed SAS: %v, want CodeInvalidArgument", err)
	}
	if _, err := mux.Open(ctx, "az", "azblob://c?connection_env=AZ_SAS_CONN&access_tier=Lukewarm"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("a SAS disk with a bad parameter: %v, want CodeInvalidArgument", err)
	}
}

// newScriptedServer serves s until the test ends and returns its URL.
func newScriptedServer(t *testing.T, s *scripted) string {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv.URL
}
