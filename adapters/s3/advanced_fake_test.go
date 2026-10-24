package s3_test

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// advState is the proxy's emulation of what gofakes3 does not implement:
// object tags, Object Lock (bucket switch, legal holds, retention),
// storage classes with archive restores, and stored checksums. It is
// keyed by request path ("/bucket/key") and guarded by the proxy's mu.
type advState struct {
	lockEnabled bool
	tags        map[string]map[string]string
	legal       map[string]bool
	retention   map[string]fakeRetention
	// cleared marks a removed retention, which the fake then reports
	// with an empty date as RustFS does.
	cleared map[string]bool
	class   map[string]string
	// restore is "" (never asked), "ongoing" or "done".
	restore     map[string]string
	restoreTier map[string]string
	checksums   map[string]http.Header
	// headDelay slows every object HEAD, and heads/maxHeads count how
	// many run at once, for the test of parallel hold checks.
	headDelay       time.Duration
	heads, maxHeads int
}

type fakeRetention struct {
	mode  string
	until time.Time
}

func newAdvState() advState {
	return advState{
		tags: map[string]map[string]string{}, legal: map[string]bool{}, retention: map[string]fakeRetention{}, cleared: map[string]bool{},
		class: map[string]string{}, restore: map[string]string{}, restoreTier: map[string]string{}, checksums: map[string]http.Header{},
	}
}

// archiveClasses must be restored before they can be read.
var archiveClasses = map[string]bool{"GLACIER": true, "DEEP_ARCHIVE": true}

type xmlTagging struct {
	XMLName xml.Name `xml:"Tagging"`
	Tags    []struct {
		Key   string `xml:"Key"`
		Value string `xml:"Value"`
	} `xml:"TagSet>Tag"`
}

type xmlLegalHold struct {
	Status string `xml:"Status"`
}

type xmlRetention struct {
	Mode            string `xml:"Mode"`
	RetainUntilDate string `xml:"RetainUntilDate"`
}

type xmlRestore struct {
	Days int    `xml:"Days"`
	Tier string `xml:"GlacierJobParameters>Tier"`
}

// lockEnable turns Object Lock on for the fake's bucket.
func (p *s3Proxy) lockEnable() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.adv.lockEnabled = true
}

// finishRestore completes a pending restore of path.
func (p *s3Proxy) finishRestore(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.adv.restore[path] = "done"
}

// tier returns the tier the last restore of path asked for.
func (p *s3Proxy) tier(path string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.adv.restoreTier[path]
}

func xmlBody(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+body)
}

