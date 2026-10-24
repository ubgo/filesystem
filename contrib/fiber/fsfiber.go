// Package fsfiber mounts github.com/ubgo/filesystem's HTTP handlers on
// fiber (v3).
//
// It is a separate module so an application that does not use fiber never
// downloads it. It re-implements nothing: every endpoint stays the
// library's own net/http handler (upload.Handler, upload.Uppy,
// fshttp.Signed, fshttp.Public, recordshttp.Handler, contrib/tus), run
// through fiber's net/http adaptor. What it adds is the fiber side of the
// seam, which matters more on fiber than anywhere else, because fiber is
// not net/http: a value a fiber middleware stores never reaches an adapted
// handler's *http.Request on its own.
//
//   - Handler adapts a library handler and carries the actor and scope the
//     middleware resolved into its request's context.
//   - Mount serves a handler under an *fiber.App or a fiber.Router group,
//     with the full matched prefix stripped, for every method.
//   - Actor and Scope resolve the signed-in user and the tenant once, in
//     middleware, for both worlds: fiber handlers read them from
//     c.Context() (records.WithActor) and ScopeFrom, adapted handlers from
//     r.Context() and ScopeOf (recordshttp.Config.Scope).
//   - Error and ErrorHandler answer the errors of the application's own
//     fiber handlers the way the library's handlers do, with
//     fshttp.ClientMessage, so a driver's message (which names disks and
//     keys) never reaches a client.
//   - Zip streams a zip download through fshttp.ServeZip.
package fsfiber

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordshttp"
	"github.com/ubgo/filesystem/upload"
)

// catchAll is the wildcard Mount registers below a prefix.
const catchAll = "*"

// MsgUnauthenticated is the error text of the 401 that Actor and Scope
// answer, the same words recordshttp uses for a request without a scope.
const MsgUnauthenticated = "not signed in"

// ErrNoScope is what ScopeOf and ScopeFrom return for a request that did
// not pass through Scope: recordshttp then answers 401, so a route mounted
// outside the middleware refuses rather than running in no tenant.
var ErrNoScope = filesystem.Errorf(filesystem.CodePermission, "fsfiber: no scope on the request; mount the route behind fsfiber.Scope")

// Router is what Mount registers on: an *fiber.App or a fiber.Router
// (a group).
type Router interface {
	All(path string, handler any, handlers ...any) fiber.Router
}

// Handler adapts h, a library handler, to fiber, with the actor and scope
// that Actor and Scope resolved in its request's context
// (records.ActorFrom and recordshttp.ScopeFromRequest find them). Use it
// instead of adaptor.HTTPHandler for every library handler: through the
// plain adaptor, recordshttp would record no uploader, and the handler's
// context would be fasthttp's pooled RequestCtx (see detach). fiber reads
// the whole request body before any handler runs, so its BodyLimit (4 MiB
// by default) must allow the largest upload plus form overhead.
// ds:def id=handler-huvrp485
func Handler(h http.Handler) fiber.Handler {
	return func(c fiber.Ctx) error {
		return adaptor.HTTPHandler(detach(c.Context(), h))(c)
	}
}

