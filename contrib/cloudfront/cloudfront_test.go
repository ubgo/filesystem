package cloudfront_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" // CloudFront's default signature hash, which the test verifies.
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/cloudfront/sign"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/cloudfront"
	"github.com/ubgo/filesystem/memory"
)

const (
	base      = "https://cdn.example.test/media"
	keyPairID = "K2JCJMDEHXQW5F"
)

var now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// testKey is generated once: RSA key generation is slow.
var testKey = func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}()

func newSigner(t *testing.T, mutate func(*cloudfront.Config)) *cloudfront.Signer {
	t.Helper()
	cfg := cloudfront.Config{BaseURL: base, KeyPairID: keyPairID, PrivateKey: testKey, Clock: func() time.Time { return now }}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := cloudfront.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// cfDecode reverses CloudFront's URL-safe base64 alphabet (+ → -, = → _,
// / → ~).
func cfDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(strings.NewReplacer("-", "+", "_", "=", "~", "/").Replace(s))
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

// verify checks sig over policy with the public key, as CloudFront does.
func verify(t *testing.T, policy, sig []byte, alg sign.HashAlgorithm) {
	t.Helper()
	var digest []byte
	h := crypto.SHA1
	if alg == sign.HashSHA256 {
		sum := sha256.Sum256(policy)
		digest, h = sum[:], crypto.SHA256
	} else {
		sum := sha1.Sum(policy)
		digest = sum[:]
	}
	if err := rsa.VerifyPKCS1v15(&testKey.PublicKey, h, digest, sig); err != nil {
		t.Fatalf("signature does not verify with the public key: %v", err)
	}
}

// cannedPolicy is CloudFront's documented canned policy for resource.
func cannedPolicy(resource string, expires int64) []byte {
	return []byte(`{"Statement":[{"Resource":"` + resource + `","Condition":{"DateLessThan":{"AWS:EpochTime":` + strconv.FormatInt(expires, 10) + `}}}]}`)
}

// splitSigned separates CloudFront's signing parameters from the resource
// URL they sign.
func splitSigned(t *testing.T, raw string) (string, url.Values) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	signing := url.Values{}
	for _, p := range []string{"Expires", "Signature", "Key-Pair-Id", "Hash-Algorithm", "Policy"} {
		if v, ok := q[p]; ok {
			signing[p] = v
			q.Del(p)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), signing
}

func TestSignURLVerifiesWithPublicKey(t *testing.T) {
	for _, alg := range []sign.HashAlgorithm{"", sign.HashSHA256} {
		t.Run(string(alg), func(t *testing.T) {
			s := newSigner(t, func(c *cloudfront.Config) { c.HashAlgorithm = alg })
			req, err := s.SignURL(context.Background(), "media", "photos/a b.jpg", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			resource, q := splitSigned(t, req.URL)
			if want := base + "/photos/a%20b.jpg"; resource != want {
				t.Fatalf("resource = %q, want %q", resource, want)
			}
			exp := now.Add(time.Hour).Unix()
			if q.Get("Expires") != strconv.FormatInt(exp, 10) || !req.ExpiresAt.Equal(time.Unix(exp, 0)) {
				t.Fatalf("Expires = %q, ExpiresAt = %v, want %d", q.Get("Expires"), req.ExpiresAt, exp)
			}
			if q.Get("Key-Pair-Id") != keyPairID {
				t.Fatalf("Key-Pair-Id = %q", q.Get("Key-Pair-Id"))
			}
			if alg != "" && q.Get("Hash-Algorithm") != string(alg) {
				t.Fatalf("Hash-Algorithm = %q, want %q", q.Get("Hash-Algorithm"), alg)
			}
			verify(t, cannedPolicy(resource, exp), cfDecode(t, q.Get("Signature")), alg)
		})
	}
}

func TestSignURLTamperedResourceFails(t *testing.T) {
	s := newSigner(t, nil)
	req, err := s.SignURL(context.Background(), "media", "a.jpg", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	_, q := splitSigned(t, req.URL)
	exp, _ := strconv.ParseInt(q.Get("Expires"), 10, 64)
	sum := sha1.Sum(cannedPolicy(base+"/b.jpg", exp))
	if rsa.VerifyPKCS1v15(&testKey.PublicKey, crypto.SHA1, sum[:], cfDecode(t, q.Get("Signature"))) == nil {
		t.Fatal("signature for a.jpg verified for b.jpg")
	}
}

func TestSignURLRefusals(t *testing.T) {
	s := newSigner(t, nil)
	ctx := context.Background()
	cases := []struct {
		name string
		opts filesystem.PresignOptions
		code filesystem.Code
	}{
		{"no expiry", filesystem.PresignOptions{Method: filesystem.MethodGet}, filesystem.CodeInvalidArgument},
		{"too long", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: filesystem.MaxPresignExpiry + time.Second}, filesystem.CodeInvalidArgument},
		{"put", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute}, filesystem.CodeUnsupported},
		{"override", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute, ContentDisposition: "attachment"}, filesystem.CodeUnsupported},
	}
	for _, c := range cases {
		if _, err := s.SignURL(ctx, "media", "a.jpg", c.opts); filesystem.CodeOf(err) != c.code {
			t.Errorf("%s: err = %v, want %s", c.name, err, c.code)
		}
	}
}

