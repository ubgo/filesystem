// Command fiber mounts every HTTP handler the library ships on a fiber v3
// app: form uploads, direct-upload tickets, Uppy multipart, tus, signed
// links, public files and the records endpoints. Fiber runs on fasthttp,
// not net/http; fiber's own adaptor middleware bridges the two. The
// storage wiring is shared with the other framework examples
// (internal/app); this file is only the routing.
//
//	go run ./fiber    (from examples/frameworks; listens on localhost:8093)
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

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"

	"github.com/ubgo/filesystem"
	fsfiber "github.com/ubgo/filesystem/contrib/fiber"
	"github.com/ubgo/filesystem/examples/frameworks/internal/app"
	"github.com/ubgo/filesystem/fshttp"
)

// Where the server listens and the URL clients reach it at; signed links
// are built from baseURL.
const (
	// shutdownTimeout bounds how long a stopping server waits for requests in flight.
	shutdownTimeout = 5 * time.Second
	addr            = "localhost:8093"
	baseURL         = "http://" + addr
	// readTimeout bounds how long a client may take to send a request.
	// Uploads are whole requests, so it must cover the largest upload on
	// the slowest link you serve.
	readTimeout = 5 * time.Minute
	// tusTempPattern names the directory unfinished tus uploads wait in.
	tusTempPattern = "tus-sessions-*"
)

// Fiber reads a whole request body into memory before any handler runs,
// and refuses bodies over BodyLimit (4 MiB by default) with 413. The limit
// must therefore admit the largest request the upload routes accept: up to
// maxFormFiles files of app.MaxUploadBytes each, plus multipart overhead.
// ds:def id=maxformfiles-6e3v4vpk
const (
	maxFormFiles   = 5
	formOverhead   = 64 << 10
	fiberBodyLimit = maxFormFiles*app.MaxUploadBytes + formOverhead
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
	r := newRouter(a.Handlers)
	r.Get(app.PathInfo+"/*", fileInfo(a.Disks.Media))
	// Stop on Ctrl+C or SIGTERM, so the deferred clean-up runs.
	go func() { <-ctx.Done(); _ = r.ShutdownWithTimeout(shutdownTimeout) }()
	return r.Listen(addr, fiber.ListenConfig{DisableStartupMessage: true})
}

// newRouter mounts the library's handlers on fiber. adaptor.HTTPHandler
// turns an http.Handler into a fiber handler; each catch-all route strips
// its prefix with http.StripPrefix, because the handlers route on the path
// below their mount point. All registers every method, PATCH included.
// ds:def id=newrouter-bqey7agv
func newRouter(h app.Handlers) *fiber.App {
	f := fiber.New(fiber.Config{BodyLimit: fiberBodyLimit, ReadTimeout: readTimeout})
	f.Post(app.PathUpload, adaptor.HTTPHandler(h.Upload))
	f.Post(app.PathTicket, adaptor.HTTPHandler(h.Ticket))
	f.All(app.PathUppy+"/*", adaptor.HTTPHandler(http.StripPrefix(app.PathUppy, h.Uppy)))
	// fsfiber, not the raw adaptor: tusd keeps a context past the request, and fasthttp's pooled RequestCtx then panics at shutdown.
	f.All(app.PathTus+"*", fsfiber.Handler(http.StripPrefix(app.PathTus, h.Tus)))
	f.All(app.PathFiles+"/*", adaptor.HTTPHandler(http.StripPrefix(app.PathFiles, h.Files)))
	f.All(app.PathPublic+"/*", adaptor.HTTPHandler(http.StripPrefix(app.PathPublic, h.Public)))
	f.All(app.PathRecords+"/*", adaptor.HTTPHandler(http.StripPrefix(app.PathRecords, h.Records)))
	return f
}

// fileInfo is a handler of the application's own, calling the library
// directly: the metadata of one object of disk, for a signed-in user.
// It passes fiber v3's request context, c.Context(), to the library.
// Errors answer
// with their code's status and fshttp.ClientMessage (app.ProblemOf),
// never err.Error(), which names the disk and the key. The Disk cleans
// the key and refuses traversal before any backend sees it.
// ds:def id=fileinfo-tk4wrea2
func fileInfo(disk *filesystem.Disk) fiber.Handler {
	return func(c fiber.Ctx) error {
		if c.Get(app.HeaderUser) == "" {
			return c.Status(http.StatusUnauthorized).JSON(app.Problem{Error: app.ErrSignedOutMessage, Code: filesystem.CodePermission})
		}
		// fiber hands parameters over still percent-encoded (unless
		// Config.UnescapePath is set for the whole app).
		key, err := url.PathUnescape(c.Params("*"))
		if err != nil {
			return c.Status(http.StatusBadRequest).JSON(app.Problem{Error: "malformed path", Code: filesystem.CodeInvalidKey})
		}
		entry, err := disk.Stat(c.Context(), key)
		if err != nil {
			return c.Status(fshttp.StatusOf(err)).JSON(app.ProblemOf(err))
		}
		return c.Status(http.StatusOK).JSON(app.InfoOf(entry))
	}
}
