package upload

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"sync"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
)

// HeaderIdempotencyKey is the request header a client sets to make an
// upload safe to retry (Stripe's convention): the same key returns the
// first response instead of storing the file again.
const HeaderIdempotencyKey = "Idempotency-Key"

// HeaderIdempotentReplayed marks a response replayed from an earlier
// request with the same Idempotency-Key.
const HeaderIdempotentReplayed = "Idempotent-Replayed"

// maxIdempotencyKey bounds an Idempotency-Key, which clients generate (a
// UUID is 36 characters).
const maxIdempotencyKey = 255

// maxTrailingBytes bounds what is read past the end of a successful
// request's body to fingerprint it (a newline after JSON, a multipart
// epilogue); a longer tail leaves the response unrecorded.
const maxTrailingBytes = 64 << 10

// Recorded is a response kept for replay, with the fingerprint of the
// request that produced it.
type Recorded struct {
	// Fingerprint identifies the request: its method, path and content
	// (for a multipart form, each part's name, file name and bytes, so a
	// retry with a new boundary still matches).
	Fingerprint string
	// Body is the response body.
	Body []byte
	// ContentType is the first response's Content-Type, so a replay
	// answers in the same media type a custom SuccessWriter chose
	// (application/vnd.api+json, a protobuf, text/html). Empty, as in a
	// response recorded before this field existed or one that set none,
	// replays as application/json, the default writer's type. A store
	// that persists Recorded must keep it beside Body.
	ContentType string
}

// clone copies r with its own Body. It copies the whole struct rather
// than naming fields, so a field added to Recorded is kept by
// MemoryIdempotency without anyone remembering to list it there (a store
// that named fields silently dropped ContentType once).
func (r Recorded) clone() Recorded {
	r.Body = bytes.Clone(r.Body)
	return r
}

// defaultReplayContentType is the Content-Type a replay sends when the
// recorded response kept none: JSONSuccess's.
const defaultReplayContentType = "application/json"

// IdempotencyStore remembers responses by idempotency key for a while. It
// is the seam for sharing them across instances (a database- or
// Redis-backed store); MemoryIdempotency is the in-process one.
type IdempotencyStore interface {
	// Reserve claims key for a request about to run. It returns the
	// recorded response and true when a request with key already finished,
	// or CodeConflict while one is still running; otherwise it reserves key
	// and returns false.
	Reserve(ctx context.Context, key string) (Recorded, bool, error)
	// Complete records the response of the request that reserved key.
	Complete(ctx context.Context, key string, r Recorded) error
	// Release drops a reservation whose request failed, so a retry runs.
	Release(ctx context.Context, key string) error
}

// Idempotency makes an upload handler honour HeaderIdempotencyKey. A
// request without the header runs as usual.
type Idempotency struct {
	// Store keeps the responses. Required.
	Store IdempotencyStore
	// Owner names who sent a request (a user or tenant ID), so one
	// client's key can never replay another's response. Required: a
	// deployment without users returns a constant.
	Owner func(r *http.Request) (string, error)
}

// replay runs handle under r's idempotency key: it replays the response
// recorded for the key when the request is the same one (a key reused for
// a different request is refused with 422), refuses a request whose twin
// is still running, or runs handle and records a successful response with
// the request's fingerprint. Failed responses (4xx, 5xx) are not recorded,
// so a retry can succeed.
func (i *Idempotency) replay(ew fshttp.ErrorWriter, w http.ResponseWriter, r *http.Request, handle func(w http.ResponseWriter, r *http.Request)) {
	key := r.Header.Get(HeaderIdempotencyKey)
	if i == nil || key == "" {
		handle(w, r)
		return
	}
	if i.Store == nil || i.Owner == nil {
		writeJSONError(ew, w, r, http.StatusInternalServerError, filesystem.Errorf(filesystem.CodeInvalidArgument, "idempotency needs a Store and an Owner"))
		return
	}
	if len(key) > maxIdempotencyKey {
		writeJSONError(ew, w, r, http.StatusBadRequest, filesystem.Errorf(filesystem.CodeInvalidArgument, "Idempotency-Key is longer than %d bytes", maxIdempotencyKey))
		return
	}
	owner, err := i.Owner(r)
	if err != nil {
		writeJSONError(ew, w, r, statusOf(err), err)
		return
	}
	scoped := owner + "\x00" + key
	prev, done, err := i.Store.Reserve(r.Context(), scoped)
	switch {
	case err != nil:
		writeJSONError(ew, w, r, statusOf(err), err)
		return
	case done:
		replayRecorded(ew, w, r, prev)
		return
	}
	fp := startFingerprint(r)
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	handle(rec, r)
	ctx := context.WithoutCancel(r.Context())
	sum, err := fp.finish()
	if rec.status < http.StatusBadRequest && err == nil {
		_ = i.Store.Complete(ctx, scoped, Recorded{Fingerprint: sum, Body: rec.body.Bytes(), ContentType: rec.Header().Get("Content-Type")})
		return
	}
	_ = i.Store.Release(ctx, scoped)
}

