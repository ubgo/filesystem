package variants_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/sqlb"
)

// TestHandlerErrorsAndSendfile pins the round-4 seams of Handler: a
// custom HandlerOptions.Errors renders every failure the handler makes
// (405, 404 path, 403 scope, a store error) with the status the default
// sends; the default keeps the plain status text; and Sendfile hands a
// proxied variant's body to the front proxy.
func TestHandlerErrorsAndSendfile(t *testing.T) {
	e := newEnv(t, memory.New(), transforms)
	f := e.image(t, "a.png", pngBytes(t, 600, 400), filesystem.VisibilityPrivate)
	allow := func(*http.Request) (string, error) { return "t", nil }
	deny := func(*http.Request) (string, error) { return "", errors.New("not signed in") }
	type seen struct {
		status int
		err    error
	}
	var got []seen
	house := func(w http.ResponseWriter, _ *http.Request, status int, err error) {
		got = append(got, seen{status, err})
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"status":` + strconv.Itoa(status) + `}`))
	}
	serve := func(scope variants.ScopeFunc, opts variants.HandlerOptions, method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		variants.Handler(e.store, e.plugin, scope, opts).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, nil))
		return rec
	}
	cases := []struct {
		name   string
		scope  variants.ScopeFunc
		method string
		path   string
		status int
	}{
		{"method", allow, http.MethodPost, "/" + f.ID + "/thumb", http.StatusMethodNotAllowed},
		{"path", allow, http.MethodGet, "/" + f.ID, http.StatusNotFound},
		{"scope", deny, http.MethodGet, "/" + f.ID + "/thumb", http.StatusForbidden},
		{"undeclared", allow, http.MethodGet, "/" + f.ID + "/poster", http.StatusNotFound},
	}
	for _, c := range cases {
		got = nil
		rec := serve(c.scope, variants.HandlerOptions{Errors: house}, c.method, c.path)
		if rec.Code != c.status || rec.Header().Get("Content-Type") != "application/problem+json" || len(got) != 1 || got[0].status != c.status || got[0].err == nil {
			t.Errorf("%s with Errors: %d %v, writer saw %+v", c.name, rec.Code, rec.Header(), got)
		}
		def := serve(c.scope, variants.HandlerOptions{}, c.method, c.path)
		if def.Code != c.status || def.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Errorf("%s default: %d %v", c.name, def.Code, def.Header())
		}
	}
	// Proxy mode reaches the writer too.
	got = nil
	if rec := serve(allow, variants.HandlerOptions{Mode: variants.ModeProxy, Errors: house}, http.MethodGet, "/"+f.ID+"/poster"); rec.Code != http.StatusNotFound || len(got) != 1 {
		t.Errorf("proxy with Errors: %d, writer saw %+v", rec.Code, got)
	}

	sf := &fshttp.Sendfile{Path: func(disk, key string) (string, bool) { return "/internal/" + disk + "/" + key, true }}
	rec := serve(allow, variants.HandlerOptions{Mode: variants.ModeProxy, Sendfile: sf}, http.MethodGet, "/"+f.ID+"/thumb")
	if rec.Code != http.StatusOK || rec.Header().Get(fshttp.HeaderAccelRedirect) == "" || rec.Body.Len() != 0 || rec.Header().Get(fshttp.HeaderContentTypeOptions) != "nosniff" {
		t.Errorf("proxy with Sendfile: %d %v (%d bytes)", rec.Code, rec.Header(), rec.Body.Len())
	}
	if rec := serve(allow, variants.HandlerOptions{Mode: variants.ModeProxy}, http.MethodGet, "/"+f.ID+"/thumb"); rec.Header().Get(fshttp.HeaderAccelRedirect) != "" || rec.Body.Len() == 0 {
		t.Errorf("proxy default must stream: %v", rec.Header())
	}
	// A recorded variant on a disk the Manager no longer has is the
	// lookup's error (404), through the writer, and nothing is streamed.
	if _, err := e.store.Exec(t.Context(), sqlb.NewUpdate(e.store.Dialect()).Table(variants.Table).Set(sqlb.Set(variants.Disk, "retired")).
		Where(sqlb.Eq(variants.FileID, f.ID))); err != nil {
		t.Fatal(err)
	}
	got = nil
	if rec := serve(allow, variants.HandlerOptions{Mode: variants.ModeProxy, Errors: house}, http.MethodGet, "/"+f.ID+"/thumb"); rec.Code != http.StatusNotFound || len(got) != 1 || filesystem.CodeOf(got[0].err) != filesystem.CodeNotFound {
		t.Errorf("proxy of a variant on an unknown disk: %d, writer saw %+v", rec.Code, got)
	}
}
