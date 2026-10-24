package share

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

// DefaultLinkLifetime is how long the signed URL a share link redirects to
// lives: long enough to start the download, short enough not to become a
// second, unlimited link.
const DefaultLinkLifetime = 5 * time.Minute

// MaxLinkLifetime bounds WithLinkLifetime: long enough for a download
// manager that resumes a large file after a pause, short enough that the
// signed URL, which outlives the visit and is not counted again, stays
// close to the link's own limits.
const MaxLinkLifetime = time.Hour

// Where a visitor's password arrives: the form field of a POST (a landing
// page's form), or this header (an API client).
const (
	FormPassword   = "password"
	HeaderPassword = "Share-Password"
	// FormFile is the multipart field of an upload through a request link.
	FormFile = "file"
)

// HandlerOption configures Handler.
type HandlerOption func(*handlerConfig)

type handlerConfig struct {
	limiter AttemptLimiter
	disk    string
	policy  upload.Policy
	uploads bool
	// errors is WithErrors'; nil writes the plain-text answers.
	errors fshttp.ErrorWriter
	// respond is WithRespond'; nil writes JSONSuccess.
	respond SuccessWriter
	// lifetime is WithLinkLifetime's, DefaultLinkLifetime by default.
	lifetime time.Duration
	// deliver is WithDeliver's; nil redirects.
	deliver Deliverer
	// check is WithAccessCheck's; nil admits every holder of a live link.
	check func(*http.Request, Share) error
	// resolve is WithUploadPolicy's; nil applies WithUploads' policy.
	resolve UploadResolver
	// diskFor is WithUploadDisk's; nil stores on WithUploads' disk.
	diskFor func(*http.Request, Share) (string, error)
	// err is the first invalid option: every request answers 500 with it,
	// so a misconfigured handler serves nothing rather than the wrong thing.
	err error
}

// fail keeps the first invalid option's error.
func (c *handlerConfig) fail(err error) {
	if c.err == nil {
		c.err = err
	}
}

// Deliverer hands a visitor the file of a link instead of the default
// redirect: a branded download page that links to url, a page that embeds
// a preview, or a handler that streams the bytes itself. url is the
// short-lived signed URL the default would redirect to, already reported
// as served (records.ServeShare) and the link's use already spent, so a
// Deliverer must not count again. It runs for GET and for a password
// form's POST (the default answers 303 there); HEAD never reaches it.
// Cache-Control: no-store and Referrer-Policy: no-referrer are set before
// it runs, since the page carries a signed URL.
type Deliverer func(w http.ResponseWriter, r *http.Request, link Share, f records.File, url string)

// UploadResolver chooses the policy of one upload through a request link
// from the link and WithUploads' policy: a size limit per link, a type
// list per collection. Its result is checked with upload.Policy.Validate
// (an invalid one answers 500, the application's resolver being wrong). An
// error refuses the upload with the error's status (fshttp.StatusOf) and
// reads nothing of the file.
type UploadResolver func(r *http.Request, link Share, base upload.Policy) (upload.Policy, error)

// WithErrors renders every failed request with w, the ErrorWriter every
// handler of this library accepts: an HTML page for a missing link, a
// branded password form for the 401, the application's JSON envelope. The
// error it receives is for logs: ErrPassword for the 401 (render the
// password form there), a CodeNotFound error for every unusable link
// (keep those alike, so links cannot be probed), the backend's error for
// a 5xx. Without it the answers are plain status text, as before.
func WithErrors(w fshttp.ErrorWriter) HandlerOption {
	return func(c *handlerConfig) { c.errors = w }
}

// WithRespond renders the handler's JSON answers with w: a collection
// link's listing (ListResponse) and an upload through a request link
// (DepositResponse), in the application's envelope ({"data": …},
// camelCase keys). It receives the status the handler would send (200
// and 201) after Cache-Control is set. A HEAD of a listing, a download's
// redirect and WithDeliver's page do not pass here. Each body carries the
// records.File behind it (ListedFile.File, DepositResponse.File, never
// encoded), for thumbnails, dates or captions. Without it the answers are
// JSONSuccess's, as before.
func WithRespond(w SuccessWriter) HandlerOption {
	return func(c *handlerConfig) { c.respond = w }
}

// WithLinkLifetime sets how long the signed URL a download redirects to
// lives, DefaultLinkLifetime by default. d must be in (0,
// MaxLinkLifetime]; otherwise the handler answers 500 to every request.
func WithLinkLifetime(d time.Duration) HandlerOption {
	return func(c *handlerConfig) {
		if d <= 0 || d > MaxLinkLifetime {
			c.fail(filesystem.Errorf(filesystem.CodeInvalidArgument, "share: link lifetime %s not in (0, %s]", d, MaxLinkLifetime))
			return
		}
		c.lifetime = d
	}
}

