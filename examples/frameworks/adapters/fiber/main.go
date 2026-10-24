// Command fiber serves the library on fiber through the fsfiber adapter:
// form uploads, signed downloads, the records endpoints behind
// fsfiber.Actor and fsfiber.Scope in a group, and an export route of the
// app's own that zips the signed-in user's files with fsfiber.Zip.
// Handlers return the library's errors as they are; fsfiber.ErrorHandler
// renders them.
//
//	go run ./adapters/fiber    (from examples/frameworks; listens on localhost:8193)
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"

	fsfiber "github.com/ubgo/filesystem/contrib/fiber"
	app "github.com/ubgo/filesystem/examples/frameworks/internal/adapterapp"
	"github.com/ubgo/filesystem/fshttp"
)

// Where the server listens and the URL clients reach it at; signed links
// are built from baseURL.
const (
	addr    = "localhost:8193"
	baseURL = "http://" + addr
	// readTimeout bounds how long a client may take to send a request.
	// fiber reads whole bodies before a handler runs, so it must cover the
	// largest upload on the slowest link you serve.
	readTimeout = 5 * time.Minute
	// bodyLimit lets the policy's largest file through fiber, plus room
	// for the multipart form around it.
	bodyLimit = app.MaxUploadBytes + 64<<10
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a, err := app.New(ctx, baseURL, fsfiber.ScopeOf)
	if err != nil {
		log.Fatal(err)
	}
	f := newApp(a)
	go func() {
		<-ctx.Done()
		_ = f.ShutdownWithTimeout(app.ShutdownTimeout)
	}()
	log.Printf("listening on %s", baseURL)
	if err := f.Listen(addr, fiber.ListenConfig{DisableStartupMessage: true}); err != nil {
		log.Fatal(err)
	}
}

// newApp mounts the library on fiber. Every library handler goes through
// fsfiber (Mount or Handler), which carries the actor and scope from
// fiber's middleware into the handler's *http.Request; fiber's own
// adaptor would drop them.
// ds:def id=newapp-tmz72tk7
func newApp(a *app.App) *fiber.App {
	f := fiber.New(fiber.Config{
		BodyLimit:    bodyLimit,
		ReadTimeout:  readTimeout,
		ErrorHandler: fsfiber.ErrorHandler(fiber.DefaultErrorHandler),
	})
	f.Use(recover.New())
	f.Post(app.PathUpload, fsfiber.Handler(a.Upload))
	fsfiber.Mount(f, app.PathFiles, a.Files)
	me := f.Group(app.GroupMe,
		fsfiber.Actor(func(c fiber.Ctx) (string, error) { return c.Get(app.HeaderUser), nil }),
		fsfiber.Scope(tenant),
	)
	fsfiber.Mount(me, app.MountMedia, a.Records)
	me.Get(app.PathExport+"/:"+app.ParamRole, func(c fiber.Ctx) error { return export(c, a) })
	return f
}

// tenant is app.Tenant for fiber, which has no *http.Request.
func tenant(c fiber.Ctx) (string, error) { return app.TenantOf(c.Get(app.HeaderUser)) }

// export zips the signed-in user's files of a role.
func export(c fiber.Ctx, a *app.App) error {
	scope, err := fsfiber.ScopeFrom(c)
	if err != nil {
		return err
	}
	role := c.Params(app.ParamRole)
	files, err := a.Export(c.Context(), scope, role)
	if err != nil {
		return err
	}
	return fsfiber.Zip(c, a.Media, app.ExportName(role), files, fshttp.ZipOptions{})
}