// serveAdvanced answers the sub-resources the fake lacks and records the
// object-level headers of writes, adding them to HEAD and GET answers. It
// reports whether it answered the request itself.
func (p *s3Proxy) serveAdvanced(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	path := r.URL.Path
	has := func(k string) bool { _, ok := q[k]; return ok }
	switch {
	case has("object-lock") && r.Method == http.MethodGet:
		p.mu.Lock()
		on := p.adv.lockEnabled
		p.mu.Unlock()
		if !on {
			writeS3Error(w, http.StatusNotFound, "ObjectLockConfigurationNotFoundError")
			return true
		}
		xmlBody(w, `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`)
		return true
	case has("tagging"), has("legal-hold"), has("retention"), has("restore"):
		if !p.exists(r) {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey")
			return true
		}
		p.serveObjectSubresource(w, r, path)
		return true
	}
	if has("uploadId") || has("partNumber") || has("acl") || has("cors") || has("versioning") {
		return false
	}
	switch r.Method {
	case http.MethodPut:
		p.recordWrite(r, path)
		if src := r.Header.Get("X-Amz-Copy-Source"); strings.Contains(src, "?versionId=") {
			p.copyVersion(w, r, src)
			return true
		}
	case http.MethodPost:
		if has("uploads") {
			p.recordWrite(r, path)
		}
	case http.MethodHead, http.MethodGet:
		if has("versions") || has("versioning") || strings.Count(strings.Trim(path, "/"), "/") == 0 {
			return false
		}
		if r.Method == http.MethodHead {
			p.trackHead()
		}
		if r.Method == http.MethodHead && has("versionId") {
			p.headVersion(w, r)
			return true
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		class := p.adv.class[path]
		if class != "" {
			w.Header().Set("X-Amz-Storage-Class", class)
		}
		switch p.adv.restore[path] {
		case "ongoing":
			w.Header().Set("X-Amz-Restore", `ongoing-request="true"`)
		case "done":
			w.Header().Set("X-Amz-Restore", `ongoing-request="false", expiry-date="Fri, 21 Dec 2040 00:00:00 GMT"`)
		}
		if p.adv.legal[path] {
			w.Header().Set("X-Amz-Object-Lock-Legal-Hold", "ON")
		}
		if p.adv.cleared[path] {
			w.Header()["X-Amz-Object-Lock-Retain-Until-Date"] = []string{""}
		}
		if ret, ok := p.adv.retention[path]; ok {
			w.Header().Set("X-Amz-Object-Lock-Mode", ret.mode)
			w.Header().Set("X-Amz-Object-Lock-Retain-Until-Date", ret.until.Format(time.RFC3339))
		}
		if strings.EqualFold(r.Header.Get("X-Amz-Checksum-Mode"), "ENABLED") {
			for k, v := range p.adv.checksums[path] {
				w.Header()[k] = v
			}
		}
		if r.Method == http.MethodGet && archiveClasses[class] && p.adv.restore[path] != "done" {
			writeS3Error(w, http.StatusForbidden, "InvalidObjectState")
			return true
		}
	}
	return false
}

// recordWrite keeps the object headers of a PUT, a copy or a multipart
// upload's creation.
func (p *s3Proxy) recordWrite(r *http.Request, path string) {
	h := r.Header
	p.mu.Lock()
	defer p.mu.Unlock()
	src := ""
	if c := h.Get("X-Amz-Copy-Source"); c != "" {
		unescaped, _ := url.PathUnescape(strings.SplitN(c, "?", 2)[0])
		src = "/" + strings.TrimPrefix(unescaped, "/")
	}
	p.adv.class[path] = h.Get("X-Amz-Storage-Class")
	switch {
	case h.Get("X-Amz-Tagging") != "":
		vals, _ := url.ParseQuery(h.Get("X-Amz-Tagging"))
		tags := map[string]string{}
		for k := range vals {
			tags[k] = vals.Get(k)
		}
		p.adv.tags[path] = tags
	case src != "" && src != path:
		p.adv.tags[path] = p.adv.tags[src]
	case src == "":
		delete(p.adv.tags, path)
	}
	if h.Get("X-Amz-Object-Lock-Legal-Hold") == "ON" {
		p.adv.legal[path] = true
	}
	if until, err := time.Parse(time.RFC3339, h.Get("X-Amz-Object-Lock-Retain-Until-Date")); err == nil {
		p.adv.retention[path] = fakeRetention{mode: h.Get("X-Amz-Object-Lock-Mode"), until: until}
	}
	sums := http.Header{}
	for k, v := range h {
		if strings.HasPrefix(k, "X-Amz-Checksum-") && k != "X-Amz-Checksum-Mode" {
			sums[k] = v
		}
	}
	// A body sent with a trailing checksum names the header it carries.
	if t := h.Get("X-Amz-Trailer"); strings.HasPrefix(strings.ToLower(t), "x-amz-checksum-") {
		sums.Set(t, "trailer")
	}
	p.adv.checksums[path] = sums
	delete(p.adv.restore, path)
}

// serveObjectSubresource answers ?tagging, ?legal-hold, ?retention and
// ?restore of an existing object.
func (p *s3Proxy) serveObjectSubresource(w http.ResponseWriter, r *http.Request, path string) {
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	defer p.mu.Unlock()
	has := func(k string) bool { _, ok := q[k]; return ok }
	switch {
	case has("tagging") && r.Method == http.MethodGet:
		var b strings.Builder
		for k, v := range p.adv.tags[path] {
			fmt.Fprintf(&b, "<Tag><Key>%s</Key><Value>%s</Value></Tag>", k, v)
		}
		xmlBody(w, "<Tagging><TagSet>"+b.String()+"</TagSet></Tagging>")
	case has("tagging") && r.Method == http.MethodPut:
		var t xmlTagging
		if err := xml.Unmarshal(body, &t); err != nil {
			writeS3Error(w, http.StatusBadRequest, "MalformedXML")
			return
		}
		tags := map[string]string{}
		for _, tag := range t.Tags {
			tags[tag.Key] = tag.Value
		}
		p.adv.tags[path] = tags
	case has("tagging") && r.Method == http.MethodDelete:
		delete(p.adv.tags, path)
		w.WriteHeader(http.StatusNoContent)
	case has("legal-hold") && r.Method == http.MethodPut:
		var lh xmlLegalHold
		_ = xml.Unmarshal(body, &lh)
		p.adv.legal[path] = lh.Status == "ON"
	case has("retention") && r.Method == http.MethodPut:
		var ret xmlRetention
		_ = xml.Unmarshal(body, &ret)
		old, had := p.adv.retention[path]
		until, _ := time.Parse(time.RFC3339, ret.RetainUntilDate)
		shortening := had && (ret.Mode == "" || until.Before(old.until))
		bypass := r.Header.Get("X-Amz-Bypass-Governance-Retention") == "true"
		if shortening && (old.mode == "COMPLIANCE" || !bypass) {
			writeS3Error(w, http.StatusForbidden, "AccessDenied")
			return
		}
		if ret.Mode == "" {
			delete(p.adv.retention, path)
			p.adv.cleared[path] = true
			return
		}
		p.adv.retention[path] = fakeRetention{mode: ret.Mode, until: until}
	case has("restore") && r.Method == http.MethodPost:
		if !archiveClasses[p.adv.class[path]] {
			writeS3Error(w, http.StatusForbidden, "InvalidObjectState")
			return
		}
		if p.adv.restore[path] == "ongoing" {
			writeS3Error(w, http.StatusConflict, "RestoreAlreadyInProgress")
			return
		}
		var rr xmlRestore
		_ = xml.Unmarshal(body, &rr)
		p.adv.restore[path], p.adv.restoreTier[path] = "ongoing", rr.Tier
		w.WriteHeader(http.StatusAccepted)
	default:
		writeS3Error(w, http.StatusNotImplemented, "NotImplemented")
	}
}

// copyVersion answers a CopyObject whose source names a version, which
// gofakes3 ignores: it reads that version and writes it to the target.
func (p *s3Proxy) copyVersion(w http.ResponseWriter, r *http.Request, src string) {
	srcPath, query, _ := strings.Cut(src, "?")
	get := httptest.NewRequestWithContext(r.Context(), http.MethodGet, "/"+strings.TrimPrefix(srcPath, "/")+"?"+query, nil)
	got := httptest.NewRecorder()
	p.next.ServeHTTP(got, get)
	if got.Code != http.StatusOK {
		writeS3Error(w, got.Code, "NoSuchVersion")
		return
	}
	put := httptest.NewRequestWithContext(r.Context(), http.MethodPut, r.URL.EscapedPath(), bytes.NewReader(got.Body.Bytes()))
	put.Header.Set("Content-Type", got.Header().Get("Content-Type"))
	put.Header.Set("Content-Length", strconv.Itoa(got.Body.Len()))
	stored := httptest.NewRecorder()
	p.next.ServeHTTP(stored, put)
	if stored.Code != http.StatusOK {
		w.WriteHeader(stored.Code)
		return
	}
	w.Header().Set("X-Amz-Version-Id", stored.Header().Get("X-Amz-Version-Id"))
	xmlBody(w, `<CopyObjectResult><ETag>`+stored.Header().Get("ETag")+`</ETag></CopyObjectResult>`)
}

// hideChecksums answers a HEAD that did not ask for checksums
// (x-amz-checksum-mode) through serve, without the stored checksums
// gofakes3 always returns and S3 returns only on request. It reports
// whether it answered.
func hideChecksums(w http.ResponseWriter, r *http.Request, serve http.HandlerFunc) bool {
	if r.Method != http.MethodHead || strings.EqualFold(r.Header.Get("X-Amz-Checksum-Mode"), "ENABLED") {
		return false
	}
	rec := httptest.NewRecorder()
	serve(rec, r)
	for k, v := range rec.Header() {
		if !strings.HasPrefix(k, "X-Amz-Checksum-") {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(rec.Code)
	return true
}

// headVersion answers a HEAD of one version, whose versionId gofakes3
// ignores, with the headers of a GET of that version.
func (p *s3Proxy) headVersion(w http.ResponseWriter, r *http.Request) {
	get := httptest.NewRequestWithContext(r.Context(), http.MethodGet, r.URL.RequestURI(), nil)
	rec := httptest.NewRecorder()
	p.next.ServeHTTP(rec, get)
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
}

// slowHeads makes every later object HEAD take d.
func (p *s3Proxy) slowHeads(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.adv.headDelay, p.adv.maxHeads = d, 0
}

// maxConcurrentHeads is the most object HEADs seen running at once since
// slowHeads.
func (p *s3Proxy) maxConcurrentHeads() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.adv.maxHeads
}

// trackHead counts one HEAD in flight for its delay.
func (p *s3Proxy) trackHead() {
	p.mu.Lock()
	delay := p.adv.headDelay
	p.adv.heads++
	p.adv.maxHeads = max(p.adv.maxHeads, p.adv.heads)
	p.mu.Unlock()
	time.Sleep(delay)
	p.mu.Lock()
	p.adv.heads--
	p.mu.Unlock()
}
