package fsgin_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	fsgin "github.com/ubgo/filesystem/contrib/gin"
	"github.com/ubgo/filesystem/contrib/internal/fwtest"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/records/recordshttp"
)

// newRouter is the fake application's routing on gin: the exact upload
// route on the engine, everything else in a route group, the records and
// whoami routes behind Actor and Scope, and gin.Recovery in front, as a
// real application has it.
func newRouter(a *fwtest.App) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.POST(fwtest.PathUpload, gin.WrapH(a.Upload))
	api := r.Group(fwtest.Group)
	fsgin.Mount(api, fwtest.MountFiles, a.Files)
	fsgin.Mount(api, fwtest.MountPublic, a.PublicH)
	fsgin.Mount(api, fwtest.MountSlash, a.Echo)
	fsgin.Mount(api, fwtest.MountPlain, a.Echo)
	api.GET(fwtest.PathZip, func(c *gin.Context) {
		fsgin.Zip(c, a.Media, fwtest.ZipName, fwtest.ZipFiles(), fshttp.ZipOptions{})
	})
	api.GET(fwtest.PathZipBroken, func(c *gin.Context) {
		fsgin.Zip(c, a.Broken, fwtest.ZipName, fwtest.ZipFiles(), fshttp.ZipOptions{})
	})
	api.GET(fwtest.PathZipEarly, func(c *gin.Context) {
		fsgin.Zip(c, a.Broken, fwtest.ZipName, fwtest.EarlyZipFiles(), fshttp.ZipOptions{})
	})
	api.GET(fwtest.PathError, func(c *gin.Context) {
		fsgin.Error(c, fwtest.Err(fwtest.ErrorCase(c.Query("case"))))
	})
	user := func(c *gin.Context) (string, error) { return fwtest.User(c.Request) }
	scope := func(c *gin.Context) (string, error) { return fwtest.ScopeOfUser(c.Request) }
	signedIn := api.Group("", fsgin.Actor(user), fsgin.Scope(scope))
	fsgin.Mount(signedIn, fwtest.MountMedia, a.Records)
	signedIn.GET(fwtest.PathWhoami, func(c *gin.Context) {
		s, err := fsgin.ScopeOf(c.Request)
		if err != nil {
			fsgin.Error(c, err)
			return
		}
		c.String(http.StatusOK, fwtest.Whoami(c.Request.Context(), s))
	})
	return r
}

// TestRoutes drives every route of the fake application through a real
// gin server.
func TestRoutes(t *testing.T) {
	ln, base := fwtest.Listen(t)
	// The records endpoints read the scope through recordshttp's shared
	// key, so the adapter's Scope middleware must set exactly that key.
	a := fwtest.NewApp(t, base, recordshttp.ScopeFromRequest)
	fwtest.Serve(t, ln, newRouter(a))
	fwtest.Exercise(t, base, a)
}

// TestScopeOf_RefusesARequestOutsideTheMiddleware proves a records route
// mounted without Scope answers 401 instead of running in no tenant.
func TestScopeOf_RefusesARequestOutsideTheMiddleware(t *testing.T) {
	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := fsgin.ScopeOf(r); err != fsgin.ErrNoScope || s != "" {
		t.Fatalf("ScopeOf = %q, %v", s, err)
	}
}

// TestMiddleware_RefusesWithoutAnIdentity proves Scope refuses an empty
// scope and Actor a failing resolver with 401, before the handler runs.
func TestMiddleware_RefusesWithoutAnIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, mw := range map[string]gin.HandlerFunc{
		"empty scope": fsgin.Scope(func(*gin.Context) (string, error) { return "", nil }),
		"scope error": fsgin.Scope(func(*gin.Context) (string, error) { return "acme", errors.New("expired") }),
		"actor error": fsgin.Actor(func(*gin.Context) (string, error) { return "alice", errors.New("expired") }),
	} {
		r := gin.New()
		r.GET("/", mw, func(c *gin.Context) { c.String(http.StatusOK, "ran") })
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"code":"permission"`) {
			t.Errorf("%s = %d %s, want 401", name, rec.Code, rec.Body.String())
		}
	}
}
