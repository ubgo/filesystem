package fsgin_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/ubgo/filesystem"
	fsgin "github.com/ubgo/filesystem/contrib/gin"
	"github.com/ubgo/filesystem/contrib/internal/fwtest"
)

// TestResponder: a custom ErrorWriter renders the middleware's 401 and
// Error's answers, Retry-After included, and the chain stops after it.
func TestResponder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	x := fsgin.Responder{Errors: fwtest.Problem}
	r := gin.New()
	ran := false
	after := func(*gin.Context) { ran = true }
	r.GET("/err", func(c *gin.Context) {
		x.Error(c, fwtest.Err(fwtest.ErrorCase(c.Query("case"))))
	}, after)
	r.GET("/scope", x.Scope(func(*gin.Context) (string, error) { return "", nil }), after)
	r.GET("/actor", x.Actor(func(*gin.Context) (string, error) { return "", errors.New("expired") }), after)
	get := func(path string) *http.Response {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Result()
	}
	fwtest.WantProblem(t, get("/scope"), http.StatusUnauthorized, filesystem.CodePermission)
	fwtest.WantProblem(t, get("/actor"), http.StatusUnauthorized, filesystem.CodePermission)
	resp := get("/err?case=" + string(fwtest.CaseRetry))
	if resp.Header.Get("Retry-After") != fwtest.RetryHeader {
		t.Errorf("Retry-After = %q", resp.Header.Get("Retry-After"))
	}
	fwtest.WantProblem(t, resp, http.StatusServiceUnavailable, filesystem.CodeUnavailable)
	if ran {
		t.Error("the chain went on after the custom writer answered")
	}
}