// replayRecorded answers with the first response's body and Content-Type
// (application/json when none was recorded), with 200 rather than its
// 201 (that request stored the file, not this one), after checking the
// request is the one recorded.
func replayRecorded(ew fshttp.ErrorWriter, w http.ResponseWriter, r *http.Request, prev Recorded) {
	sum, err := fingerprint(r, r.Body)
	if err != nil || sum != prev.Fingerprint {
		writeJSONError(ew, w, r, http.StatusUnprocessableEntity, filesystem.Errorf(filesystem.CodeInvalidArgument, "this Idempotency-Key was used for a different request"))
		return
	}
	contentType := prev.ContentType
	if contentType == "" {
		contentType = defaultReplayContentType
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set(HeaderIdempotentReplayed, "true")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(prev.Body)
}

// pendingFingerprint fingerprints a request body as the handler reads it,
// through a pipe, so nothing is buffered.
type pendingFingerprint struct {
	body   io.ReadCloser
	pw     *io.PipeWriter
	result chan fingerprintResult
}

type fingerprintResult struct {
	sum string
	err error
}

// startFingerprint swaps r.Body for a reader that also feeds the
// fingerprint.
func startFingerprint(r *http.Request) *pendingFingerprint {
	pr, pw := io.Pipe()
	p := &pendingFingerprint{body: r.Body, pw: pw, result: make(chan fingerprintResult, 1)}
	go func() {
		sum, err := fingerprint(r, pr)
		_, _ = io.Copy(io.Discard, pr) // let the tee finish however the parse ended
		p.result <- fingerprintResult{sum, err}
	}()
	r.Body = io.NopCloser(io.TeeReader(p.body, pw))
	return p
}

// finish reads what the handler left of the body (at most
// maxTrailingBytes), then returns the fingerprint.
func (p *pendingFingerprint) finish() (string, error) {
	n, err := io.Copy(p.pw, io.LimitReader(p.body, maxTrailingBytes+1))
	if err == nil && n > maxTrailingBytes {
		err = filesystem.Errorf(filesystem.CodeTooLarge, "request body continues past what the handler read")
	}
	_ = p.pw.CloseWithError(err)
	res := <-p.result
	return res.sum, errors.Join(err, res.err)
}

// fingerprint hashes what r asks for: its method and path, and its body,
// which for a multipart form is each part's name, file name and content,
// so the random boundary a client picks per attempt does not matter.
func fingerprint(r *http.Request, body io.Reader) (string, error) {
	h := sha256.New()
	writeField(h, []byte(r.Method))
	writeField(h, []byte(r.URL.Path))
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		if err := writeContent(h, body); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	mr := multipart.NewReader(body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return hex.EncodeToString(h.Sum(nil)), nil
		}
		if err != nil {
			return "", err
		}
		writeField(h, []byte(part.FormName()))
		writeField(h, []byte(clientFilename(part)))
		if err := writeContent(h, part); err != nil {
			return "", err
		}
	}
}

