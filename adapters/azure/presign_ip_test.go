package azure_test

import (
	"net/url"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
)

// TestPresignAllowedIP proves AllowedIP becomes the SAS sip range under
// the signature (a /32 as one address, a wider prefix as its first and
// last address), and that an IPv6 prefix is CodeUnsupported. Signing is
// local, so no server is needed. Azure enforces sip; Azurite (checked
// 2026-10-07) ignores it, so enforcement cannot be shown there.
func TestPresignAllowedIP(t *testing.T) {
	cred, err := azblob.NewSharedKeyCredential("devstoreaccount1", devKey)
	if err != nil {
		t.Fatal(err)
	}
	client, err := azblob.NewClientWithSharedKeyCredential("https://acct.blob.core.windows.net/", cred, nil)
	if err != nil {
		t.Fatal(err)
	}
	drv, err := azure.New(azure.Config{Client: client, Container: "media", SharedKey: cred})
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("azure", drv)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Capabilities().PresignIP {
		t.Fatal("PresignIP not declared")
	}
	sign := func(cidr string) (url.Values, error) {
		r, err := d.SignedURL(t.Context(), "a.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute, AllowedIP: cidr})
		if err != nil {
			return nil, err
		}
		u, err := url.Parse(r.URL)
		return u.Query(), err
	}
	for cidr, sip := range map[string]string{"127.0.0.1/32": "127.0.0.1", "203.0.113.0/24": "203.0.113.0-203.0.113.255", "0.0.0.0/0": "0.0.0.0-255.255.255.255", "10.0.0.0/31": "10.0.0.0-10.0.0.1"} {
		q, err := sign(cidr)
		if err != nil || q.Get("sip") != sip || q.Get("sig") == "" {
			t.Errorf("%s: sip = %q, want %q (%v)", cidr, q.Get("sip"), sip, err)
		}
	}
	if q, err := sign(""); err != nil || q.Has("sip") {
		t.Errorf("unrestricted URL carries sip: %v, %v", q, err)
	}
	if _, err := sign("2001:db8::/32"); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("IPv6 prefix: %v", err)
	}
}
