package fsfiber_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"

	fsfiber "github.com/ubgo/filesystem/contrib/fiber"
	"github.com/ubgo/filesystem/contrib/internal/fwtest"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/records/recordshttp"
)

// bodyLimit lets the fake application's largest upload through fiber,
// which reads every body before a handler runs.
const bodyLimit = 4 << 20

// newRouter is the fake application's routing on fiber: the exact upload
// route on the app, everything else in a group, the records and whoami
// routes behind Actor and Scope, recover in front and the library's errors
// rendered by ErrorHandler, as a real application has it.
func newRouter(a *fwtest.App) *fiber.App {
	f := fiber.New(fiber.Config{BodyLimit: bodyLimit, ErrorHandler: fsfiber.ErrorHandler(fiber.DefaultErrorHandler)})
	f.Use(recover.New())
	f.Post(fwtest.PathUpload, fsfiber.Handler(a.Upload))
	api := f.Group(fwtest.Group)
	fsfiber.Mount(api, fwtest.MountFiles, a.Files)
	fsfiber.Mount(api, fwtest.MountPublic, a.PublicH)
	fsfiber.Mount(api, fwtest.MountSlash, a.Echo)
	fsfiber.Mount(api, fwtest.MountPlain, a.Echo)
	api.Get(fwtest.PathZip, func(c fiber.Ctx) error {
		return fsfiber.Zip(c, a.Media, fwtest.ZipName, fwtest.ZipFiles(), fshttp.ZipOptions{})
	})
	api.Get(fwtest.PathZipBroken, func(c fiber.Ctx) error {
		return fsfiber.Zip(c, a.Broken, fwtest.ZipName, fwtest.ZipFiles(), fshttp.ZipOptions{})
	})
	api.Get(fwtest.PathZipEarly, func(c fiber.Ctx) error {
		return fsfiber.Zip(c, a.Broken, fwtest.ZipName, fwtest.EarlyZipFiles(), fshttp.ZipOptions{})
	})
	// The error route returns the error as it is: ErrorHandler renders it.
	api.Get(fwtest.PathError, func(c fiber.Ctx) error {
		return fwtest.Err(fwtest.ErrorCase(c.Query("case")))
	})
	user := func(c fiber.Ctx) (string, error) { return c.Get(fwtest.HeaderUser), nil }
	scope := func(c fiber.Ctx) (string, error) {
		if c.Get(fwtest.HeaderUser) == "" {
			return "", errors.New("sign in first")
		}
		return fwtest.Tenant, nil
	}
	signedIn := api.Group("", fsfiber.Actor(user), fsfiber.Scope(scope))
	fsfiber.Mount(signedIn, fwtest.MountMedia, a.Records)
	signedIn.Get(fwtest.PathWhoami, func(c fiber.Ctx) error {
		s, err := fsfiber.ScopeFrom(c)
		if err != nil {
			return fsfiber.Error(c, err)
		}
		return c.SendString(fwtest.Whoami(c.Context(), s))
	})
	return f
}

// TestRoutes drives every route of the fake application through a real
// fiber server.
func TestRoutes(t *testing.T) {
	ln, base := fwtest.Listen(t)
	// The records endpoints read the scope through recordshttp's shared
	// key, so the adapter's Scope middleware must set exactly that key.
	a := fwtest.NewApp(t, base, recordshttp.ScopeFromRequest)
	f := newRouter(a)
	go func() { _ = f.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() { _ = f.Shutdown() })
	fwtest.Exercise(t, base, a)
}

// TestErrorHandler_PassesOtherErrorsOn proves fiber's own errors and the
// application's keep fiber's rendering.
func TestErrorHandler_PassesOtherErrorsOn(t *testing.T) {
	f := fiber.New(fiber.Config{ErrorHandler: fsfiber.ErrorHandler(fiber.DefaultErrorHandler)})
	f.Get("/teapot", func(fiber.Ctx) error { return fiber.NewError(http.StatusTeapot, "short and stout") })
	for path, want := range map[string]string{"/teapot": "short and stout", "/missing": "Not Found"} {
		resp, err := f.Test(httptest.NewRequest(http.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		body := readAll(t, resp)
		if !strings.Contains(body, want) || strings.Contains(body, `"code"`) {
			t.Errorf("%s = %d %s", path, resp.StatusCode, body)
		}
	}
}

// TestScope_OutsideTheMiddleware proves both readers refuse a request that
// did not pass through Scope.
func TestScope_OutsideTheMiddleware(t *testing.T) {
	if s, err := fsfiber.ScopeOf(httptest.NewRequest(http.MethodGet, "/", nil)); !errors.Is(err, fsfiber.ErrNoScope) || s != "" {
		t.Fatalf("ScopeOf = %q, %v", s, err)
	}
	f := fiber.New()
	f.Get("/", func(c fiber.Ctx) error {
		_, err := fsfiber.ScopeFrom(c)
		return fsfiber.Error(c, err)
	})
	resp, err := f.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, resp); resp.StatusCode == http.StatusOK || !strings.Contains(body, `"code":"permission"`) {
		t.Fatalf("ScopeFrom outside Scope = %d %s", resp.StatusCode, body)
	}
}

// TestMiddleware_RefusesWithoutAnIdentity proves Scope refuses an empty
// scope and Actor a failing resolver with 401, before the handler runs.
func TestMiddleware_RefusesWithoutAnIdentity(t *testing.T) {
	for name, mw := range map[string]fiber.Handler{
		"empty scope": fsfiber.Scope(func(fiber.Ctx) (string, error) { return "", nil }),
		"scope error": fsfiber.Scope(func(fiber.Ctx) (string, error) { return "acme", errors.New("expired") }),
		"actor error": fsfiber.Actor(func(fiber.Ctx) (string, error) { return "alice", errors.New("expired") }),
	} {
		f := fiber.New()
		f.Get("/", mw, func(c fiber.Ctx) error { return c.SendString("ran") })
		resp, err := f.Test(httptest.NewRequest(http.MethodGet, "/", nil))
		if err != nil {
			t.Fatal(err)
		}
		if body := readAll(t, resp); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, `"code":"permission"`) {
			t.Errorf("%s = %d %s, want 401", name, resp.StatusCode, body)
		}
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var b strings.Builder
	if _, err := io.Copy(&b, resp.Body); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