// writeContent hashes r's bytes as one field, streaming, so a body of any
// size costs no memory.
func writeContent(h hash.Hash, r io.Reader) error {
	content := sha256.New()
	if _, err := io.Copy(content, r); err != nil {
		return err
	}
	writeField(h, content.Sum(nil))
	return nil
}

// writeField hashes b length-prefixed, so no field can run into the next.
func writeField(h hash.Hash, b []byte) {
	_ = binary.Write(h, binary.BigEndian, uint64(len(b)))
	h.Write(b)
}

// recorder passes a response through while keeping its status and body.
type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

// DefaultIdempotencyEntries bounds MemoryIdempotency unless its options
// say otherwise.
const DefaultIdempotencyEntries = 100_000

// MemoryIdempotencyOptions configure NewMemoryIdempotency.
type MemoryIdempotencyOptions struct {
	// TTL is how long a response (or a running request's reservation) is
	// kept. Required, positive.
	TTL time.Duration
	// MaxEntries bounds the keys kept; 0 means DefaultIdempotencyEntries.
	// When full, the oldest entry is dropped: a flood of keys can then
	// cost an old retry its dedup, never the store's memory.
	MaxEntries int
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// MemoryIdempotency is an IdempotencyStore in this process's memory,
// bounded by TTL and MaxEntries. Responses do not survive a restart and
// are not shared between instances; behind a load balancer, use a shared
// store. Create it with NewMemoryIdempotency.
type MemoryIdempotency struct {
	opts    MemoryIdempotencyOptions
	mu      sync.Mutex
	entries map[string]*list.Element
	// order holds entries oldest first; with one TTL for every entry,
	// that is also expiry order, so expiring stops at the first live one.
	order *list.List
}

type memoryEntry struct {
	key      string
	done     bool
	recorded Recorded
	expires  time.Time
}

// NewMemoryIdempotency returns an in-memory store. A non-positive TTL or a
// negative MaxEntries is CodeInvalidArgument.
func NewMemoryIdempotency(opts MemoryIdempotencyOptions) (*MemoryIdempotency, error) {
	if opts.TTL <= 0 || opts.MaxEntries < 0 {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "idempotency TTL %s must be positive and MaxEntries not negative", opts.TTL)
	}
	if opts.MaxEntries == 0 {
		opts.MaxEntries = DefaultIdempotencyEntries
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &MemoryIdempotency{opts: opts, entries: map[string]*list.Element{}, order: list.New()}, nil
}

// Reserve implements IdempotencyStore. It drops expired entries from the
// old end as it goes, touching only those.
func (m *MemoryIdempotency) Reserve(_ context.Context, key string) (Recorded, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.opts.Now()
	for front := m.order.Front(); front != nil && !now.Before(front.Value.(*memoryEntry).expires); front = m.order.Front() {
		m.remove(front)
	}
	if el, ok := m.entries[key]; ok {
		e := el.Value.(*memoryEntry)
		if !e.done {
			return Recorded{}, false, filesystem.Errorf(filesystem.CodeConflict, "a request with this Idempotency-Key is still running")
		}
		return e.recorded.clone(), true, nil
	}
	m.put(&memoryEntry{key: key, expires: now.Add(m.opts.TTL)})
	return Recorded{}, false, nil
}

// Complete implements IdempotencyStore.
func (m *MemoryIdempotency) Complete(_ context.Context, key string, r Recorded) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.entries[key]; ok {
		m.remove(el)
	}
	m.put(&memoryEntry{key: key, done: true, recorded: r.clone(), expires: m.opts.Now().Add(m.opts.TTL)})
	return nil
}

// Release implements IdempotencyStore.
func (m *MemoryIdempotency) Release(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.entries[key]; ok {
		m.remove(el)
	}
	return nil
}

// put adds e as the newest entry, dropping the oldest when full.
func (m *MemoryIdempotency) put(e *memoryEntry) {
	if m.order.Len() >= m.opts.MaxEntries {
		m.remove(m.order.Front())
	}
	m.entries[e.key] = m.order.PushBack(e)
}

func (m *MemoryIdempotency) remove(el *list.Element) {
	delete(m.entries, el.Value.(*memoryEntry).key)
	m.order.Remove(el)
}
