// Package recordshttp serves the records layer over net/http: upload files
// and attach them to an owner, list an owner's files, reorder and detach
// them, make an existing file an owner's only one, redirect to a file,
// replace it, change its visibility, trash, restore and purge it. It is the endpoint set most apps
// write by hand, written once, and it works with any router or framework
// that accepts an http.Handler (chi, gin's WrapH, echo's WrapHandler).
//
// The library has no notion of users. Two hooks connect it to the
// application's authentication: Scope maps a request to the tenant it may
// act in (never read from the URL), and Authorize decides whether the
// request may perform an Action on a Target. Both are required: an
// endpoint set without authorization is a data leak, so the careless path
// does not compile into one.
package recordshttp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

// Action names what a request wants to do, for Authorize. It is
// records.Action, so a permission plugin (acl) can decide on actions
// without importing this package.
type Action = records.Action

// The closed set of actions, records' own under this package's names.
const (
	ActionUpload        = records.ActionUpload
	ActionList          = records.ActionList
	ActionReorder       = records.ActionReorder
	ActionDetach        = records.ActionDetach
	ActionRead          = records.ActionRead
	ActionReplace       = records.ActionReplace
	ActionTrash         = records.ActionTrash
	ActionRestore       = records.ActionRestore
	ActionPurge         = records.ActionPurge
	ActionSetAttachment = records.ActionSetAttachment
	ActionSetVisibility = records.ActionSetVisibility
	ActionSetText       = records.ActionSetText
)

// ActionValues lists every Action (records.ActionValues).
var ActionValues = records.ActionValues

// Target is what an action applies to (records.ActionTarget): Owner and
// Role on the owner routes and a detach, FileID on the file routes, the
// detach and set-attachment, and File, the file the handler loaded before
// calling Authorize, on every route that names a file. Loading first
// means a file of another scope is 404 and never reaches Authorize;
// Authorize must not modify *File. Change is the change the visibility,
// text and set-attachment routes ask for, read from the capped body before
// Authorize (records.Change), so a rule can tell publishing from
// unpublishing.
type Target = records.ActionTarget

// DefaultLinkLifetime is how long the signed URLs the file route redirects
// to live when Config.LinkLifetime is 0.
const DefaultLinkLifetime = 10 * time.Minute

// FormField is the multipart field files are uploaded in.
const FormField = "file"

// maxFormOverhead allows for boundaries and part headers on top of the
// files when capping an upload request.
const maxFormOverhead = 64 << 10

// maxJSONBody caps the JSON body of every JSON route (reorder,
// set-attachment, visibility, text). The visibility and text bodies are
// read before Authorize (Target.Change), so this cap is also what an
// unauthorized caller can make the handler read.
const maxJSONBody = 1 << 20

// Config configures Handler.
type Config struct {
	Store *records.Store
	// Disk is the name of the disk uploads are stored on. Leave it empty
	// to choose per request: with DiskFor, or, without it, by the upload
	// policy's visibility through the Store's WithVisibilityDisks (a
	// public CDN bucket and a private one behind one handler). Handler
	// refuses an empty Disk when neither is there, and refuses Disk and
	// DiskFor together, since one would silently override the other.
	Disk string
	// DiskFor chooses the disk of each upload request, after Scope,
	// Authorize and Resolve and before any byte of the body is read: a
	// tenant's own bucket, a disk per role. target is the upload's
	// (scope, owner, role). It must return the name of a disk in the
	// Store's manager. An error refuses the request with the code's
	// status (500 for an uncoded one) and stores nothing, as does an
	// empty or unknown name (500: the application's DiskFor is wrong, not
	// the request). The replace route keeps a file on its own disk and
	// never calls it.
	DiskFor func(r *http.Request, target Target) (string, error)
	// Policy governs uploads (types, size, keys, visibility).
	Policy upload.Policy
	// MaxFiles caps the files of one upload request; 0 means 10.
	MaxFiles int
	// Scope returns the scope the request acts in; an error refuses it
	// with 401 (the request is not signed in to a scope).
	Scope func(r *http.Request) (string, error)
	// Authorize decides whether the request may perform action on target;
	// an error refuses it with 403.
	Authorize func(r *http.Request, action Action, target Target) error
	// LinkLifetime of the signed URLs the file route redirects to.
	LinkLifetime time.Duration
	// Partial makes the upload route store every file it can, each in its
	// own transaction, and report the others in ListResponse.Failed, for
	// bulk uploads where one bad file should not sink the rest (as
	// upload.HandlerOptions.Partial). Without it a request attaches all
	// its files or none, which is right for a form whose files belong
	// together. Request-level failures (not multipart, over the request
	// cap, malformed, more than MaxFiles) still fail the request, after
	// purging the files it already attached.
	Partial bool
	// Resolve chooses the policy of each upload and replace request from
	// Policy, after Authorize and before any byte of the body is read
	// (the plan's size limit, a stricter type list per role); its result
	// is validated like Policy. Its error refuses the request with the
	// code's status, and a CodeUnavailable carrying a retry delay answers
	// 503 with Retry-After (a rate limit). nil uses Policy. The same
	// upload.Resolver the upload package's Handler takes.
	Resolve upload.Resolver
	// MaxTotal, when positive, caps the bytes of all the files of one
	// request together, on top of the policy's per-file MaxSize, as
	// upload.HandlerOptions.MaxTotal: each file may use only what the
	// earlier ones left, and one that does not fit is CodeTooLarge (a
	// failed file under Partial). The request body is capped at MaxTotal
	// plus form overhead when that is lower than MaxFiles × MaxSize.
	// Negative is refused by Handler.
	MaxTotal int64
	// Errors renders every failed request in the application's own
	// envelope (problem+json, an HTML page) and is where the cause of a
	// 5xx can be logged: it receives the status this handler would send
	// and the error (for the 401, 403 and 412 refusals, the hook's or the
	// precondition's error, coded CodePermission or CodeConflict).
	// Retry-After is already set when the backend asked for one. Text for
	// the client should come from fshttp.ClientMessage(err), never
	// err.Error(), which may name keys and disks. nil keeps the default
	// ErrorResponse JSON. The partial-upload answer with some files
	// failed is a ListResponse, not an error, and does not pass here.
	Errors fshttp.ErrorWriter
	// Respond renders every successful request that has a body (the
	// ListResponse, AttachedFile, FileResponse and StatusResponse routes;
	// a 204 has none and does not pass here) in the application's own
	// envelope: {"data": …}, camelCase keys, or extra fields the
	// application adds. It receives the status this handler would send
	// and the body, after the handler set its headers (ETag on the
	// replace route, Retry-After on a partial upload). Every body carries
	// the records.File behind it (AttachedFile.File, FileResponse.File,
	// StatusResponse.File, never encoded), so a gallery can add
	// dimensions, metadata or variant URLs without a lookup per row. nil
	// keeps the default JSON, byte for byte (JSONSuccess).
	Respond SuccessWriter
	// Quarantine records every file the upload route stores, and every
	// new content the replace route stores, as quarantined
	// (records.Quarantine, records.QuarantineContent): unusable until a
	// worker scans it (records.Store.Scan). The file route answers 409
	// for it meanwhile, listings show it without a URL, and the status
	// route reports "quarantined". Policy must not be public.
	Quarantine bool
}

