package azure_test

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
)

// keySkew mirrors the driver's back-dating of a user delegation key's
// start and shortening of its expiry, so a reference key asks for the
// same validity and comes back with the same signing parameters.
const keySkew = 5 * time.Minute

// referenceCredential is a user delegation key fetched at now that the
// SDK itself binds to account: the reference client's host starts with
// account (a name under example.com, which the test certificate covers)
// and its transport dials s, so the key's bytes and dates match the
// driver's.
func referenceCredential(t *testing.T, s *delegationServer, account string, now time.Time) *sas.UserDelegationCredential {
	t.Helper()
	tr := s.Client().Transport.(*http.Transport).Clone()
	addr := s.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	c, err := service.NewClient("https://"+account+".example.com/", fakeToken{}, &service.ClientOptions{ClientOptions: policy.ClientOptions{Transport: &http.Client{Transport: tr}}})
	if err != nil {
		t.Fatal(err)
	}
	start, expiry := now.Add(-keySkew).UTC().Format(sas.TimeFormat), now.Add(azure.MaxUserDelegationKeyLifetime-keySkew).UTC().Format(sas.TimeFormat)
	cred, err := c.GetUserDelegationCredential(t.Context(), service.KeyInfo{Start: &start, Expiry: &expiry}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return cred
}

// referenceSignature is the signature of the one-hour GET SAS for
// media/a.pdf signed at now with referenceCredential's key for account.
func referenceSignature(t *testing.T, s *delegationServer, account string, now time.Time) string {
	t.Helper()
	cred := referenceCredential(t, s, account, now)
	v := sas.BlobSignatureValues{Protocol: sas.ProtocolHTTPS, ExpiryTime: now.Add(time.Hour).UTC(), ContainerName: "media", BlobName: "a.pdf", Permissions: (&sas.BlobPermissions{Read: true}).String()}
	q, err := v.SignWithUserDelegation(cred)
	if err != nil {
		t.Fatal(err)
	}
	return q.Signature()
}

// TestAccountNameSigning proves Config.AccountName signs user delegation
// URLs for that account while the key request and the URLs keep the
// configured host, and that without it the host's first label is still
// the account (today's behaviour).
func TestAccountNameSigning(t *testing.T) {
	ctx := t.Context()
	now := time.Now().Truncate(time.Second)
	s := newDelegationServer(t)
	serverHost := strings.TrimPrefix(s.URL, "https://")
	open := func(account string) *filesystem.Disk {
		t.Helper()
		cfg := azure.Config{Client: s.client(t), Container: "media", UserDelegation: true, Clock: func() time.Time { return now }}
		if account != "" {
			cfg.AccountName, cfg.Credential = account, fakeToken{}
			cfg.DelegationClientOptions = &azblob.ClientOptions{ClientOptions: policy.ClientOptions{Transport: s.Client(), Retry: policy.RetryOptions{MaxRetries: -1}}}
		}
		drv, err := azure.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		d, err := filesystem.NewDisk("azure", drv)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	sign := func(d *filesystem.Disk) *url.URL {
		t.Helper()
		r, err := d.SignedURL(ctx, "a.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(r.URL)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}

	// Default: the account is the host's first label ("127" here).
	def := sign(open(""))
	if got, want := def.Query().Get("sig"), referenceSignature(t, s, "127", now); got != want {
		t.Errorf("default signature %s, want the one for account 127, %s", got, want)
	}

	custom := sign(open("myacct"))
	if got, want := custom.Query().Get("sig"), referenceSignature(t, s, "myacct", now); got != want {
		t.Errorf("AccountName signature %s, want the one for myacct, %s", got, want)
	}
	if custom.Host != serverHost {
		t.Errorf("signed URL host %s, want the configured %s", custom.Host, serverHost)
	}
	// Fetches: default driver, reference 127, AccountName driver, reference myacct.
	s.mu.Lock()
	hosts := append([]string(nil), s.hosts...)
	s.mu.Unlock()
	if len(hosts) != 4 || hosts[2] != serverHost {
		t.Errorf("key requests went to %v, want the AccountName one (third) at %s", hosts, serverHost)
	}
}

// TestAccountNameConfig proves AccountName's companions are required and
// checked, and that they are refused without it rather than ignored.
func TestAccountNameConfig(t *testing.T) {
	s := newDelegationServer(t)
	cases := map[string]azure.Config{
		"no UserDelegation":    {AccountName: "myacct", Credential: fakeToken{}},
		"no Credential":        {AccountName: "myacct", UserDelegation: true},
		"bad name":             {AccountName: "my.acct", Credential: fakeToken{}, UserDelegation: true},
		"upper case":           {AccountName: "MyAcct", Credential: fakeToken{}, UserDelegation: true},
		"Credential alone":     {Credential: fakeToken{}, UserDelegation: true},
		"client options alone": {DelegationClientOptions: &azblob.ClientOptions{}, UserDelegation: true},
	}
	for name, cfg := range cases {
		cfg.Client, cfg.Container = s.client(t), "media"
		if _, err := azure.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: New = %v", name, err)
		}
	}
	// A client with no endpoint host has nothing to rewrite requests to.
	hostless, err := azblob.NewClientWithNoCredential("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := azure.New(azure.Config{Client: hostless, Container: "media", AccountName: "myacct", Credential: fakeToken{}, UserDelegation: true}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("a hostless client: New = %v", err)
	}
}

// TestUserDelegationCredentialRefused proves a key request the service
// refuses is the mapped error, not a credential.
func TestUserDelegationCredentialRefused(t *testing.T) {
	drv, _, s := scriptedDriver(t, azure.Config{UserDelegation: true})
	s.on("comp=userdelegationkey", failure(http.StatusForbidden, "AuthorizationPermissionMismatch"))
	cred, _, err := drv.UserDelegationCredential(t.Context(), time.Now().Add(time.Hour))
	if cred != nil || filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("a refused key request = %v, %v", cred, err)
	}
}

// TestTokenOpenerFrontEnd proves the token opener accepts a custom
// endpoint together with the account, keeping the endpoint as the
// client's host and declaring Presign.
func TestTokenOpenerFrontEnd(t *testing.T) {
	mux := filesystem.NewMux()
	_ = mux.Register(azure.Scheme, azure.TokenOpener(fakeToken{}))
	d, err := mux.Open(t.Context(), "azure", "azblob://media?account=myacct&endpoint="+url.QueryEscape("https://files.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if !d.Capabilities().Presign {
		t.Error("front-end disk does not declare Presign")
	}
	if c, _ := filesystem.As[*azblob.Client](d); c.URL() != "https://files.example.com/" {
		t.Errorf("endpoint = %s", c.URL())
	}
	for _, bad := range []string{
		"azblob://media?account=myacct&endpoint=" + url.QueryEscape("http://files.example.com"),
		"azblob://media?account=My.Acct&endpoint=" + url.QueryEscape("https://files.example.com"),
	} {
		if _, err := mux.Open(t.Context(), "azure", bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("Open(%s) = %v", bad, err)
		}
	}
}

// TestUserDelegationCredentialForDelegatePrefix proves the exported key
// is the driver's own, bound to Config.AccountName, so a prefix SAS from
// DelegatePrefix signs for that account behind a custom host; and that a
// disk without UserDelegation refuses it.
func TestUserDelegationCredentialForDelegatePrefix(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s := newDelegationServer(t)
	drv, err := azure.New(azure.Config{
		Client: s.client(t), Container: "media", UserDelegation: true, Clock: func() time.Time { return now },
		AccountName: "myacct", Credential: fakeToken{},
		DelegationClientOptions: &azblob.ClientOptions{ClientOptions: policy.ClientOptions{Transport: s.Client(), Retry: policy.RetryOptions{MaxRetries: -1}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cred, exp, err := drv.UserDelegationCredential(t.Context(), now.Add(time.Hour))
	if err != nil || exp.Before(now.Add(time.Hour)) {
		t.Fatalf("credential expiring %v, %v", exp, err)
	}
	opts := azure.DelegateOptions{ContainerURL: s.URL + "/media", Prefix: "users/42", Expires: time.Hour, Clock: func() time.Time { return now }}
	opts.UserDelegation = cred
	got, err := azure.DelegatePrefix(opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.UserDelegation = referenceCredential(t, s, "myacct", now)
	want, err := azure.DelegatePrefix(opts)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != want.Token {
		t.Errorf("prefix SAS %s, want the one signed for myacct, %s", got.Token, want.Token)
	}
	plain, _, _ := scriptedDriver(t, azure.Config{})
	_, _, err = plain.UserDelegationCredential(t.Context(), now)
	wantCode(t, err, filesystem.CodeUnsupported)
}
