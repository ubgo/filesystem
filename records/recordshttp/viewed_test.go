package recordshttp_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
)

// TestReadViewFailure: when counting the view of a view-once file fails,
// GET /files/{id} answers 500 with no link, and the file keeps its one
// view for a later request.
func TestReadViewFailure(t *testing.T) {
	testdb.OnlySQLite(t)
	ctx := t.Context()
	e := newEnv(t)
	once, err := e.store.Upload(ctx, tenant, "media", strings.NewReader("secret"), "s.txt", testPolicy, records.ViewOnce())
	if err != nil {
		t.Fatal(err)
	}
	testdb.Exec(t, e.db, "CREATE TRIGGER fail_view BEFORE UPDATE ON "+records.TableNameFiles+" BEGIN SELECT RAISE(FAIL, 'injected'); END")
	rec := e.do(http.MethodGet, "/files/"+once.ID, nil, "")
	wantStatus(t, rec, http.StatusInternalServerError)
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("a failed view handed out %q", loc)
	}
	testdb.Exec(t, e.db, "DROP TRIGGER fail_view")
	wantStatus(t, e.do(http.MethodGet, "/files/"+once.ID, nil, ""), http.StatusFound)
}