// WithDeliver replaces the redirect that hands a visitor a file (see
// Deliverer).
func WithDeliver(d Deliverer) HandlerOption {
	return func(c *handlerConfig) { c.deliver = d }
}

// WithAccessCheck adds a check on who may use a link, beyond holding it
// and knowing its password: "anyone signed in to this organisation",
// "only addresses at example.com". It runs for every request to a live
// link, before the password and before anything is spent, with the link
// as stored (its Scope, Target, Mode, ID); an unusable link never reaches
// it and stays the same 404. nil allows; an error refuses with its status
// (fshttp.StatusOf: CodePermission is 403, an uncoded error 500), so a
// failing check never admits. Return a CodeNotFound error to hide the
// link from the visitor altogether.
func WithAccessCheck(check func(r *http.Request, link Share) error) HandlerOption {
	return func(c *handlerConfig) { c.check = check }
}

// WithUploadPolicy chooses the policy of each upload through a request
// link (see UploadResolver). It needs WithUploads, whose policy is the
// base it receives.
func WithUploadPolicy(resolve UploadResolver) HandlerOption {
	return func(c *handlerConfig) { c.resolve = resolve }
}

// WithUploadDisk chooses the disk of each upload through a request link,
// from the request and the link as stored (its Scope, Target, ID):
// a tenant's own bucket, a disk per collection, without one Handler per
// tenant. It runs after WithUploadPolicy and before any byte of the file
// is read, as the policy does: for a live link whose password, if it has
// one, the deposit itself checks afterwards, so the choice must not leak
// anything about the link. It must return the name of a disk in the
// Store's manager; an error refuses the upload with its status
// (fshttp.StatusOf, 500 for an uncoded one), and an empty or unknown name
// is 500 (the application's function is wrong, not the visitor), so
// nothing is stored. It needs WithUploads, whose disk it replaces; without
// it every deposit goes to that disk, as before.
func WithUploadDisk(diskFor func(r *http.Request, link Share) (string, error)) HandlerOption {
	return func(c *handlerConfig) {
		if diskFor == nil {
			c.fail(filesystem.Errorf(filesystem.CodeInvalidArgument, "share: nil WithUploadDisk"))
			return
		}
		c.diskFor = diskFor
	}
}

// WithAttemptLimiter replaces the Handler's own password limiter
// (NewAttemptLimiterClock(DefaultAttempts, DefaultAttemptWindow,
// s.Now), in this process, on the Store's clock), for instance with one
// shared by every instance.
func WithAttemptLimiter(l AttemptLimiter) HandlerOption {
	return func(c *handlerConfig) { c.limiter = l }
}

// WithUploads lets request links accept uploads, stored on the named disk
// under policy (size, types, keys). Without it a request link serves
// nothing and accepts nothing.
func WithUploads(disk string, policy upload.Policy) HandlerOption {
	return func(c *handlerConfig) { c.disk, c.policy, c.uploads = disk, policy, true }
}

// ListedFile is one file of a collection link's listing.
type ListedFile struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	// File is the file listed, for a WithRespond that shows more than
	// these fields (a thumbnail, a date, a caption) without a lookup per
	// row. It is never encoded, so JSONSuccess's bytes stay the
	// documented shape, and is always set by the handler. A SuccessWriter
	// must not modify it.
	File *records.File `json:"-"`
}

// ListResponse is the body of a collection link's listing: a JSON array
// of its unexpired files.
type ListResponse []ListedFile

// DepositResponse is the body of an upload through a request link: the
// new file's ID.
type DepositResponse struct {
	ID string `json:"id"`
	// File is the file stored, never encoded (see ListedFile.File).
	File *records.File `json:"-"`
}

// SuccessBody is a JSON answer of the handler: ListResponse or
// DepositResponse. It is closed (its method is unexported), so a
// SuccessWriter's type switch over the two covers every answer.
type SuccessBody interface{ successBody() }

func (ListResponse) successBody()    {}
func (DepositResponse) successBody() {}

// SuccessWriter renders a JSON answer (WithRespond): status is the status
// the handler would send. It must write the status and a body.
type SuccessWriter func(w http.ResponseWriter, r *http.Request, status int, body SuccessBody)

