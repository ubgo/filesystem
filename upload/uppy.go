package upload

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
)

// UppyOptions configure Uppy.
type UppyOptions struct {
	// Expires is the lifetime of each signed part URL. 0 means 15 minutes.
	Expires time.Duration
	// Authorize is called with every request that continues an upload
	// (sign, list, complete, abort) and the key it names. The upload ID and
	// key come from the client, so without this check one user could
	// complete or abort another's upload. nil allows everything, which is
	// only safe when the endpoints are not reachable by untrusted users.
	Authorize func(r *http.Request, key string) error
	// Resolve chooses the policy for each request (create, and complete,
	// whose Confirm checks the assembled file) from the one given to Uppy;
	// its error refuses the request. See Resolver. It must return the
	// same limits for the requests of one upload, or a file accepted at
	// create can be refused at complete. nil uses the given policy.
	Resolve Resolver
	// OnStored runs once a completed upload has passed Confirm, with the
	// file as it is now stored, typically to create its database row (a
	// records.Store.Create). If it fails, the file is deleted and the
	// complete request fails with its error, so a stored file never
	// lacks the row the application expected. nil does nothing more.
	OnStored func(ctx context.Context, r *http.Request, file Result) error
	// Errors renders every failed request; see fshttp.ErrorWriter. nil
	// is JSONErrors.
	Errors fshttp.ErrorWriter
}

// defaultPartURLExpiry is long enough for one slow part on a mobile link.
const defaultPartURLExpiry = 15 * time.Minute

// maxUppyBodyBytes caps the JSON bodies of create and complete requests;
// a complete request lists up to 10,000 parts.
const maxUppyBodyBytes = 1 << 20

// Wire shapes of the Uppy AWS S3 multipart protocol (@uppy/aws-s3 with
// "companionUrl" or the equivalent custom endpoints). Field names are the
// protocol's and must not change.
type (
	uppyCreate struct {
		Filename string `json:"filename"`
		Type     string `json:"type"`
	}
	uppyCreated struct {
		UploadID string `json:"uploadId"`
		Key      string `json:"key"`
	}
	uppySigned struct {
		URL     string              `json:"url"`
		Headers map[string][]string `json:"headers,omitempty"`
	}
	uppyPart struct {
		PartNumber int    `json:"PartNumber"`
		Size       int64  `json:"Size,omitempty"`
		ETag       string `json:"ETag"`
	}
	uppyComplete struct {
		Parts []uppyPart `json:"parts"`
		// Filename is the client's name for the file, optional: Uppy's
		// own complete request does not send it, but a custom
		// completeMultipartUpload can repeat the one given at create.
		// It goes through Confirm's name rules (AcceptsName) and into
		// Result.Filename, display only, exactly as a presigned
		// upload's filename does.
		Filename string `json:"filename,omitempty"`
	}
	uppyCompleted struct {
		Location string `json:"location"`
		Key      string `json:"key"`
	}
)

// Uppy returns the server side of browser multipart uploads, speaking the
// protocol of Uppy's AWS S3 plugin. Mount it with the prefix stripped:
//
//	mux.Handle("/uploads/s3/", http.StripPrefix("/uploads/s3", upload.Uppy(disk, policy, opts)))
//
// Routes: POST /multipart (create), GET /multipart/{id}/{part} (sign a
// part), GET /multipart/{id} (list parts), POST /multipart/{id}/complete,
// DELETE /multipart/{id}. Every route but create takes the key as ?key=.
//
// The declared type is checked at create; on complete the assembled object
// is checked with Confirm (size and detected type) and deleted if refused.
// The disk needs the Multipart and PresignPart capabilities (S3-compatible
// backends); on any other disk every route answers 501.
func Uppy(disk *filesystem.Disk, p Policy, opts UppyOptions) http.Handler {
	if opts.Expires <= 0 {
		opts.Expires = defaultPartURLExpiry
	}
	u := &uppy{disk: disk, opts: opts}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /multipart", u.create)
	mux.HandleFunc("GET /multipart/{id}/{part}", u.authorized(u.signPart))
	mux.HandleFunc("GET /multipart/{id}", u.authorized(u.listParts))
	mux.HandleFunc("POST /multipart/{id}/complete", u.authorized(u.complete))
	mux.HandleFunc("DELETE /multipart/{id}", u.authorized(u.abort))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caps := disk.Capabilities()
		if !caps.Multipart || !caps.PresignPart {
			writeJSONError(opts.Errors, w, r, http.StatusNotImplemented, filesystem.Errorf(filesystem.CodeUnsupported, "disk %s cannot sign multipart uploads", disk.Name()))
			return
		}
		if err := p.Validate(); err != nil {
			writeJSONError(opts.Errors, w, r, http.StatusInternalServerError, err)
			return
		}
		resolved, err := resolve(opts.Resolve, r, p)
		if err != nil {
			writeResolveError(opts.Errors, w, r, err)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), policyKey{}, resolved))
		if _, pattern := mux.Handler(r); pattern == "" && opts.Errors != nil {
			// No route matches: ServeMux would answer its own plain 404
			// or 405, which a custom writer renders instead.
			unrouted(mux, opts.Errors, w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// errNoRoute is the error an ErrorWriter gets for a request no Uppy route
// matches (a wrong path or method).
var errNoRoute = filesystem.Errorf(filesystem.CodeInvalidArgument, "no Uppy route for this method and path")

// unrouted answers a request no route of mux matches through ew, with the
// status ServeMux chose (404 or 405) and its Allow header.
func unrouted(mux *http.ServeMux, ew fshttp.ErrorWriter, w http.ResponseWriter, r *http.Request) {
	probe := &statusProbe{header: http.Header{}, status: http.StatusOK}
	mux.ServeHTTP(probe, r)
	if allow := probe.header.Get("Allow"); allow != "" {
		w.Header().Set("Allow", allow)
	}
	writeJSONError(ew, w, r, probe.status, errNoRoute)
}

// statusProbe records the status and headers a handler chose and drops
// its body.
type statusProbe struct {
	header http.Header
	status int
}

func (p *statusProbe) Header() http.Header         { return p.header }
func (p *statusProbe) WriteHeader(status int)      { p.status = status }
func (p *statusProbe) Write(b []byte) (int, error) { return len(b), nil }

type uppy struct {
	disk *filesystem.Disk
	opts UppyOptions
}

// authorized extracts ?key=, runs the Authorize hook and passes the key on.
func (u *uppy) authorized(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		if key == "" {
			writeJSONError(u.opts.Errors, w, r, http.StatusBadRequest, filesystem.Errorf(filesystem.CodeInvalidArgument, "missing key parameter"))
			return
		}
		if u.opts.Authorize != nil {
			if err := u.opts.Authorize(r, key); err != nil {
				writeJSONError(u.opts.Errors, w, r, http.StatusForbidden, filesystem.NewError(filesystem.CodePermission, err))
				return
			}
		}
		next(w, r, key)
	}
}

func decode[T uppyCreate | uppyComplete](ew fshttp.ErrorWriter, w http.ResponseWriter, r *http.Request, into *T) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxUppyBodyBytes)).Decode(into); err != nil {
		writeJSONError(ew, w, r, http.StatusBadRequest, filesystem.Errorf(filesystem.CodeInvalidArgument, "invalid JSON body"))
		return false
	}
	return true
}