// detach runs h with a context of its own instead of the one fiber's
// adaptor gives it, which is fasthttp's RequestCtx: pooled and reused for
// later requests, and incoherent after Shutdown (its done channel is
// closed and then set to nil, so Err reads nil after Done was closed). A
// handler that keeps a derived context past the request, as tusd does,
// would then panic the process in context.propagateCancel ("missing
// cancel error"). The new context descends from base, the fiber
// handler's context (carrying the actor and scope), is cancelled when the
// server shuts down while h runs, and is always cancelled once h returns.
func detach(base context.Context, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pooled := r.Context()
		ctx, cancel := context.WithCancel(base)
		defer cancel()
		if shutdown := pooled.Done(); shutdown != nil {
			go func() {
				select {
				case <-shutdown:
					cancel()
				case <-ctx.Done():
				}
			}()
		}
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Mount serves h for every method at every path below prefix on r, with
// the full matched prefix (every group's included) stripped from the
// request's path: the library's handlers route on the path below their
// mount point (recordshttp's "/owners/{type}/{id}/{role}", fshttp.Signed's
// "/{disk}/{key}"). The handler itself answers 405 for the methods it does
// not serve.
//
// Keys reach the handler decoded: the prefix is stripped from the
// request's decoded path, never rebuilt from c.Params, which fiber leaves
// percent-encoded unless the app sets Config.UnescapePath. So a key with a
// space, a literal %20 or non-ASCII characters arrives as stored, and an
// encoded ".." decodes into the key, which the Disk then refuses.
//
// A prefix ending in "/" is stripped with its slash ("/tus/" hands the
// handler "" and "abc"), which is what tus expects; without it the
// handler sees a leading "/" ("/media" hands it "/files/fil_…"), which is
// what every other handler expects. For an exact route (upload.Handler,
// upload.PresignHandler) use r.Post(path, fsfiber.Handler(h)) instead.
func Mount(r Router, prefix string, h http.Handler) {
	keepSlash := strings.HasSuffix(prefix, "/")
	route := prefix
	if !keepSlash {
		route += "/"
	}
	r.All(route+catchAll, func(c fiber.Ctx) error {
		// The route's path is the registered pattern with every group's
		// prefix, so the strip is right however deeply it is nested.
		strip := strings.TrimSuffix(c.Route().Path, catchAll)
		if !keepSlash {
			strip = strings.TrimSuffix(strip, "/")
		}
		return adaptor.HTTPHandler(detach(c.Context(), http.StripPrefix(strip, h)))(c)
	})
}

// Actor returns middleware that resolves the request's actor with fn (a
// user ID, "api-key:…") and records it, so every change the request makes
// through a records.Store is attributed to it (File.UploadedBy, the audit
// log): in c.Context() with records.WithActor for the application's own
// fiber handlers, and for the library's handlers adapted with Handler or
// Mount. fn returning "" with no error lets an anonymous request through
// without an actor; an error refuses it with 401.
func Actor(fn func(c fiber.Ctx) (string, error)) fiber.Handler {
	return Responder{}.Actor(fn)
}

// Responder answers the errors of this package's middleware and of Error
// in an application's own envelope. Its zero value is the package's
// default, the JSON upload.ErrorResponse, and the package-level Actor,
// Scope, Error and ErrorHandler are the zero Responder's.
//
// Why: an API bound to problem+json or its own {error: {...}} shape
// should not have to rewrite the 401 of Actor and Scope or the answer of
// Error by hand; the same fshttp.ErrorWriter it gives fshttp, upload and
// recordshttp renders these too.
type Responder struct {
	// Errors renders every error: the 401 of Actor and Scope (with
	// ErrUnauthenticated, or the resolver's own error wrapped as
	// CodePermission) and the errors given to Error (with the status of
	// fshttp.StatusOf). A Retry-After is set before it is called. nil
	// keeps the JSON upload.ErrorResponse.
	Errors fshttp.ErrorWriter
}

// ErrUnauthenticated is the error a Responder's Errors receives for a
// request Actor or Scope refuse without an error of the resolver's own
// (Scope's resolver returning "").
var ErrUnauthenticated = filesystem.Errorf(filesystem.CodePermission, MsgUnauthenticated)

// refusal is the error an ErrorWriter gets for a 401: the resolver's own
// error as CodePermission, kept for logs, else ErrUnauthenticated.
func refusal(cause error) error {
	if cause == nil {
		return ErrUnauthenticated
	}
	return filesystem.NewError(filesystem.CodePermission, cause)
}

// Actor is the package-level Actor answering its 401 through x.
func (x Responder) Actor(fn func(c fiber.Ctx) (string, error)) fiber.Handler {
	return func(c fiber.Ctx) error {
		actor, err := fn(c)
		if err != nil {
			return x.unauthenticated(c, err)
		}
		if actor != "" {
			c.SetContext(records.WithActor(c.Context(), actor))
		}
		return c.Next()
	}
}

// Scope returns middleware that resolves the tenant the request may act
// in with fn and stores it with recordshttp.WithScope, where ScopeFrom
// (fiber handlers), ScopeOf and recordshttp.ScopeFromRequest (adapted
// handlers, recordshttp.Config.Scope) find it. The scope must
// come from the authenticated session, never from the URL or a header
// the client chooses. An error or "" refuses the request with 401 before
// any handler runs.
func Scope(fn func(c fiber.Ctx) (string, error)) fiber.Handler {
	return Responder{}.Scope(fn)
}

// Scope is the package-level Scope answering its 401 through x.
func (x Responder) Scope(fn func(c fiber.Ctx) (string, error)) fiber.Handler {
	return func(c fiber.Ctx) error {
		scope, err := fn(c)
		if err != nil || scope == "" {
			return x.unauthenticated(c, err)
		}
		c.SetContext(recordshttp.WithScope(c.Context(), scope))
		return c.Next()
	}
}

// ScopeOf returns the scope Scope stored for r, a request built by
// fiber's adaptor (Handler, Mount), or ErrNoScope. It has the signature of
// recordshttp.Config.Scope.
func ScopeOf(r *http.Request) (string, error) {
	return scopeIn(r.Context())
}

// ScopeFrom returns the scope Scope stored for c, or ErrNoScope, for the
// application's own fiber handlers.
func ScopeFrom(c fiber.Ctx) (string, error) {
	return scopeIn(c.Context())
}

// scopeIn reads the scope recordshttp.WithScope set on ctx.
func scopeIn(ctx context.Context) (string, error) {
	if scope, ok := recordshttp.ScopeFromContext(ctx); ok && scope != "" {
		return scope, nil
	}
	return "", ErrNoScope
}

// Error answers err the way the library's handlers do and returns nil, so
// a handler can end with `return fsfiber.Error(c, err)`: the status of
// fshttp.StatusOf (413 for a body over http.MaxBytesReader's cap), a JSON
// upload.ErrorResponse whose text is fshttp.ClientMessage (only the words
// of the client's own input's errors, never a driver's message with its
// disk and key) and whose code is filesystem.CodeOf, and Retry-After when
// the backend asked for a wait.
func Error(c fiber.Ctx, err error) error {
	return Responder{}.Error(c, err)
}

// Error is the package-level Error answering through x.Errors when set:
// fiber is not net/http, so the writer runs through fiber's adaptor, on
// the request converted to an *http.Request.
func (x Responder) Error(c fiber.Ctx, err error) error {
	err = classify(err)
	if x.Errors != nil {
		return x.write(c, fshttp.StatusOf(err), err)
	}
	// fiber's headers are not an http.Header; let fshttp format the value.
	h := http.Header{}
	fshttp.SetRetryAfter(h, err)
	if v := h.Get(fshttp.HeaderRetryAfter); v != "" {
		c.Set(fshttp.HeaderRetryAfter, v)
	}
	return c.Status(fshttp.StatusOf(err)).JSON(upload.ErrorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
}

// ErrorHandler returns a fiber.ErrorHandler that answers the library's
// errors (any error with a *filesystem.Error in its chain, and a tripped
// http.MaxBytesReader) with Error, and hands every other error (a
// *fiber.Error, the application's own) to next, usually
// fiber.DefaultErrorHandler. Install it as fiber.Config.ErrorHandler and
// handlers can return the library's errors as they are.
func ErrorHandler(next fiber.ErrorHandler) fiber.ErrorHandler {
	return Responder{}.ErrorHandler(next)
}

// ErrorHandler is the package-level ErrorHandler answering the library's
// errors through x.Error.
func (x Responder) ErrorHandler(next fiber.ErrorHandler) fiber.ErrorHandler {
	return func(c fiber.Ctx, err error) error {
		var fe *filesystem.Error
		var mbe *http.MaxBytesError
		if errors.As(err, &fe) || errors.As(err, &mbe) {
			return x.Error(c, err)
		}
		return next(c, err)
	}
}

// classify turns a tripped request-body cap into CodeTooLarge, wrapping
// the cap itself rather than err, whose text may name the disk and key.
func classify(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return filesystem.NewError(filesystem.CodeTooLarge, mbe)
	}
	return err
}

// unauthenticated answers the library's 401.
func (x Responder) unauthenticated(c fiber.Ctx, cause error) error {
	if x.Errors != nil {
		return x.write(c, http.StatusUnauthorized, refusal(cause))
	}
	return c.Status(http.StatusUnauthorized).JSON(upload.ErrorResponse{Error: MsgUnauthenticated, Code: filesystem.CodePermission})
}

// write runs x.Errors on c through fiber's adaptor, with the Retry-After
// the backend asked for already set.
func (x Responder) write(c fiber.Ctx, status int, err error) error {
	return adaptor.HTTPHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fshttp.SetRetryAfter(w.Header(), err)
		x.Errors(w, r, status, err)
	})(c)
}

