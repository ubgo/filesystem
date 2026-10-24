package share_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/filesystem/records/share"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// suite is a Store with share (folders and owners collections), folders
// and quota installed.
type suite struct {
	db      *sql.DB
	faults  *testdb.Faults
	store   *records.Store
	disk    *filesystem.Disk
	plugin  *share.Plugin
	folders *folders.Plugin
}

var anyText = upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}}

func newSuite(t *testing.T, quotaBytes int64) *suite {
	t.Helper()
	db, faults := testdb.OpenFaulty(t)
	disk, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithURLSigner(signer{}))
	mgr, _ := filesystem.NewManager(disk)
	fp := folders.New()
	sp := share.New(share.WithCollections(fp.Collection(), share.Owners{}))
	q, err := quota.New(func(context.Context, string) (quota.Limit, error) { return quota.Limit{Bytes: quotaBytes}, nil })
	if err != nil {
		t.Fatal(err)
	}
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(sp, fp, q), records.WithOwnerTypes("post"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &suite{db: db, faults: faults, store: s, disk: disk, plugin: sp, folders: fp}
}

func (u *suite) file(t *testing.T, name string) records.File {
	t.Helper()
	f, err := u.store.Upload(t.Context(), "t", "media", strings.NewReader(name), name, anyText)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (u *suite) uses(t *testing.T, sh share.Share) int64 {
	t.Helper()
	st, err := u.plugin.Stats(t.Context(), u.store, "t", sh.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return st.Uses
}

func serve(h http.Handler, method, path string, body *bytes.Buffer, header http.Header) *httptest.ResponseRecorder {
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, body)
	}
	for k, v := range header {
		r.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestPassword: a link with a password spends nothing until the right one
// is given, through the API, a POST form or the header; the hash, not the
// password, is stored.
func TestPassword(t *testing.T) {
	ctx := t.Context()
	u := newSuite(t, 0)
	f := u.file(t, "secret.txt")
	sh, err := u.plugin.Create(ctx, u.store, "t", f.ID, share.Options{TTL: time.Hour, Password: "correct horse"})
	if err != nil || !sh.HasPassword {
		t.Fatalf("Create = %+v, %v", sh, err)
	}
	for _, opts := range [][]share.AccessOption{nil, {share.WithPassword("wrong")}} {
		if _, err := u.plugin.Resolve(ctx, u.store, sh.Token, opts...); !errors.Is(err, share.ErrPassword) {
			t.Fatalf("Resolve without the password = %v", err)
		}
		if _, err := u.plugin.Check(ctx, u.store, sh.Token, opts...); !errors.Is(err, share.ErrPassword) {
			t.Fatalf("Check without the password = %v", err)
		}
	}
	if n := u.uses(t, sh); n != 0 {
		t.Fatalf("a refused password spent %d", n)
	}
	var stored string
	if err := u.store.One(ctx, sqlbSelectHash(u.store, sh.ID, &stored)); err != nil || stored == "" || strings.Contains(stored, "correct horse") {
		t.Fatalf("stored hash = %q, %v", stored, err)
	}

	h := share.Handler(u.store, u.plugin)
	if rec := serve(h, http.MethodGet, "/"+sh.Token, nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET without a password = %d", rec.Code)
	}
	form := func(pw string) *httptest.ResponseRecorder {
		return serve(h, http.MethodPost, "/"+sh.Token, bytes.NewBufferString(url.Values{"password": {pw}}.Encode()),
			http.Header{"Content-Type": {"application/x-www-form-urlencoded"}})
	}
	if rec := form("wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST a wrong password = %d", rec.Code)
	}
	if rec := form("correct horse"); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") == "" {
		t.Fatalf("POST the password = %d", rec.Code)
	}
	if rec := serve(h, http.MethodGet, "/"+sh.Token, nil, http.Header{"Share-Password": {"correct horse"}}); rec.Code != http.StatusFound {
		t.Fatalf("GET with the header = %d", rec.Code)
	}
	if n := u.uses(t, sh); n != 2 {
		t.Fatalf("uses = %d, want 2", n)
	}
}

// countingLimiter is an AttemptLimiter holding at most max attempts in
// total, recording the keys it was asked for.
type countingLimiter struct {
	max, held int
	keys      []string
}

func (l *countingLimiter) Allow(key string) bool {
	l.keys = append(l.keys, key)
	if l.held >= l.max {
		return false
	}
	l.held++
	return true
}

func (l *countingLimiter) Release(string) { l.held-- }

