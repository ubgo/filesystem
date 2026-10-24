package recordshttp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordshttp"
)

func TestScopeContext(t *testing.T) {
	if _, ok := recordshttp.ScopeFromContext(context.Background()); ok {
		t.Fatal("a bare context has a scope")
	}
	for _, scope := range []string{"acme", ""} {
		ctx := recordshttp.WithScope(context.Background(), scope)
		if got, ok := recordshttp.ScopeFromContext(ctx); !ok || got != scope {
			t.Fatalf("ScopeFromContext = %q, %v; want %q", got, ok, scope)
		}
		r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
		if got, err := recordshttp.ScopeFromRequest(r); err != nil || got != scope {
			t.Fatalf("ScopeFromRequest = %q, %v", got, err)
		}
	}
	_, err := recordshttp.ScopeFromRequest(httptest.NewRequest(http.MethodGet, "/", nil))
	if filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("no scope = %v", err)
	}
}

// TestScopeFromRequestAsConfigScope wires ScopeFromRequest as Config.Scope:
// a request carrying the scope reaches its files, one without is 401.
func TestScopeFromRequestAsConfigScope(t *testing.T) {
	e := newEnv(t, func(c *recordshttp.Config, _ *[]filesystem.Option) { c.Scope = recordshttp.ScopeFromRequest })
	body, ct := form(t, part{field: recordshttp.FormField, filename: "a.txt", body: "a"})
	send := func(ctx context.Context) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/owners/post/p1/gallery", body).WithContext(ctx)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		e.srv.ServeHTTP(rec, req)
		return rec
	}
	wantError(t, send(context.Background()), http.StatusUnauthorized, filesystem.CodePermission, "not signed in")
	body, ct = form(t, part{field: recordshttp.FormField, filename: "a.txt", body: "a"})
	wantStatus(t, send(recordshttp.WithScope(context.Background(), tenant)), http.StatusCreated)
	if list, err := e.store.Attachments(t.Context(), tenant, records.Owner{Type: "post", ID: "p1"}, "gallery"); err != nil || len(list) != 1 {
		t.Fatal("the upload did not land in the scope")
	}
}
