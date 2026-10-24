package gcs_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/option"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gcs"
)

const signer = "files@proj.iam.gserviceaccount.com"

// fakeIAM stands in for the IAM Credentials API: it answers signBlob for
// one service account by signing with a local RSA key, the way Google signs
// with the account's own key, and records each payload.
type fakeIAM struct {
	key    *rsa.PrivateKey
	status int // non-zero: answer every call with this status

	mu       sync.Mutex
	payloads [][]byte
	paths    []string
}

func (f *fakeIAM) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "iamcredentials.googleapis.com" {
		return nil, &url.Error{Op: r.Method, URL: r.URL.String(), Err: io.EOF}
	}
	reply := func(status int, body string) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
	if f.status != 0 {
		return reply(f.status, `{"error":{"code":403,"message":"Permission 'iam.serviceAccounts.signBlob' denied"}}`)
	}
	var req struct {
		Payload string `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return reply(http.StatusBadRequest, `{}`)
	}
	payload, _ := base64.StdEncoding.DecodeString(req.Payload)
	sum := sha256.Sum256(payload)
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.payloads = append(f.payloads, payload)
	f.paths = append(f.paths, r.URL.Path)
	f.mu.Unlock()
	out, _ := json.Marshal(map[string]string{"keyId": "k1", "signedBlob": base64.StdEncoding.EncodeToString(sig)})
	return reply(http.StatusOK, string(out))
}

func keylessDisk(t *testing.T, iam *fakeIAM) *filesystem.Disk {
	t.Helper()
	client, err := storage.NewClient(t.Context(), option.WithHTTPClient(&http.Client{Transport: iam}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return disk(t, gcs.Config{Client: client, Bucket: "b", Signing: &storage.SignedURLOptions{GoogleAccessID: signer}})
}

// TestKeylessSigning proves a Signing with an account and no key signs
// through IAM signBlob as that account, and that the URL carries the
// signature IAM returned over the string the SDK built.
func TestKeylessSigning(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	iam := &fakeIAM{key: key}
	d := keylessDisk(t, iam)
	if !d.Capabilities().Presign {
		t.Fatal("keyless signing does not declare Presign")
	}
	got, err := d.SignedURL(t.Context(), "report.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(got.URL)
	q := u.Query()
	if !strings.HasPrefix(q.Get("X-Goog-Credential"), signer+"/") {
		t.Errorf("credential = %q, want the signing account", q.Get("X-Goog-Credential"))
	}
	if len(iam.payloads) != 1 || !strings.Contains(iam.paths[0], "serviceAccounts/"+signer+":signBlob") {
		t.Fatalf("signBlob calls = %v", iam.paths)
	}
	payload := iam.payloads[0]
	if !strings.HasPrefix(string(payload), "GOOG4-RSA-SHA256\n"+q.Get("X-Goog-Date")+"\n") {
		t.Errorf("signed payload is not the V4 string to sign: %q", payload)
	}
	sig, err := hex.DecodeString(q.Get("X-Goog-Signature"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Errorf("URL signature does not verify against the account's key: %v", err)
	}
}

// TestKeylessSigningDenied proves a refused signBlob (the caller lacks
// roles/iam.serviceAccountTokenCreator) is a permission error.
func TestKeylessSigningDenied(t *testing.T) {
	d := keylessDisk(t, &fakeIAM{status: http.StatusForbidden})
	_, err := d.SignedURL(t.Context(), "k", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	if filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("denied signBlob = %v", err)
	}
}

// metadataServer fakes the GCE metadata server's default service account
// email; status non-zero makes it fail.
func metadataServer(t *testing.T, email string, status int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" || r.URL.Path != "/computeMetadata/v1/instance/service-accounts/default/email" {
			http.NotFound(w, r)
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		_, _ = io.WriteString(w, email)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(srv.URL, "http://"))
}

func openGCS(ctx context.Context, t *testing.T, dsn string) (*gcs.Driver, error) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	drv, err := gcs.Opener(ctx, u)
	if err != nil {
		return nil, err
	}
	d, ok := drv.(*gcs.Driver)
	if !ok {
		t.Fatalf("Opener returned %T", drv)
	}
	return d, nil
}

// TestOpenerSigningAccount proves signing_account configures keyless
// signing, by name or from the metadata server, and is refused when it
// cannot work.
func TestOpenerSigningAccount(t *testing.T) {
	ctx := t.Context()
	d, err := openGCS(ctx, t, "gs://b?no_auth=true&signing_account="+url.QueryEscape(signer))
	if err != nil {
		t.Fatal(err)
	}
	if s := gcs.SigningOf(d); s == nil || s.GoogleAccessID != signer || s.PrivateKey != nil || s.SignBytes != nil || !d.Capabilities().Presign {
		t.Fatalf("named account: signing = %+v", s)
	}
	if gcs.IAMOf(d) == nil {
		t.Error("the opener did not give keyless signing an IAM client")
	}

	metadataServer(t, "runtime@proj.iam.gserviceaccount.com", 0)
	d, err = openGCS(ctx, t, "gs://b?no_auth=true&signing_account=default")
	if err != nil {
		t.Fatal(err)
	}
	if s := gcs.SigningOf(d); s == nil || s.GoogleAccessID != "runtime@proj.iam.gserviceaccount.com" {
		t.Fatalf("default account: signing = %+v", s)
	}

	metadataServer(t, "", http.StatusInternalServerError)
	if _, err := openGCS(ctx, t, "gs://b?no_auth=true&signing_account=default"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("failing metadata server = %v", err)
	}
	metadataServer(t, "", 0)
	if _, err := openGCS(ctx, t, "gs://b?no_auth=true&signing_account=default"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("metadata server without an account = %v", err)
	}

	keyFile := filepath.Join(t.TempDir(), "sa.json")
	raw, _ := json.Marshal(map[string]string{"client_email": signer, "private_key": string(pemKey(t))})
	_ = os.WriteFile(keyFile, raw, 0o600)
	if _, err := openGCS(ctx, t, "gs://b?no_auth=true&signing_account=x&signing_key="+url.QueryEscape(keyFile)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("signing_key with signing_account = %v", err)
	}
}

// TestOpenerSigningAccountOffGoogle proves signing_account=default off
// Google Cloud fails at open. The context is already cancelled, so the
// metadata probe fails at once without touching the network.
func TestOpenerSigningAccountOffGoogle(t *testing.T) {
	t.Setenv("GCE_METADATA_HOST", "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := openGCS(ctx, t, "gs://b?no_auth=true&signing_account=default"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("default account off Google Cloud = %v", err)
	}
}

// iamDisk is a keyless disk whose storage client was built from a service
// account key of another account, and which signs through an IAM client
// talking to iam.
func iamDisk(t *testing.T, iam http.RoundTripper) *filesystem.Disk {
	t.Helper()
	keyJSON, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "other@proj.iam.gserviceaccount.com", "private_key": string(pemKey(t)), "token_uri": "https://oauth2.googleapis.com/token"})
	client, err := storage.NewClient(t.Context(), option.WithAuthCredentialsJSON(option.ServiceAccount, keyJSON))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	svc, err := iamcredentials.NewService(t.Context(), option.WithHTTPClient(&http.Client{Transport: iam}))
	if err != nil {
		t.Fatal(err)
	}
	return disk(t, gcs.Config{Client: client, Bucket: "b", Signing: &storage.SignedURLOptions{GoogleAccessID: signer}, IAM: svc})
}

// TestKeylessSigningWithIAM proves that with Config.IAM the URL is signed
// by Google as the named account, even when the storage client holds a
// key for another account (which the SDK alone would sign with, producing
// URLs GCS rejects).
func TestKeylessSigningWithIAM(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	iam := &fakeIAM{key: key}
	d := iamDisk(t, iam)
	got, err := d.SignedURL(t.Context(), "r.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	q := mustParse(t, got.URL)
	if !strings.HasPrefix(q.Get("X-Goog-Credential"), signer+"/") || len(iam.payloads) != 1 {
		t.Fatalf("credential %q, signBlob calls %d", q.Get("X-Goog-Credential"), len(iam.payloads))
	}
	sig, _ := hex.DecodeString(q.Get("X-Goog-Signature"))
	sum := sha256.Sum256(iam.payloads[0])
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Errorf("URL not signed by the account's key: %v", err)
	}
}

// hangingIAM never answers until the request's context ends.
type hangingIAM struct{}

func (hangingIAM) RoundTrip(r *http.Request) (*http.Response, error) {
	<-r.Context().Done()
	return nil, r.Context().Err()
}

// TestKeylessSigningHonoursContext proves a slow signBlob is abandoned at
// the caller's deadline, as unavailable.
func TestKeylessSigningHonoursContext(t *testing.T) {
	d := iamDisk(t, hangingIAM{})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := d.SignedURL(ctx, "k", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable || time.Since(start) > 2*time.Second {
		t.Fatalf("signing past the deadline = %v after %v", err, time.Since(start))
	}
}

func mustParse(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

// TestOpenerSigningAccountBadCredentials proves keyless signing with
// unusable default credentials is refused at open, as the storage client
// is.
func TestOpenerSigningAccountBadCredentials(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"type":"user"}`), 0o600)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", bad)
	if _, err := openGCS(t.Context(), t, "gs://b?signing_account="+url.QueryEscape(signer)); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Errorf("signing_account with unusable credentials = %v", err)
	}
}
