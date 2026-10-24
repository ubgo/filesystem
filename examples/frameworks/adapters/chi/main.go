// Command chi serves the library on chi through the fschi adapter: form
// uploads, signed downloads, the records endpoints behind fschi.Actor and
// fschi.Scope in a sub-router, and an export route of the app's own that
// zips the signed-in user's files with fschi.Zip and answers failures
// with fschi.Error.
//
//	go run ./adapters/chi    (from examples/frameworks; listens on localhost:8194)
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	fschi "github.com/ubgo/filesystem/contrib/chi"
	app "github.com/ubgo/filesystem/examples/frameworks/internal/adapterapp"
	"github.com/ubgo/filesystem/fshttp"
)

// Where the server listens and the URL clients reach it at; signed links
// are built from baseURL.
const (
	addr    = "localhost:8194"
	baseURL = "http://" + addr
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a, err := app.New(ctx, baseURL, fschi.ScopeOf)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening on %s", baseURL)
	srv := &http.Server{Addr: addr, Handler: newRouter(a), ReadHeaderTimeout: app.ReadHeaderTimeout}
	if err := app.ServeUntilDone(ctx, srv); err != nil {
		log.Fatal(err)
	}
}

// newRouter mounts the library on chi. The /me sub-router resolves the
// user and the tenant once with fschi.Actor and fschi.Scope, which are
// plain net/http middleware.
// ds:def id=newrouter-h2hv55wp
func newRouter(a *app.App) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Method(http.MethodPost, app.PathUpload, a.Upload)
	fschi.Mount(r, app.PathFiles, a.Files)
	r.Route(app.GroupMe, func(me chi.Router) {
		me.Use(fschi.Actor(app.User), fschi.Scope(app.Tenant))
		fschi.Mount(me, app.MountMedia, a.Records)
		me.Get(app.PathExport+"/{"+app.ParamRole+"}", func(w http.ResponseWriter, r *http.Request) { export(w, r, a) })
	})
	return r
}

// export zips the signed-in user's files of a role.
func export(w http.ResponseWriter, r *http.Request, a *app.App) {
	scope, err := fschi.ScopeOf(r)
	if err != nil {
		fschi.Error(w, err)
		return
	}
	role := chi.URLParam(r, app.ParamRole)
	files, err := a.Export(r.Context(), scope, role)
	if err != nil {
		fschi.Error(w, err)
		return
	}
	fschi.Zip(w, r, a.Media, app.ExportName(role), files, fshttp.ZipOptions{})
}
