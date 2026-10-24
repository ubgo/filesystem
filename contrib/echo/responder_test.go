package fsecho_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/ubgo/filesystem"
	fsecho "github.com/ubgo/filesystem/contrib/echo"
	"github.com/ubgo/filesystem/contrib/internal/fwtest"
)

// TestResponder: a custom ErrorWriter renders the middleware's 401, the
// answers of Error and of ErrorHandler, Retry-After included.
func TestResponder(t *testing.T) {
	x := fsecho.Responder{Errors: fwtest.Problem}
	e := echo.New()
	e.HTTPErrorHandler = x.ErrorHandler(e.DefaultHTTPErrorHandler)
	e.GET("/err", func(c echo.Context) error {
		return x.Error(c, fwtest.Err(fwtest.ErrorCase(c.QueryParam("case"))))
	})
	e.GET("/returned", func(c echo.Context) error { return fwtest.Err(fwtest.CaseRejected) })
	e.GET("/scope", func(echo.Context) error { return nil }, x.Scope(func(echo.Context) (string, error) { return "", nil }))
	e.GET("/actor", func(echo.Context) error { return nil }, x.Actor(func(echo.Context) (string, error) { return "", errors.New("expired") }))
	get := func(path string) *http.Response {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Result()
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

// TestPackageError: the package-level Error is the zero Responder's, the
// JSON upload.ErrorResponse with the error's status and code.
func TestPackageError(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	if err := fsecho.Error(c, fwtest.Err(fwtest.CaseRejected)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnsupportedMediaType || !strings.Contains(rec.Body.String(), `"code":"rejected"`) {
		t.Fatalf("Error = %d %s", rec.Code, rec.Body)
	}
}
