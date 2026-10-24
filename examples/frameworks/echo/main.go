// Command echo mounts every HTTP handler the library ships on an echo
// server: form uploads, direct-upload tickets, Uppy multipart, tus, signed
// links, public files and the records endpoints. The storage wiring is
// shared with the other framework examples (internal/app); this file is
// only the routing.
//
//	go run ./echo     (from examples/frameworks; listens on localhost:8092)
package main

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/examples/frameworks/internal/app"
	"github.com/ubgo/filesystem/fshttp"
)

// Where the server listens and the URL clients reach it at; signed links
// are built from baseURL.
const (
	addr    = "localhost:8092"
	baseURL = "http://" + addr
	// readHeaderTimeout stops a slow client from holding a connection open
	// before it has sent its headers.
	readHeaderTimeout = 5 * time.Second
	// tusTempPattern names the directory unfinished tus uploads wait in.
	tusTempPattern = "tus-sessions-*"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

// run wires the app and serves it until the process is stopped.
func run(ctx context.Context) error {
	s3URL, stopS3, err := app.StartFakeS3()
	if err != nil {
		return err
	}
	defer stopS3()
	tusDir, err := os.MkdirTemp("", tusTempPattern)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tusDir) }()
	a, err := app.New(ctx, baseURL, s3URL, tusDir)
	if err != nil {
		return err
	}
	log.Printf("listening on %s", baseURL)
	e := newRouter(a.Handlers)
	e.GET(app.PathInfo+"/*", fileInfo(a.Disks.Media))
	srv := &http.Server{Addr: addr, Handler: e, ReadHeaderTimeout: readHeaderTimeout}
	return app.ServeUntilDone(ctx, srv)
}

// newRouter mounts the library's handlers on echo. echo.WrapHandler adapts
// an http.Handler; each catch-all route strips its prefix with
// http.StripPrefix, because the handlers route on the path below their
// mount point. Any registers every method, PATCH included for tus.
// ds:def id=newrouter-kbd8hm3u
func newRouter(h app.Handlers) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.POST(app.PathUpload, echo.WrapHandler(h.Upload))
	e.POST(app.PathTicket, echo.WrapHandler(h.Ticket))
	e.Any(app.PathUppy+"/*", echo.WrapHandler(http.StripPrefix(app.PathUppy, h.Uppy)))
	e.Any(app.PathTus+"*", echo.WrapHandler(http.StripPrefix(app.PathTus, h.Tus)))
	e.Any(app.PathFiles+"/*", echo.WrapHandler(http.StripPrefix(app.PathFiles, h.Files)))
	e.Any(app.PathPublic+"/*", echo.WrapHandler(http.StripPrefix(app.PathPublic, h.Public)))
	e.Any(app.PathRecords+"/*", echo.WrapHandler(http.StripPrefix(app.PathRecords, h.Records)))
	return e
}

// fileInfo is a handler of the application's own, calling the library
// directly: the metadata of one object of disk, for a signed-in user.
// It uses the request's context, so a client that goes away cancels the
// backend call, and answers errors with their code's status and
// fshttp.ClientMessage (app.ProblemOf), never err.Error(), which names
// the disk and the key. The key is the rest of the path; the Disk cleans
// it and refuses traversal before any backend sees it.
// ds:def id=fileinfo-6u4e3d28
func fileInfo(disk *filesystem.Disk) echo.HandlerFunc {
	return func(c echo.Context) error {
		if c.Request().Header.Get(app.HeaderUser) == "" {
			return c.JSON(http.StatusUnauthorized, app.Problem{Error: app.ErrSignedOutMessage, Code: filesystem.CodePermission})
		}
		// echo routes on the escaped path (URL.RawPath) only when it holds
		// an escape Go would not write itself, such as %2F; only then is
		// the parameter still percent-encoded. Otherwise it is already
		// decoded, and unescaping it again would turn a literal "%20" in a
		// key into a space.
		key := c.Param("*")
		if c.Request().URL.RawPath != "" {
			var err error
			if key, err = url.PathUnescape(key); err != nil {
				return c.JSON(http.StatusBadRequest, app.Problem{Error: "malformed path", Code: filesystem.CodeInvalidKey})
			}
		}
		entry, err := disk.Stat(c.Request().Context(), key)
		if err != nil {
			return c.JSON(fshttp.StatusOf(err), app.ProblemOf(err))
		}
		return c.JSON(http.StatusOK, app.InfoOf(entry))
	}
}
