// Command chi mounts every HTTP handler the library ships on a chi router:
// form uploads, direct-upload tickets, Uppy multipart, tus, signed links,
// public files and the records endpoints. The storage wiring is shared with
// the other framework examples (internal/app); this file is only the
// routing.
//
//	go run ./chi      (from examples/frameworks; listens on localhost:8094)
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ubgo/filesystem/examples/frameworks/internal/app"
)

// Where the server listens and the URL clients reach it at; signed links
// are built from baseURL.
const (
	addr    = "localhost:8094"
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
	srv := &http.Server{Addr: addr, Handler: newRouter(a.Handlers), ReadHeaderTimeout: readHeaderTimeout}
	return app.ServeUntilDone(ctx, srv)
}

// newRouter mounts the library's handlers on chi. chi takes http.Handler
// values as they are. chi's Mount does not rewrite r.URL.Path for a handler
// that is not a chi router, so each catch-all route strips its prefix with
// http.StripPrefix, exactly as on http.ServeMux.
// ds:def id=newrouter-2z3crjca
func newRouter(h app.Handlers) chi.Router {
	r := chi.NewRouter()
	r.Method(http.MethodPost, app.PathUpload, h.Upload)
	r.Method(http.MethodPost, app.PathTicket, h.Ticket)
	r.Handle(app.PathUppy+"/*", http.StripPrefix(app.PathUppy, h.Uppy))
	r.Handle(app.PathTus+"*", http.StripPrefix(app.PathTus, h.Tus))
	r.Handle(app.PathFiles+"/*", http.StripPrefix(app.PathFiles, h.Files))
	r.Handle(app.PathPublic+"/*", http.StripPrefix(app.PathPublic, h.Public))
	r.Handle(app.PathRecords+"/*", http.StripPrefix(app.PathRecords, h.Records))
	return r
}