// Zip streams the objects of d named by files to the client as one zip
// archive called filename, through fshttp.ServeZip: every name and the
// total size are checked before the first byte, and the archive is
// streamed, never held in memory. Authorise the request and choose the
// keys before calling it.
//
// It does not go through fiber's net/http adaptor, which either buffers
// the whole response or, once flushed, ends the stream cleanly when the
// handler fails, so a client would keep a truncated archive as if it were
// complete. Zip feeds fasthttp a body stream instead, and a failure after
// the first byte (ServeZip's http.ErrAbortHandler) fails that stream, so
// fasthttp closes the connection before the end of the body. A panic
// other than the abort is reported to opts.OnError, when set, and fails
// the download the same way.
func Zip(c fiber.Ctx, d *filesystem.Disk, filename string, files []fshttp.ZipFile, opts fshttp.ZipOptions) error {
	r, err := adaptor.ConvertRequest(c, true)
	if err != nil {
		// The request line itself is malformed (a bad escape): the client's.
		return Error(c, filesystem.NewError(filesystem.CodeInvalidArgument, err))
	}
	// The archive streams after this handler returns, so its context is
	// the fiber handler's (never the pooled RequestCtx), cancelled when
	// the stream ends.
	ctx, cancel := context.WithCancel(c.Context())
	r = r.WithContext(ctx)
	w := newStreamWriter()
	go func() {
		defer cancel()
		defer w.finish(opts.OnError)
		fshttp.ServeZip(w, r, d, filename, files, opts)
	}()
	streaming := <-w.started
	c.Status(w.status)
	for k, vv := range w.sent {
		for _, v := range vv {
			c.Response().Header.Add(k, v)
		}
	}
	if streaming {
		// ServeZip sends no Content-Length (the archive's size is known
		// only as it streams), so the body is chunked. Without a first
		// body byte (a HEAD), the headers are the whole answer.
		c.Response().SetBodyStream(w.pr, -1)
	}
	return nil
}

