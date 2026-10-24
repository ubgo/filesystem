package share_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/share"
)

// TestRespondCarriesFile pins R-I10: a collection's listing and a
// deposit hand WithRespond the records.File behind each body, so a share
// page can show a date or a caption without a lookup; the default bytes
// are unchanged (TestDefaultSuccessUnchanged).
func TestRespondCarriesFile(t *testing.T) {
	u, coll, req, inID := successSetup(t)
	var listed []*records.File
	var deposited *records.File
	respond := func(w http.ResponseWriter, r *http.Request, status int, body share.SuccessBody) {
		switch b := body.(type) {
		case share.ListResponse:
			for _, f := range b {
				listed = append(listed, f.File)
			}
		case share.DepositResponse:
			deposited = b.File
		}
		share.JSONSuccess(w, r, status, body)
	}
	h := share.Handler(u.store, u.plugin, share.WithUploads("media", anyText), share.WithRespond(respond))
	if rec := serve(h, http.MethodGet, "/"+coll.Token, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("listing = %d %s", rec.Code, rec.Body)
	}
	if len(listed) != 1 || listed[0] == nil || listed[0].ID != inID || listed[0].CreatedAt.IsZero() || listed[0].Scope != "t" {
		t.Fatalf("listed files %+v", listed)
	}
	body, ct := depositBody()
	rec := serve(h, http.MethodPost, "/"+req.Token, body, http.Header{"Content-Type": {ct}})
	if rec.Code != http.StatusCreated || deposited == nil || deposited.Filename != "doc.txt" || deposited.Size != 5 {
		t.Fatalf("deposit = %d, File %+v", rec.Code, deposited)
	}
	if strings.Contains(rec.Body.String(), "doc.txt") {
		t.Fatalf("the File leaked into the body: %s", rec.Body)
	}
}

// diskSuite is a Store with folders and share over two disks: "media",
// WithUploads' disk, and "tenant", a tenant's own bucket.
func diskSuite(t *testing.T) (*records.Store, *share.Plugin, share.Share, *filesystem.Disk) {
	t.Helper()
	ctx := t.Context()
	db, _ := testdb.OpenFaulty(t)
	media, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithURLSigner(signer{}))
	tenant, _ := filesystem.NewDisk("tenant", memory.New(), filesystem.WithURLSigner(signer{}))
	mgr, _ := filesystem.NewManager(media, tenant)
	fp := folders.New()
	sp := share.New(share.WithCollections(fp.Collection()))
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(sp, fp))
	must(t, err)
	_, err = s.Migrate(ctx)
	must(t, err)
	dir, err := fp.Create(ctx, s, "t", folders.RootID, "Inbox")
	must(t, err)
	req, err := sp.CreateRequest(ctx, s, "t", share.Target{Kind: folders.KindFolder, Ref: dir.ID}, share.Options{TTL: time.Hour})
	must(t, err)
	return s, sp, req, tenant
}

// TestWithUploadDisk pins R-I11: the chooser routes a deposit to the
// disk it names, seeing the link; its error refuses with its status and
// stores nothing; an empty or unknown name is 500; without it the deposit
// goes to WithUploads' disk, as before.
func TestWithUploadDisk(t *testing.T) {
	deposit := func(t *testing.T, opts ...share.HandlerOption) (*records.Store, *httpResult) {
		t.Helper()
		s, sp, req, _ := diskSuite(t)
		h := share.Handler(s, sp, append([]share.HandlerOption{share.WithUploads("media", anyText)}, opts...)...)
		body, ct := depositBody()
		rec := serve(h, http.MethodPost, "/"+req.Token, body, http.Header{"Content-Type": {ct}})
		return s, &httpResult{code: rec.Code, body: rec.Body.String(), link: req}
	}
	files := func(t *testing.T, s *records.Store) []records.File {
		t.Helper()
		page, err := s.Files(t.Context(), records.FileQuery{Scope: "t"})
		must(t, err)
		return page.Files
	}

	var saw share.Share
	s, res := deposit(t, share.WithUploadDisk(func(_ *http.Request, link share.Share) (string, error) {
		saw = link
		return "tenant", nil
	}))
	if res.code != http.StatusCreated || saw.ID != res.link.ID || saw.Scope != "t" {
		t.Fatalf("custom disk: %d %s (saw %+v)", res.code, res.body, saw)
	}
	if fs := files(t, s); len(fs) != 1 || fs[0].Disk != "tenant" {
		t.Fatalf("stored %+v", fs)
	}

	s, res = deposit(t, share.WithUploadDisk(func(*http.Request, share.Share) (string, error) {
		return "", filesystem.Errorf(filesystem.CodePermission, "tenant suspended")
	}))
	if res.code != http.StatusForbidden || len(files(t, s)) != 0 {
		t.Fatalf("a refusing chooser: %d %s", res.code, res.body)
	}
	for name, fn := range map[string]func(*http.Request, share.Share) (string, error){
		"empty":   func(*http.Request, share.Share) (string, error) { return "", nil },
		"unknown": func(*http.Request, share.Share) (string, error) { return "nowhere", nil },
		"uncoded": func(*http.Request, share.Share) (string, error) { return "", errors.New("lookup failed") },
		"no store": func(*http.Request, share.Share) (string, error) {
			return "", filesystem.Errorf(filesystem.CodeUnknown, "down")
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, res := deposit(t, share.WithUploadDisk(fn))
			if res.code != http.StatusInternalServerError || len(files(t, s)) != 0 {
				t.Fatalf("%d %s", res.code, res.body)
			}
		})
	}

	s, res = deposit(t)
	if fs := files(t, s); res.code != http.StatusCreated || len(fs) != 1 || fs[0].Disk != "media" {
		t.Fatalf("default disk: %d %+v", res.code, fs)
	}
	s, res = deposit(t, share.WithUploadDisk(nil))
	if res.code != http.StatusInternalServerError || len(files(t, s)) != 0 {
		t.Fatalf("a nil chooser must fail closed: %d %s", res.code, res.body)
	}
}

// httpResult is one deposit's answer and the link it went through.
type httpResult struct {
	code int
	body string
	link share.Share
}
