package recordshttp_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordshttp"
)

// TestListUsesStoreClock: the list route decides expiry by the Store's
// clock (records.WithClock), not the wall clock. The Store runs a year
// ahead, so a file expiring between the wall clock and the Store's is
// hidden only when the route reads the Store's.
func TestListUsesStoreClock(t *testing.T) {
	ahead := time.Now().AddDate(1, 0, 0)
	e := newRouteEnv(t, recordshttp.Config{Disk: "media"}, records.WithClock(func() time.Time { return ahead }))
	ctx := t.Context()
	p := records.Owner{Type: "post", ID: "p1"}
	for name, exp := range map[string]time.Time{"soon.txt": time.Now().Add(time.Hour), "later.txt": ahead.Add(time.Hour)} {
		f, err := e.store.Upload(ctx, tenant, "media", strings.NewReader("x"), name, testPolicy, records.Expires(exp))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.store.Attach(ctx, tenant, f.ID, p, "gallery"); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/owners/post/p1/gallery", nil))
	wantStatus(t, rec, http.StatusOK)
	files := decode[recordshttp.ListResponse](t, rec).Files
	if len(files) != 1 || files[0].Filename != "later.txt" {
		t.Fatalf("listed = %+v, want only later.txt", files)
	}
}