// defaultMaxFiles caps an upload request when Config.MaxFiles is 0.
const defaultMaxFiles = 10

// Handler returns the endpoints. Mount it with its prefix stripped:
//
//	mux.Handle("/media/", http.StripPrefix("/media", recordshttp.Handler(cfg)))
//
// Routes:
//
//	POST   /owners/{type}/{id}/{role}        upload (multipart "file") and attach; 201 with the attachments
//	GET    /owners/{type}/{id}/{role}        list the attached files, in order (?limit=&cursor= for one page)
//	PUT    /owners/{type}/{id}/{role}/order  reorder: {"ids": ["att_…", …]}; 204
//	PUT    /owners/{type}/{id}/{role}        make an existing file the only one: {"file_id": "fil_…", "previous": "keep"}; 200 with the attachment
//	DELETE /attachments/{id}                 detach; 204
//	GET    /files/{id}                       302 to the file's URL
//	PUT    /files/{id}                       new content (multipart "file", one file); 200 with the file
//	DELETE /files/{id}                       move to the trash; 204
//	POST   /files/{id}/restore               take out of the trash; 204
//	POST   /files/{id}/purge                 delete for good; 204
//	PUT    /files/{id}/visibility            {"visibility": "public"}; 204
//	PUT    /files/{id}/text                  {"alt": "…", "caption": "…", "title": "…"}; 204
//	GET    /files/{id}/status                the processing status, for polling; 200
//
// A file's ETag is its SHA-256 in quotes, sent on PUT /files/{id}. The
// file routes that change it (PUT /files/{id}, DELETE /files/{id},
// restore, purge, visibility) honour If-Match: with a stale ETag they
// answer 412 and change nothing; each passes it to the Store as
// records.IfUnchanged, checked again under the row lock.
func Handler(cfg Config) (http.Handler, error) {
	if cfg.Store == nil || cfg.Scope == nil || cfg.Authorize == nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "recordshttp: Store, Scope and Authorize are required")
	}
	switch {
	case cfg.Disk != "" && cfg.DiskFor != nil:
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "recordshttp: set Disk or DiskFor, not both")
	case cfg.Disk == "" && cfg.DiskFor == nil && !cfg.Store.RoutesVisibility():
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "recordshttp: Disk is required unless DiskFor is set or the Store has WithVisibilityDisks")
	}
	if err := cfg.Policy.Validate(); err != nil {
		return nil, err
	}
	if cfg.MaxTotal < 0 {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "recordshttp: negative MaxTotal")
	}
	if cfg.MaxFiles <= 0 {
		cfg.MaxFiles = defaultMaxFiles
	}
	if cfg.LinkLifetime <= 0 {
		cfg.LinkLifetime = DefaultLinkLifetime
	}
	h := &handler{cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /owners/{type}/{id}/{role}", h.upload)
	mux.HandleFunc("GET /owners/{type}/{id}/{role}", h.list)
	mux.HandleFunc("PUT /owners/{type}/{id}/{role}", h.setAttachment)
	mux.HandleFunc("PUT /owners/{type}/{id}/{role}/order", h.reorder)
	mux.HandleFunc("DELETE /attachments/{id}", h.detach)
	mux.HandleFunc("GET /files/{id}", h.read)
	mux.HandleFunc("PUT /files/{id}", h.replace)
	mux.HandleFunc("DELETE /files/{id}", h.trash)
	mux.HandleFunc("POST /files/{id}/restore", h.restore)
	mux.HandleFunc("POST /files/{id}/purge", h.purge)
	mux.HandleFunc("PUT /files/{id}/visibility", h.setVisibility)
	mux.HandleFunc("PUT /files/{id}/text", h.setText)
	mux.HandleFunc("GET /files/{id}/status", h.status)
	return mux, nil
}

