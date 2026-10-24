// Command gin mounts every HTTP handler the library ships on a gin engine:
// form uploads, direct-upload tickets, Uppy multipart, tus, signed links,
// public files and the records endpoints. The storage wiring is shared
// with the other framework examples (internal/app); this file is only the
// routing.
//
//	go run ./gin      (from examples/frameworks; listens on localhost:8091)
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/examples/frameworks/internal/app"
	"github.com/ubgo/filesystem/fshttp"
)

// Where the server listens and the URL clients reach it at; signed links
// are built from baseURL.
const (
	addr    = "localhost:8091"
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
	r := newRouter(a.Handlers)
	r.GET(app.PathInfo+"/*key", fileInfo(a.Disks.Media))
	srv := &http.Server{Addr: addr, Handler: r, ReadHeaderTimeout: readHeaderTimeout}
	return app.ServeUntilDone(ctx, srv)
}

// newRouter mounts the library's handlers on gin. gin.WrapH adapts an
// http.Handler; the handlers route on the path below their mount point, so
// each catch-all route strips its prefix with http.StripPrefix. Any
// registers every method, which tus (PATCH, HEAD, DELETE, OPTIONS) and the
// signed-link handler (GET, HEAD, PUT) need.
// ds:def id=newrouter-h32462p2
func newRouter(h app.Handlers) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.POST(app.PathUpload, gin.WrapH(h.Upload))
	r.POST(app.PathTicket, gin.WrapH(h.Ticket))
	r.Any(app.PathUppy+"/*path", gin.WrapH(http.StripPrefix(app.PathUppy, h.Uppy)))
	r.Any(app.PathTus+"*path", gin.WrapH(http.StripPrefix(app.PathTus, h.Tus)))
	r.Any(app.PathFiles+"/*path", gin.WrapH(http.StripPrefix(app.PathFiles, h.Files)))
	r.Any(app.PathPublic+"/*path", gin.WrapH(http.StripPrefix(app.PathPublic, h.Public)))
	r.Any(app.PathRecords+"/*path", gin.WrapH(http.StripPrefix(app.PathRecords, h.Records)))
	return r
}

// fileInfo is a handler of the application's own, calling the library
// directly: the metadata of one object of disk, for a signed-in user.
// It uses the request's context, so a client that goes away cancels the
// backend call, and answers errors with their code's status and
// fshttp.ClientMessage (app.ProblemOf), never err.Error(), which names
// the disk and the key. gin's catch-all parameter is already unescaped
// and keeps its leading slash, which the Disk's key cleaning drops; the
// Disk also refuses traversal before any backend sees the key.
// ds:def id=fileinfo-zpspbcnp
func fileInfo(disk *filesystem.Disk) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader(app.HeaderUser) == "" {
			c.JSON(http.StatusUnauthorized, app.Problem{Error: app.ErrSignedOutMessage, Code: filesystem.CodePermission})
			return
		}
		entry, err := disk.Stat(c.Request.Context(), c.Param("key"))
		if err != nil {
			c.JSON(fshttp.StatusOf(err), app.ProblemOf(err))
			return
		}
		c.JSON(http.StatusOK, app.InfoOf(entry))
	}
}
