package upload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/sniff"
)

// TicketRequest is what a client sends to ask for a direct upload.
type TicketRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	// SHA256 is the hex SHA-256 the client computed, optional. Presign
	// checks its form only; keep it with the ticket and pass it to
	// ConfirmExpected, which refuses an object whose bytes do not match.
	SHA256 string `json:"sha256,omitempty"`
	// Visibility, optional, asks for the uploaded object to be stored
	// public or private; the ticket then signs it, so the client cannot
	// change it at upload time. Public needs Policy.AllowPublic. Empty
	// signs nothing and the object gets the backend's default, as before
	// this field existed.
	Visibility filesystem.Visibility `json:"visibility,omitempty"`
}

// Expected is what the client declared about a direct upload, for
// ConfirmExpected to hold the stored object to: the size it announced
// (catches a truncated or padded upload) and, when it sent one, the
// SHA-256 of its bytes (catches corruption and a swapped file). Zero
// fields are not checked.
type Expected struct {
	Size   int64
	SHA256 string
}

// sha256HexLen is the length of a hex SHA-256 digest.
const sha256HexLen = sha256.Size * 2

// validDigest reports whether s is empty or a hex SHA-256.
func validDigest(s string) error {
	if s == "" {
		return nil
	}
	if _, err := hex.DecodeString(s); err != nil || len(s) != sha256HexLen {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "SHA-256 %q is not %d hex characters", s, sha256HexLen)
	}
	return nil
}

// Ticket authorises one direct upload: the client sends the file with
// Method to URL with Headers, then asks the server to Confirm Key. A form
// ticket (Policy.PostForm) has Method POST and Fields instead of Headers:
// the client posts a multipart form with every field, then the file in
// the field named "file", last.
type Ticket struct {
	Key       string              `json:"key"`
	Method    string              `json:"method"`
	URL       string              `json:"url"`
	Headers   map[string][]string `json:"headers,omitempty"`
	Fields    map[string]string   `json:"fields,omitempty"`
	ExpiresAt time.Time           `json:"expires_at"`
}

// Presign issues a Ticket for uploading straight to the disk's backend,
// so large files never pass through the application. The declared type and
// size are checked against p, but they are the client's word: Confirm must
// run after the upload, because only then can the server see the bytes.
// The signed URL pins the declared content type, so the backend refuses a
// different Content-Type header. The policy's Headers are pinned the same
// way: the Ticket's Headers list them, the client must send them, and the
// backend stores them. A disk that cannot pin them in a presigned PUT
// (see filesystem.PresignOptions.Headers) refuses with CodeUnsupported
// rather than issue a ticket that would lose them. A request asking for a
// visibility (TicketRequest.Visibility) has it signed the same way;
// public needs Policy.AllowPublic (CodePermission otherwise).
func Presign(ctx context.Context, disk *filesystem.Disk, req TicketRequest, p Policy, expires time.Duration) (Ticket, error) {
	if err := p.Validate(); err != nil {
		return Ticket{}, err
	}
	if req.Size <= 0 || req.Size > p.MaxSize {
		return Ticket{}, filesystem.Errorf(filesystem.CodeTooLarge, "declared size %d is not in (0, %d]", req.Size, p.MaxSize)
	}
	if err := p.Accepts(req.ContentType); err != nil {
		return Ticket{}, err
	}
	if err := p.AcceptsName(req.Filename, req.ContentType); err != nil {
		return Ticket{}, err
	}
	if err := p.checkMinSize(req.Size); err != nil {
		return Ticket{}, err
	}
	if err := validDigest(req.SHA256); err != nil {
		return Ticket{}, err
	}
	visibility, err := p.ticketVisibility(req.Visibility)
	if err != nil {
		return Ticket{}, err
	}
	if p.Collision != CollisionOverwrite {
		return Ticket{}, filesystem.Errorf(filesystem.CodeUnsupported, "a presigned upload replaces any object at its key; collision policy %q needs Receive or the Uppy handler", p.Collision)
	}
	info := Info{Filename: SafeFilename(req.Filename), ContentType: req.ContentType}
	headers, err := p.headers(info)
	if err != nil {
		return Ticket{}, err
	}
	key, err := p.key(ctx, info)
	if err != nil {
		return Ticket{}, err
	}
	if p.PostForm {
		// The declared size is the only size the backend accepts.
		post, err := disk.PresignPost(ctx, filesystem.PostPolicy{Key: key, ContentType: req.ContentType, MinSize: req.Size, MaxSize: req.Size, Expires: expires, Headers: headers, Visibility: visibility})
		if err != nil {
			return Ticket{}, err
		}
		return Ticket{Key: key, Method: http.MethodPost, URL: post.URL, Fields: post.Fields, ExpiresAt: post.ExpiresAt}, nil
	}
	signed, err := disk.SignedURL(ctx, key, filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: expires, ContentType: req.ContentType, Headers: headers, Visibility: visibility})
	if err != nil {
		return Ticket{}, err
	}
	return Ticket{Key: key, Method: string(signed.Method), URL: signed.URL, Headers: signed.Header, ExpiresAt: signed.ExpiresAt}, nil
}