type handler struct{ cfg Config }

// AttachedFile is the JSON shape of one attached file.
type AttachedFile struct {
	AttachmentID string `json:"attachment_id"`
	FileID       string `json:"file_id"`
	Filename     string `json:"filename"`
	ContentType  string `json:"content_type"`
	Size         int64  `json:"size"`
	Position     int64  `json:"position"`
	UploadedBy   string `json:"uploaded_by,omitempty"`
	// URL is a link to the file, empty for a view-once file (fetch it
	// through GET /files/{id}, which counts its one view) and for a
	// quarantined one (poll GET /files/{id}/status until it is ready).
	URL string `json:"url,omitempty"`
	// Alt, Caption and Title are the file's describing text
	// (records.File.Text), omitted when empty.
	Alt     string `json:"alt,omitempty"`
	Caption string `json:"caption,omitempty"`
	Title   string `json:"title,omitempty"`
	// Status is the file's processing status (records.Status), and
	// StatusDetail its note, omitted when empty.
	Status       records.Status `json:"status"`
	StatusDetail string         `json:"status_detail,omitempty"`
	// File is the file the body describes, for a Config.Respond that
	// needs more than these fields: metadata, image dimensions, a variant
	// URL, without one Store.File lookup per row. It is never encoded
	// (JSONSuccess's bytes are the documented shape) and is set on every
	// body the handler builds. A SuccessWriter must not modify it.
	File *records.File `json:"-"`
}

// StatusResponse is the body of the status route: what a client polls
// while a file is processed (thumbnails, a scan), until Status is ready
// or failed.
type StatusResponse struct {
	FileID       string         `json:"file_id"`
	Status       records.Status `json:"status"`
	StatusDetail string         `json:"status_detail,omitempty"`
	// File is the file polled, never encoded (see AttachedFile.File).
	File *records.File `json:"-"`
}

