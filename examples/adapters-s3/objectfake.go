package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"
)

// objectFeatures wraps the fake with the object features gofakes3 does
// not model and the driver relies on, kept to what the steps need:
// object tags, Object Lock (the bucket switch, legal holds, retention)
// and archive storage classes with restores. State is keyed by request
// path ("/bucket/key"). It is a stand-in for the demo, not a model of S3:
// the driver's own tests (adapters/s3) and the integration suites against
// SeaweedFS and RustFS cover the real behaviour.
type objectFeatures struct {
	next http.Handler

	mu        sync.Mutex
	locked    map[string]bool // bucket path ("/bucket") → Object Lock on
	tags      map[string]map[string]string
	legal     map[string]bool
	retention map[string]time.Time
	class     map[string]string
	restore   map[string]string // "" never asked, "ongoing" or "done"
	tier      map[string]string
}

// newObjectFeatures returns the wrapper around next.
func newObjectFeatures(next http.Handler) *objectFeatures {
	return &objectFeatures{
		next: next, locked: map[string]bool{}, tags: map[string]map[string]string{}, legal: map[string]bool{},
		retention: map[string]time.Time{}, class: map[string]string{}, restore: map[string]string{}, tier: map[string]string{},
	}
}

// archiveClasses must be restored before they can be read.
var archiveClasses = map[string]bool{"GLACIER": true, "DEEP_ARCHIVE": true}

// enableLock turns Object Lock on for a bucket, as creating it with
// ObjectLockEnabledForBucket does on S3.
func (o *objectFeatures) enableLock(bucket string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.locked["/"+bucket] = true
}

// finishRestore completes a pending restore, which takes hours on S3.
func (o *objectFeatures) finishRestore(path string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.restore[path] = "done"
}

// restoreTier returns the tier the last restore of path asked for.
func (o *objectFeatures) restoreTier(path string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.tier[path]
}

