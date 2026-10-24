package upload

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
)

// DefaultField is the multipart form field Handler reads files from.
const DefaultField = "file"

// formOverheadBytes allows for multipart boundaries, part headers and small
// non-file fields on top of the files themselves when capping a request.
const formOverheadBytes = 64 << 10

// HandlerOptions configure Handler.
type HandlerOptions struct {
	// Field is the form field holding files. Empty means DefaultField.
	Field string
	// MaxFiles caps the files per request. 0 means 1.
	MaxFiles int
	// Partial stores every file it can and reports the others in
	// Response.Failed, for bulk uploads where one bad file should not sink
	// the rest. Without it a request stores all its files or none, which
	// is right for a form whose files belong together. Request-level
	// failures (not multipart, over the request cap, malformed) still fail
	// the request, after deleting what it stored.
	Partial bool
	// Resolve chooses the policy for each request from the one given to
	// Handler, before the body is read; its error refuses the request.
	// See Resolver. nil uses the given policy.
	Resolve Resolver
	// MaxTotal, when positive, caps the bytes of all the files of one
	// request together, on top of the policy's per-file MaxSize: each file
	// may use only what the earlier ones left, and one that does not fit
	// is CodeTooLarge (a failed file under Partial). The request body is
	// capped at MaxTotal plus form overhead when that is lower than
	// MaxFiles × MaxSize.
	MaxTotal int64
	// Idempotency, when set, makes retries of one upload (same
	// Idempotency-Key header) return the first response instead of
	// storing the files again: offline queues and flaky mobile links
	// retry without duplicates. nil ignores the header.
	Idempotency *Idempotency
	// OnStored runs after the request's files are stored (with Partial, the
	// ones that were), typically to create database records. If it fails,
	// the stored files are deleted and the request fails.
	OnStored func(ctx context.Context, r *http.Request, files []Result) error
	// Errors renders every failed request (the policy's refusals, the
	// Disk's errors, a malformed form, a refused Idempotency-Key); see
	// fshttp.ErrorWriter. nil is JSONErrors, the ErrorResponse body. A
	// Partial request that stored nothing still answers its Response
	// with Failed, which is a result, not an error.
	Errors fshttp.ErrorWriter
	// Respond renders the Response of every request that stored files,
	// and of a Partial request that stored none; see SuccessWriter. nil
	// is JSONSuccess, today's body unchanged.
	Respond SuccessWriter
}

// Response is the JSON body Handler answers with on success. Failed is
// only ever set with HandlerOptions.Partial.
type Response struct {
	Files  []Result      `json:"files"`
	Failed []FileFailure `json:"failed,omitempty"`
}

// FileFailure is one file a Partial request did not store.
type FileFailure struct {
	Filename string          `json:"filename"`
	Error    string          `json:"error"`
	Code     filesystem.Code `json:"code"`
}

// ErrorResponse is the JSON body of a failed request. Code is the
// filesystem.Code, so clients can tell "too large" from "wrong type".
type ErrorResponse struct {
	Error string          `json:"error"`
	Code  filesystem.Code `json:"code"`
}

// Handler accepts multipart/form-data POST uploads into disk under policy p.
// Files stream straight to the disk (no temporary copy, no memory buffer of
// the whole body); the whole request is capped at MaxFiles × MaxSize plus a
// small allowance for form overhead. All files of a request are stored, or
// none: a failure deletes the ones already stored.
func Handler(disk *filesystem.Disk, base Policy, opts HandlerOptions) http.Handler {
	if opts.Field == "" {
		opts.Field = DefaultField
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = 1
	}
	handle := func(w http.ResponseWriter, r *http.Request) {
		if err := base.Validate(); err != nil {
			writeJSONError(opts.Errors, w, r, http.StatusInternalServerError, err)
			return
		}
		if opts.MaxTotal < 0 {
			writeJSONError(opts.Errors, w, r, http.StatusInternalServerError, filesystem.Errorf(filesystem.CodeInvalidArgument, "negative MaxTotal"))
			return
		}
		p, err := resolve(opts.Resolve, r, base)
		if err != nil {
			writeResolveError(opts.Errors, w, r, err)
			return
		}
		limit := int64(opts.MaxFiles) * p.MaxSize
		if opts.MaxTotal > 0 && opts.MaxTotal < limit {
			limit = opts.MaxTotal
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit+formOverheadBytes)
		files, failed, err := receiveForm(r, disk, p, opts)
		if err == nil && len(files) == 0 && len(failed) > 0 {
			// Nothing stored: the request fails as the first file did, and
			// says what happened to each.
			writeSuccess(opts.Respond, w, r, failed[0].status, Response{Files: []Result{}, Failed: failures(failed)})
			return
		}
		if err == nil && len(files) == 0 {
			err = filesystem.Errorf(filesystem.CodeInvalidArgument, "no file in field %q", opts.Field)
		}
		if err == nil && opts.OnStored != nil {
			err = opts.OnStored(r.Context(), r, files)
		}
		if err != nil {
			discard(r.Context(), disk, files)
			writeJSONError(opts.Errors, w, r, statusOf(err), err)
			return
		}
		status := http.StatusCreated
		if len(failed) > 0 {
			status = http.StatusOK // some files were stored, not all
		}
		writeSuccess(opts.Respond, w, r, status, Response{Files: files, Failed: failures(failed)})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSONError(opts.Errors, w, r, http.StatusMethodNotAllowed, filesystem.Errorf(filesystem.CodeInvalidArgument, "uploads must be POSTed"))
			return
		}
		opts.Idempotency.replay(opts.Errors, w, r, handle)
	})
}

