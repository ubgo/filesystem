package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// fakeS3 is the in-process S3 this program runs against: gofakes3 for
// objects, buckets and versions, plus two things gofakes3 lacks, kept
// small on purpose.
//
//   - It records the headers of every request, so the program can show
//     what the driver sent for features the fake accepts but does not act
//     on (KMS keys, customer-provided keys, checksums, the signing key).
//   - It stores bucket lifecycle and replication documents the way S3
//     does (the body of the PUT, returned on the GET), so SetLifecycle and
//     SetReplication can be read back. gofakes3 treats those PUTs as a
//     bucket creation.
type fakeS3 struct {
	next http.Handler
	// objects emulates the object features gofakes3 lacks (tags, Object
	// Lock, archive restores); see objectfake.go.
	objects *objectFeatures
	// endpoint is the fake's base URL, for steps that build their own
	// client (a role's credentials).
	endpoint string

	mu   sync.Mutex
	sent []sentRequest
	docs map[string][]byte
}

// sentRequest is one request as the fake received it.
type sentRequest struct {
	method string
	path   string
	query  string
	header http.Header
}

// bucketConfigs are the bucket subresources the fake stores itself, each
// with the error S3 answers when the bucket has none.
var bucketConfigs = map[string]string{
	"lifecycle":   "NoSuchLifecycleConfiguration",
	"replication": "ReplicationConfigurationNotFoundError",
}

// newFakeS3 returns an empty fake.
func newFakeS3() *fakeS3 {
	objects := newObjectFeatures(gofakes3.New(s3mem.New()).Server())
	return &fakeS3{next: objects, objects: objects, docs: map[string][]byte{}}
}

// ServeHTTP records the request, answers lifecycle and replication itself
// and hands everything else to gofakes3.
func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.sent = append(f.sent, sentRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone()})
	f.mu.Unlock()
	q := r.URL.Query()
	for sub, missing := range bucketConfigs {
		if q.Has(sub) {
			f.serveConfig(w, r, sub, missing)
			return
		}
	}
	f.next.ServeHTTP(w, r)
}

// serveConfig keeps the body of a PUT to a bucket subresource and returns
// it on the matching GET, so the SDK's encoding and decoding both run.
func (f *fakeS3) serveConfig(w http.ResponseWriter, r *http.Request, sub, missing string) {
	id := strings.TrimSuffix(r.URL.Path, "/") + "?" + sub
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		b, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.docs[id] = b
	case http.MethodGet:
		b, ok := f.docs[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code></Error>", missing)
			return
		}
		_, _ = w.Write(b)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// lastSent returns the headers of the latest request with this method
// whose path is path and whose query contains query, or nil.
func (f *fakeS3) lastSent(method, path, query string) http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		s := f.sent[i]
		if s.method == method && s.path == path && strings.Contains(s.query, query) {
			return s.header
		}
	}
	return nil
}
