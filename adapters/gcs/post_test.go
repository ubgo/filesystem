package gcs_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/option"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gcs"
	"github.com/ubgo/filesystem/filesystemtest"
)

// rsaKeyPEM returns a fresh RSA key and its PKCS #8 PEM, the shape of a
// service account JSON key's private_key.
func rsaKeyPEM(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	return k, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// verifyPost checks a form's signature against pub: RSA-SHA256 over the
// base64 policy, hex-encoded, and returns the decoded policy.
func verifyPost(t *testing.T, pub *rsa.PublicKey, post filesystem.PresignedPost) string {
	t.Helper()
	sig, err := hex.DecodeString(post.Fields["x-goog-signature"])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(post.Fields["policy"]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		t.Errorf("signature does not verify: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(post.Fields["policy"])
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestPresignPostPolicy proves the form pins the size range, the key, the
// type, the stored headers and the KMS key, carries every pinned value as
// a field, and is signed with the disk's key.
func TestPresignPostPolicy(t *testing.T) {
	ctx := t.Context()
	key, keyPEM := rsaKeyPEM(t)
	srv := fakeServer(t)
	d := disk(t, gcs.Config{Client: srv.Client(), Bucket: "media", KMSKeyName: testKMSKey, Signing: &storage.SignedURLOptions{GoogleAccessID: signer, PrivateKey: keyPEM}})
	if !d.Capabilities().PresignPost {
		t.Fatal("PresignPost not declared")
	}
	post, err := d.PresignPost(ctx, filesystem.PostPolicy{Key: "in/a.png", ContentType: "image/png", MinSize: 1, MaxSize: 1 << 20, Expires: 10 * time.Minute, Headers: filesystem.HTTPHeaders{CacheControl: "no-store"}})
	if err != nil {
		t.Fatal(err)
	}
	if post.URL != "https://storage.googleapis.com/media/" {
		t.Errorf("URL = %s", post.URL)
	}
	raw := verifyPost(t, &key.PublicKey, post)
	for _, want := range []string{
		`["content-length-range",1,1048576]`, `{"bucket":"media"}`, `{"key":"in/a.png"}`, `{"Content-Type":"image/png"}`,
		`{"Cache-Control":"no-store"}`, `{"X-Goog-Encryption-Kms-Key-Name":"` + testKMSKey + `"}`, `{"x-goog-algorithm":"GOOG4-RSA-SHA256"}`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("policy lacks %s:\n%s", want, raw)
		}
	}
	if post.Fields["key"] != "in/a.png" || post.Fields["Content-Type"] != "image/png" || post.Fields["X-Goog-Encryption-Kms-Key-Name"] != testKMSKey ||
		!strings.HasPrefix(post.Fields["x-goog-credential"], signer+"/") {
		t.Errorf("fields = %v", post.Fields)
	}
	if !strings.Contains(raw, `"expiration":"`+post.ExpiresAt.UTC().Format(time.RFC3339)+`"`) {
		t.Errorf("policy expiration does not match ExpiresAt %v:\n%s", post.ExpiresAt, raw)
	}
}

// TestPresignPostPrefix proves a prefix form lets the client choose any
// key under the prefix (starts-with) rather than one exact key.
func TestPresignPostPrefix(t *testing.T) {
	key, keyPEM := rsaKeyPEM(t)
	srv := fakeServer(t)
	d := disk(t, gcs.Config{Client: srv.Client(), Bucket: "media", Signing: &storage.SignedURLOptions{GoogleAccessID: signer, PrivateKey: keyPEM}})
	post, err := d.PresignPost(t.Context(), filesystem.PostPolicy{KeyPrefix: "users/42", MaxSize: 100, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	raw := verifyPost(t, &key.PublicKey, post)
	if !strings.Contains(raw, `["starts-with","$key","users/42/"]`) || strings.Contains(raw, `{"key":`) || strings.Contains(raw, "Content-Type") {
		t.Errorf("prefix policy:\n%s", raw)
	}
}

// TestPresignPostKeyless proves a keyless disk signs forms through IAM as
// its account, and that without an IAM client it does not claim forms.
func TestPresignPostKeyless(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	iam := &fakeIAM{key: key}
	svc, err := iamcredentials.NewService(t.Context(), option.WithHTTPClient(&http.Client{Transport: iam}))
	if err != nil {
		t.Fatal(err)
	}
	srv := fakeServer(t)
	d := disk(t, gcs.Config{Client: srv.Client(), Bucket: "media", Signing: &storage.SignedURLOptions{GoogleAccessID: signer}, IAM: svc})
	post, err := d.PresignPost(t.Context(), filesystem.PostPolicy{Key: "a.txt", MaxSize: 10, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	verifyPost(t, &key.PublicKey, post)
	if len(iam.paths) != 1 || !strings.Contains(iam.paths[0], signer) {
		t.Errorf("signBlob calls = %v", iam.paths)
	}
	if noIAM := disk(t, gcs.Config{Client: srv.Client(), Bucket: "media", Signing: &storage.SignedURLOptions{GoogleAccessID: signer}}); noIAM.Capabilities().PresignPost {
		t.Error("PresignPost claimed for keyless signing without an IAM client")
	}
	denied := disk(t, gcs.Config{Client: srv.Client(), Bucket: "media", Signing: &storage.SignedURLOptions{GoogleAccessID: signer}, IAM: mustIAM(t, &fakeIAM{status: http.StatusForbidden})})
	if _, err := denied.PresignPost(t.Context(), filesystem.PostPolicy{Key: "a.txt", MaxSize: 10, Expires: time.Minute}); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Errorf("denied signBlob = %v", err)
	}
}

// TestPresignPostBadKey covers keys that cannot sign: not PEM, not RSA,
// not a key at all, and a SignBytes that fails.
func TestPresignPostBadKey(t *testing.T) {
	srv := fakeServer(t)
	_, ecKey := ecKeyPEM(t)
	for name, s := range map[string]*storage.SignedURLOptions{
		"not PEM":    {GoogleAccessID: signer, PrivateKey: []byte("nope")},
		"not RSA":    {GoogleAccessID: signer, PrivateKey: ecKey},
		"bad PKCS1":  {GoogleAccessID: signer, PrivateKey: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("x")})},
		"sign fails": {GoogleAccessID: signer, SignBytes: func([]byte) ([]byte, error) { return nil, errSign }},
	} {
		d := disk(t, gcs.Config{Client: srv.Client(), Bucket: "media", Signing: s})
		if _, err := d.PresignPost(t.Context(), filesystem.PostPolicy{Key: "a.txt", MaxSize: 10, Expires: time.Minute}); err == nil {
			t.Errorf("%s: signed", name)
		}
	}
	pkcs1 := disk(t, gcs.Config{Client: srv.Client(), Bucket: "media", Signing: &storage.SignedURLOptions{GoogleAccessID: signer, PrivateKey: pemKey(t)}})
	if _, err := pkcs1.PresignPost(t.Context(), filesystem.PostPolicy{Key: "a.txt", MaxSize: 10, Expires: time.Minute}); err != nil {
		t.Errorf("PKCS #1 key: %v", err)
	}
}

var errSign = errorString("signer offline")

type errorString string

func (e errorString) Error() string { return string(e) }

func ecKeyPEM(t *testing.T) (crypto.Signer, []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	return k, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func mustIAM(t *testing.T, rt http.RoundTripper) *iamcredentials.Service {
	t.Helper()
	svc, err := iamcredentials.NewService(t.Context(), option.WithHTTPClient(&http.Client{Transport: rt}))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// TestConformanceSigned runs the suite on a disk that can sign, so the
// signed URL and POST form cases run too.
func TestConformanceSigned(t *testing.T) {
	client, fake := conformanceFake(t)
	_, keyPEM := rsaKeyPEM(t)
	filesystemtest.RunWith(t, func(t *testing.T) *filesystem.Disk {
		return disk(t, gcs.Config{Client: client, Bucket: newBucket(t, fake), Signing: &storage.SignedURLOptions{GoogleAccessID: signer, PrivateKey: keyPEM}})
	}, conformanceOptions)
}
