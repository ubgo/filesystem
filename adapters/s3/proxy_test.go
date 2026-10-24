package s3_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
)

// s3Proxy sits in front of the in-process fake S3 and adds what the fake
// lacks and the driver relies on: canned ACLs (PUT and GET ?acl, x-amz-acl
// on writes and copies), If-None-Match on PUT and multipart completion,
// and Cache-Control, which the fake drops (kept from PUT and copies,
// returned on HEAD and GET).
// It can also answer the next matching request with an injected error.
type s3Proxy struct {
	next http.Handler

	mu     sync.Mutex
	public map[string]bool   // path → public-read
	cache  map[string]string // path → Cache-Control
	inject []injection
	// requests records method, path and the encryption headers of every
	// request, for tests that check what was sent.
	requests []recorded
	// cors is the body of the last PutBucketCors, which the proxy answers
	// itself (the fake has no CORS support).
	cors string
	// adv emulates the object features the fake lacks (tags, object lock,
	// storage classes and restores, checksums); see advanced_fake_test.go.
	adv advState
}

// recorded is one request as the proxy saw it.
type recorded struct {
	method, path, query string
	sse, kmsKey         string
	// checksum and contentSHA are x-amz-checksum-sha256 and
	// x-amz-content-sha256.
	checksum, contentSHA string
	// header is every request header, for the newer tests.
	header http.Header
}

// sent returns the recorded requests whose method matches and whose query
// contains query (empty matches all).
func (p *s3Proxy) sent(method, query string) []recorded {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []recorded
	for _, r := range p.requests {
		if r.method == method && strings.Contains(r.query, query) {
			out = append(out, r)
		}
	}
	return out
}

// injection answers the first request whose method and query contain
// match with an S3 error.
type injection struct {
	method, match string
	status        int
	code          string
	// retryAfter, when set, is sent as the Retry-After header.
	retryAfter string
}

func newProxy(next http.Handler) *s3Proxy {
	return &s3Proxy{next: next, public: map[string]bool{}, cache: map[string]string{}, adv: newAdvState()}
}

// trackCacheControl records the Cache-Control of a single PUT or a copy,
// and adds the recorded one to HEAD and GET responses.
func (p *s3Proxy) trackCacheControl(w http.ResponseWriter, r *http.Request, part bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case r.Method == http.MethodPut && !part:
		v := r.Header.Get("Cache-Control")
		if src := r.Header.Get("X-Amz-Copy-Source"); src != "" {
			unescaped, _ := url.PathUnescape(src)
			v = p.cache["/"+strings.TrimPrefix(unescaped, "/")]
		}
		p.cache[r.URL.Path] = v
	case (r.Method == http.MethodHead || r.Method == http.MethodGet) && p.cache[r.URL.Path] != "":
		w.Header().Set("Cache-Control", p.cache[r.URL.Path])
	}
}

func (p *s3Proxy) fail(method, match string, status int, code string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inject = append(p.inject, injection{method: method, match: match, status: status, code: code})
}

// failRetryAfter is fail with a Retry-After header on the answer.
func (p *s3Proxy) failRetryAfter(method, match string, status int, code, retryAfter string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inject = append(p.inject, injection{method: method, match: match, status: status, code: code, retryAfter: retryAfter})
}

func writeS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>injected</Message></Error>`, code)
}

// exists asks the fake whether an object exists.
func (p *s3Proxy) exists(r *http.Request) bool {
	head := httptest.NewRequestWithContext(r.Context(), http.MethodHead, r.URL.EscapedPath(), nil)
	rec := httptest.NewRecorder()
	p.next.ServeHTTP(rec, head)
	return rec.Code == http.StatusOK
}

func (p *s3Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.requests = append(p.requests, recorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Amz-Server-Side-Encryption"), r.Header.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"),
		r.Header.Get("X-Amz-Checksum-Sha256"), r.Header.Get("X-Amz-Content-Sha256"), r.Header.Clone()})
	for i, inj := range p.inject {
		if r.Method == inj.method && strings.Contains(r.URL.RawQuery+"|"+r.URL.Path, inj.match) {
			p.inject = append(p.inject[:i], p.inject[i+1:]...)
			p.mu.Unlock()
			if inj.retryAfter != "" {
				w.Header().Set("Retry-After", inj.retryAfter)
			}
			writeS3Error(w, inj.status, inj.code)
			return
		}
	}
	p.mu.Unlock()
	if hideChecksums(w, r, p.serve) {
		return
	}
	p.serve(w, r)
}

// serve answers r after the injected failures.
func (p *s3Proxy) serve(w http.ResponseWriter, r *http.Request) {
	if p.serveAdvanced(w, r) {
		return
	}

	q := r.URL.Query()
	if _, isCORS := q["cors"]; isCORS && r.Method == http.MethodPut {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.cors = string(body)
		p.mu.Unlock()
		return
	}
	_, isACL := q["acl"]
	switch {
	case isACL && r.Method == http.MethodPut:
		if !p.exists(r) {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		p.setACL(r.URL.Path, r.Header.Get("X-Amz-Acl"))
		w.WriteHeader(http.StatusOK)
		return
	case isACL && r.Method == http.MethodGet:
		if !p.exists(r) {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		p.mu.Lock()
		public := p.public[r.URL.Path]
		p.mu.Unlock()
		grants := `<Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="CanonicalUser"><ID>owner</ID></Grantee><Permission>FULL_CONTROL</Permission></Grant>`
		if public {
			grants += `<Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="Group"><URI>http://acs.amazonaws.com/groups/global/AllUsers</URI></Grantee><Permission>READ</Permission></Grant>`
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><AccessControlPolicy><Owner><ID>owner</ID></Owner><AccessControlList>%s</AccessControlList></AccessControlPolicy>`, grants)
		return
	}
	_, completing := q["uploadId"]
	conditional := r.Header.Get("If-None-Match") == "*" && (r.Method == http.MethodPut || (r.Method == http.MethodPost && completing))
	if conditional && p.exists(r) {
		writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
		return
	}
	if r.Method == http.MethodPut && !completing {
		p.setACL(r.URL.Path, r.Header.Get("X-Amz-Acl"))
	}
	p.trackCacheControl(w, r, completing)
	p.next.ServeHTTP(w, r)
}

func (p *s3Proxy) setACL(path, acl string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.public[path] = acl == "public-read"
}