// JSONSuccess is the default SuccessWriter: body as JSON with
// Content-Type application/json.
func JSONSuccess(w http.ResponseWriter, _ *http.Request, status int, body SuccessBody) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// succeed writes a JSON answer through WithRespond, JSONSuccess without it.
func (h *handler) succeed(w http.ResponseWriter, r *http.Request, status int, body SuccessBody) {
	if h.c.respond != nil {
		h.c.respond(w, r, status, body)
		return
	}
	JSONSuccess(w, r, status, body)
}

// Handler serves share links. Mount it with the prefix stripped:
//
//	mux.Handle("/s/", http.StripPrefix("/s/", share.Handler(store, plugin)))
//
// Routes, by link:
//
//   - a file link: GET /{token} spends one download and redirects to a
//     short-lived signed URL that saves the file under its name; HEAD
//     answers 204 for a usable link without spending anything.
//   - a collection link: GET /{token} lists the live files as JSON
//     ([]ListedFile) without spending; GET /{token}/{fileID} downloads one
//     as a file link does.
//   - a request link (WithUploads): POST /{token} with a multipart "file"
//     uploads into the target, 201 with the file's ID; a password goes in
//     a "password" part before the file, or the header.
//
// Every unusable link, and every file outside its collection, is the same
// 404. WithErrors, WithLinkLifetime, WithDeliver, WithAccessCheck,
// WithUploadPolicy and WithUploadDisk change how it answers, for how long
// a download link lives, how the file is handed over, who may use a link,
// what a request link accepts and where it stores it. A link with a password answers 401 until one is given, as the form
// field "password" of a POST to the same path or the "Share-Password"
// header; a wrong one is 401 too, and spends nothing. Wrong passwords
// are counted per link (WithAttemptLimiter); past the limit the answer is
// 429, to every password, until the limiter allows the link again. Expired and view-once files follow the rule of every route that
// serves a file: an expired file is not served, a view-once one is served
// once (records.Store.Viewed).
func Handler(s *records.Store, p *Plugin, opts ...HandlerOption) http.Handler {
	c := handlerConfig{limiter: NewAttemptLimiterClock(DefaultAttempts, DefaultAttemptWindow, s.Now), lifetime: DefaultLinkLifetime}
	for _, o := range opts {
		o(&c)
	}
	h := &handler{s: s, p: p, c: c}
	return http.HandlerFunc(h.serve)
}

type handler struct {
	s *records.Store
	p *Plugin
	c handlerConfig
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	if h.c.err != nil {
		h.answer(w, r, http.StatusInternalServerError, "", h.c.err)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, HEAD, POST")
		h.answer(w, r, http.StatusMethodNotAllowed, "", filesystem.Errorf(filesystem.CodeInvalidArgument, "share: method %s not allowed", r.Method))
		return
	}
	token, fileID, _ := strings.Cut(strings.Trim(r.URL.Path, "/"), "/")
	if token == "" || strings.Contains(fileID, "/") {
		h.answer(w, r, http.StatusNotFound, "", errGone())
		return
	}
	sh, err := h.p.peek(r.Context(), h.s, token)
	if err != nil {
		h.fail(w, r, &visit{}, err)
		return
	}
	v := &visit{link: sh}
	defer h.settle(v)
	if h.c.check != nil && h.p.alive(h.s, sh) {
		if err := h.c.check(r, sh); err != nil {
			h.answer(w, r, fshttp.StatusOf(err), "", err)
			return
		}
	}
	if !h.access(w, r, v) {
		return
	}
	switch {
	case sh.Mode == ModeRequest:
		h.deposit(w, r, token, fileID, v)
	case sh.Target.Kind == KindFile && fileID == "":
		h.file(w, r, v, func() (records.File, error) {
			if r.Method == http.MethodHead {
				return h.p.Check(r.Context(), h.s, token, v.access...)
			}
			return h.p.Resolve(r.Context(), h.s, token, v.access...)
		})
	case sh.Target.Kind != KindFile && fileID == "":
		h.list(w, r, token, v)
	case sh.Target.Kind != KindFile:
		h.file(w, r, v, func() (records.File, error) {
			if r.Method == http.MethodHead {
				return h.p.CheckFile(r.Context(), h.s, token, fileID, v.access...)
			}
			return h.p.ResolveFile(r.Context(), h.s, token, fileID, v.access...)
		})
	default:
		h.answer(w, r, http.StatusNotFound, "", errGone())
	}
}