// TestActiveTypeNeverInline signs through a Disk: the Disk forces an
// attachment for HTML, and the signer refuses rather than drop it.
func TestActiveTypeNeverInline(t *testing.T) {
	ctx := context.Background()
	disk, err := filesystem.NewDisk("media", memory.New(), filesystem.WithURLSigner(newSigner(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	opts := filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute, ContentType: "text/html"}
	if _, err := disk.SignedURL(ctx, "page.html", opts); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Fatalf("HTML without overrides: err = %v, want unsupported", err)
	}
	overrides, err := filesystem.NewDisk("media", memory.New(), filesystem.WithURLSigner(newSigner(t, func(c *cloudfront.Config) { c.ResponseOverrides = true })))
	if err != nil {
		t.Fatal(err)
	}
	req, err := overrides.SignedURL(ctx, "page.html", opts)
	if err != nil {
		t.Fatal(err)
	}
	resource, q := splitSigned(t, req.URL)
	u, _ := url.Parse(resource)
	if got := u.Query().Get(cloudfront.ParamResponseContentDisposition); got != "attachment" {
		t.Fatalf("%s = %q, want attachment", cloudfront.ParamResponseContentDisposition, got)
	}
	// The override is part of the signed resource, so it cannot be
	// stripped. The SDK signs the query exactly as the URL carries it.
	signedResource := req.URL[:strings.Index(req.URL, "&Expires=")]
	exp, _ := strconv.ParseInt(q.Get("Expires"), 10, 64)
	verify(t, cannedPolicy(signedResource, exp), cfDecode(t, q.Get("Signature")), "")
}

func TestSignCookies(t *testing.T) {
	s := newSigner(t, func(c *cloudfront.Config) { c.CookieDomain = "example.test" })
	cookies, at, err := s.SignCookies(context.Background(), "/videos/42/", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !at.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("expiry = %v", at)
	}
	byName := map[string]*http.Cookie{}
	for _, c := range cookies {
		if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Domain != "example.test" || !c.Expires.Equal(at) {
			t.Fatalf("cookie %s attributes: %+v", c.Name, c)
		}
		byName[c.Name] = c
	}
	if byName[sign.CookieKeyIDName].Value != keyPairID {
		t.Fatalf("key pair cookie = %+v", byName[sign.CookieKeyIDName])
	}
	policy := cfDecode(t, byName[sign.CookiePolicyName].Value)
	var p struct {
		Statement []struct {
			Resource  string
			Condition struct {
				DateLessThan struct {
					Epoch int64 `json:"AWS:EpochTime"`
				}
			}
		}
	}
	if err := json.Unmarshal(policy, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Statement) != 1 || p.Statement[0].Resource != base+"/videos/42/*" || p.Statement[0].Condition.DateLessThan.Epoch != at.Unix() {
		t.Fatalf("policy = %s", policy)
	}
	verify(t, policy, cfDecode(t, byName[sign.CookieSignatureName].Value), "")
}

func TestSignCookiesPrefixCannotWiden(t *testing.T) {
	s := newSigner(t, nil)
	cookies, _, err := s.SignCookies(context.Background(), "a*", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cookies {
		if c.Name == sign.CookiePolicyName {
			if policy := string(cfDecode(t, c.Value)); !strings.Contains(policy, base+"/a%2A/*") {
				t.Fatalf("policy %s does not escape the prefix's wildcard", policy)
			}
		}
	}
	for _, bad := range []string{"../etc", "a/../../b"} {
		if _, _, err := s.SignCookies(context.Background(), bad, time.Minute); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
			t.Errorf("prefix %q: err = %v, want invalid_key", bad, err)
		}
	}
	if _, _, err := s.SignCookies(context.Background(), "a", 0); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("no expiry: err = %v", err)
	}
	// With overrides forwarded, "*" would let the holder append
	// response-content-type=text/html to any object.
	overrides := newSigner(t, func(c *cloudfront.Config) { c.ResponseOverrides = true })
	if cookies, _, err := overrides.SignCookies(context.Background(), "gallery", time.Minute); filesystem.CodeOf(err) != filesystem.CodeUnsupported || cookies != nil {
		t.Errorf("cookies with ResponseOverrides: %v, %v", cookies, err)
	}
}

