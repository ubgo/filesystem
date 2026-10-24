// Command echo serves the library on echo through the fsecho adapter: form
// uploads, signed downloads, the records endpoints behind fsecho.Actor and
// fsecho.Scope in a group, and an export route of the app's own that zips
// the signed-in user's files with fsecho.Zip. Handlers return the
// library's errors as they are; fsecho.ErrorHandler renders them.
//
//	go run ./adapters/echo    (from examples/frameworks; listens on localhost:8192)
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	fsecho "github.com/ubgo/filesystem/contrib/echo"
	app "github.com/ubgo/filesystem/examples/frameworks/internal/adapterapp"
	"github.com/ubgo/filesystem/fshttp"
)

// Where the server listens and the URL clients reach it at; signed links
// are built from baseURL.
const (
	addr    = "localhost:8192"
	baseURL = "http://" + addr
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a, err := app.New(ctx, baseURL, fsecho.ScopeOf)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening on %s", baseURL)
	srv := &http.Server{Addr: addr, Handler: newRouter(a), ReadHeaderTimeout: app.ReadHeaderTimeout}
	if err := app.ServeUntilDone(ctx, srv); err != nil {
		log.Fatal(err)
	}
}

// newRouter mounts the library on echo. fsecho.ErrorHandler answers the
// library's errors returned by any handler; the signed-in group resolves
// the user and the tenant once with fsecho.Actor and fsecho.Scope.
// ds:def id=newrouter-qwzddubq
func newRouter(a *app.App) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HTTPErrorHandler = fsecho.ErrorHandler(e.DefaultHTTPErrorHandler)
	e.Use(middleware.Recover())
	e.POST(app.PathUpload, echo.WrapHandler(a.Upload))
	fsecho.Mount(e, app.PathFiles, a.Files)
	me := e.Group(app.GroupMe,
		fsecho.Actor(func(c echo.Context) (string, error) { return app.User(c.Request()) }),
		fsecho.Scope(func(c echo.Context) (string, error) { return app.Tenant(c.Request()) }),
	)
	fsecho.Mount(me, app.MountMedia, a.Records)
	me.GET(app.PathExport+"/:"+app.ParamRole, func(c echo.Context) error { return export(c, a) })
	return e
}

// export zips the signed-in user's files of a role.
// ds:def id=export-mqkcmukz
func export(c echo.Context, a *app.App) error {
	scope, err := fsecho.ScopeOf(c.Request())
	if err != nil {
		return err
	}
	role := c.Param(app.ParamRole)
	files, err := a.Export(c.Request().Context(), scope, role)
	if err != nil {
		return err
	}
	return fsecho.Zip(c, a.Media, app.ExportName(role), files, fshttp.ZipOptions{})
}