// TestPasswordAttemptsLimited: only wrong passwords count, keyed by the
// link's ID; past the allowance the Handler answers 429 without checking,
// even the right password; the right password before that is never
// limited; and a token that names no link never reaches the limiter.
func TestPasswordAttemptsLimited(t *testing.T) {
	ctx := t.Context()
	u := newSuite(t, 0)
	sh, err := u.plugin.Create(ctx, u.store, "t", u.file(t, "s.txt").ID, share.Options{TTL: time.Hour, Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	l := &countingLimiter{max: 2}
	h := share.Handler(u.store, u.plugin, share.WithAttemptLimiter(l))
	with := func(token, pw string) int {
		return serve(h, http.MethodGet, "/"+token, nil, http.Header{"Share-Password": {pw}}).Code
	}
	for range 3 {
		if code := with(sh.Token, "pw"); code != http.StatusFound {
			t.Fatalf("the right password = %d", code)
		}
	}
	if a, b, c := with(sh.Token, "x"), with(sh.Token, "y"), with(sh.Token, "pw"); a != 401 || b != 401 || c != http.StatusTooManyRequests {
		t.Fatalf("attempts = %d %d %d", a, b, c)
	}
	if l.held != 2 || len(l.keys) != 6 || slices.ContainsFunc(l.keys, func(k string) bool { return k != sh.ID }) {
		t.Fatalf("held %d, asked for %q; want the two wrong ones held, every key %q", l.held, l.keys, sh.ID)
	}
	if n := u.uses(t, sh); n != 3 {
		t.Fatalf("uses = %d, want the 3 right ones", n)
	}
	if code := with("no-such-token", "x"); code != http.StatusNotFound || len(l.keys) != 6 {
		t.Fatalf("unknown token = %d, limiter asked %d times", code, len(l.keys))
	}

	// The default limiter: past DefaultAttempts wrong passwords the right
	// one is refused too; any number of right ones before is fine.
	sh2, err := u.plugin.Create(ctx, u.store, "t", u.file(t, "d.txt").ID, share.Options{TTL: time.Hour, Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	h = share.Handler(u.store, u.plugin)
	for range share.DefaultAttempts + 2 {
		if code := serve(h, http.MethodGet, "/"+sh2.Token, nil, http.Header{"Share-Password": {"pw"}}).Code; code != http.StatusFound {
			t.Fatalf("the right password under the default limiter = %d", code)
		}
	}
	for range share.DefaultAttempts {
		if code := serve(h, http.MethodGet, "/"+sh2.Token, nil, http.Header{"Share-Password": {"no"}}).Code; code != http.StatusUnauthorized {
			t.Fatalf("a wrong password = %d", code)
		}
	}
	if code := serve(h, http.MethodGet, "/"+sh2.Token, nil, http.Header{"Share-Password": {"pw"}}).Code; code != http.StatusTooManyRequests {
		t.Fatalf("past the allowance = %d", code)
	}

	dl := share.NewAttemptLimiter(2, 50*time.Millisecond)
	// Each Allow takes an attempt, so the calls run one by one, in order.
	first, second, third, other := dl.Allow("k"), dl.Allow("k"), dl.Allow("k"), dl.Allow("other")
	if !first || !second || third || !other {
		t.Fatalf("the default limiter's allowance: k %v %v %v, other %v", first, second, third, other)
	}
	dl.Release("k")
	again, beyond := dl.Allow("k"), dl.Allow("k")
	if !again || beyond {
		t.Fatalf("a released attempt was not given back exactly once: %v %v", again, beyond)
	}
	time.Sleep(60 * time.Millisecond)
	if !dl.Allow("k") {
		t.Fatal("the window did not reset")
	}
}

// TestDepositReceivesFirst: Deposit reads the whole body before it takes a
// database connection, so a slow uploader holds none.
func TestDepositReceivesFirst(t *testing.T) {
	ctx := t.Context()
	u := newSuite(t, 0)
	post := records.Owner{Type: "post", ID: "p1"}
	sh, err := u.plugin.CreateRequest(ctx, u.store, "t", share.Target{Kind: share.KindOwner, Ref: share.OwnerRef(post, "inbox")}, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	body := &poolWatcher{db: u.db, r: strings.NewReader("slow bytes")}
	if _, err := u.plugin.Deposit(ctx, u.store, sh.Token, "media", body, "slow.txt", anyText); err != nil {
		t.Fatal(err)
	}
	if body.reads == 0 || body.busy != 0 {
		t.Fatalf("reads %d, with a connection in use during %d", body.reads, body.busy)
	}
	if n := u.uses(t, sh); n != 1 {
		t.Fatalf("uses = %d", n)
	}
}

// poolWatcher counts its reads made while db had a connection in use.
type poolWatcher struct {
	db          *sql.DB
	r           io.Reader
	reads, busy int
}

func (p *poolWatcher) Read(b []byte) (int, error) {
	p.reads++
	if p.db.Stats().InUse > 0 {
		p.busy++
	}
	return p.r.Read(b)
}

// TestFolderLink: a folder link lists and serves the folder's live files
// at each request: a file added later is included, a trashed one and one
// outside the folder are not, and each download counts.
func TestFolderLink(t *testing.T) {
	ctx := t.Context()
	u := newSuite(t, 0)
	dir, err := u.folders.Create(ctx, u.store, "t", folders.RootID, "Reports")
	if err != nil {
		t.Fatal(err)
	}
	put := func(f records.File) {
		if err := u.folders.Put(ctx, u.store, "t", f.ID, dir.ID); err != nil {
			t.Fatal(err)
		}
	}
	a := u.file(t, "a.txt")
	put(a)
	outside := u.file(t, "outside.txt")
	sh, err := u.plugin.CreateFor(ctx, u.store, "t", share.Target{Kind: folders.KindFolder, Ref: dir.ID}, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	later := u.file(t, "later.txt")
	put(later)
	gone := u.file(t, "gone.txt")
	put(gone)
	if err := u.store.Trash(ctx, "t", gone.ID); err != nil {
		t.Fatal(err)
	}
	h := share.Handler(u.store, u.plugin)
	rec := serve(h, http.MethodGet, "/"+sh.Token, nil, nil)
	var listed []share.ListedFile
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || rec.Code != 200 {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	names := map[string]bool{}
	for _, l := range listed {
		names[l.Filename] = true
	}
	if len(listed) != 2 || !names["a.txt"] || !names["later.txt"] {
		t.Fatalf("listed %v", names)
	}
	if rec := serve(h, http.MethodGet, "/"+sh.Token+"/"+later.ID, nil, nil); rec.Code != http.StatusFound {
		t.Fatalf("download of a file added later = %d", rec.Code)
	}
	for _, id := range []string{outside.ID, gone.ID, "fil_unknown"} {
		if rec := serve(h, http.MethodGet, "/"+sh.Token+"/"+id, nil, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("download of %s = %d", id, rec.Code)
		}
	}
	if n := u.uses(t, sh); n != 1 {
		t.Fatalf("uses = %d, want 1", n)
	}
	if _, err := u.plugin.CreateFor(ctx, u.store, "t", share.Target{Kind: folders.KindFolder, Ref: folders.RootID}, share.Options{TTL: time.Hour}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("sharing the root = %v", err)
	}
	if _, err := u.plugin.CreateFor(ctx, u.store, "t", share.Target{Kind: "album", Ref: "x"}, share.Options{TTL: time.Hour}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("an uninstalled collection = %v", err)
	}
}

// TestOwnerLinkAndStats: a link to an owner's role serves its files; the
// stats record each download with the visitor when known.
func TestOwnerLinkAndStats(t *testing.T) {
	ctx := t.Context()
	u := newSuite(t, 0)
	post := records.Owner{Type: "post", ID: "p/1"}
	f := u.file(t, "photo.txt")
	if _, err := u.store.Attach(ctx, "t", f.ID, post, "gallery"); err != nil {
		t.Fatal(err)
	}
	other := u.file(t, "other.txt")
	sh, err := u.plugin.CreateFor(ctx, u.store, "t", share.Target{Kind: share.KindOwner, Ref: share.OwnerRef(post, "gallery")}, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	files, _, err := u.plugin.List(ctx, u.store, sh.Token)
	if err != nil || len(files) != 1 || files[0].ID != f.ID {
		t.Fatalf("List = %v, %v", files, err)
	}
	if _, err := u.plugin.ResolveFile(records.WithActor(ctx, "user:bob"), u.store, sh.Token, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := u.plugin.ResolveFile(ctx, u.store, sh.Token, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := u.plugin.ResolveFile(ctx, u.store, sh.Token, other.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a file outside the role = %v", err)
	}
	st, err := u.plugin.Stats(ctx, u.store, "t", sh.ID, 10)
	if err != nil || st.Uses != 2 || len(st.Recent) != 2 || st.Recent[1].Viewer != "user:bob" || st.Recent[0].Viewer != "" {
		t.Fatalf("Stats = %+v, %v", st, err)
	}
	if _, err := u.plugin.Prune(ctx, u.store, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st, _ := u.plugin.Stats(ctx, u.store, "t", sh.ID, 10); st.Uses != 0 {
		t.Fatalf("records outlived their pruned link: %+v", st)
	}
}

// TestRequestLink: a request link accepts uploads into its target under
// the Handler's policy, as the scope's quota allows, counts them, and
// lists and serves nothing.
func TestRequestLink(t *testing.T) {
	ctx := t.Context()
	u := newSuite(t, 40)
	post := records.Owner{Type: "post", ID: "p1"}
	sh, err := u.plugin.CreateRequest(ctx, u.store, "t", share.Target{Kind: share.KindOwner, Ref: share.OwnerRef(post, "inbox")}, share.Options{TTL: time.Hour, MaxDownloads: 2})
	if err != nil {
		t.Fatal(err)
	}
	h := share.Handler(u.store, u.plugin, share.WithUploads("media", anyText))
	send := func(h http.Handler, body string) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", "doc.txt")
		_, _ = fw.Write([]byte(body))
		_ = mw.Close()
		return serve(h, http.MethodPost, "/"+sh.Token, &buf, http.Header{"Content-Type": {mw.FormDataContentType()}})
	}
	if rec := send(share.Handler(u.store, u.plugin), "x"); rec.Code != http.StatusNotFound {
		t.Fatalf("upload without WithUploads = %d", rec.Code)
	}
	if rec := send(h, strings.Repeat("a", 30)); rec.Code != http.StatusCreated {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body)
	}
	if rec := send(h, strings.Repeat("b", 30)); rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("upload past the scope's quota = %d", rec.Code)
	}
	if rec := send(h, "c"); rec.Code != http.StatusCreated {
		t.Fatalf("second upload = %d", rec.Code)
	}
	if rec := send(h, "d"); rec.Code != http.StatusNotFound {
		t.Fatalf("upload past MaxDownloads = %d", rec.Code)
	}
	attached, err := u.store.Attachments(ctx, "t", post, "inbox")
	if err != nil || len(attached) != 2 || attached[0].File.UploadedBy != "share:"+sh.ID {
		t.Fatalf("attached = %+v, %v", attached, err)
	}
	if rec := serve(h, http.MethodGet, "/"+sh.Token, nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("GET of a request link = %d", rec.Code)
	}
	if _, _, err := u.plugin.List(ctx, u.store, sh.Token); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("List of a request link = %v", err)
	}
}

// sqlbSelectHash reads a link's stored password hash.
func sqlbSelectHash(s *records.Store, id string, dst *string) records.Statement {
	return sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(share.PasswordHash, dst)).From(share.Table).Where(sqlb.Eq(share.ID, id))
}

// TestDepositNeedsThePlugin: Deposit through a Plugin the Store does not
// have installed fails and leaves no file, rather than recording one that
// nothing placed or counted.
func TestDepositNeedsThePlugin(t *testing.T) {
	ctx := t.Context()
	u := newSuite(t, 0)
	post := records.Owner{Type: "post", ID: "p1"}
	sh, err := u.plugin.CreateRequest(ctx, u.store, "t", share.Target{Kind: share.KindOwner, Ref: share.OwnerRef(post, "inbox")}, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// A second Store on the same database, without the plugin.
	mgr, _ := filesystem.NewManager(u.disk)
	bare, err := records.New(u.db, testdb.Dialect(), mgr, records.WithOwnerTypes("post"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.plugin.Deposit(ctx, bare, sh.Token, "media", strings.NewReader("x"), "x.txt", anyText); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("Deposit through an uninstalled plugin = %v", err)
	}
	if files, err := u.store.Files(ctx, records.FileQuery{Scope: "t"}); err != nil || len(files.Files) != 0 {
		t.Fatalf("files left = %+v, %v", files, err)
	}
}

// TestParallelGuessesLimited: many wrong passwords sent at once are
// checked at most the limiter's allowance times; the rest are 429.
func TestParallelGuessesLimited(t *testing.T) {
	ctx := t.Context()
	u := newSuite(t, 0)
	sh, err := u.plugin.Create(ctx, u.store, "t", u.file(t, "p.txt").ID, share.Options{TTL: time.Hour, Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	h := share.Handler(u.store, u.plugin)
	const guesses = 4 * share.DefaultAttempts
	codes := make(chan int, guesses)
	var wg sync.WaitGroup
	for range guesses {
		wg.Go(func() {
			codes <- serve(h, http.MethodGet, "/"+sh.Token, nil, http.Header{"Share-Password": {"wrong"}}).Code
		})
	}
	wg.Wait()
	close(codes)
	checked := 0
	for code := range codes {
		switch code {
		case http.StatusUnauthorized:
			checked++
		case http.StatusTooManyRequests:
		default:
			t.Fatalf("a guess answered %d", code)
		}
	}
	if checked == 0 || checked > share.DefaultAttempts {
		t.Fatalf("%d passwords checked, allowance %d", checked, share.DefaultAttempts)
	}
}