// FileResponse is the body of the replace route.
type FileResponse struct {
	FileID      string `json:"file_id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	// File is the file as replaced, never encoded (see AttachedFile.File).
	File *records.File `json:"-"`
}

// ListResponse is the body of the upload and list routes. Failed is only
// ever set by an upload with Config.Partial: the files it did not store,
// in the order they were sent, with the same message rules as
// ErrorResponse.
type ListResponse struct {
	Files  []AttachedFile       `json:"files"`
	Failed []upload.FileFailure `json:"failed,omitempty"`
	// NextCursor is set by a paged list (QueryLimit or QueryCursor on the
	// request) when more files follow: pass it back as QueryCursor.
	NextCursor string `json:"next_cursor,omitempty"`
}

// Query parameters of the list route. Without either, the route lists
// every file of the owner's role in one answer, as it always has; with
// either, it answers one page (records.Store.AttachmentsPage): QueryLimit
// files at most (0 or absent is records.DefaultPageSize, at most
// records.MaxPageSize), after QueryCursor, with ListResponse.NextCursor
// when more follow. A page leaves out expired files after reading them,
// so it can hold fewer than QueryLimit files and still have a NextCursor.
const (
	QueryLimit  = "limit"
	QueryCursor = "cursor"
)

// ReorderRequest is the body of the reorder route.
type ReorderRequest struct {
	IDs []string `json:"ids"`
}

// SetAttachmentRequest is the body of the set-attachment route. Previous
// says what happens to the files it detaches (records.PreviousKeep,
// PreviousTrash or PreviousPurge, applied only to files no other
// attachment uses); empty means records.PreviousKeep, so a client that
// does not say gets the choice that loses nothing.
type SetAttachmentRequest struct {
	FileID   string           `json:"file_id"`
	Previous records.Previous `json:"previous,omitempty"`
}

// VisibilityRequest is the body of the visibility route.
type VisibilityRequest struct {
	Visibility filesystem.Visibility `json:"visibility"`
}

// TextRequest is the body of the text route: the file's whole describing
// text, as records.Store.SetText takes it ("" or an absent field removes
// that field).
type TextRequest struct {
	Alt     string `json:"alt"`
	Caption string `json:"caption"`
	Title   string `json:"title"`
}

// ErrorResponse is the body of every failed request.
type ErrorResponse struct {
	Error string          `json:"error"`
	Code  filesystem.Code `json:"code"`
}

// SuccessBody is the body of a successful request: ListResponse,
// AttachedFile, FileResponse or StatusResponse. It is closed (its method
// is unexported), so a SuccessWriter's type switch over those four
// covers every body, and a new one is a compile-visible change here.
type SuccessBody interface{ successBody() }

func (ListResponse) successBody()   {}
func (AttachedFile) successBody()   {}
func (FileResponse) successBody()   {}
func (StatusResponse) successBody() {}

// SuccessWriter renders a successful request's body (Config.Respond):
// status is the status the handler would send. It must write the status
// and a body; the handler writes nothing after it.
type SuccessWriter func(w http.ResponseWriter, r *http.Request, status int, body SuccessBody)

// JSONSuccess is the default SuccessWriter: body as JSON with
// Content-Type application/json, the shape documented on each type.
func JSONSuccess(w http.ResponseWriter, _ *http.Request, status int, body SuccessBody) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// succeed writes a successful request's body through Config.Respond,
// JSONSuccess when it is nil.
func (h *handler) succeed(w http.ResponseWriter, r *http.Request, status int, body SuccessBody) {
	if h.cfg.Respond != nil {
		h.cfg.Respond(w, r, status, body)
		return
	}
	JSONSuccess(w, r, status, body)
}

// writeJSON writes v as JSON with status: the default error answer.
func writeJSON(w http.ResponseWriter, status int, v ErrorResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fail answers err, passing on a backend's requested wait as Retry-After.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, body, err := errorBody(err)
	fshttp.SetRetryAfter(w.Header(), err)
	h.answer(w, r, status, body, err)
}

// answer writes one failed request: Config.Errors when set, given status
// and the cause, else body as JSON (the default ErrorResponse shape).
func (h *handler) answer(w http.ResponseWriter, r *http.Request, status int, body ErrorResponse, cause error) {
	if h.cfg.Errors != nil {
		h.cfg.Errors(w, r, status, cause)
		return
	}
	writeJSON(w, status, body)
}

// errorBody is the status and body err answers with, and the error to
// hand an ErrorWriter: the request cap is 413 too_large, and the message
// is fshttp.ClientMessage's, the shared rule that shows a client only the
// words of its own input's errors.
func errorBody(err error) (int, ErrorResponse, error) {
	status := fshttp.StatusOf(err)
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		// Wrap the cap itself, not err: a cap tripped mid-write arrives as
		// the Disk's annotated error, whose text names the disk and key.
		status = http.StatusRequestEntityTooLarge
		err = filesystem.NewError(filesystem.CodeTooLarge, mbe)
	}
	return status, ErrorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)}, err
}

// The messages of the hook refusals. The hooks' own errors are not
// returned, so they cannot leak policy details; like every other error
// body, they carry only the client-visible message, never the
// "filesystem: permission:" prefix of the error's text.
const (
	msgUnauthenticated = "not signed in"
	msgForbidden       = "forbidden"
	// msgPrecondition answers a failed If-Match: the client's copy is
	// stale and it should reload before saving again.
	msgPrecondition = "the file changed since it was read"
)

// scope resolves the request's scope, answering 401 itself on refusal.
func (h *handler) scope(w http.ResponseWriter, r *http.Request) (string, bool) {
	scope, err := h.cfg.Scope(r)
	if err != nil {
		cause := filesystem.NewError(filesystem.CodePermission, fmt.Errorf("recordshttp: %s: %w", msgUnauthenticated, err))
		h.answer(w, r, http.StatusUnauthorized, ErrorResponse{Error: msgUnauthenticated, Code: filesystem.CodePermission}, cause)
		return "", false
	}
	return scope, true
}

// authorize asks Config.Authorize, answering 403 itself on refusal.
func (h *handler) authorize(w http.ResponseWriter, r *http.Request, action Action, target Target) bool {
	if err := h.cfg.Authorize(r, action, target); err != nil {
		cause := filesystem.NewError(filesystem.CodePermission, fmt.Errorf("recordshttp: %s %s: %w", msgForbidden, action, err))
		h.answer(w, r, http.StatusForbidden, ErrorResponse{Error: msgForbidden, Code: filesystem.CodePermission}, cause)
		return false
	}
	return true
}

// gate resolves the scope and authorizes action on target in it.
func (h *handler) gate(w http.ResponseWriter, r *http.Request, action Action, target Target) (string, bool) {
	scope, ok := h.scope(w, r)
	if !ok {
		return "", false
	}
	target.Scope = scope
	return scope, h.authorize(w, r, action, target)
}

func ownerOf(r *http.Request) (records.Owner, string) {
	return records.Owner{Type: r.PathValue("type"), ID: r.PathValue("id")}, r.PathValue("role")
}

// gateFile resolves the scope, loads the file the URL names and
// authorizes action on it, answering the refusal itself. The file is
// loaded before Authorize so the rule can decide on it; a file of another
// scope is therefore 404, never 403, and never reaches the hook.
//
// change, when not nil, reads the change the request asks for after the
// file is found and before Authorize, into Target.Change.
func (h *handler) gateFile(w http.ResponseWriter, r *http.Request, action Action, change func() *records.Change) (records.File, bool) {
	scope, ok := h.scope(w, r)
	if !ok {
		return records.File{}, false
	}
	f, err := h.cfg.Store.File(r.Context(), scope, r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return records.File{}, false
	}
	target := Target{Scope: scope, FileID: f.ID, File: &f}
	if change != nil {
		target.Change = change()
	}
	return f, h.authorize(w, r, action, target)
}

// openForm caps the request body at limit and opens it as multipart.
func openForm(w http.ResponseWriter, r *http.Request, limit int64) (*multipart.Reader, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	return mr, nil
}

// eachFile calls fn for every file part in FormField, at most max of
// them, closing each after fn; other parts are skipped. A body that
// cannot be read (malformed, or cut off by the size cap, which fail still
// finds through the wrap), a part past max, or an error from fn ends it.
func eachFile(mr *multipart.Reader, limit int, fn func(*multipart.Part) error) error {
	seen := 0
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return filesystem.NewError(filesystem.CodeInvalidArgument, err)
		}
		if part.FormName() != FormField || part.FileName() == "" {
			_ = part.Close()
			continue
		}
		if seen == limit {
			_ = part.Close()
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "more than %d files", limit)
		}
		seen++
		err = fn(part)
		_ = part.Close()
		if err != nil {
			return err
		}
	}
}

// errNoFile is the refusal of a form without a file in FormField.
var errNoFile = filesystem.Errorf(filesystem.CodeInvalidArgument, "no file in field %q", FormField)

// upload stores every file of the form and attaches it, in one transaction:
// all files are stored and attached, or none (records.Upload deletes the
// objects of a rolled-back transaction). With Config.Partial it hands off
// to uploadPartial.
func (h *handler) upload(w http.ResponseWriter, r *http.Request) {
	owner, role := ownerOf(r)
	scope, ok := h.gate(w, r, ActionUpload, Target{Owner: owner, Role: role})
	if !ok {
		return
	}
	p, err := h.policy(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	disk, err := h.disk(r, Target{Scope: scope, Owner: owner, Role: role})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	limit := int64(h.cfg.MaxFiles) * p.MaxSize
	if h.cfg.MaxTotal > 0 && h.cfg.MaxTotal < limit {
		limit = h.cfg.MaxTotal
	}
	mr, err := openForm(w, r, limit+maxFormOverhead)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if h.cfg.Partial {
		h.uploadPartial(w, r, mr, p, disk, scope, owner, role)
		return
	}
	var out []AttachedFile
	var used int64
	err = h.cfg.Store.Tx(r.Context(), func(ctx context.Context, _ *sql.Tx) error {
		err := eachFile(mr, h.cfg.MaxFiles, func(part *multipart.Part) error {
			af, err := h.attachPart(ctx, p, disk, used, scope, owner, role, part)
			if err != nil {
				return err
			}
			used += af.Size
			out = append(out, af)
			return nil
		})
		if err == nil && len(out) == 0 {
			err = errNoFile
		}
		return err
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.succeed(w, r, http.StatusCreated, ListResponse{Files: out})
}

// uploadPartial attaches each file in its own transaction and reports the
// ones that failed, as upload.Handler does with Partial. The answer is
// 201 when every file was attached, 200 when some were, and the first
// failure's status (with the failures listed) when none was. A failure of
// the request itself purges what it attached, so the client, which only
// sees the error, is not left with files it does not know about; a purge
// that fails leaves its file attached, and a failed object deletion is
// for records.Store.SweepOrphans.
func (h *handler) uploadPartial(w http.ResponseWriter, r *http.Request, mr *multipart.Reader, p upload.Policy, disk, scope string, owner records.Owner, role string) {
	var out []AttachedFile
	var failed []upload.FileFailure
	firstStatus := 0
	// retry is the failure asking for the longest wait, if any: its wait
	// is the answer's Retry-After, so a client retrying the failed files
	// learns when to, as it would from a whole failed request.
	var retry error
	var longest time.Duration
	var used int64
	err := eachFile(mr, h.cfg.MaxFiles, func(part *multipart.Part) error {
		var af AttachedFile
		err := h.cfg.Store.Tx(r.Context(), func(ctx context.Context, _ *sql.Tx) error {
			var err error
			af, err = h.attachPart(ctx, p, disk, used, scope, owner, role, part)
			return err
		})
		var mbe *http.MaxBytesError
		switch {
		case err == nil:
			used += af.Size
			out = append(out, af)
		case errors.As(err, &mbe):
			return err // the request cap is the request's failure, not the file's
		default:
			status, body, _ := errorBody(err)
			if firstStatus == 0 {
				firstStatus = status
			}
			if after, ok := filesystem.RetryAfterOf(err); ok && after > longest {
				retry, longest = err, after
			}
			failed = append(failed, upload.FileFailure{Filename: upload.SafeFilename(part.FileName()), Error: body.Error, Code: body.Code})
		}
		return nil
	})
	if err == nil && retry != nil {
		fshttp.SetRetryAfter(w.Header(), retry)
	}
	if err == nil && len(out) == 0 && len(failed) > 0 {
		h.succeed(w, r, firstStatus, ListResponse{Files: []AttachedFile{}, Failed: failed})
		return
	}
	if err == nil && len(out) == 0 {
		err = errNoFile
	}
	if err != nil {
		ctx := context.WithoutCancel(r.Context())
		for _, af := range out {
			_ = h.cfg.Store.Purge(ctx, scope, af.FileID)
		}
		h.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if len(failed) > 0 {
		status = http.StatusOK // some files were attached, not all
	}
	h.succeed(w, r, status, ListResponse{Files: out, Failed: failed})
}

// attachPart uploads one file part under p to disk ("" lets the Store
// choose by visibility), within what MaxTotal leaves after used bytes, and
// attaches it to owner under role.
func (h *handler) attachPart(ctx context.Context, p upload.Policy, disk string, used int64, scope string, owner records.Owner, role string, part *multipart.Part) (AttachedFile, error) {
	f, err := h.within(p, used, func(p upload.Policy) (records.File, error) {
		return h.cfg.Store.Upload(ctx, scope, disk, part, part.FileName(), p, h.uploadOptions()...)
	})
	if err != nil {
		return AttachedFile{}, err
	}
	a, err := h.cfg.Store.Attach(ctx, scope, f.ID, owner, role)
	if err != nil {
		return AttachedFile{}, err
	}
	return toJSON(a, f, ""), nil
}

func toJSON(a records.Attachment, f records.File, url string) AttachedFile {
	text := f.Text()
	return AttachedFile{
		AttachmentID: a.ID, FileID: f.ID, Filename: f.Filename, ContentType: f.ContentType, Size: f.Size, Position: a.Position,
		UploadedBy: f.UploadedBy, URL: url, Alt: text.Alt, Caption: text.Caption, Title: text.Title,
		Status: f.Status, StatusDetail: f.StatusDetail, File: &f,
	}
}

// policy is the request's upload policy: Resolve's choice from Policy,
// validated, or Policy itself. It reads nothing of the body. An error
// Resolve returns keeps its code; a policy it returns that fails Validate
// is CodeUnknown (500).
func (h *handler) policy(r *http.Request) (upload.Policy, error) {
	if h.cfg.Resolve == nil {
		return h.cfg.Policy, nil
	}
	p, err := h.cfg.Resolve(r, h.cfg.Policy)
	if err != nil {
		return upload.Policy{}, err
	}
	if err := p.Validate(); err != nil {
		// The application's Resolve is wrong, not the request: 500 with
		// the status text only, since the message describes server
		// configuration.
		return upload.Policy{}, filesystem.NewError(filesystem.CodeUnknown, fmt.Errorf("recordshttp: Resolve returned an invalid policy: %w", err))
	}
	return p, nil
}

// disk is the upload request's disk: Disk, DiskFor's choice, or "" for
// the Store to choose by the policy's visibility (WithVisibilityDisks,
// checked by Handler). A DiskFor error keeps its code; an empty or unknown
// name from it is CodeUnknown (500), since the application's DiskFor is
// wrong, not the request. Nothing of the body has been read yet, so a
// refusal stores nothing.
func (h *handler) disk(r *http.Request, target Target) (string, error) {
	if h.cfg.DiskFor == nil {
		return h.cfg.Disk, nil
	}
	name, err := h.cfg.DiskFor(r, target)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", filesystem.NewError(filesystem.CodeUnknown, errors.New("recordshttp: DiskFor returned no disk"))
	}
	if _, err := h.cfg.Store.Disks().Disk(name); err != nil {
		return "", filesystem.NewError(filesystem.CodeUnknown, fmt.Errorf("recordshttp: DiskFor returned disk %q: %w", name, err))
	}
	return name, nil
}

// within runs store with p's MaxSize lowered to what MaxTotal leaves after
// used bytes, as upload's receiveWithin does: a file that cannot fit, or
// that passes the lowered size, is CodeTooLarge naming the request total
// rather than the per-file limit it did not reach.
func (h *handler) within(p upload.Policy, used int64, store func(upload.Policy) (records.File, error)) (records.File, error) {
	total := h.cfg.MaxTotal
	overTotal := filesystem.Errorf(filesystem.CodeTooLarge, "the files of this request exceed %d bytes in total", total)
	bound := false
	if total > 0 && total-used < p.MaxSize {
		p.MaxSize, bound = total-used, true
		if p.MaxSize <= 0 || p.MaxSize < p.MinSize {
			return records.File{}, overTotal
		}
	}
	f, err := store(p)
	if bound && filesystem.CodeOf(err) == filesystem.CodeTooLarge {
		return records.File{}, overTotal
	}
	return f, err
}

// uploadOptions are the records.UploadOptions of the upload route.
func (h *handler) uploadOptions() []records.UploadOption {
	if h.cfg.Quarantine {
		return []records.UploadOption{records.Quarantine()}
	}
	return nil
}

// replaceOptions are the records.ChangeOptions the replace route adds to
// its precondition guard.
func (h *handler) replaceOptions() []records.ChangeOption {
	if h.cfg.Quarantine {
		return []records.ChangeOption{records.QuarantineContent()}
	}
	return nil
}

// list returns an owner's files in a role with a link to each.
func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	owner, role := ownerOf(r)
	scope, ok := h.gate(w, r, ActionList, Target{Owner: owner, Role: role})
	if !ok {
		return
	}
	list, next, err := h.attachments(r, scope, owner, role)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]AttachedFile, 0, len(list))
	now := h.cfg.Store.Now()
	for _, af := range list {
		// The rule of GET /files/{id}: an expired file (a viewed view-once
		// one included) is not served, so it is left out; a view-once file
		// gets no link, since a listing must not spend its one view; the
		// client follows GET /files/{id}, which counts it.
		if af.File.Expired(now) {
			continue
		}
		// A quarantined file is listed, so the client can show it as
		// being scanned, but gets no link until its scan passes.
		var url string
		if !af.File.ViewOnce && !af.File.Quarantined() {
			var err error
			if url, err = h.cfg.Store.URL(r.Context(), af.File, h.cfg.LinkLifetime); err != nil {
				h.fail(w, r, err)
				return
			}
		}
		out = append(out, toJSON(af.Attachment, af.File, url))
	}
	h.succeed(w, r, http.StatusOK, ListResponse{Files: out, NextCursor: next})
}

// attachments reads the list route's files: all of them, or one page when
// the request names QueryLimit or QueryCursor, with the next page's
// cursor. A limit that is not a number is CodeInvalidArgument.
func (h *handler) attachments(r *http.Request, scope string, owner records.Owner, role string) ([]records.AttachedFile, string, error) {
	q := r.URL.Query()
	if !q.Has(QueryLimit) && !q.Has(QueryCursor) {
		list, err := h.cfg.Store.Attachments(r.Context(), scope, owner, role)
		return list, "", err
	}
	limit := 0
	if v := q.Get(QueryLimit); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, "", filesystem.Errorf(filesystem.CodeInvalidArgument, "limit must be a number")
		}
		limit = n
	}
	page, err := h.cfg.Store.AttachmentsPage(r.Context(), records.AttachmentQuery{Scope: scope, Owner: owner, Role: role, Limit: limit, Cursor: q.Get(QueryCursor)})
	return page.Files, page.NextCursor, err
}

// decodeJSON reads a capped JSON body that may hold only v's fields.
func decodeJSON[T ReorderRequest | SetAttachmentRequest | VisibilityRequest | TextRequest](w http.ResponseWriter, r *http.Request, v *T) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "invalid JSON body")
	}
	return nil
}

func (h *handler) reorder(w http.ResponseWriter, r *http.Request) {
	owner, role := ownerOf(r)
	scope, ok := h.gate(w, r, ActionReorder, Target{Owner: owner, Role: role})
	if !ok {
		return
	}
	var req ReorderRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.cfg.Store.Reorder(r.Context(), scope, owner, role, req.IDs); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setAttachment makes an existing file the only one of an owner's role
// (the new avatar replacing the old). It loads the file first, so a file
// of another scope is 404, then asks Authorize twice: ActionSetAttachment
// on the owner, role and file, and ActionRead on the file alone, because
// the owner's list will hand out a link to it. A rule written for the
// owner routes alone therefore cannot let a user attach, and so read, a
// file they may not read.
func (h *handler) setAttachment(w http.ResponseWriter, r *http.Request) {
	owner, role := ownerOf(r)
	scope, ok := h.scope(w, r)
	if !ok {
		return
	}
	var req SetAttachmentRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if req.FileID == "" {
		h.fail(w, r, filesystem.Errorf(filesystem.CodeInvalidArgument, "file_id is required"))
		return
	}
	if req.Previous == "" {
		req.Previous = records.PreviousKeep
	}
	f, err := h.cfg.Store.File(r.Context(), scope, req.FileID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !h.authorize(w, r, ActionSetAttachment, Target{Scope: scope, Owner: owner, Role: role, FileID: f.ID, File: &f, Change: &records.Change{Previous: req.Previous}}) ||
		!h.authorize(w, r, ActionRead, Target{Scope: scope, FileID: f.ID, File: &f}) {
		return
	}
	a, err := h.cfg.Store.SetAttachment(r.Context(), scope, f.ID, owner, role, req.Previous)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.succeed(w, r, http.StatusOK, toJSON(a, f, ""))
}

// detach authorizes against the attachment's owner, which it loads first:
// a detach URL names only the attachment.
func (h *handler) detach(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.scope(w, r)
	if !ok {
		return
	}
	a, err := h.cfg.Store.Attachment(r.Context(), scope, r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !h.authorize(w, r, ActionDetach, Target{Scope: scope, Owner: a.Owner, Role: a.Role, FileID: a.FileID}) {
		return
	}
	if err := h.cfg.Store.Detach(r.Context(), scope, a.ID); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// read redirects to a live file's URL. A trashed file is 404, decided
// after Authorize so only a request allowed to read it learns its state.
func (h *handler) read(w http.ResponseWriter, r *http.Request) {
	f, ok := h.gateFile(w, r, ActionRead, nil)
	if !ok {
		return
	}
	if f.DeletedAt != nil {
		h.fail(w, r, filesystem.Errorf(filesystem.CodeNotFound, "file %q is in the trash", f.ID))
		return
	}
	// Counts the view of a view-once file and refuses an expired one
	// (records.Store.Viewed): an ephemeral file is never handed out past
	// its expiry or its one view. A quarantined file is refused there too,
	// with records.ErrQuarantined, answered 409 so the client keeps
	// polling the status route. The link this answers stays valid for
	// LinkLifetime, so keep it short where view-once files are served.
	switch ok, err := h.cfg.Store.Viewed(r.Context(), f.Scope, f.ID); {
	case err != nil:
		h.fail(w, r, err)
		return
	case !ok:
		h.fail(w, r, filesystem.Errorf(filesystem.CodeNotFound, "file %q has expired", f.ID))
		return
	}
	// Store.URL reports the link as served, and issues none it could not
	// report.
	url, err := h.cfg.Store.URL(r.Context(), f, h.cfg.LinkLifetime)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	http.Redirect(w, r, url, http.StatusFound)
}

// status answers a file's processing status, authorized as ActionRead: a
// client allowed to fetch the file may watch it being processed. A
// trashed file is not found, as on the read route. The answer is never
// cached, since polling expects a fresh one.
func (h *handler) status(w http.ResponseWriter, r *http.Request) {
	f, ok := h.gateFile(w, r, ActionRead, nil)
	if !ok {
		return
	}
	if f.DeletedAt != nil {
		h.fail(w, r, filesystem.Errorf(filesystem.CodeNotFound, "file %q is in the trash", f.ID))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	h.succeed(w, r, http.StatusOK, StatusResponse{FileID: f.ID, Status: f.Status, StatusDetail: f.StatusDetail, File: &f})
}

// replace stores the first file of the form as the new content of a file;
// the file keeps its ID, attachments and name unless the upload names it.
func (h *handler) replace(w http.ResponseWriter, r *http.Request) {
	f, ok := h.gateFile(w, r, ActionReplace, nil)
	if !ok {
		return
	}
	guard, err := ifMatch(r, f)
	if err != nil {
		h.failRequest(w, r, err)
		return
	}
	p, err := h.policy(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	mr, err := openForm(w, r, p.MaxSize+maxFormOverhead)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var out records.File
	errDone := errors.New("recordshttp: first file replaced") // ends eachFile after the first file
	err = eachFile(mr, 1, func(part *multipart.Part) error {
		var err error
		out, err = h.within(p, 0, func(p upload.Policy) (records.File, error) {
			return h.cfg.Store.Replace(r.Context(), f.Scope, f.ID, part, part.FileName(), p, append(guard, h.replaceOptions()...)...)
		})
		if err != nil {
			return err
		}
		return errDone
	})
	switch {
	case errors.Is(err, errDone):
		w.Header().Set("ETag", etag(out))
		h.succeed(w, r, http.StatusOK, FileResponse{FileID: out.ID, Filename: out.Filename, ContentType: out.ContentType, Size: out.Size, File: &out})
	case err == nil:
		h.fail(w, r, errNoFile)
	default:
		h.failRequest(w, r, err)
	}
}

// failRequest is fail for a guarded request: errPrecondition and the
// Store's IfUnchanged refusal (records.ErrChanged) answer 412 instead of
// 409.
func (h *handler) failRequest(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errPrecondition) || errors.Is(err, records.ErrChanged) {
		h.answer(w, r, http.StatusPreconditionFailed, ErrorResponse{Error: msgPrecondition, Code: filesystem.CodeConflict}, filesystem.NewError(filesystem.CodeConflict, err))
		return
	}
	h.fail(w, r, err)
}

// fileAction is a file route that runs one Store call and answers 204;
// change, when not nil, reads the requested change for Authorize (see
// gateFile).
func (h *handler) fileAction(action Action, change func() *records.Change, run func(ctx context.Context, f records.File, guard []records.ChangeOption) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, ok := h.gateFile(w, r, action, change)
		if !ok {
			return
		}
		// If-Match is checked against the file as loaded, and again by the
		// Store under the row lock (the guard), so a replacement landing in
		// between is refused too.
		guard, err := ifMatch(r, f)
		if err != nil {
			h.failRequest(w, r, err)
			return
		}
		if err := run(r.Context(), f, guard); err != nil {
			h.failRequest(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *handler) trash(w http.ResponseWriter, r *http.Request) {
	h.fileAction(ActionTrash, nil, func(ctx context.Context, f records.File, guard []records.ChangeOption) error {
		return h.cfg.Store.Trash(ctx, f.Scope, f.ID, guard...)
	})(w, r)
}

func (h *handler) restore(w http.ResponseWriter, r *http.Request) {
	h.fileAction(ActionRestore, nil, func(ctx context.Context, f records.File, guard []records.ChangeOption) error {
		return h.cfg.Store.Restore(ctx, f.Scope, f.ID, guard...)
	})(w, r)
}

// purge deletes a file for good, trashed or not: whether a live file may
// be purged directly is Authorize's call (Target.File.DeletedAt says).
//
// The purge goes ahead only if the file is still in the trash state
// Authorize saw (records.IfTrashed): a Restore or Trash landing between
// the load and the purge is 503 unavailable, nothing purged, so a rule
// such as "only trashed files may be purged" cannot be raced.
func (h *handler) purge(w http.ResponseWriter, r *http.Request) {
	h.fileAction(ActionPurge, nil, func(ctx context.Context, f records.File, guard []records.ChangeOption) error {
		return h.cfg.Store.Purge(ctx, f.Scope, f.ID, append(guard, records.IfTrashed(f.DeletedAt != nil))...)
	})(w, r)
}

// setVisibility reads the capped body before Authorize, so the rule sees
// the visibility asked for (Target.Change); a body that cannot be read
// leaves Change nil and is answered 400 only once Authorize allows the
// request, after If-Match, in the order the route always answered.
func (h *handler) setVisibility(w http.ResponseWriter, r *http.Request) {
	var req VisibilityRequest
	var bodyErr error
	read := func() *records.Change {
		if bodyErr = decodeJSON(w, r, &req); bodyErr != nil {
			return nil
		}
		return &records.Change{Visibility: req.Visibility}
	}
	h.fileAction(ActionSetVisibility, read, func(ctx context.Context, f records.File, guard []records.ChangeOption) error {
		if bodyErr != nil {
			return bodyErr
		}
		return h.cfg.Store.SetVisibility(ctx, f.Scope, f.ID, req.Visibility, guard...)
	})(w, r)
}

// setText replaces a file's describing text; like setVisibility it reads
// the body before Authorize (Target.Change.Text) and answers an
// unreadable one after it. The text is not the content an ETag names, so
// an If-Match here is checked against the file as loaded only.
func (h *handler) setText(w http.ResponseWriter, r *http.Request) {
	var req TextRequest
	var bodyErr error
	read := func() *records.Change {
		if bodyErr = decodeJSON(w, r, &req); bodyErr != nil {
			return nil
		}
		return &records.Change{Text: &records.Text{Alt: req.Alt, Caption: req.Caption, Title: req.Title}}
	}
	h.fileAction(ActionSetText, read, func(ctx context.Context, f records.File, _ []records.ChangeOption) error {
		if bodyErr != nil {
			return bodyErr
		}
		_, err := h.cfg.Store.SetText(ctx, f.Scope, f.ID, records.Text{Alt: req.Alt, Caption: req.Caption, Title: req.Title})
		return err
	})(w, r)
}