// errZipAborted fails the body stream of a zip whose source failed after
// the response started.
var errZipAborted = errors.New("fsfiber: zip download aborted")

// streamWriter is the http.ResponseWriter ServeZip writes to: the headers
// are captured at WriteHeader, and the body goes through a pipe that
// fasthttp reads as the response's body stream.
type streamWriter struct {
	header  http.Header
	sent    http.Header // header as it was at WriteHeader
	status  int
	pr      *io.PipeReader
	pw      *io.PipeWriter
	once    sync.Once
	started chan bool // true at the first body byte, false when done without one
}

func newStreamWriter() *streamWriter {
	pr, pw := io.Pipe()
	return &streamWriter{header: http.Header{}, status: http.StatusOK, pr: pr, pw: pw, started: make(chan bool, 1)}
}

func (w *streamWriter) Header() http.Header { return w.header }

func (w *streamWriter) WriteHeader(status int) {
	if w.sent == nil {
		w.status = status
		w.sent = w.header.Clone()
	}
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	w.once.Do(func() { w.started <- true })
	return w.pw.Write(p)
}

// finish ends the body: cleanly when ServeZip returned, with an error
// (no end of body, the connection closed) when it panicked.
func (w *streamWriter) finish(onError func(error)) {
	rec := recover()
	w.WriteHeader(http.StatusOK)
	if rec == nil {
		w.once.Do(func() { w.started <- false })
		_ = w.pw.Close()
		return
	}
	// A failure before the first body byte must fail the response too,
	// never send its headers with an empty, clean body.
	w.once.Do(func() { w.started <- true })
	if rec != http.ErrAbortHandler && onError != nil { // net/http compares the panic value by identity too
		onError(fmt.Errorf("fsfiber: zip handler panicked: %v", rec))
	}
	_ = w.pw.CloseWithError(errZipAborted)
}
