// Package fsgin mounts github.com/ubgo/filesystem's HTTP handlers on gin.
//
// It is a separate module so an application that does not use gin never
// downloads it. It re-implements nothing: every endpoint stays the
// library's own net/http handler (upload.Handler, upload.Uppy,
// fshttp.Signed, fshttp.Public, recordshttp.Handler, contrib/tus). What it
// adds is the gin side of the seam:
//
//   - Mount serves a handler under a route group, with the group's and
//     the mount's prefix stripped, for every method.
//   - Actor and Scope resolve the signed-in user and the tenant from the
//     gin context once, in middleware, and put them where the library
//     reads them: records.WithActor for the Store, ScopeOf for
//     recordshttp.Config.Scope.
//   - Error answers an error from the application's own gin handlers the
//     way the library's handlers do, with fshttp.ClientMessage, so a
//     driver's message (which names disks and keys) never reaches a
//     client.
//   - Zip streams a zip download through fshttp.ServeZip.
package fsgin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordshttp"
	"github.com/ubgo/filesystem/upload"
)

// catchAll is the wildcard Mount registers below a prefix; gin hands the
// rest of the path to it, leading slash included.
const catchAll = "*path"

// MsgUnauthenticated is the error text of the 401 that Actor, Scope and
// ScopeOf answer, the same words recordshttp uses for a request without a
// scope.
const MsgUnauthenticated = "not signed in"

// ErrNoScope is what ScopeOf returns for a request that did not pass
// through Scope (or whose resolver returned ""): recordshttp then answers
// 401, so a route mounted outside the middleware refuses rather than
// running in no tenant.
var ErrNoScope = filesystem.Errorf(filesystem.CodePermission, "fsgin: no scope on the request; mount the route behind fsgin.Scope")

// Router is what Mount registers on: a *gin.Engine or a *gin.RouterGroup.
// BasePath is needed to strip the group's own prefix before the library's
// handler routes on the path below its mount point.
type Router interface {
	gin.IRoutes
	BasePath() string
}

// Mount serves h for every method at every path below prefix on r, with
// r's base path and prefix stripped from the request's path: the
// library's handlers route on the path below their mount point
// (recordshttp's "/owners/{type}/{id}/{role}", fshttp.Signed's
// "/{disk}/{key}"). Every method is registered because tus (PATCH, HEAD,
// DELETE, OPTIONS) and the signed-link handler (GET, HEAD, PUT) need them;
// the handler itself answers 405 for the ones it does not serve.
//
// A prefix ending in "/" is stripped with its slash ("/tus/" hands the
// handler "" and "abc"), which is what tus expects; without it the
// handler sees a leading "/" ("/media" hands it "/files/fil_…"), which is
// what every other handler expects. For an exact route (upload.Handler,
// upload.PresignHandler) use r.POST(path, gin.WrapH(h)) instead.
// ds:def id=mount-abds8w87
func Mount(r Router, prefix string, h http.Handler) {
	route := prefix
	if !strings.HasSuffix(route, "/") {
		route += "/"
	}
	strip := joinPath(r.BasePath(), prefix)
	r.Any(route+catchAll, gin.WrapH(http.StripPrefix(strip, h)))
}

// joinPath joins a group's base path and a prefix as gin does for the
// route, keeping a trailing slash of prefix.
func joinPath(base, prefix string) string {
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(prefix, "/")
}

// Actor returns middleware that resolves the request's actor with fn (a
// user ID, "api-key:…") and records it with records.WithActor, so every
// change the request makes through a records.Store is attributed to it
// (File.UploadedBy, the audit log). fn returning "" with no error lets an
// anonymous request through without an actor; an error refuses it with
// 401. Put it before the routes that write records.
func Actor(fn func(c *gin.Context) (string, error)) gin.HandlerFunc {
	return Responder{}.Actor(fn)
}

// Responder answers the errors of this package's middleware and of Error
// in an application's own envelope. Its zero value is the package's
// default, the JSON upload.ErrorResponse, and the package-level Actor,
// Scope, Error are the zero Responder's.
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
func (x Responder) Actor(fn func(c *gin.Context) (string, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor, err := fn(c)
		if err != nil {
			x.unauthenticated(c, err)
			return
		}
		if actor != "" {
			c.Request = c.Request.WithContext(records.WithActor(c.Request.Context(), actor))
		}
		c.Next()
	}
}