// ticketVisibility is the visibility a ticket signs. With
// Policy.SignVisibility it is the policy's, and a request asking for a
// different one is refused with CodePermission rather than overridden;
// otherwise it is the request's, checked by allowsVisibility.
func (p Policy) ticketVisibility(requested filesystem.Visibility) (filesystem.Visibility, error) {
	if !p.SignVisibility {
		return requested, p.allowsVisibility(requested)
	}
	if requested != "" && requested != p.Visibility {
		return "", filesystem.Errorf(filesystem.CodePermission, "this upload's visibility is %q, set by the server", p.Visibility)
	}
	return p.Visibility, p.allowsVisibility(p.Visibility)
}

// allowsVisibility refuses a ticket request's visibility the policy does
// not allow: an unknown value is CodeInvalidArgument, public without
// AllowPublic is CodePermission. Empty and private are always allowed;
// whether the disk can pin them is the Disk's check.
func (p Policy) allowsVisibility(v filesystem.Visibility) error {
	switch {
	case v == "":
		return nil
	case !v.Valid():
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "unknown visibility %q", v)
	case v == filesystem.VisibilityPublic && !p.AllowPublic:
		return filesystem.Errorf(filesystem.CodePermission, "this upload cannot be public")
	}
	return nil
}

// Confirm checks a directly uploaded object against p using the stored
// bytes: its size, its type detected from its first bytes, and, where the
// backend stores one, that the stored Content-Type names that same type
// (the client must send the Content-Type the Ticket's Headers carry). An
// object that fails is deleted and the error returned (CodeTooLarge or
// CodeRejected), so nothing the policy refuses stays on the disk. filename
// is the name the client reported, for the Result only.
func Confirm(ctx context.Context, disk *filesystem.Disk, key, filename string, p Policy) (Result, error) {
	return ConfirmExpected(ctx, disk, key, filename, p, Expected{})
}

// ConfirmExpected is Confirm that also holds the stored object to what the
// client declared when it asked for the ticket: a stored size other than
// want.Size, or a SHA-256 other than want.SHA256 (compared without case),
// deletes the object and refuses it with CodeRejected. A malformed
// want.SHA256 is CodeInvalidArgument and nothing is read or deleted.
func ConfirmExpected(ctx context.Context, disk *filesystem.Disk, key, filename string, p Policy, want Expected) (Result, error) {
	if err := p.Validate(); err != nil {
		return Result{}, err
	}
	if err := validDigest(want.SHA256); err != nil {
		return Result{}, err
	}
	e, err := disk.Stat(ctx, key)
	if err != nil {
		return Result{}, err
	}
	if e.Size > p.MaxSize {
		return Result{}, reject(ctx, disk, e.Key, filesystem.Errorf(filesystem.CodeTooLarge, "stored size %d exceeds %d", e.Size, p.MaxSize))
	}
	if want.Size > 0 && e.Size != want.Size {
		return Result{}, reject(ctx, disk, e.Key, filesystem.Errorf(filesystem.CodeRejected, "stored size %d is not the %d bytes declared", e.Size, want.Size))
	}
	if err := p.checkMinSize(e.Size); err != nil {
		return Result{}, reject(ctx, disk, e.Key, err)
	}
	head, sum, sums, err := headAndHash(ctx, disk, e, p.Checksums)
	if err != nil {
		return Result{}, err
	}
	if want.SHA256 != "" && !strings.EqualFold(sum, want.SHA256) {
		return Result{}, reject(ctx, disk, e.Key, filesystem.Errorf(filesystem.CodeRejected, "the stored bytes do not match the declared SHA-256"))
	}
	for _, c := range p.Checksums {
		if c.Want != "" && !strings.EqualFold(sums[c.Name], c.Want) {
			return Result{}, reject(ctx, disk, e.Key, filesystem.Errorf(filesystem.CodeRejected, "the stored bytes do not match the declared %s checksum", c.Name))
		}
	}
	name := SafeFilename(filename)
	contentType := p.detect(head, name)
	if err := p.Accepts(contentType); err != nil {
		return Result{}, reject(ctx, disk, e.Key, err)
	}
	if err := p.AcceptsName(name, contentType); err != nil {
		return Result{}, reject(ctx, disk, e.Key, err)
	}
	if err := storedTypeMatches(disk.Capabilities().ContentType, e.ContentType, contentType); err != nil {
		return Result{}, reject(ctx, disk, e.Key, err)
	}
	res, err := p.inspect(ctx, disk, Result{Key: e.Key, Filename: name, Path: SafePath(filename), ContentType: contentType, Size: e.Size, SHA256: sum, Checksums: sums})
	if err != nil {
		return Result{}, err
	}
	return p.settle(ctx, disk, res)
}

