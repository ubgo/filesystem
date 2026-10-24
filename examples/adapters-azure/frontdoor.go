package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
)

// Decisions of the offline steps: the account behind the front end, and
// the tenant whose contract gets options of its own.
const (
	// frontDoorAccount is the storage account the front end forwards to.
	frontDoorAccount = "mystorageacct"
	// tenantScope is the encryption scope one tenant's rows name.
	tenantScope = "tenant-acme"
	// contractKey is the blob the per-call options are bound to.
	contractKey = "contracts/2026/acme.pdf"
	// entraToken is the token the stand-in credential hands out.
	entraToken = "entra-token"
)

// staticToken is a stand-in Entra ID credential: one token, an hour long.
type staticToken struct{}

func (staticToken) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: entraToken, ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// blobService is a stand-in for the Blob service that answers what these
// steps call (Get User Delegation Key, Put Blob, Set Legal Hold, Get Blob
// Properties) and records what each request carried. Azurite is not
// needed for them.
type blobService struct {
	mu       sync.Mutex
	keyHosts []string               // the Host of each key request
	puts     map[string]http.Header // the headers of each Put Blob, by path
	holds    []string               // the paths given a legal hold
}

func newBlobService() *blobService { return &blobService{puts: map[string]http.Header{}} }

func (b *blobService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	comp := r.URL.Query().Get("comp")
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && comp == "userdelegationkey":
		if r.Header.Get("Authorization") != "Bearer "+entraToken {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var info struct{ Start, Expiry string }
		_ = xml.Unmarshal(body, &info)
		b.keyHosts = append(b.keyHosts, r.Host)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><UserDelegationKey><SignedOid>oid</SignedOid><SignedTid>tid</SignedTid><SignedStart>%s</SignedStart><SignedExpiry>%s</SignedExpiry><SignedService>b</SignedService><SignedVersion>2023-11-03</SignedVersion><Value>a2V5LWJ5dGVzLWZvci1leGFtcGxlLW9ubHk=</Value></UserDelegationKey>`, info.Start, info.Expiry)
	case r.Method == http.MethodPut && comp == "legalhold":
		b.holds = append(b.holds, r.URL.Path)
		w.Header().Set("x-ms-legal-hold", r.Header.Get("x-ms-legal-hold"))
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPut && comp == "":
		b.puts[r.URL.Path] = r.Header.Clone()
		w.Header().Set("ETag", `"0x1"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodHead:
		w.Header().Set("Content-Length", "1")
		w.Header().Set("ETag", `"0x1"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("x-ms-blob-type", "BlockBlob")
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// standInOptions are client options that reach srv and never retry.
func standInOptions(srv *httptest.Server) *azblob.ClientOptions {
	return &azblob.ClientOptions{ClientOptions: policy.ClientOptions{Transport: srv.Client(), Retry: policy.RetryOptions{MaxRetries: -1}}}
}

// writeContract stores one tenant's contract in that tenant's encryption
// scope, in the Cool tier, tagged and under a legal hold. The options are
// bound to the contract's key, so the same context reused for a
// thumbnail or a staging file never inherits them.
// ds:def id=writecontract-k9v4n2xs
func writeContract(ctx context.Context, disk *filesystem.Disk, pdf string) (context.Context, error) {
	ctx, err := azure.WithRequestOptions(ctx, disk, contractKey, azure.RequestOptions{
		EncryptionScope: tenantScope, // this tenant's customer-managed key
		AccessTier:      blob.AccessTierCool,
		Tags:            map[string]string{"tenant": "acme"}, // stored with the blob, in the same request
		LegalHold:       true,                                // set right after it is stored
	})
	if err != nil {
		return ctx, err // an invalid key
	}
	_, err = disk.Write(ctx, contractKey, strings.NewReader(pdf), filesystem.WriteOptions{})
	return ctx, err
}

// perCall writes a contract with per-call options and a thumbnail with
// the same context against the stand-in, and prints what each Put Blob
// carried; an unknown tier is refused before any request.
// ds:def id=percall-t5c8w3mq
func perCall(ctx context.Context, out io.Writer) error {
	svc := newBlobService()
	srv := httptest.NewTLSServer(svc)
	defer srv.Close()
	client, err := azblob.NewClientWithNoCredential(srv.URL+"/", standInOptions(srv))
	if err != nil {
		return err
	}
	drv, err := azure.New(azure.Config{Client: client, Container: "legal"})
	if err != nil {
		return err
	}
	disk, err := filesystem.NewDisk("legal", drv)
	if err != nil {
		return err
	}
	bound, err := writeContract(ctx, disk, "%PDF-1.7 contract")
	if err != nil {
		return err
	}
	if _, err := disk.Write(bound, "thumbs/acme.png", strings.NewReader("png"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	svc.mu.Lock()
	for _, key := range []string{contractKey, "thumbs/acme.png"} {
		h := svc.puts["/legal/"+key]
		fmt.Fprintf(out, "per call: %s tier=%q scope=%q tags=%q\n", key, h.Get("x-ms-access-tier"), h.Get("x-ms-encryption-scope"), h.Get("x-ms-tags"))
	}
	fmt.Fprintf(out, "per call: legal holds set on %s\n", strings.Join(svc.holds, ","))
	svc.mu.Unlock()
	bad, err := azure.WithRequestOptions(ctx, disk, "a.txt", azure.RequestOptions{AccessTier: "Lukewarm"})
	if err != nil {
		return err
	}
	_, err = disk.Write(bad, "a.txt", strings.NewReader("x"), filesystem.WriteOptions{})
	svc.mu.Lock()
	_, sent := svc.puts["/legal/a.txt"]
	svc.mu.Unlock()
	fmt.Fprintf(out, "per call: unknown tier %s, request sent %t\n", filesystem.CodeOf(err), sent)
	return nil
}

// frontDoorDisk opens a container reached through a front end (a custom
// domain, Azure Front Door, a private-endpoint name) whose host does not
// start with the account: AccountName names the account the user
// delegation SAS is signed for, and the credential is passed again
// because the driver cannot read it back from the client.
// ds:def id=frontdoordisk-r2h6p9wd
func frontDoorDisk(endpoint string, cred azcore.TokenCredential, opts *azblob.ClientOptions, account string) (*filesystem.Disk, error) {
	client, err := azblob.NewClient(endpoint, cred, opts)
	if err != nil {
		return nil, err
	}
	cfg := azure.Config{Client: client, Container: "media", UserDelegation: true}
	if account != "" {
		cfg.AccountName, cfg.Credential, cfg.DelegationClientOptions = account, cred, opts
	}
	drv, err := azure.New(cfg)
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk("media", drv)
}

// signGet signs a one-hour GET for media/a.pdf.
func signGet(ctx context.Context, disk *filesystem.Disk) (*url.URL, error) {
	r, err := disk.SignedURL(ctx, "a.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Hour})
	if err != nil {
		return nil, err
	}
	return url.Parse(r.URL)
}

// frontDoor signs a URL through a front end with and without AccountName,
// then shows the settings New refuses.
// ds:def id=frontdoor-a7m3x5qn
func frontDoor(ctx context.Context, out io.Writer) error {
	svc := newBlobService()
	srv := httptest.NewTLSServer(svc)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	opts := standInOptions(srv)
	named, err := frontDoorDisk(srv.URL+"/", staticToken{}, opts, frontDoorAccount)
	if err != nil {
		return err
	}
	signed, err := signGet(ctx, named)
	if err != nil {
		return err
	}
	byHost, err := frontDoorDisk(srv.URL+"/", staticToken{}, opts, "")
	if err != nil {
		return err
	}
	unnamed, err := signGet(ctx, byHost)
	if err != nil {
		return err
	}
	svc.mu.Lock()
	hosts := append([]string(nil), svc.keyHosts...)
	svc.mu.Unlock()
	sort.Strings(hosts)
	fmt.Fprintf(out, "front door: key requests went to the configured host: %t, signed URL keeps it: %t\n",
		len(hosts) == 2 && hosts[0] == host && hosts[1] == host, signed.Host == host)
	fmt.Fprintf(out, "front door: signature differs from the host-label account's: %t\n", signed.Query().Get("sig") != unnamed.Query().Get("sig"))
	for _, c := range []struct {
		label   string
		account string
		cred    azcore.TokenCredential
	}{{"AccountName without Credential", frontDoorAccount, nil}, {`AccountName "Files.Example"`, "Files.Example", staticToken{}}} {
		client, err := azblob.NewClient(srv.URL+"/", staticToken{}, opts)
		if err != nil {
			return err
		}
		_, err = azure.New(azure.Config{Client: client, Container: "media", UserDelegation: true, AccountName: c.account, Credential: c.cred})
		fmt.Fprintf(out, "front door: %s: %s\n", c.label, filesystem.CodeOf(err))
	}
	return nil
}

// offline runs the steps that need no Azure account, in order: block size
// and concurrency, per-call options, and a disk behind a front end.
func offline(ctx context.Context, out io.Writer) error {
	for _, step := range []func(context.Context, io.Writer) error{largeBlobs, perCall, frontDoor} {
		if err := step(ctx, out); err != nil {
			return err
		}
	}
	return nil
}
