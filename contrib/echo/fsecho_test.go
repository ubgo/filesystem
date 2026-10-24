package fsecho_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	fsecho "github.com/ubgo/filesystem/contrib/echo"
	"github.com/ubgo/filesystem/contrib/internal/fwtest"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/records/recordshttp"
)

// newRouter is the fake application's routing on echo: the exact upload
// route on the engine, everything else in a group, the records and whoami
// routes behind Actor and Scope, Recover in front and the library's
// errors rendered by ErrorHandler, as a real application has it.
func newRouter(a *fwtest.App) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HTTPErrorHandler = fsecho.ErrorHandler(e.DefaultHTTPErrorHandler)
	e.Use(middleware.Recover())
	e.POST(fwtest.PathUpload, echo.WrapHandler(a.Upload))
	api := e.Group(fwtest.Group)
	fsecho.Mount(api, fwtest.MountFiles, a.Files)
	fsecho.Mount(api, fwtest.MountPublic, a.PublicH)
	fsecho.Mount(api, fwtest.MountSlash, a.Echo)
	fsecho.Mount(api, fwtest.MountPlain, a.Echo)
	api.GET(fwtest.PathZip, func(c echo.Context) error {
		return fsecho.Zip(c, a.Media, fwtest.ZipName, fwtest.ZipFiles(), fshttp.ZipOptions{})
	})
	api.GET(fwtest.PathZipBroken, func(c echo.Context) error {
		return fsecho.Zip(c, a.Broken, fwtest.ZipName, fwtest.ZipFiles(), fshttp.ZipOptions{})
	})
	api.GET(fwtest.PathZipEarly, func(c echo.Context) error {
		return fsecho.Zip(c, a.Broken, fwtest.ZipName, fwtest.EarlyZipFiles(), fshttp.ZipOptions{})
	})
	// The error route returns the error as it is: ErrorHandler renders it.
	api.GET(fwtest.PathError, func(c echo.Context) error {
		return fwtest.Err(fwtest.ErrorCase(c.QueryParam("case")))
	})
	user := func(c echo.Context) (string, error) { return fwtest.User(c.Request()) }
	scope := func(c echo.Context) (string, error) { return fwtest.ScopeOfUser(c.Request()) }
	signedIn := api.Group("", fsecho.Actor(user), fsecho.Scope(scope))
	fsecho.Mount(signedIn, fwtest.MountMedia, a.Records)
	signedIn.GET(fwtest.PathWhoami, func(c echo.Context) error {
		s, err := fsecho.ScopeOf(c.Request())
		if err != nil {
			return fsecho.Error(c, err)
		}
		return c.String(http.StatusOK, fwtest.Whoami(c.Request().Context(), s))
	})
	return e
}

// TestRoutes drives every route of the fake application through a real
// echo server.
func TestRoutes(t *testing.T) {
	ln, base := fwtest.Listen(t)
	// The records endpoints read the scope through recordshttp's shared
	// key, so the adapter's Scope middleware must set exactly that key.
	a := fwtest.NewApp(t, base, recordshttp.ScopeFromRequest)
	fwtest.Serve(t, ln, newRouter(a))
	fwtest.Exercise(t, base, a)
}

// TestErrorHandler_PassesOtherErrorsOn proves echo's own errors and the
// application's keep echo's rendering.
func TestErrorHandler_PassesOtherErrorsOn(t *testing.T) {
	e := echo.New()
	e.HTTPErrorHandler = fsecho.ErrorHandler(e.DefaultHTTPErrorHandler)
	e.GET("/teapot", func(echo.Context) error { return echo.NewHTTPError(http.StatusTeapot, "short and stout") })
	for path, want := range map[string]string{"/teapot": "short and stout", "/missing": "Not Found"} {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if !strings.Contains(rec.Body.String(), want) || strings.Contains(rec.Body.String(), `"code"`) {
			t.Errorf("%s = %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

// TestScopeOf_RefusesARequestOutsideTheMiddleware proves a records route
// mounted without Scope gets an error instead of running in no tenant.
func TestScopeOf_RefusesARequestOutsideTheMiddleware(t *testing.T) {
	if s, err := fsecho.ScopeOf(httptest.NewRequest(http.MethodGet, "/", nil)); !errors.Is(err, fsecho.ErrNoScope) || s != "" {
		t.Fatalf("ScopeOf = %q, %v", s, err)
	}
}

// TestMiddleware_RefusesWithoutAnIdentity proves Scope refuses an empty
// scope and Actor a failing resolver with 401, before the handler runs.
func TestMiddleware_RefusesWithoutAnIdentity(t *testing.T) {
	for name, mw := range map[string]echo.MiddlewareFunc{
		"empty scope": fsecho.Scope(func(echo.Context) (string, error) { return "", nil }),
		"scope error": fsecho.Scope(func(echo.Context) (string, error) { return "acme", errors.New("expired") }),
		"actor error": fsecho.Actor(func(echo.Context) (string, error) { return "alice", errors.New("expired") }),
	} {
		e := echo.New()
		e.GET("/", func(c echo.Context) error { return c.String(http.StatusOK, "ran") }, mw)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"code":"permission"`) {
			t.Errorf("%s = %d %s, want 401", name, rec.Code, rec.Body.String())
		}
	}
}
