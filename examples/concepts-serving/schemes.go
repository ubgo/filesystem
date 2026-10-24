package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
	"github.com/ubgo/filesystem/contrib/cloudfront"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/signedurl"
)

// Hosts of the CDN step; nothing connects to either.
const (
	cdnMedia   = "https://cdn.example.com/media"
	bucketHost = "https://s3.example.com"
	// cdnKeyBits is the RSA key size CloudFront accepts.
	cdnKeyBits = 2048
)

// bearerHeader carries the download token in the bearer step.
const bearerHeader = "Authorization"

// tokenStore is the app's own table of download tokens: each names the
// disk, key, method and expiry it grants.
type tokenStore struct {
	mu     sync.Mutex
	claims map[string]signedurl.Claims
}

// issue stores a new token for c and returns it.
func (s *tokenStore) issue(c signedurl.Claims) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok := rand.Text()
	s.claims[tok] = c
	return tok
}

// bearer verifies download tokens carried in the Authorization header
// instead of a signed query string. Signed calls VerifyRequest, which
// sees the whole request; Verify is never called.
// ds:def id=bearer-c4n9w2rk
type bearer struct{ tokens *tokenStore }

// Verify refuses: a bearer link has no signature in its query.
func (bearer) Verify(string, string, filesystem.HTTPMethod, url.Values) (signedurl.Claims, error) {
	return signedurl.Claims{}, filesystem.ErrPermission
}

// VerifyRequest looks the header's token up. Signed still checks what
// the claims say: the method, the expiry, the disk and the key.
func (b bearer) VerifyRequest(r *http.Request, _ filesystem.HTTPMethod) (signedurl.Claims, error) {
	tok, ok := strings.CutPrefix(r.Header.Get(bearerHeader), "Bearer ")
	if !ok {
		return signedurl.Claims{}, filesystem.ErrPermission
	}
	b.tokens.mu.Lock()
	defer b.tokens.mu.Unlock()
	c, ok := b.tokens.claims[tok]
	if !ok {
		return signedurl.Claims{}, filesystem.ErrPermission
	}
	return c, nil
}

// bearerTokens serves the private disk through fshttp.Signed with the
// bearer verifier, and fetches one file with its token, without one, with
// a token for uploads only, and with an expired token.
// ds:def id=bearertokens-h8v3m5qx
func bearerTokens(ctx context.Context, a *app, out io.Writer) error {
	if _, err := a.private.WriteBytes(ctx, "reports/q3.pdf", []byte("quarterly"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	disks, err := filesystem.NewManager(a.private)
	if err != nil {
		return err
	}
	tokens := &tokenStore{claims: map[string]signedurl.Claims{}}
	srv := httptest.NewServer(fshttp.Signed(disks, bearer{tokens}))
	defer srv.Close()
	grant := signedurl.Claims{Disk: "private", Key: "reports/q3.pdf", Method: filesystem.MethodGet, ExpiresAt: time.Now().Add(linkLifetime)}
	put := grant
	put.Method = filesystem.MethodPut
	old := grant
	old.ExpiresAt = time.Now().Add(-time.Minute)
	for _, c := range []struct {
		label, token string
	}{
		{"with its token", tokens.issue(grant)},
		{"without a token", ""},
		{"with an upload token", tokens.issue(put)},
		{"with an expired token", tokens.issue(old)},
	} {
		h := http.Header{}
		if c.token != "" {
			h.Set(bearerHeader, "Bearer "+c.token)
		}
		resp, body, err := fetch(http.MethodGet, srv.URL+"/any/path", nil, h)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "bearer GET %s: %d %q\n", c.label, resp.StatusCode, strings.TrimSpace(body))
	}
	return nil
}

// cdnOverPresigning puts a CloudFront signer on an S3 disk: downloads are
// CloudFront links, uploads stay S3 presigned PUTs that go straight to
// the bucket. Naming the methods routes uploads to the signer too.
// ds:def id=cdnoverpresigning-z7k2p4tw
func cdnOverPresigning(ctx context.Context, _ *app, out io.Writer) error {
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("AKIAEXAMPLE", "secret", ""),
		BaseEndpoint: aws.String(bucketHost),
		UsePathStyle: true,
	})
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: "media"})
	if err != nil {
		return err
	}
	key, err := rsa.GenerateKey(rand.Reader, cdnKeyBits) // from the secret store in production
	if err != nil {
		return err
	}
	cdn, err := cloudfront.New(cloudfront.Config{BaseURL: cdnMedia, KeyPairID: "K2JCJMDEHXQW5F", PrivateKey: key})
	if err != nil {
		return err
	}
	for _, c := range []struct {
		label   string
		methods []filesystem.HTTPMethod
	}{
		{"WithURLSigner(cdn)", nil},
		{"WithURLSigner(cdn, GET, PUT)", []filesystem.HTTPMethod{filesystem.MethodGet, filesystem.MethodPut}},
	} {
		disk, err := filesystem.NewDisk("media", drv, filesystem.WithURLSigner(cdn, c.methods...))
		if err != nil {
			return err
		}
		var hosts []string
		for _, m := range []filesystem.HTTPMethod{filesystem.MethodGet, filesystem.MethodPut} {
			link, err := disk.SignedURL(ctx, "videos/42/poster.jpg", filesystem.PresignOptions{Method: m, Expires: linkLifetime})
			if err != nil {
				hosts = append(hosts, fmt.Sprintf("%s %s", m, filesystem.CodeOf(err)))
				continue
			}
			hosts = append(hosts, fmt.Sprintf("%s %s", m, signedBy(link.URL)))
		}
		fmt.Fprintf(out, "%s on S3: %s\n", c.label, strings.Join(hosts, ", "))
	}
	return nil
}

// signedBy names who signed a link from its host and parameters.
func signedBy(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return err.Error()
	}
	switch {
	case u.Query().Has("Key-Pair-Id"):
		return "CloudFront link on " + u.Host
	case u.Query().Has("X-Amz-Signature"):
		return "S3 presigned on " + u.Host
	}
	return "unsigned link on " + u.Host
}
