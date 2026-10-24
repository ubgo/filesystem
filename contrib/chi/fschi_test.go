package fschi_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	fschi "github.com/ubgo/filesystem/contrib/chi"
	"github.com/ubgo/filesystem/contrib/internal/fwtest"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/records/recordshttp"
)

// newRouter is the fake application's routing on chi: the exact upload
// route on the root, everything else in a sub-router, the records and
// whoami routes in a group behind Actor and Scope, and Recoverer in
// front, as a real application has it.
func newRouter(a *fwtest.App) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Method(http.MethodPost, fwtest.PathUpload, a.Upload)
	r.Route(fwtest.Group, func(api chi.Router) {
		fschi.Mount(api, fwtest.MountFiles, a.Files)
		fschi.Mount(api, fwtest.MountPublic, a.PublicH)
		fschi.Mount(api, fwtest.MountSlash, a.Echo)
		fschi.Mount(api, fwtest.MountPlain, a.Echo)
		api.Get(fwtest.PathZip, func(w http.ResponseWriter, r *http.Request) {
			fschi.Zip(w, r, a.Media, fwtest.ZipName, fwtest.ZipFiles(), fshttp.ZipOptions{})
		})
		api.Get(fwtest.PathZipBroken, func(w http.ResponseWriter, r *http.Request) {
			fschi.Zip(w, r, a.Broken, fwtest.ZipName, fwtest.ZipFiles(), fshttp.ZipOptions{})
		})
		api.Get(fwtest.PathZipEarly, func(w http.ResponseWriter, r *http.Request) {
			fschi.Zip(w, r, a.Broken, fwtest.ZipName, fwtest.EarlyZipFiles(), fshttp.ZipOptions{})
		})
		api.Get(fwtest.PathError, func(w http.ResponseWriter, r *http.Request) {
			fschi.Error(w, fwtest.Err(fwtest.ErrorCase(r.URL.Query().Get("case"))))
		})
		api.Group(func(signedIn chi.Router) {
			signedIn.Use(fschi.Actor(fwtest.User), fschi.Scope(fwtest.ScopeOfUser))
			fschi.Mount(signedIn, fwtest.MountMedia, a.Records)
			signedIn.Get(fwtest.PathWhoami, func(w http.ResponseWriter, r *http.Request) {
				s, err := fschi.ScopeOf(r)
				if err != nil {
					fschi.Error(w, err)
					return
				}
				_, _ = w.Write([]byte(fwtest.Whoami(r.Context(), s)))
			})
		})
	})
	return r
}

// TestRoutes drives every route of the fake application through a real
// chi server.
func TestRoutes(t *testing.T) {
	ln, base := fwtest.Listen(t)
	// The records endpoints read the scope through recordshttp's shared
	// key, so the adapter's Scope middleware must set exactly that key.
	a := fwtest.NewApp(t, base, recordshttp.ScopeFromRequest)
	fwtest.Serve(t, ln, newRouter(a))
	fwtest.Exercise(t, base, a)
}

// TestScopeOf_RefusesARequestOutsideTheMiddleware proves a records route
// mounted without Scope gets an error instead of running in no tenant.
func TestScopeOf_RefusesARequestOutsideTheMiddleware(t *testing.T) {
	if s, err := fschi.ScopeOf(httptest.NewRequest(http.MethodGet, "/", nil)); !errors.Is(err, fschi.ErrNoScope) || s != "" {
		t.Fatalf("ScopeOf = %q, %v", s, err)
	}
}

// TestMiddleware_RefusesWithoutAnIdentity proves Scope refuses an empty
// scope and Actor a failing resolver with 401, before the handler runs.
func TestMiddleware_RefusesWithoutAnIdentity(t *testing.T) {
	for name, mw := range map[string]func(http.Handler) http.Handler{
		"empty scope": fschi.Scope(func(*http.Request) (string, error) { return "", nil }),
		"scope error": fschi.Scope(func(*http.Request) (string, error) { return "acme", errors.New("expired") }),
		"actor error": fschi.Actor(func(*http.Request) (string, error) { return "alice", errors.New("expired") }),
	} {
		r := chi.NewRouter()
		r.With(mw).Get("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ran")) })
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"code":"permission"`) {
			t.Errorf("%s = %d %s, want 401", name, rec.Code, rec.Body.String())
		}
	}
}
