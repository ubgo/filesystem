// Package fsecho mounts github.com/ubgo/filesystem's HTTP handlers on echo
// (v4).
//
// It is a separate module so an application that does not use echo never
// downloads it. It re-implements nothing: every endpoint stays the
// library's own net/http handler (upload.Handler, upload.Uppy,
// fshttp.Signed, fshttp.Public, recordshttp.Handler, contrib/tus). What it
// adds is the echo side of the seam:
//
//   - Mount serves a handler under an *echo.Echo or *echo.Group, with the
//     group's and the mount's prefix stripped, for every method.
//   - Actor and Scope resolve the signed-in user and the tenant from the
//     echo context once, in middleware, and put them where the library
//     reads them: records.WithActor for the Store, ScopeOf for
//     recordshttp.Config.Scope.
//   - Error and ErrorHandler answer the errors of the application's own
//     echo handlers the way the library's handlers do, with
//     fshttp.ClientMessage, so a driver's message (which names disks and
//     keys) never reaches a client.
//   - Zip streams a zip download through fshttp.ServeZip.
package fsecho

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

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

// ErrNoScope is what ScopeOf returns for a request that did not pass
// through Scope: recordshttp then answers 401, so a route mounted outside
// the middleware refuses rather than running in no tenant.
var ErrNoScope = filesystem.Errorf(filesystem.CodePermission, "fsecho: no scope on the request; mount the route behind fsecho.Scope")

// Router is what Mount registers on: an *echo.Echo or an *echo.Group.
type Router interface {
	Any(path string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) []*echo.Route
}

// Mount serves h for every method at every path below prefix on r, with
// the full matched prefix (the group's included) stripped from the
// request's path: the library's handlers route on the path below their
// mount point (recordshttp's "/owners/{type}/{id}/{role}", fshttp.Signed's
// "/{disk}/{key}"). Every method is registered because tus (PATCH, HEAD,
// DELETE, OPTIONS) and the signed-link handler (GET, HEAD, PUT) need them;
// the handler itself answers 405 for the ones it does not serve.
//
// Keys reach the handler decoded: the prefix is stripped from the
// request's decoded path, never rebuilt from c.Param, which echo leaves
// percent-encoded whenever the request path holds a non-canonical escape
// such as %2F. So a key with a space, a literal %20 or non-ASCII characters
// arrives as stored, and an encoded ".." decodes into the key, which the
// Disk then refuses.
//
// A prefix ending in "/" is stripped with its slash ("/tus/" hands the
// handler "" and "abc"), which is what tus expects; without it the
// handler sees a leading "/" ("/media" hands it "/files/fil_…"), which is
// what every other handler expects. For an exact route (upload.Handler,
// upload.PresignHandler) use r.POST(path, echo.WrapHandler(h)) instead.
func Mount(r Router, prefix string, h http.Handler) {
	keepSlash := strings.HasSuffix(prefix, "/")
	route := prefix
	if !keepSlash {
		route += "/"
	}
	r.Any(route+catchAll, func(c echo.Context) error {
		// c.Path is the registered route with every group's prefix, so
		// the strip is right however deeply the group is nested.
		strip := strings.TrimSuffix(c.Path(), catchAll)
		if !keepSlash {
			strip = strings.TrimSuffix(strip, "/")
		}
		http.StripPrefix(strip, h).ServeHTTP(c.Response(), c.Request())
		return nil
	})
}

// Actor returns middleware that resolves the request's actor with fn (a
// user ID, "api-key:…") and records it with records.WithActor, so every
// change the request makes through a records.Store is attributed to it
// (File.UploadedBy, the audit log). fn returning "" with no error lets an
// anonymous request through without an actor; an error refuses it with
// 401. Put it before the routes that write records.
func Actor(fn func(c echo.Context) (string, error)) echo.MiddlewareFunc {
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
func (x Responder) Actor(fn func(c echo.Context) (string, error)) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			actor, err := fn(c)
			if err != nil {
				return x.unauthenticated(c, err)
			}
			if actor != "" {
				c.SetRequest(c.Request().WithContext(records.WithActor(c.Request().Context(), actor)))
			}
			return next(c)
		}
	}
}