func (u *uppy) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeJSONError(u.opts.Errors, w, r, statusOf(err), err)
}

// policyKey carries the request's resolved policy from Uppy's handler to
// its routes.
type policyKey struct{}

// policyOf is the policy resolved for r. Every route runs behind Uppy's
// handler, which always sets it.
func policyOf(r *http.Request) Policy {
	p, _ := r.Context().Value(policyKey{}).(Policy)
	return p
}

func (u *uppy) create(w http.ResponseWriter, r *http.Request) {
	var req uppyCreate
	if !decode(u.opts.Errors, w, r, &req) {
		return
	}
	policy := policyOf(r)
	if err := policy.Accepts(req.Type); err != nil {
		u.fail(w, r, err)
		return
	}
	if err := policy.AcceptsName(req.Filename, req.Type); err != nil {
		u.fail(w, r, err)
		return
	}
	info := Info{Filename: SafeFilename(req.Filename), ContentType: req.Type}
	key, err := policy.key(r.Context(), info)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	headers, err := policy.headers(info)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	key, conditional, err := policy.claim(r.Context(), u.disk, key)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	id, err := u.disk.CreateMultipart(r.Context(), key, filesystem.WriteOptions{ContentType: req.Type, Visibility: policy.Visibility, Headers: headers, IfNotExists: conditional})
	if err != nil {
		u.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, uppyCreated{UploadID: id, Key: key})
}

func (u *uppy) signPart(w http.ResponseWriter, r *http.Request, key string) {
	n, err := strconv.Atoi(r.PathValue("part"))
	if err != nil {
		u.fail(w, r, filesystem.Errorf(filesystem.CodeInvalidArgument, "part number %q is not a number", r.PathValue("part")))
		return
	}
	req, err := u.disk.PresignPart(r.Context(), key, r.PathValue("id"), n, u.opts.Expires)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, uppySigned{URL: req.URL, Headers: req.Header})
}

func (u *uppy) listParts(w http.ResponseWriter, r *http.Request, key string) {
	parts, err := u.disk.ListParts(r.Context(), key, r.PathValue("id"))
	if err != nil {
		u.fail(w, r, err)
		return
	}
	out := make([]uppyPart, 0, len(parts))
	for _, p := range parts {
		out = append(out, uppyPart{PartNumber: p.Number, Size: p.Size, ETag: p.ETag})
	}
	writeJSON(w, http.StatusOK, out)
}

func (u *uppy) complete(w http.ResponseWriter, r *http.Request, key string) {
	var req uppyComplete
	if !decode(u.opts.Errors, w, r, &req) {
		return
	}
	parts := make([]filesystem.Part, 0, len(req.Parts))
	for _, p := range req.Parts {
		parts = append(parts, filesystem.Part{Number: p.PartNumber, ETag: strings.Trim(p.ETag, `"`)})
	}
	e, err := u.disk.CompleteMultipart(r.Context(), key, r.PathValue("id"), parts)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	// Confirm's Result is where the file is now: a Settle step
	// (ContentAddressed) may have moved it from the key the client sent.
	res, err := Confirm(r.Context(), u.disk, e.Key, req.Filename, policyOf(r))
	if err != nil {
		u.fail(w, r, err)
		return
	}
	if u.opts.OnStored != nil {
		if err := u.opts.OnStored(r.Context(), r, res); err != nil {
			discard(r.Context(), u.disk, []Result{res})
			u.fail(w, r, err)
			return
		}
	}
	// Uppy reports location as the file's URL; a disk without public URLs
	// (private files) leaves it empty and the client uses the key.
	done := uppyCompleted{Key: res.Key}
	if loc, err := u.disk.PublicURL(r.Context(), res.Key); err == nil {
		done.Location = loc
	}
	writeJSON(w, http.StatusOK, done)
}

func (u *uppy) abort(w http.ResponseWriter, r *http.Request, key string) {
	if err := u.disk.AbortMultipart(r.Context(), key, r.PathValue("id")); err != nil {
		u.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
