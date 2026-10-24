package s3_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// authRecorder records the access key ID each request was signed with.
type authRecorder struct {
	next http.Handler
	mu   sync.Mutex
	keys []string
}

func (a *authRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// "AWS4-HMAC-SHA256 Credential=<key id>/<date>/<region>/s3/aws4_request, …"
	_, cred, _ := strings.Cut(r.Header.Get("Authorization"), "Credential=")
	key, _, _ := strings.Cut(cred, "/")
	a.mu.Lock()
	a.keys = append(a.keys, key)
	a.mu.Unlock()
	a.next.ServeHTTP(w, r)
}

func (a *authRecorder) take() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := a.keys
	a.keys = nil
	return k
}

// TestPerDiskCredentials proves two disks opened from DSNs in one process
// sign with their own keys, not the process-wide chain, and that neither
// the keys nor the session token leak through formatting.
func TestPerDiskCredentials(t *testing.T) {
	ctx := t.Context()
	_, bucket, proxy := fakeS3WithProxy(t)
	rec := &authRecorder{next: proxy}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ACCESS_KEY_ID", "PROCESSKEY")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "process-secret")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")
	t.Setenv("TENANT_A_KEY", "TENANTAKEY")
	t.Setenv("TENANT_A_SECRET", "tenant-a-very-secret")
	t.Setenv("TENANT_A_TOKEN", "tenant-a-session-token")
	t.Setenv("TENANT_B_KEY", "TENANTBKEY")
	t.Setenv("TENANT_B_SECRET", "tenant-b-very-secret")
	mux := filesystem.NewMux()
	_ = mux.Register(fss3.Scheme, fss3.Opener)
	base := "s3://" + bucket + "?region=us-east-1&path_style=true&endpoint=" + url.QueryEscape(srv.URL)
	for _, c := range []struct{ dsn, key string }{
		{base + "&access_key_env=TENANT_A_KEY&secret_key_env=TENANT_A_SECRET&session_token_env=TENANT_A_TOKEN", "TENANTAKEY"},
		{base + "&access_key_env=TENANT_B_KEY&secret_key_env=TENANT_B_SECRET", "TENANTBKEY"},
		{base, "PROCESSKEY"},
	} {
		d, err := mux.Open(ctx, "s3", c.dsn)
		if err != nil {
			t.Fatal(err)
		}
		rec.take()
		if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		for _, k := range rec.take() {
			if k != c.key {
				t.Errorf("%s signed with %q, want %q", c.dsn, k, c.key)
			}
		}
		client, _ := filesystem.As[*s3.Client](d)
		opts := client.Options()
		for idx, s := range []string{
			fmt.Sprintf("%v %+v %#v", d, d, d),
			fmt.Sprintf("%v %+v %#v", client, client, client),
			fmt.Sprintf("%v %+v %#v", opts, opts, opts),
			fmt.Sprintf("%v %+v %#v %s", opts.Credentials, opts.Credentials, opts.Credentials, opts.Credentials),
		} {
			for _, secret := range []string{"tenant-a-very-secret", "tenant-a-session-token", "tenant-b-very-secret"} {
				if i := strings.Index(s, secret); i >= 0 {
					t.Fatalf("formatting %d leaked %q: …%s…", idx, secret, s[max(0, i-1500):i])
				}
			}
		}
	}
}

// TestCredentialsPrinters calls the provider's String and GoString
// directly, as code that type-asserts fmt.Stringer (rather than formatting
// with fmt, which uses Format) would, and requires the placeholder.
func TestCredentialsPrinters(t *testing.T) {
	t.Setenv("PRINT_KEY", "PRINTKEY")
	t.Setenv("PRINT_SECRET", "print-very-secret")
	u, _ := url.Parse("s3://b?region=us-east-1&access_key_env=PRINT_KEY&secret_key_env=PRINT_SECRET")
	drv, err := fss3.Opener(t.Context(), u)
	if err != nil {
		t.Fatal(err)
	}
	creds := drv.(*fss3.Driver).Native().Options().Credentials
	s, ok := creds.(fmt.Stringer)
	g, gok := creds.(fmt.GoStringer)
	if !ok || !gok {
		t.Fatalf("%T does not implement fmt.Stringer and fmt.GoStringer", creds)
	}
	for _, out := range []string{s.String(), g.GoString()} {
		if strings.Contains(out, "print-very-secret") || strings.Contains(out, "PRINTKEY") || !strings.Contains(out, "redacted") {
			t.Errorf("printer = %q", out)
		}
	}
}

// TestProfileCredentials proves profile selects a shared-config profile
// for one disk.
func TestProfileCredentials(t *testing.T) {
	ctx := t.Context()
	_, bucket, proxy := fakeS3WithProxy(t)
	rec := &authRecorder{next: proxy}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	creds := filepath.Join(dir, "credentials")
	_ = os.WriteFile(creds, []byte("[default]\naws_access_key_id = DEFAULTKEY\naws_secret_access_key = d\n\n[partner]\naws_access_key_id = PARTNERKEY\naws_secret_access_key = p\n"), 0o600)
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", creds)
	mux := filesystem.NewMux()
	_ = mux.Register(fss3.Scheme, fss3.Opener)
	d, err := mux.Open(ctx, "s3", "s3://"+bucket+"?region=us-east-1&path_style=true&profile=partner&endpoint="+url.QueryEscape(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	rec.take()
	if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, k := range rec.take() {
		if k != "PARTNERKEY" {
			t.Errorf("signed with %q, want the partner profile's key", k)
		}
	}
	if _, err := mux.Open(ctx, "s3", "s3://"+bucket+"?region=us-east-1&profile=missing"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("unknown profile = %v", err)
	}
}

// TestCredentialParamErrors proves malformed credential parameters are
// refused and that the refusal never contains a secret.
func TestCredentialParamErrors(t *testing.T) {
	t.Setenv("SET_KEY", "SETKEY")
	t.Setenv("SET_SECRET", "set-very-secret")
	t.Setenv("EMPTY", "")
	mux := filesystem.NewMux()
	_ = mux.Register(fss3.Scheme, fss3.Opener)
	for _, dsn := range []string{
		"s3://b?access_key_env=SET_KEY",
		"s3://b?secret_key_env=SET_SECRET",
		"s3://b?session_token_env=SET_SECRET",
		"s3://b?access_key_env=SET_KEY&secret_key_env=UNSET_VAR_FOR_TEST",
		"s3://b?access_key_env=SET_KEY&secret_key_env=EMPTY",
		"s3://b?access_key_env=SET_KEY&secret_key_env=SET_SECRET&session_token_env=UNSET_VAR_FOR_TEST",
		"s3://b?access_key_env=SET_KEY&secret_key_env=SET_SECRET&profile=p",
		"s3://SETKEY:set-very-secret@b?region=us-east-1",
	} {
		_, err := mux.Open(t.Context(), "s3", dsn)
		if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("Open(%s) = %v", dsn, err)
			continue
		}
		if msg := fmt.Sprintf("%v %+v", err, err); strings.Contains(msg, "set-very-secret") {
			t.Errorf("Open(%s) error leaks the secret: %s", dsn, msg)
		}
	}
}