// answer writes one failed request: WithErrors' writer when set, given
// status and err; otherwise the plain answers this handler always gave,
// http.NotFound's for a 404 and text (or the status text when "") for the
// rest.
func (h *handler) answer(w http.ResponseWriter, r *http.Request, status int, text string, err error) {
	switch {
	case h.c.errors != nil:
		h.c.errors(w, r, status, err)
	case status == http.StatusNotFound:
		http.NotFound(w, r)
	default:
		if text == "" {
			text = http.StatusText(status)
		}
		http.Error(w, text, status)
	}
}

// visit is one request's link, read before its password is checked, the
// password the visitor gave, if any, whether it took a limiter attempt
// (taken) and whether the password proved wrong.
type visit struct {
	link   Share
	access []AccessOption
	taken  bool
	wrong  bool
}

// settle gives back the limiter attempt a visit took unless its password
// proved wrong, so only wrong passwords stay counted.
func (h *handler) settle(v *visit) {
	if v.taken && !v.wrong {
		h.c.limiter.Release(v.link.ID)
	}
}

// access reads the visitor's password, if any, into v; past the limiter's
// allowance for the link it answers 429 itself and returns false.
func (h *handler) access(w http.ResponseWriter, r *http.Request, v *visit) bool {
	pw := r.Header.Get(HeaderPassword)
	if pw == "" && r.Method == http.MethodPost && !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		pw = r.PostFormValue(FormPassword)
	}
	if pw == "" {
		return true
	}
	return h.guess(w, r, v, pw)
}

// guess sets v's password when the limiter grants an attempt at the
// link's password, which settle gives back unless it proves wrong;
// otherwise it answers 429 and returns false. A link without a password
// is never limited: no password of it is checked.
func (h *handler) guess(w http.ResponseWriter, r *http.Request, v *visit, pw string) bool {
	if v.link.HasPassword {
		if !h.c.limiter.Allow(v.link.ID) {
			h.answer(w, r, http.StatusTooManyRequests, "", filesystem.Errorf(filesystem.CodeUnavailable, "share: too many password attempts on link %q", v.link.ID))
			return false
		}
		v.taken = true
	}
	v.access = []AccessOption{WithPassword(pw)}
	return true
}

// msgPasswordRequired is the default 401's text.
const msgPasswordRequired = "password required"

// maxFormBytes caps a password form.
const maxFormBytes = 4 << 10

// fail answers a link error: 401 for ErrPassword, marking the visit's
// password wrong so its limiter attempt stays counted, 404 for an
// unusable link, 500 otherwise.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, v *visit, err error) {
	switch {
	case err == ErrPassword:
		v.wrong = true
		w.Header().Set("Cache-Control", "no-store")
		h.answer(w, r, http.StatusUnauthorized, msgPasswordRequired, err)
	case filesystem.CodeOf(err) == filesystem.CodeNotFound:
		h.answer(w, r, http.StatusNotFound, "", err)
	default:
		h.answer(w, r, http.StatusInternalServerError, "", err)
	}
}