// failedFile is a file a Partial request did not store, with the status
// its error maps to.
type failedFile struct {
	FileFailure
	status int
}

func failures(list []failedFile) []FileFailure {
	if len(list) == 0 {
		return nil
	}
	out := make([]FileFailure, len(list))
	for i, f := range list {
		out[i] = f.FileFailure
	}
	return out
}

// receiveForm streams every file part of the request through Receive. A
// file's own failure ends the request unless opts.Partial, which records
// it and goes on; err is a failure of the request itself.
func receiveForm(r *http.Request, disk *filesystem.Disk, p Policy, opts HandlerOptions) (files []Result, failed []failedFile, err error) {
	var stored int64
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, nil, filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return files, failed, nil
		}
		if err != nil {
			return files, failed, bodyError(err)
		}
		if part.FormName() != opts.Field || part.FileName() == "" {
			_ = part.Close()
			continue
		}
		if len(files)+len(failed) == opts.MaxFiles {
			_ = part.Close()
			return files, failed, filesystem.Errorf(filesystem.CodeInvalidArgument, "more than %d files", opts.MaxFiles)
		}
		res, err := receiveWithin(r.Context(), disk, part, p, opts.MaxTotal, stored)
		_ = part.Close()
		var mbe *http.MaxBytesError
		switch {
		case err == nil:
			files = append(files, res)
			stored += res.Size
		case !opts.Partial || errors.As(err, &mbe):
			// The request cap is the request's failure, not the file's.
			return files, failed, bodyError(err)
		default:
			status := statusOf(err)
			failed = append(failed, failedFile{FileFailure{Filename: SafeFilename(part.FileName()), Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)}, status})
		}
	}
}

// receiveWithin is Receive with the file's MaxSize lowered to what is left
// of the request's MaxTotal after used bytes, reporting a file that does
// not fit as over the request total rather than over a per-file limit the
// client never set.
func receiveWithin(ctx context.Context, disk *filesystem.Disk, part *multipart.Part, p Policy, total, used int64) (Result, error) {
	overTotal := filesystem.Errorf(filesystem.CodeTooLarge, "the files of this request exceed %d bytes in total", total)
	bound := false
	if total > 0 && total-used < p.MaxSize {
		p.MaxSize, bound = total-used, true
		if p.MaxSize <= 0 || p.MaxSize < p.MinSize {
			return Result{}, overTotal
		}
	}
	res, err := Receive(ctx, disk, part, clientFilename(part), p)
	// too_large here is the lowered MaxSize: the request cap tripping
	// mid-write surfaces from the Disk as the driver's read error, which
	// receiveForm turns into the request's own too_large.
	if bound && filesystem.CodeOf(err) == filesystem.CodeTooLarge {
		return Result{}, overTotal
	}
	return res, err
}

// bodyError turns the request-size cap into CodeTooLarge. The cause is the
// *http.MaxBytesError alone, not err: when the cap trips inside a write,
// err is the Disk's annotated error, whose text carries the storage key of
// the object being written, and the cause of a CodeTooLarge error is shown
// to the client.
func bodyError(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return filesystem.NewError(filesystem.CodeTooLarge, mbe)
	}
	return err
}

// discard deletes files stored by a request that then failed. Deletion
// errors are ignored: Sweep removes anything left behind.
func discard(ctx context.Context, disk *filesystem.Disk, files []Result) {
	for _, f := range files {
		_ = disk.Delete(context.WithoutCancel(ctx), f.Key)
	}
}

func statusOf(err error) int { return fshttp.StatusOf(err) }

// writeJSON answers the bodies whose shape is a protocol's (Uppy's) or the
// error default's; success bodies go through writeSuccess.
func writeJSON[T ErrorResponse | uppyCreated | uppySigned | []uppyPart | uppyCompleted](w http.ResponseWriter, status int, body T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// JSONErrors is upload's default fshttp.ErrorWriter: an ErrorResponse
// whose text is fshttp.ClientMessage(err), so internals never reach the
// client, and whose code is filesystem.CodeOf(err). Every handler of this
// package answers with it unless its options set Errors.
func JSONErrors(w http.ResponseWriter, _ *http.Request, status int, err error) {
	writeJSON(w, status, ErrorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
}

// writeJSONError answers err with status through ew (JSONErrors when nil),
// after passing on a Retry-After the backend asked for.
func writeJSONError(ew fshttp.ErrorWriter, w http.ResponseWriter, r *http.Request, status int, err error) {
	fshttp.SetRetryAfter(w.Header(), err)
	if ew == nil {
		ew = JSONErrors
	}
	ew(w, r, status, err)
}