// ServeHTTP answers the sub-resources gofakes3 lacks and adds the
// recorded object state to HEAD and GET answers.
func (o *objectFeatures) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := r.URL.Path
	switch {
	case q.Has("object-lock") && r.Method == http.MethodGet:
		o.mu.Lock()
		on := o.locked[strings.TrimSuffix(path, "/")]
		o.mu.Unlock()
		if !on {
			s3Error(w, http.StatusNotFound, "ObjectLockConfigurationNotFoundError")
			return
		}
		xmlBody(w, `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`)
		return
	case q.Has("tagging"), q.Has("legal-hold"), q.Has("retention"), q.Has("restore"):
		if !o.exists(r) {
			s3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		o.subresource(w, r, path)
		return
	case q.Has("uploadId"), q.Has("uploads"), q.Has("partNumber"), q.Has("acl"), q.Has("versions"), q.Has("versioning"),
		strings.Count(strings.Trim(path, "/"), "/") == 0:
		o.next.ServeHTTP(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		o.recordWrite(r.Header, path)
	case http.MethodHead, http.MethodGet:
		if o.objectState(w, r.Method, path) {
			return
		}
	}
	o.next.ServeHTTP(w, r)
}

// objectState adds the object's class, restore, hold and retention
// headers, and refuses a GET of an archived object that is not restored.
// It reports whether it answered the request itself.
func (o *objectFeatures) objectState(w http.ResponseWriter, method, path string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	class := o.class[path]
	if class != "" {
		w.Header().Set("X-Amz-Storage-Class", class)
	}
	switch o.restore[path] {
	case "ongoing":
		w.Header().Set("X-Amz-Restore", `ongoing-request="true"`)
	case "done":
		w.Header().Set("X-Amz-Restore", `ongoing-request="false", expiry-date="Fri, 21 Dec 2040 00:00:00 GMT"`)
	}
	if o.legal[path] {
		w.Header().Set("X-Amz-Object-Lock-Legal-Hold", "ON")
	}
	if until, ok := o.retention[path]; ok {
		w.Header().Set("X-Amz-Object-Lock-Mode", "GOVERNANCE")
		w.Header().Set("X-Amz-Object-Lock-Retain-Until-Date", until.Format(time.RFC3339))
	}
	if method == http.MethodGet && archiveClasses[class] && o.restore[path] != "done" {
		s3Error(w, http.StatusForbidden, "InvalidObjectState")
		return true
	}
	return false
}

// recordWrite keeps the object headers of a PUT or a copy.
func (o *objectFeatures) recordWrite(h http.Header, path string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	src := ""
	if c := h.Get("X-Amz-Copy-Source"); c != "" {
		unescaped, _ := url.PathUnescape(strings.SplitN(c, "?", 2)[0])
		src = "/" + strings.TrimPrefix(unescaped, "/")
	}
	o.class[path] = h.Get("X-Amz-Storage-Class")
	switch {
	case h.Get("X-Amz-Tagging") != "":
		vals, _ := url.ParseQuery(h.Get("X-Amz-Tagging"))
		tags := map[string]string{}
		for k := range vals {
			tags[k] = vals.Get(k)
		}
		o.tags[path] = tags
	case src != "" && src != path:
		o.tags[path] = o.tags[src]
	case src == "":
		delete(o.tags, path)
	}
	if h.Get("X-Amz-Object-Lock-Legal-Hold") == "ON" {
		o.legal[path] = true
	}
	if until, err := time.Parse(time.RFC3339, h.Get("X-Amz-Object-Lock-Retain-Until-Date")); err == nil {
		o.retention[path] = until
	}
	delete(o.restore, path)
	// gofakes3 would keep these as metadata and return them on every
	// HEAD, after the hold is lifted too; the state above answers them.
	for _, k := range []string{"X-Amz-Object-Lock-Legal-Hold", "X-Amz-Object-Lock-Mode", "X-Amz-Object-Lock-Retain-Until-Date"} {
		h.Del(k)
	}
}

// subresource answers ?tagging, ?legal-hold, ?retention and ?restore of
// an existing object.
func (o *objectFeatures) subresource(w http.ResponseWriter, r *http.Request, path string) {
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case q.Has("tagging") && r.Method == http.MethodGet:
		var b strings.Builder
		for k, v := range o.tags[path] {
			fmt.Fprintf(&b, "<Tag><Key>%s</Key><Value>%s</Value></Tag>", k, v)
		}
		xmlBody(w, "<Tagging><TagSet>"+b.String()+"</TagSet></Tagging>")
	case q.Has("tagging") && r.Method == http.MethodPut:
		var t struct {
			Tags []struct{ Key, Value string } `xml:"TagSet>Tag"`
		}
		if err := xml.Unmarshal(body, &t); err != nil {
			s3Error(w, http.StatusBadRequest, "MalformedXML")
			return
		}
		tags := map[string]string{}
		for _, tag := range t.Tags {
			tags[tag.Key] = tag.Value
		}
		o.tags[path] = tags
	case q.Has("tagging") && r.Method == http.MethodDelete:
		delete(o.tags, path)
		w.WriteHeader(http.StatusNoContent)
	case q.Has("legal-hold") && r.Method == http.MethodPut:
		var lh struct{ Status string }
		_ = xml.Unmarshal(body, &lh)
		o.legal[path] = lh.Status == "ON"
	case q.Has("retention") && r.Method == http.MethodPut:
		var ret struct{ Mode, RetainUntilDate string }
		_ = xml.Unmarshal(body, &ret)
		until, err := time.Parse(time.RFC3339, ret.RetainUntilDate)
		if ret.Mode == "" || err != nil {
			delete(o.retention, path)
			return
		}
		o.retention[path] = until
	case q.Has("restore") && r.Method == http.MethodPost:
		if !archiveClasses[o.class[path]] {
			s3Error(w, http.StatusForbidden, "InvalidObjectState")
			return
		}
		if o.restore[path] == "ongoing" {
			s3Error(w, http.StatusConflict, "RestoreAlreadyInProgress")
			return
		}
		var rr struct {
			Tier string `xml:"GlacierJobParameters>Tier"`
		}
		_ = xml.Unmarshal(body, &rr)
		o.restore[path], o.tier[path] = "ongoing", rr.Tier
		w.WriteHeader(http.StatusAccepted)
	default:
		s3Error(w, http.StatusNotImplemented, "NotImplemented")
	}
}

// exists asks gofakes3 whether the object of r exists.
func (o *objectFeatures) exists(r *http.Request) bool {
	head := httptest.NewRequestWithContext(r.Context(), http.MethodHead, r.URL.EscapedPath(), nil)
	rec := httptest.NewRecorder()
	o.next.ServeHTTP(rec, head)
	return rec.Code == http.StatusOK
}

// xmlBody answers an S3 XML document.
func xmlBody(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+body)
}

// s3Error answers an S3 error document.
func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}