// file serves one file of a link: resolve spends (or, for a HEAD, checks).
// Resolve and ResolveFile refuse an expired file and count a view-once
// file's view before they spend; a HEAD's expired file is refused here.
func (h *handler) file(w http.ResponseWriter, r *http.Request, v *visit, resolve func() (records.File, error)) {
	if r.Method == http.MethodPost && len(v.access) == 0 {
		h.answer(w, r, http.StatusNotFound, "", errGone())
		return
	}
	f, err := resolve()
	if err != nil {
		h.fail(w, r, v, err)
		return
	}
	if r.Method == http.MethodHead {
		// Check and CheckFile spend no view, so an expired file is refused
		// here; a GET's resolve refused it before spending.
		if f.Expired(h.s.Now()) {
			h.answer(w, r, http.StatusNotFound, "", errGone())
			return
		}
		// No Location: a signed URL would be a download that spent nothing.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// DownloadURL reports the link as served through this share link, and
	// issues none it could not report.
	link, err := h.s.DownloadURL(r.Context(), f, h.c.lifetime, records.ServedAs(records.ServeShare, v.link.ID))
	if err != nil {
		h.answer(w, r, http.StatusInternalServerError, "", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if h.c.deliver != nil {
		h.c.deliver(w, r, v.link, f, link)
		return
	}
	status := http.StatusFound
	if r.Method == http.MethodPost {
		status = http.StatusSeeOther // the password form's answer: follow with a GET
	}
	http.Redirect(w, r, link, status)
}

// list answers a collection link's files, leaving out expired ones.
func (h *handler) list(w http.ResponseWriter, r *http.Request, token string, v *visit) {
	files, _, err := h.p.List(r.Context(), h.s, token, v.access...)
	if err != nil {
		h.fail(w, r, v, err)
		return
	}
	now := h.s.Now()
	out := make(ListResponse, 0, len(files))
	for _, f := range files {
		if !f.Expired(now) {
			out = append(out, ListedFile{ID: f.ID, Filename: f.Filename, ContentType: f.ContentType, Size: f.Size, File: &f})
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.succeed(w, r, http.StatusOK, out)
}

// deposit takes one upload through a request link.
func (h *handler) deposit(w http.ResponseWriter, r *http.Request, token, fileID string, v *visit) {
	if !h.c.uploads || fileID != "" || r.Method != http.MethodPost {
		h.answer(w, r, http.StatusNotFound, "", errGone())
		return
	}
	policy, err := h.uploadPolicy(r, v.link)
	if err != nil {
		h.answer(w, r, fshttp.StatusOf(err), "", err)
		return
	}
	disk, err := h.uploadDisk(r, v.link)
	if err != nil {
		h.answer(w, r, fshttp.StatusOf(err), "", err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, policy.MaxSize+maxFormBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		h.answer(w, r, http.StatusBadRequest, "", filesystem.NewError(filesystem.CodeInvalidArgument, err))
		return
	}
	for {
		part, err := mr.NextPart()
		if err != nil {
			h.answer(w, r, http.StatusBadRequest, "", filesystem.NewError(filesystem.CodeInvalidArgument, err))
			return
		}
		switch part.FormName() {
		case FormPassword:
			if len(v.access) == 0 {
				pw, err := io.ReadAll(io.LimitReader(part, MaxPasswordBytes+1))
				if err != nil {
					h.answer(w, r, http.StatusBadRequest, "", filesystem.NewError(filesystem.CodeInvalidArgument, err))
					return
				}
				if !h.guess(w, r, v, string(pw)) {
					return
				}
			}
		case FormFile:
			f, err := h.p.Deposit(r.Context(), h.s, token, disk, part, part.FileName(), policy, v.access...)
			if err != nil {
				if err == ErrPassword || filesystem.CodeOf(err) == filesystem.CodeNotFound {
					h.fail(w, r, v, err)
					return
				}
				code := filesystem.CodeOf(err)
				status := http.StatusBadRequest
				switch code {
				case filesystem.CodeQuotaExceeded:
					status = http.StatusInsufficientStorage
				case filesystem.CodeTooLarge:
					status = http.StatusRequestEntityTooLarge
				case filesystem.CodeRejected:
					status = http.StatusUnsupportedMediaType
				case filesystem.CodeUnknown, filesystem.CodeUnavailable:
					status = http.StatusInternalServerError
				}
				h.answer(w, r, status, "", err)
				return
			}
			h.succeed(w, r, http.StatusCreated, DepositResponse{ID: f.ID, File: &f})
			return
		}
	}
}

// uploadPolicy is the policy of an upload through link: WithUploadPolicy's
// choice from WithUploads' policy, validated, or that policy itself. A
// resolver's error keeps its code; an invalid policy it returns is
// CodeUnknown (500), the application's resolver being wrong.
func (h *handler) uploadPolicy(r *http.Request, link Share) (upload.Policy, error) {
	if h.c.resolve == nil {
		return h.c.policy, nil
	}
	p, err := h.c.resolve(r, link, h.c.policy)
	if err != nil {
		return upload.Policy{}, err
	}
	if err := p.Validate(); err != nil {
		return upload.Policy{}, filesystem.NewError(filesystem.CodeUnknown, fmt.Errorf("share: WithUploadPolicy returned an invalid policy: %w", err))
	}
	return p, nil
}

// uploadDisk is the disk of an upload through link: WithUploadDisk's
// choice, checked against the Store's manager, or WithUploads' disk. A
// chooser's error keeps its code; an empty or unknown name is CodeUnknown
// (500), the application's chooser being wrong.
func (h *handler) uploadDisk(r *http.Request, link Share) (string, error) {
	if h.c.diskFor == nil {
		return h.c.disk, nil
	}
	name, err := h.c.diskFor(r, link)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", filesystem.NewError(filesystem.CodeUnknown, errors.New("share: WithUploadDisk returned no disk"))
	}
	if _, err := h.s.Disks().Disk(name); err != nil {
		return "", filesystem.NewError(filesystem.CodeUnknown, fmt.Errorf("share: WithUploadDisk returned disk %q: %w", name, err))
	}
	return name, nil
}
