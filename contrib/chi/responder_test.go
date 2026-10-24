package fschi_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ubgo/filesystem"
	fschi "github.com/ubgo/filesystem/contrib/chi"
	"github.com/ubgo/filesystem/contrib/internal/fwtest"
)

// TestResponder: a custom ErrorWriter renders the middleware's 401 and
// Error's answers, Retry-After included; the zero Responder is the
// package-level default (covered by TestRoutes and the refusal test).
func TestResponder(t *testing.T) {
	x := fschi.Responder{Errors: fwtest.Problem}
	r := chi.NewRouter()
	r.Get("/err", func(w http.ResponseWriter, req *http.Request) {
		x.Error(w, req, fwtest.Err(fwtest.ErrorCase(req.URL.Query().Get("case"))))
	})
	r.With(x.Scope(func(*http.Request) (string, error) { return "", nil })).Get("/scope", func(http.ResponseWriter, *http.Request) {})
	r.With(x.Actor(func(*http.Request) (string, error) { return "", errors.New("expired") })).Get("/actor", func(http.ResponseWriter, *http.Request) {})
	get := func(path string) *http.Response {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Result()
	}
	fwtest.WantProblem(t, get("/scope"), http.StatusUnauthorized, filesystem.CodePermission)
	fwtest.WantProblem(t, get("/actor"), http.StatusUnauthorized, filesystem.CodePermission)
	fwtest.WantProblem(t, get("/err?case="+string(fwtest.CaseTooLarge)), http.StatusRequestEntityTooLarge, filesystem.CodeTooLarge)
	resp := get("/err?case=" + string(fwtest.CaseRetry))
	if resp.Header.Get("Retry-After") != fwtest.RetryHeader {
		t.Errorf("Retry-After = %q", resp.Header.Get("Retry-After"))
	}
	fwtest.WantProblem(t, resp, http.StatusServiceUnavailable, filesystem.CodeUnavailable)
}
