package azure_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
)

// devKey is Azurite's well-known development account key, public by
// design, so signing can be checked without a server.
const devKey = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="

// TestDelegatePrefixSAS proves the SAS is scoped as asked: a directory
// SAS (sr=d) at the prefix's depth, a container SAS (sr=c) only when
// WholeContainer says so, read and list only unless Write is set, HTTPS
// only on an HTTPS endpoint; that options which would widen or blur the
// scope are refused; and that the token never prints.
func TestDelegatePrefixSAS(t *testing.T) {
	cred, err := azblob.NewSharedKeyCredential("devstoreaccount1", devKey)
	if err != nil {
		t.Fatal(err)
	}
	o := azure.DelegateOptions{ContainerURL: "https://acct.blob.core.windows.net/media", SharedKey: cred, Prefix: "users/42", Expires: time.Hour}
	s, err := azure.DelegatePrefix(o)
	if err != nil {
		t.Fatal(err)
	}
	q, _ := url.ParseQuery(s.Token)
	for param, want := range map[string]string{"sr": "d", "sdd": "2", "sp": "rl", "spr": "https"} {
		if got := q.Get(param); got != want {
			t.Errorf("%s = %q, want %q (token %s)", param, got, want, s.Token)
		}
	}
	if q.Get("sig") == "" || time.Until(s.Expires) > time.Hour || time.Until(s.Expires) < 59*time.Minute {
		t.Errorf("signature or expiry: %s, %v", s.Token, s.Expires)
	}
	if !strings.HasPrefix(s.URL(), "https://acct.blob.core.windows.net/media?") {
		t.Errorf("URL %s", s.URL())
	}

	o.Write = true
	w, err := azure.DelegatePrefix(o)
	if q, _ := url.ParseQuery(w.Token); err != nil || q.Get("sp") != "racwdl" {
		t.Errorf("write SAS %s, %v", w.Token, err)
	}
	whole := azure.DelegateOptions{ContainerURL: "http://127.0.0.1:10000/devstoreaccount1/media/", SharedKey: cred, WholeContainer: true, Expires: time.Hour}
	c, err := azure.DelegatePrefix(whole)
	if q, _ := url.ParseQuery(c.Token); err != nil || q.Get("sr") != "c" || q.Get("sdd") != "" || q.Get("spr") != "https,http" {
		t.Errorf("container SAS %s, %v", c.Token, err)
	}

	for name, mutate := range map[string]func(*azure.DelegateOptions){
		"empty prefix":         func(o *azure.DelegateOptions) { o.Prefix = "" },
		"prefix and container": func(o *azure.DelegateOptions) { o.WholeContainer = true },
		"traversal":            func(o *azure.DelegateOptions) { o.Prefix = "users/../admin" },
		"unclean":              func(o *azure.DelegateOptions) { o.Prefix = "users//42" },
		"no signer":            func(o *azure.DelegateOptions) { o.SharedKey = nil },
		"no lifetime":          func(o *azure.DelegateOptions) { o.Expires = 0 },
		"too long":             func(o *azure.DelegateOptions) { o.Expires = filesystem.MaxPresignExpiry + time.Second },
		"query in URL":         func(o *azure.DelegateOptions) { o.ContainerURL += "?sv=1" },
		"no container":         func(o *azure.DelegateOptions) { o.ContainerURL = "https://acct.blob.core.windows.net/" },
	} {
		bad := o
		mutate(&bad)
		if _, err := azure.DelegatePrefix(bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}

	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("issued", "sas", s)
	printed := fmt.Sprintf("%v %+v %#v %s", s, s, s, s) + logged.String() + fmt.Sprintf("%+v", struct{ S azure.ScopedSAS }{s})
	if strings.Contains(printed, q.Get("sig")) || strings.Contains(printed, url.QueryEscape(q.Get("sig"))) || !strings.Contains(printed, "users/42") {
		t.Errorf("printed SAS: %s", printed)
	}
}

// TestContainerSASAgainstAzurite proves on Azurite that a container SAS
// works in its own container (write, list, read) and is refused in
// another container of the same account.
func TestContainerSASAgainstAzurite(t *testing.T) {
	ctx := t.Context()
	client, cred, _ := account(t)
	mine, other := newContainer(t, client), newContainer(t, client)
	base := strings.TrimRight(client.URL(), "/")
	s, err := azure.DelegatePrefix(azure.DelegateOptions{ContainerURL: base + "/" + mine, SharedKey: cred, WholeContainer: true, Write: true, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	cc, err := container.NewClientWithNoCredential(s.URL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cc.NewBlockBlobClient("users/42/a.txt").UploadBuffer(ctx, []byte("hi"), nil); err != nil {
		t.Fatalf("upload in the delegated container: %v", err)
	}
	pager := cc.NewListBlobsFlatPager(nil)
	page, err := pager.NextPage(ctx)
	if err != nil || len(page.Segment.BlobItems) != 1 {
		t.Fatalf("list in the delegated container: %v", err)
	}
	// The same token presented for another container.
	foreign, err := container.NewClientWithNoCredential(base+"/"+other+"?"+s.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.NewBlockBlobClient("users/42/a.txt").UploadBuffer(ctx, []byte("hi"), nil); err == nil {
		t.Fatal("a container SAS wrote to another container")
	}
}