// storedTypeMatches refuses an object whose stored Content-Type (the one
// the backend will serve it with) would make a browser render bytes that
// are not of that type. Not every backend can pin the type of a presigned
// PUT (an Azure SAS cannot), so a client could store PNG bytes declared as
// text/html and have them served as HTML; checking here closes that on
// every backend.
//
// Only a dangerous declaration is refused: an active type (sniff.IsActive)
// the content is not, or a rendered text or markup type (rendersAsText)
// on content that is not text. Every other difference is an alias or a
// generic label (audio/wav for audio/wave, application/x-zip-compressed
// for application/zip, application/octet-stream for anything) and passes,
// because refusing it would delete good uploads. Parameters and case are
// ignored. A disk without Capabilities.ContentType is not compared: what
// its Stat reports is derived (local disk guesses from the extension), not
// what the client declared. The message names both types and never the key.
func storedTypeMatches(storesType bool, stored, detected string) error {
	if !storesType || stored == "" {
		return nil
	}
	st, dt := mediaType(stored), mediaType(detected)
	if st == dt {
		return nil
	}
	if sniff.IsActive(st) || (rendersAsText(st) && !rendersAsText(dt)) {
		return filesystem.Errorf(filesystem.CodeRejected, "the upload was declared as %q but the content is %q", st, dt)
	}
	return nil
}

// Media type patterns a browser displays as text or markup when served
// inline, which a declared type must not claim for content that is not.
const (
	textTypePrefix   = "text/"
	xmlSuffix        = "+xml"
	xmlSubtypeSuffix = "/xml"
)

// scriptTypes are the script types outside text/ a browser may execute.
var scriptTypes = []string{"application/javascript", "application/ecmascript", "application/x-javascript"}

// rendersAsText reports whether a browser shows a media type as text or
// markup: text/*, any XML (+xml or /xml), and scripts.
func rendersAsText(mt string) bool {
	return strings.HasPrefix(mt, textTypePrefix) || strings.HasSuffix(mt, xmlSuffix) || strings.HasSuffix(mt, xmlSubtypeSuffix) || slices.Contains(scriptTypes, mt)
}

// mediaType is the lowercase type/subtype of a Content-Type value, without
// parameters. A value mime cannot parse is cut at its first ";" instead,
// so a malformed declaration still compares by its type.
func mediaType(v string) string {
	if mt, _, err := mime.ParseMediaType(v); err == nil {
		return mt
	}
	t, _, _ := strings.Cut(v, ";")
	return strings.ToLower(strings.TrimSpace(t))
}