// Scope returns middleware that resolves the tenant the request may act
// in with fn and stores it on the request with recordshttp.WithScope,
// where ScopeOf and recordshttp.ScopeFromRequest find it. The
// scope must come from the authenticated session, never from the URL or
// a header the client chooses. An error or "" refuses the request with
// 401 before any handler runs.
func Scope(fn func(c echo.Context) (string, error)) echo.MiddlewareFunc {
	return Responder{}.Scope(fn)
}

// Scope is the package-level Scope answering its 401 through x.
func (x Responder) Scope(fn func(c echo.Context) (string, error)) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			scope, err := fn(c)
			if err != nil || scope == "" {
				return x.unauthenticated(c, err)
			}
			c.SetRequest(c.Request().WithContext(recordshttp.WithScope(c.Request().Context(), scope)))
			return next(c)
		}
	}
}

// ScopeOf returns the scope Scope stored on r, or ErrNoScope. It has the
// signature of recordshttp.Config.Scope, so the records endpoints read the
// tenant the echo middleware resolved.
func ScopeOf(r *http.Request) (string, error) {
	if scope, ok := recordshttp.ScopeFromContext(r.Context()); ok && scope != "" {
		return scope, nil
	}
	return "", ErrNoScope
}

// Error answers err the way the library's handlers do and returns nil,
// so a handler can end with `return fsecho.Error(c, err)`: the status of
// fshttp.StatusOf (413 for a body over http.MaxBytesReader's cap), a JSON
// upload.ErrorResponse whose text is fshttp.ClientMessage (only the words
// of the client's own input's errors, never a driver's message with its
// disk and key) and whose code is filesystem.CodeOf, and Retry-After when
// the backend asked for a wait.
func Error(c echo.Context, err error) error {
	return Responder{}.Error(c, err)
}

// Error is the package-level Error answering through x.Errors when set.
func (x Responder) Error(c echo.Context, err error) error {
	err = classify(err)
	fshttp.SetRetryAfter(c.Response().Header(), err)
	if x.Errors != nil {
		x.Errors(c.Response(), c.Request(), fshttp.StatusOf(err), err)
		return nil
	}
	return c.JSON(fshttp.StatusOf(err), upload.ErrorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
}

// ErrorHandler returns an echo.HTTPErrorHandler that answers the
// library's errors (any error with a *filesystem.Error in its chain, and
// a tripped http.MaxBytesReader) with Error, and hands every other error
// (an *echo.HTTPError, the application's own) to next, which is usually
// e.DefaultHTTPErrorHandler. Install it with
// e.HTTPErrorHandler = fsecho.ErrorHandler(e.DefaultHTTPErrorHandler), and
// handlers can return the library's errors as they are.
func ErrorHandler(next echo.HTTPErrorHandler) echo.HTTPErrorHandler {
	return Responder{}.ErrorHandler(next)
}

// ErrorHandler is the package-level ErrorHandler answering the library's
// errors through x.Error.
func (x Responder) ErrorHandler(next echo.HTTPErrorHandler) echo.HTTPErrorHandler {
	return func(err error, c echo.Context) {
		if c.Response().Committed || !libraryError(err) {
			next(err, c)
			return
		}
		_ = x.Error(c, err)
	}
}

// libraryError reports whether err is one of the library's errors.
func libraryError(err error) bool {
	var fe *filesystem.Error
	var mbe *http.MaxBytesError
	return errors.As(err, &fe) || errors.As(err, &mbe)
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
func (x Responder) unauthenticated(c echo.Context, cause error) error {
	if x.Errors != nil {
		x.Errors(c.Response(), c.Request(), http.StatusUnauthorized, refusal(cause))
		return nil
	}
	return c.JSON(http.StatusUnauthorized, upload.ErrorResponse{Error: MsgUnauthenticated, Code: filesystem.CodePermission})
}

// Zip streams the objects of d named by files to the client as one zip
// archive called filename, through fshttp.ServeZip, and returns nil:
// every name and the total size are checked before the first byte, and a
// failure after it aborts the response with http.ErrAbortHandler, which
// echo's Recover middleware passes on to net/http so the client sees a
// failed download rather than a truncated archive. Authorise the request
// and choose the keys before calling it.
func Zip(c echo.Context, d *filesystem.Disk, filename string, files []fshttp.ZipFile, opts fshttp.ZipOptions) error {
	fshttp.ServeZip(c.Response(), c.Request(), d, filename, files, opts)
	return nil
}
