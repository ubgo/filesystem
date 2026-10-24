package fsfiber_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/ubgo/filesystem"
	fsfiber "github.com/ubgo/filesystem/contrib/fiber"
	"github.com/ubgo/filesystem/contrib/internal/fwtest"
)

// TestResponder: a custom ErrorWriter, run through fiber's adaptor,
// renders the middleware's 401 and the answers of Error and of
// ErrorHandler, Retry-After included.
func TestResponder(t *testing.T) {
	x := fsfiber.Responder{Errors: fwtest.Problem}
	f := fiber.New(fiber.Config{ErrorHandler: x.ErrorHandler(fiber.DefaultErrorHandler)})
	f.Get("/err", func(c fiber.Ctx) error {
		return x.Error(c, fwtest.Err(fwtest.ErrorCase(c.Query("case"))))
	})
	f.Get("/returned", func(fiber.Ctx) error { return fwtest.Err(fwtest.CaseRejected) })
	f.Get("/scope", x.Scope(func(fiber.Ctx) (string, error) { return "", nil }), func(fiber.Ctx) error { return nil })
	f.Get("/actor", x.Actor(func(fiber.Ctx) (string, error) { return "", errors.New("expired") }), func(fiber.Ctx) error { return nil })
	get := func(path string) *http.Response {
		resp, err := f.Test(httptest.NewRequest(http.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	fwtest.WantProblem(t, get("/scope"), http.StatusUnauthorized, filesystem.CodePermission)
	fwtest.WantProblem(t, get("/actor"), http.StatusUnauthorized, filesystem.CodePermission)
	fwtest.WantProblem(t, get("/returned"), http.StatusUnsupportedMediaType, filesystem.CodeRejected)
	resp := get("/err?case=" + string(fwtest.CaseRetry))
	if resp.Header.Get("Retry-After") != fwtest.RetryHeader {
		t.Errorf("Retry-After = %q", resp.Header.Get("Retry-After"))
	}
	fwtest.WantProblem(t, resp, http.StatusServiceUnavailable, filesystem.CodeUnavailable)
}