// Scope returns middleware that resolves the tenant the request may act
// in with fn and stores it on the request with recordshttp.WithScope,
// where ScopeOf and recordshttp.ScopeFromRequest find it. The
// scope must come from the authenticated session, never from the URL or
// a header the client chooses. An error or "" refuses the request with
// 401 before any handler runs.
func Scope(fn func(c *gin.Context) (string, error)) gin.HandlerFunc {
	return Responder{}.Scope(fn)
}

// Scope is the package-level Scope answering its 401 through x.
func (x Responder) Scope(fn func(c *gin.Context) (string, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, err := fn(c)
		if err != nil || scope == "" {
			x.unauthenticated(c, err)
			return
		}
		c.Request = c.Request.WithContext(recordshttp.WithScope(c.Request.Context(), scope))
		c.Next()
	}
}

// ScopeOf returns the scope Scope stored on r, or ErrNoScope. It has the
// signature of recordshttp.Config.Scope, so the records endpoints read the
// tenant the gin middleware resolved.
func ScopeOf(r *http.Request) (string, error) {
	if scope, ok := recordshttp.ScopeFromContext(r.Context()); ok && scope != "" {
		return scope, nil
	}
	return "", ErrNoScope
}

// Error answers err the way the library's handlers do and aborts the
// chain: the status of fshttp.StatusOf (413 for a body over
// http.MaxBytesReader's cap), a JSON upload.ErrorResponse whose text is
// fshttp.ClientMessage (only the words of the client's own input's
// errors, never a driver's message with its disk and key) and whose code
// is filesystem.CodeOf, and Retry-After when the backend asked for a wait.
func Error(c *gin.Context, err error) {
	Responder{}.Error(c, err)
}

// Error is the package-level Error answering through x.Errors when set.
func (x Responder) Error(c *gin.Context, err error) {
	err = classify(err)
	fshttp.SetRetryAfter(c.Writer.Header(), err)
	if x.Errors != nil {
		x.Errors(c.Writer, c.Request, fshttp.StatusOf(err), err)
		c.Abort()
		return
	}
	c.AbortWithStatusJSON(fshttp.StatusOf(err), upload.ErrorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
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

// unauthenticated aborts with the library's 401.
func (x Responder) unauthenticated(c *gin.Context, cause error) {
	if x.Errors != nil {
		x.Errors(c.Writer, c.Request, http.StatusUnauthorized, refusal(cause))
		c.Abort()
		return
	}
	c.AbortWithStatusJSON(http.StatusUnauthorized, upload.ErrorResponse{Error: MsgUnauthenticated, Code: filesystem.CodePermission})
}

// Zip streams the objects of d named by files to the client as one zip
// archive called filename, through fshttp.ServeZip: every name and the
// total size are checked before the first byte, and a failure after it
// resets the connection rather than ending a truncated archive cleanly.
// Authorise the request and choose the keys before calling it.
//
// ServeZip aborts with http.ErrAbortHandler, and gin.Recovery swallows
// that panic, after which net/http would end the response cleanly and the
// client would keep a truncated archive as if complete. So Zip catches the
// abort itself and closes the connection (HTTP/1.x, through Hijack). Where
// the connection cannot be hijacked (HTTP/2) it re-panics; serve zip routes
// over HTTP/2 only without gin.Recovery in front of them.
func Zip(c *gin.Context, d *filesystem.Disk, filename string, files []fshttp.ZipFile, opts fshttp.ZipOptions) {
	defer func() {
		if rec := recover(); rec != nil {
			if rec != http.ErrAbortHandler { // net/http compares the panic value by identity too
				panic(rec)
			}
			abort(c)
		}
	}()
	fshttp.ServeZip(c.Writer, c.Request, d, filename, files, opts)
}

// abort closes the client's connection mid-response, so it sees a failed
// download, and stops the gin chain.
func abort(c *gin.Context) {
	c.Abort()
	// gin's writer refuses Hijack once body bytes are written (it guards
	// websocket upgrades), so hijack the server's writer beneath it.
	var w http.ResponseWriter = c.Writer
	if u, ok := w.(interface{ Unwrap() http.ResponseWriter }); ok {
		w = u.Unwrap()
	}
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	_ = conn.Close()
}