func TestNewRefusesBadConfig(t *testing.T) {
	good := cloudfront.Config{BaseURL: base, KeyPairID: keyPairID, PrivateKey: testKey}
	cases := map[string]func(*cloudfront.Config){
		"http base":   func(c *cloudfront.Config) { c.BaseURL = "http://cdn.example.test" },
		"query base":  func(c *cloudfront.Config) { c.BaseURL = base + "?a=b" },
		"no key id":   func(c *cloudfront.Config) { c.KeyPairID = "" },
		"bad key id":  func(c *cloudfront.Config) { c.KeyPairID = "a&b" },
		"no key":      func(c *cloudfront.Config) { c.PrivateKey = nil },
		"bad hashing": func(c *cloudfront.Config) { c.HashAlgorithm = "MD5" },
	}
	for name, mutate := range cases {
		cfg := good
		mutate(&cfg)
		if _, err := cloudfront.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestPrivateKeyRedacted(t *testing.T) {
	cfg := cloudfront.Config{BaseURL: base, KeyPairID: keyPairID, PrivateKey: testKey}
	s, err := cloudfront.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	secret := testKey.D.String()
	modulus := testKey.N.String()
	var outputs []string
	for _, v := range []any{cfg, s} {
		var logs bytes.Buffer
		slog.New(slog.NewJSONHandler(&logs, nil)).Info("cfg", "value", v)
		outputs = append(outputs, logs.String())
	}
	// Format answers every verb, so fmt never reaches GoString; call it
	// directly so a direct caller cannot see the secret either.
	outputs = append(outputs, cfg.GoString(), s.GoString())
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d"} {
		outputs = append(outputs, fmt.Sprintf(verb, cfg), fmt.Sprintf(verb, s), fmt.Sprintf(verb, &cfg), fmt.Sprintf(verb, struct{ C cloudfront.Config }{cfg}))
	}
	for _, out := range outputs {
		if strings.Contains(out, secret) || strings.Contains(out, modulus) || !strings.Contains(out, keyPairID) || !strings.Contains(out, "REDACTED") {
			t.Fatalf("output is not the redacted form: %.200s", out)
		}
	}
}

// failingSigner is a private key whose signing fails, as a key held in a
// KMS or an HSM does when that service is unreachable.
type failingSigner struct{ crypto.Signer }

func (failingSigner) Sign(io.Reader, []byte, crypto.SignerOpts) ([]byte, error) {
	return nil, errors.New("hsm unreachable")
}

// TestSigningFailureIsReported pins that a key that cannot sign fails
// SignURL and SignCookies with an error, never an unsigned URL or cookie.
func TestSigningFailureIsReported(t *testing.T) {
	s := newSigner(t, func(c *cloudfront.Config) { c.PrivateKey = failingSigner{testKey} })
	if req, err := s.SignURL(context.Background(), "disk", "a.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute}); err == nil || req.URL != "" {
		t.Fatalf("SignURL = %+v, %v; want an error", req, err)
	}
	if cookies, _, err := s.SignCookies(context.Background(), "", time.Minute); err == nil || cookies != nil {
		t.Fatalf("SignCookies = %v, %v; want an error", cookies, err)
	}
}
