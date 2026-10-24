// Command gin serves the library on gin through the fsgin adapter: form
// uploads, signed downloads, the records endpoints behind fsgin.Actor and
// fsgin.Scope in a route group, and an export route of the app's own that
// zips the signed-in user's files with fsgin.Zip and answers failures with
// fsgin.Error.
//
//	go run ./adapters/gin    (from examples/frameworks; listens on localhost:8191)
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"

	fsgin "github.com/ubgo/filesystem/contrib/gin"
	app "github.com/ubgo/filesystem/examples/frameworks/internal/adapterapp"
	"github.com/ubgo/filesystem/fshttp"
)

// Where the server listens and the URL clients reach it at; signed links
// are built from baseURL.
const (
	addr    = "localhost:8191"
	baseURL = "http://" + addr
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a, err := app.New(ctx, baseURL, fsgin.ScopeOf)
	if err != nil {
		log.Fatal(err)
	}
	gin.SetMode(gin.ReleaseMode)
	log.Printf("listening on %s", baseURL)
	srv := &http.Server{Addr: addr, Handler: newRouter(a), ReadHeaderTimeout: app.ReadHeaderTimeout}
	if err := app.ServeUntilDone(ctx, srv); err != nil {
		log.Fatal(err)
	}
}

// newRouter mounts the library on gin. The signed-in group resolves the
// user and the tenant once: fsgin.Actor records the user for the records
// store, fsgin.Scope the tenant that recordshttp reads through
// fsgin.ScopeOf (given to app.New).
// ds:def id=newrouter-d2ksmg3g
func newRouter(a *app.App) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.POST(app.PathUpload, gin.WrapH(a.Upload))
	fsgin.Mount(r, app.PathFiles, a.Files)
	me := r.Group(app.GroupMe,
		fsgin.Actor(func(c *gin.Context) (string, error) { return app.User(c.Request) }),
		fsgin.Scope(func(c *gin.Context) (string, error) { return app.Tenant(c.Request) }),
	)
	fsgin.Mount(me, app.MountMedia, a.Records)
	me.GET(app.PathExport+"/:"+app.ParamRole, func(c *gin.Context) { export(c, a) })
	return r
}

// export zips the signed-in user's files of a role.
// ds:def id=export-d258zs7b
func export(c *gin.Context, a *app.App) {
	scope, err := fsgin.ScopeOf(c.Request)
	if err != nil {
		fsgin.Error(c, err)
		return
	}
	role := c.Param(app.ParamRole)
	files, err := a.Export(c.Request.Context(), scope, role)
	if err != nil {
		fsgin.Error(c, err)
		return
	}
	fsgin.Zip(c, a.Media, app.ExportName(role), files, fshttp.ZipOptions{})
}
