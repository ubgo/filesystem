package azure_test

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
)

// fakeToken is an Entra ID credential that always returns one token.
type fakeToken struct{}

func (fakeToken) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "entra-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// delegationServer fakes Get User Delegation Key: it checks the bearer
// token, records each requested expiry, and answers with a fixed key.
type delegationServer struct {
	*httptest.Server
	status int // non-zero: refuse every request with this status

	mu       sync.Mutex
	expiries []time.Time
	hosts    []string // the Host of each key request
}

func newDelegationServer(t *testing.T) *delegationServer {
	t.Helper()
	s := &delegationServer{}
	s.Server = httptest.NewTLSServer(s)
	t.Cleanup(s.Close)
	return s
}

func (s *delegationServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Query().Get("comp") != "userdelegationkey" || r.Header.Get("Authorization") != "Bearer entra-token" {
		http.Error(w, "unexpected request", http.StatusBadRequest)
		return
	}
	if s.status != 0 {
		w.Header().Set("x-ms-error-code", "AuthorizationPermissionMismatch")
		w.WriteHeader(s.status)
		return
	}
	var info struct {
		Start  string `xml:"Start"`
		Expiry string `xml:"Expiry"`
	}
	body, _ := io.ReadAll(r.Body)
	if err := xml.Unmarshal(body, &info); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	exp, _ := time.Parse(time.RFC3339, info.Expiry)
	s.mu.Lock()
	s.expiries = append(s.expiries, exp)
	s.hosts = append(s.hosts, r.Host)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><UserDelegationKey><SignedOid>oid-1</SignedOid><SignedTid>tid-1</SignedTid><SignedStart>%s</SignedStart><SignedExpiry>%s</SignedExpiry><SignedService>b</SignedService><SignedVersion>2023-11-03</SignedVersion><Value>a2V5LWJ5dGVzLWZvci10ZXN0aW5nLW9ubHk=</Value></UserDelegationKey>`, info.Start, info.Expiry)
}

func (s *delegationServer) fetches() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.expiries...)
}

func (s *delegationServer) client(t *testing.T) *azblob.Client {
	t.Helper()
	c, err := azblob.NewClient(s.URL+"/", fakeToken{}, &azblob.ClientOptions{ClientOptions: policy.ClientOptions{Transport: s.Client(), Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func delegationDisk(t *testing.T, s *delegationServer) (*azure.Driver, *filesystem.Disk) {
	t.Helper()
	drv, err := azure.New(azure.Config{Client: s.client(t), Container: "media", UserDelegation: true})
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("azure", drv)
	if err != nil {
		t.Fatal(err)
	}
	return drv, d
}

// TestUserDelegationSAS proves an Entra ID client signs URLs with a user
// delegation key it fetched with its token, reuses the key, and fetches a
// new one once the cached key would expire before the URL.
func TestUserDelegationSAS(t *testing.T) {
	ctx := t.Context()
	s := newDelegationServer(t)
	drv, d := delegationDisk(t, s)
	if !d.Capabilities().Presign {
		t.Fatal("user delegation does not declare Presign")
	}
	get, err := d.SignedURL(ctx, "a.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Hour, ContentDisposition: "attachment"})
	if err != nil {
		t.Fatal(err)
	}
	q := mustQuery(t, get.URL)
	if q.Get("skoid") != "oid-1" || q.Get("sktid") != "tid-1" || q.Get("sig") == "" || q.Get("sp") != "r" || q.Get("rscd") != "attachment" {
		t.Errorf("GET SAS = %s", get.URL)
	}
	ske, _ := time.Parse(time.RFC3339, q.Get("ske"))
	se, _ := time.Parse(time.RFC3339, q.Get("se"))
	if ske.Before(se) {
		t.Errorf("key expires %v before the URL %v", ske, se)
	}
	put, err := d.SignedURL(ctx, "up.png", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Hour, ContentType: "image/png"})
	if err != nil || mustQuery(t, put.URL).Get("sp") != "cw" || put.Header.Get("x-ms-blob-type") != "BlockBlob" {
		t.Errorf("PUT SAS = %+v, %v", put, err)
	}
	if n := len(s.fetches()); n != 1 {
		t.Fatalf("%d key fetches for two URLs, want 1", n)
	}
	first := s.fetches()[0]
	// Azure grants at most seven days; the driver leaves room for clock skew.
	if lifetime := time.Until(first); lifetime < azure.MaxUserDelegationKeyLifetime-10*time.Minute || lifetime > azure.MaxUserDelegationKeyLifetime-4*time.Minute {
		t.Errorf("key requested for %v", lifetime)
	}

	// A seven-day URL is signed with the cached key and clamped to its
	// expiry, rather than fetching a key that could not reach further.
	week, err := d.SignedURL(ctx, "a.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: filesystem.MaxPresignExpiry})
	if err != nil {
		t.Fatal(err)
	}
	wq := mustQuery(t, week.URL)
	if len(s.fetches()) != 1 || wq.Get("se") != wq.Get("ske") || week.ExpiresAt.After(first) {
		t.Errorf("7-day URL: fetches %d, se %s, ske %s, ExpiresAt %v", len(s.fetches()), wq.Get("se"), wq.Get("ske"), week.ExpiresAt)
	}

	// Six days and two hours later a one-day URL would outlive the key by
	// more than the reuse slack: fetch another.
	later := time.Now().Add(6*24*time.Hour + 2*time.Hour)
	azure.SetClock(drv, func() time.Time { return later })
	if _, err := d.SignedURL(ctx, "a.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if f := s.fetches(); len(f) != 2 || !f[1].After(first) {
		t.Fatalf("key fetches after the cached key ran short = %v", f)
	}
}

// TestUserDelegationOneFetch proves concurrent signers share one key
// request.
func TestUserDelegationOneFetch(t *testing.T) {
	s := newDelegationServer(t)
	_, d := delegationDisk(t, s)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := d.SignedURL(t.Context(), "k", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := len(s.fetches()); n != 1 {
		t.Fatalf("%d key fetches, want 1", n)
	}
}

// TestUserDelegationDenied proves an identity without the delegator role
// gets a permission error, and that a failed fetch is not cached.
func TestUserDelegationDenied(t *testing.T) {
	s := newDelegationServer(t)
	s.status = http.StatusForbidden
	_, d := delegationDisk(t, s)
	_, err := d.SignedURL(t.Context(), "k", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	wantCode(t, err, filesystem.CodePermission)
	s.status = 0
	if _, err := d.SignedURL(t.Context(), "k", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute}); err != nil {
		t.Fatalf("after the role was granted = %v", err)
	}
}

func TestUserDelegationExclusive(t *testing.T) {
	cred, _ := azblob.NewSharedKeyCredential("acct", "a2V5")
	s := newDelegationServer(t)
	_, err := azure.New(azure.Config{Client: s.client(t), Container: "c", SharedKey: cred, UserDelegation: true})
	wantCode(t, err, filesystem.CodeInvalidArgument)
}

// TestTokenOpener proves the opener bound to an Entra ID credential opens
// DSNs by account or endpoint, with user delegation signing.
func TestTokenOpener(t *testing.T) {
	ctx := t.Context()
	mux := filesystem.NewMux()
	_ = mux.Register(azure.Scheme, azure.TokenOpener(fakeToken{}))
	d, err := mux.Open(ctx, "azure", "azblob://media?account=myacct")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Capabilities().Presign {
		t.Error("token disk does not declare Presign")
	}
	c, _ := filesystem.As[*azblob.Client](d)
	if c.URL() != "https://myacct.blob.core.windows.net/" {
		t.Errorf("endpoint from account = %s", c.URL())
	}
	if _, err := mux.Open(ctx, "azure", "azblob://media?endpoint="+url.QueryEscape("https://myacct.blob.core.chinacloudapi.cn")); err != nil {
		t.Errorf("endpoint DSN = %v", err)
	}
	for _, bad := range []string{
		"azblob://?account=a",
		"azblob://c",
		"azblob://c?account=a&endpoint=" + url.QueryEscape("ftp://x"),
		"azblob://c?account=a&connection_env=X",
		"azblob://c?endpoint=" + url.QueryEscape("https://files.example.com"),
		"azblob://c?endpoint=" + url.QueryEscape("http://myacct.blob.core.windows.net"),
		"azblob://c?endpoint=" + url.QueryEscape("https://127.0.0.1:10000/devstoreaccount1"),
		"azblob://c?endpoint=" + url.QueryEscape("https://myacct.blob.core.windows.net:8443"),
		"azblob://c?endpoint=" + url.QueryEscape("https://myacct.blob.core.windows.net/sub"),
		"azblob://c?endpoint=" + url.QueryEscape("https://myacct.files.core.windows.net"),
		"azblob://c?endpoint=" + url.QueryEscape("https://%zz.blob.core.windows.net"),
		"azblob://c?account=a&access_tier=Frozen",
	} {
		if _, err := mux.Open(ctx, "azure", bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("Open(%s) = %v", bad, err)
		}
	}
	nilMux := filesystem.NewMux()
	_ = nilMux.Register(azure.Scheme, azure.TokenOpener(nil))
	if _, err := nilMux.Open(ctx, "azure", "azblob://c?account=a"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("nil credential = %v", err)
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

// TestPresignHeadersUnsupported proves Azure does not claim PresignHeaders
// (a SAS signs no request headers, so it cannot make Azure store or demand
// Cache-Control and the like) and that a signed PUT asking for them is
// refused rather than issued without them.
func TestPresignHeadersUnsupported(t *testing.T) {
	s := newDelegationServer(t)
	_, d := delegationDisk(t, s)
	if d.Capabilities().PresignHeaders {
		t.Fatal("PresignHeaders claimed")
	}
	_, err := d.SignedURL(t.Context(), "k", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, Headers: filesystem.HTTPHeaders{CacheControl: "no-store"}})
	wantCode(t, err, filesystem.CodeUnsupported)
}