// headAndHash reads the first sniff.HeadSize bytes of a stored object and
// its SHA-256, plus a lowercase hex digest for each of sums (nil when sums
// is empty). A backend that reports the SHA-256 is trusted and, with no
// sums, only the head is read; otherwise the object is read once, whole,
// which its size (already checked against the policy) bounds. The bytes
// are the client's, so every other digest is the server's own.
func headAndHash(ctx context.Context, disk *filesystem.Disk, e filesystem.Entry, sums []filesystem.Checksum) ([]byte, string, map[string]string, error) {
	whole := e.SHA256 == "" || len(sums) > 0
	length := int64(sniff.HeadSize)
	if whole {
		length = 0 // the whole object
	}
	rc, err := disk.ReadRange(ctx, e.Key, filesystem.ByteRange{Length: length})
	if err != nil {
		return nil, "", nil, err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	writers := []io.Writer{h}
	extra := make([]hash.Hash, len(sums))
	for i, c := range sums {
		extra[i] = c.New()
		writers = append(writers, extra[i])
	}
	tee := io.TeeReader(rc, io.MultiWriter(writers...))
	head := make([]byte, sniff.HeadSize)
	n, err := io.ReadFull(tee, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, "", nil, err
	}
	if !whole {
		return head[:n], e.SHA256, nil, nil
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return nil, "", nil, err
	}
	var digests map[string]string
	if len(sums) > 0 {
		digests = make(map[string]string, len(sums))
		for i, c := range sums {
			digests[c.Name] = hex.EncodeToString(extra[i].Sum(nil))
		}
	}
	sum := e.SHA256
	if sum == "" {
		sum = hex.EncodeToString(h.Sum(nil))
	}
	return head[:n], sum, digests, nil
}

// reject deletes a refused object and returns why it was refused. A failed
// delete is joined into the error so the caller knows the object remains.
func reject(ctx context.Context, disk *filesystem.Disk, key string, why error) error {
	if err := disk.Delete(context.WithoutCancel(ctx), key); err != nil {
		return errors.Join(why, err)
	}
	return why
}

// maxTicketRequestBytes caps the JSON body of a ticket request.
const maxTicketRequestBytes = 16 << 10

// PresignHandler answers POSTed TicketRequest JSON with a Ticket. Mount it
// behind the application's authentication. A KeyFunc sees only the file's
// Info, never the request, so to place keys per user build the handler per
// request with a Policy whose Key closes over that user:
//
//	http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//		p := base
//		p.Key = upload.RandomKey("users/" + userID(r))
//		upload.PresignHandler(disk, p, time.Minute).ServeHTTP(w, r)
//	})
func PresignHandler(disk *filesystem.Disk, p Policy, expires time.Duration) http.Handler {
	return PresignHandlerWith(disk, p, expires, PresignHandlerOptions{})
}

// PresignHandlerOptions configure PresignHandlerWith.
type PresignHandlerOptions struct {
	// Resolve chooses the policy for each ticket request from the one
	// given, before the body is read; its error refuses the request. See
	// Resolver. nil uses the given policy.
	Resolve Resolver
	// Idempotency, when set, makes a retried ticket request (same
	// Idempotency-Key) return the first ticket instead of a new key.
	Idempotency *Idempotency
	// Errors renders a failed ticket request; see fshttp.ErrorWriter.
	// nil is JSONErrors.
	Errors fshttp.ErrorWriter
	// Respond renders the Ticket of a successful request; see
	// SuccessWriter. nil is JSONSuccess, today's body unchanged.
	Respond SuccessWriter
}

// PresignHandlerWith is PresignHandler with options, chiefly a Resolver,
// so per-user limits and keys need no handler built per request.
func PresignHandlerWith(disk *filesystem.Disk, base Policy, expires time.Duration, opts PresignHandlerOptions) http.Handler {
	handle := func(w http.ResponseWriter, r *http.Request) {
		p, err := resolve(opts.Resolve, r, base)
		if err != nil {
			writeResolveError(opts.Errors, w, r, err)
			return
		}
		var req TicketRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTicketRequestBytes))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSONError(opts.Errors, w, r, http.StatusBadRequest, filesystem.Errorf(filesystem.CodeInvalidArgument, "invalid ticket request"))
			return
		}
		t, err := Presign(r.Context(), disk, req, p, expires)
		if err != nil {
			writeJSONError(opts.Errors, w, r, statusOf(err), err)
			return
		}
		writeSuccess(opts.Respond, w, r, http.StatusOK, t)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSONError(opts.Errors, w, r, http.StatusMethodNotAllowed, filesystem.Errorf(filesystem.CodeInvalidArgument, "ticket requests must be POSTed"))
			return
		}
		opts.Idempotency.replay(opts.Errors, w, r, handle)
	})
}
