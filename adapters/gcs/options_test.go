package gcs_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gcs"
)

// Per-tenant Cloud KMS keys of the tests below.
const (
	tenantAKey = "projects/p/locations/eu/keyRings/r/cryptoKeys/tenant-a"
	tenantBKey = "projects/p/locations/eu/keyRings/r/cryptoKeys/tenant-b"
)

func tenantKMS(key string) string {
	switch {
	case strings.HasPrefix(key, "tenants/a/"):
		return tenantAKey
	case strings.HasPrefix(key, "tenants/b/"):
		return tenantBKey
	}
	return ""
}

// TestKMSKeyFor proves KMSKeyFor picks each tenant's key on writes, copies
// and presigned PUTs, falls back to KMSKeyName outside every tenant, and
// is refused beside a CustomerKey.
func TestKMSKeyFor(t *testing.T) {
	ctx := t.Context()
	client, fake, p, _ := proxied(t)
	d := disk(t, gcs.Config{Client: client, Bucket: newBucket(t, fake), KMSKeyName: testKMSKey, KMSKeyFor: tenantKMS,
		Signing: &storage.SignedURLOptions{GoogleAccessID: "svc@example.iam.gserviceaccount.com", PrivateKey: pemKey(t)}})
	for key, want := range map[string]string{"tenants/a/x": tenantAKey, "tenants/b/x": tenantBKey, "shared/x": testKMSKey} {
		if _, err := d.WriteBytes(ctx, key, []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		if len(p.sent("POST", "uploadType=", "kmsKeyName="+url.QueryEscape(want))) == 0 {
			t.Errorf("write of %s did not name %s", key, want)
		}
	}
	if _, err := d.Copy(ctx, "shared/x", "tenants/b/copy"); err != nil {
		t.Fatal(err)
	}
	if len(p.sent("POST", "rewriteTo", "destinationKmsKeyName="+url.QueryEscape(tenantBKey))) == 0 {
		t.Error("copy into tenant b did not name its key")
	}
	put, err := d.SignedURL(ctx, "tenants/a/direct", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if put.Header.Get("X-Goog-Encryption-Kms-Key-Name") != tenantAKey {
		t.Errorf("presigned PUT header = %v", put.Header)
	}
	key, _ := gcs.NewCustomerKey(make([]byte, 32))
	_, err = gcs.New(gcs.Config{Client: client, Bucket: "b", KMSKeyFor: tenantKMS, CustomerKey: key})
	wantCode(t, err, filesystem.CodeInvalidArgument)
}

// TestClock proves Config.Clock dates RetainFor retentions and a form
// upload's policy.
func TestClock(t *testing.T) {
	ctx := t.Context()
	at := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	clock := func() time.Time { return at }
	drv, d, _ := advanced(t, gcs.Config{RetainFor: time.Hour, Clock: clock}, false)
	if _, err := d.WriteBytes(ctx, "kept", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	info, err := drv.ObjectInfo(ctx, "kept")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Hold.RetainUntil.Equal(at.Add(time.Hour)) {
		t.Errorf("retain until %v, want an hour after the clock (%v)", info.Hold.RetainUntil, at.Add(time.Hour))
	}

	client, fake, _, _ := proxied(t)
	_, keyPEM := rsaKeyPEM(t)
	pd := disk(t, gcs.Config{Client: client, Bucket: newBucket(t, fake), Clock: clock,
		Signing: &storage.SignedURLOptions{GoogleAccessID: "svc@example.iam.gserviceaccount.com", PrivateKey: keyPEM}})
	post, err := pd.PresignPost(ctx, filesystem.PostPolicy{Key: "up", Expires: time.Hour, MaxSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !post.ExpiresAt.Equal(at.Add(time.Hour)) || post.Fields["x-goog-date"] != at.Format("20060102T150405Z") {
		t.Errorf("form ExpiresAt %v, date %q; want the clock's", post.ExpiresAt, post.Fields["x-goog-date"])
	}
}

// serviceAccountJSON returns a service account key for
// email whose token endpoint is a local server answering token.
func serviceAccountJSON(t *testing.T, email, token string) []byte {
	t.Helper()
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": 3600})
	}))
	t.Cleanup(tokens.Close)
	raw, err := json.Marshal(map[string]string{
		"type": "service_account", "client_email": email, "private_key": string(pemKey(t)),
		"private_key_id": "k1", "token_uri": tokens.URL, "project_id": "p",
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// caller names who a request authenticated as: the token the local token
// endpoint issued, or the issuer of a self-signed JWT.
func caller(h http.Header) string {
	tok := strings.TrimPrefix(h.Get("Authorization"), "Bearer ")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return tok
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return tok
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	_ = json.Unmarshal(payload, &claims)
	return claims.Iss
}

// TestOpenerCredentials proves two gs:// disks in one process act as two
// service accounts, one from credentials_file and one from
// credentials_env, and that the parameters refuse what is not a service
// account key or is combined with another source.
func TestOpenerCredentials(t *testing.T) {
	ctx := t.Context()
	_, fake, p, base := proxied(t)
	bucket := newBucket(t, fake)
	file := filepath.Join(t.TempDir(), "a.json")
	if err := os.WriteFile(file, serviceAccountJSON(t, "a@p.iam.gserviceaccount.com", "token-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GCS_KEY_B", string(serviceAccountJSON(t, "b@p.iam.gserviceaccount.com", "token-b")))
	ep := "&endpoint=" + url.QueryEscape(base+"/storage/v1/")
	mux := filesystem.NewMux()
	_ = mux.Register(gcs.Scheme, gcs.Opener)
	for dsn, want := range map[string][]string{
		"gs://" + bucket + "?credentials_file=" + url.QueryEscape(file) + ep: {"token-a", "a@p.iam.gserviceaccount.com"},
		"gs://" + bucket + "?credentials_env=GCS_KEY_B" + ep:                 {"token-b", "b@p.iam.gserviceaccount.com"},
	} {
		d, err := mux.Open(ctx, "gcs", dsn)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		p.mu.Lock()
		got := caller(p.headers[len(p.headers)-1])
		p.mu.Unlock()
		if got != want[0] && got != want[1] {
			t.Errorf("%s: authenticated as %q, want %v", dsn, got, want)
		}
	}
	notKey := filepath.Join(t.TempDir(), "x.json")
	_ = os.WriteFile(notKey, []byte(`{"type":"authorized_user"}`), 0o600)
	t.Setenv("GCS_EMPTY", "")
	for _, q := range []string{
		"credentials_file=" + url.QueryEscape(notKey),
		"credentials_file=/does/not/exist",
		"credentials_env=GCS_EMPTY",
		"credentials_env=GCS_KEY_B&no_auth=true",
		"credentials_env=GCS_KEY_B&credentials_file=" + url.QueryEscape(file),
	} {
		if _, err := mux.Open(ctx, "gcs", "gs://"+bucket+"?"+q+ep); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v, want CodeInvalidArgument", q, err)
		}
	}
}
