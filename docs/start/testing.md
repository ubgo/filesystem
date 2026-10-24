# Test your app

You will have fast, offline tests of your own code that stores files: a fake disk you can assert on, the memory driver behind the same DSNs production uses, the records store on a SQLite file, backend failures injected on purpose, a clock you move instead of sleeping, and, if you write a driver, the conformance suite every built-in driver passes. None of it needs a network, a bucket or Docker.

The code under test and its tests are [examples/start-testing](../../examples/start-testing): a small avatar service built on a `Disk`, and a gallery built on `records`. Every snippet below is copied from them.

## Before you start

- Code that receives its storage instead of building it: a `*filesystem.Disk`, a `*filesystem.Manager` or a `*records.Store` passed in as a parameter or a field. That is all a test needs to put a fake in its place.
- For the records tests, `modernc.org/sqlite` in your module ([install](install.md), step 4).

## Step 1 — A fake disk and its assertions

The service under test stores one avatar per user. It holds a `*filesystem.Disk` and nothing else:

<!-- ds:block id=testing-avatars-fkc7929x -->
**Avatars** · [`examples/start-testing/main.go:52-54`](../../examples/start-testing/main.go#L52-L54)

```go
type Avatars struct {
	disk *filesystem.Disk
}
```
<!-- /ds:block hash=5fae23 -->

<!-- ds:block id=testing-avatarpolicy-kycd9dze -->
**avatarPolicy** · [`examples/start-testing/main.go:64-70`](../../examples/start-testing/main.go#L64-L70)

```go
func avatarPolicy(userID string) upload.Policy {
	return upload.Policy{
		MaxSize: maxAvatarSize,
		Allowed: []string{"image/png", "image/jpeg", "image/gif"},
		Key:     func(upload.Info) (string, error) { return avatarDir + "/" + userID, nil },
	}
}
```
<!-- /ds:block hash=7f0c0f -->

<!-- ds:block id=testing-save-m55jh9c4 -->
**Avatars.Save** · [`examples/start-testing/main.go:75-81`](../../examples/start-testing/main.go#L75-L81)

```go
func (a *Avatars) Save(ctx context.Context, userID string, body io.Reader) (string, error) {
	res, err := upload.Receive(ctx, a.disk, body, "avatar", avatarPolicy(userID))
	if err != nil {
		return "", err
	}
	return res.Key, nil
}
```
<!-- /ds:block hash=b805c3 -->

`filesystemtest.NewFake` returns an empty in-memory disk with assertions. It embeds a real `*filesystem.Disk`, so `disk.Disk` goes wherever your code takes one, and every behaviour of the library (key cleaning, content sniffing, size caps, typed errors) runs exactly as in production:

<!-- ds:block id=testing-avatarsaved-fx8nkktf -->
**TestAvatarSaved** · [`examples/start-testing/main_test.go:32-41`](../../examples/start-testing/main_test.go#L32-L41)

```go
func TestAvatarSaved(t *testing.T) {
	disk := filesystemtest.NewFake(t, "media")
	key, err := NewAvatars(disk.Disk).Save(t.Context(), "u1", strings.NewReader(gif))
	if err != nil {
		t.Fatal(err)
	}
	disk.AssertExists(key)
	disk.AssertContent("avatars/u1", gif)
	disk.AssertCount("avatars", 1)
}
```
<!-- /ds:block hash=2ce3f9 -->

| Assertion | Fails the test unless |
|---|---|
| `AssertExists(key)` | `key` exists |
| `AssertMissing(key)` | `key` does not exist |
| `AssertContent(key, want)` | `key` holds exactly `want` |
| `AssertCount(dir, n)` | `dir` holds exactly `n` objects, counted recursively |
| `AssertEmpty()` | the whole disk is empty |

`NewFake` takes the same options as `NewDisk`, so a test can give the fake a URL signer, a default visibility or interceptors (step 4). A refusal is checked by its code, with `errors.Is` against the `filesystem.Err…` sentinels, never by its message, which may change:

<!-- ds:block id=testing-avatarrefused-pm2dhgbn -->
**TestAvatarRefused** · [`examples/start-testing/main_test.go:46-53`](../../examples/start-testing/main_test.go#L46-L53)

```go
func TestAvatarRefused(t *testing.T) {
	disk := filesystemtest.NewFake(t, "media")
	_, err := NewAvatars(disk.Disk).Save(t.Context(), "u1", strings.NewReader("<svg onload=alert(1)>"))
	if !errors.Is(err, filesystem.ErrRejected) {
		t.Fatalf("err = %v, want CodeRejected", err)
	}
	disk.AssertEmpty()
}
```
<!-- /ds:block hash=207af0 -->

Check: `go test ./start-testing -run Avatar` passes, and `AssertEmpty` proves the refused SVG never reached storage.

## Step 2 — The memory driver behind a DSN

The fake is the memory driver plus assertions. When the code under test opens its disks from configuration, open them the same way in the test, with a `mem://` DSN. Then the DSN's parameters are under test too; here `readonly=true` turns the upload into a `CodeReadOnly` error:

<!-- ds:block id=testing-fromdsn-jyf9ghyk -->
**TestFromDSN** · [`examples/start-testing/main_test.go:73-85`](../../examples/start-testing/main_test.go#L73-L85)

```go
func TestFromDSN(t *testing.T) {
	mux := filesystem.NewMux()
	if err := mux.Register(memory.Scheme, memory.Opener); err != nil {
		t.Fatal(err)
	}
	disk, err := mux.Open(t.Context(), "media", "mem://?readonly=true")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewAvatars(disk).Save(t.Context(), "u1", strings.NewReader(gif)); !errors.Is(err, filesystem.ErrReadOnly) {
		t.Fatalf("err = %v, want CodeReadOnly", err)
	}
}
```
<!-- /ds:block hash=7df38c -->

The memory driver has every capability that needs no network: server-side copy, atomic move, per-object visibility, stored metadata, content types and headers, conditional writes and multipart. That makes it a good stand-in for S3, with three differences to keep in mind: it cannot sign URLs itself (give the disk a `signedurl` signer, as a local disk needs), nothing survives the process, and `mem://` opens a new, empty store every time, so two `Open` calls never share objects. `memory.WithClock` fixes modification times when a test asserts on them.

## Step 3 — Records on SQLite

Code built on `records` is tested against a real database, because the store's guarantees (transactions, unique single roles, quota charges) live in SQL. SQLite needs no server, and a file in the test's temporary directory makes each test start empty:

<!-- ds:block id=testing-newstore-6vkdpbv5 -->
**newStore** · [`examples/start-testing/main_test.go:92-111`](../../examples/start-testing/main_test.go#L92-L111)

```go
func newStore(t *testing.T, disk *filesystem.Disk, opts ...records.Option) *records.Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		t.Fatal(err)
	}
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, append([]records.Option{records.WithOwnerTypes(ownerUser)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return store
}
```
<!-- /ds:block hash=bedc61 -->

Give the store a `Manager` holding the fake, and you can assert on rows and bytes in the same test. The gallery under test uploads a photo and attaches it to the user in one transaction:

<!-- ds:block id=testing-galleryadd-vx94ngb9 -->
**Gallery.Add** · [`examples/start-testing/main.go:106-117`](../../examples/start-testing/main.go#L106-L117)

```go
func (g *Gallery) Add(ctx context.Context, userID string, body io.Reader, filename string) (records.File, error) {
	var f records.File
	err := g.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var err error
		if f, err = g.store.Upload(ctx, userID, mediaDisk, body, filename, photoPolicy()); err != nil {
			return err
		}
		_, err = g.store.Attach(ctx, userID, f.ID, records.Owner{Type: ownerUser, ID: userID}, rolePhotos)
		return err
	})
	return f, err
}
```
<!-- /ds:block hash=90ae0f -->

<!-- ds:block id=testing-gallery-x5rhck7j -->
**TestGallery** · [`examples/start-testing/main_test.go:116-129`](../../examples/start-testing/main_test.go#L116-L129)

```go
func TestGallery(t *testing.T) {
	disk := filesystemtest.NewFake(t, mediaDisk)
	gallery := NewGallery(newStore(t, disk.Disk))
	for _, name := range []string{"a.gif", "b.gif"} {
		if _, err := gallery.Add(t.Context(), "u1", strings.NewReader(gif), name); err != nil {
			t.Fatal(err)
		}
	}
	photos, err := gallery.Photos(t.Context(), "u1")
	if err != nil || len(photos) != 2 || photos[0].File.Filename != "a.gif" {
		t.Fatalf("photos = %+v, %v", photos, err)
	}
	disk.AssertCount(rolePhotos, 2)
}
```
<!-- /ds:block hash=369040 -->

Use a file, not `:memory:`: `database/sql` opens more than one connection as soon as two statements overlap (a transaction and a read), and each connection to `:memory:` is a separate, empty database, so the next statement fails with `no such table: fs_files`. The library's own suite runs on SQLite, Postgres and MySQL (`task test:engines`); if your production database is Postgres, run your records tests against it as well before a release, through the same helper with a different `sql.Open`.

## Step 4 — Inject failures

The paths that matter most are the ones a laptop never takes: the database refuses a row, the backend times out, a retry happens. Interceptors make every one of them reproducible, without a mock of any library type.

**The database step fails.** The store writes the object first and the row second; if the row fails, the object must go. A `quota` plugin limited to one file is a realistic way to make the second upload fail inside its transaction, and the fake proves the object was deleted again:

<!-- ds:block id=testing-rollback-w3zc6n3s -->
**TestRolledBackUploadLeavesNoBytes** · [`examples/start-testing/main_test.go:134-148`](../../examples/start-testing/main_test.go#L134-L148)

```go
func TestRolledBackUploadLeavesNoBytes(t *testing.T) {
	disk := filesystemtest.NewFake(t, mediaDisk)
	oneFile, err := quota.New(func(context.Context, string) (quota.Limit, error) { return quota.Limit{Files: 1}, nil })
	if err != nil {
		t.Fatal(err)
	}
	gallery := NewGallery(newStore(t, disk.Disk, records.WithPlugins(oneFile)))
	if _, err := gallery.Add(t.Context(), "u1", strings.NewReader(gif), "a.gif"); err != nil {
		t.Fatal(err)
	}
	if _, err := gallery.Add(t.Context(), "u1", strings.NewReader(gif), "b.gif"); !errors.Is(err, filesystem.ErrQuotaExceeded) {
		t.Fatalf("err = %v, want CodeQuotaExceeded", err)
	}
	disk.AssertCount(rolePhotos, 1)
}
```
<!-- /ds:block hash=bebf1e -->

**The backend has an outage.** An interceptor that fails the first operations of a kind with `CodeUnavailable`, the code drivers give transient failures, and never reaches the driver:

<!-- ds:block id=testing-failfirst-gg62jgzr -->
**failFirst** · [`examples/start-testing/main_test.go:154-168`](../../examples/start-testing/main_test.go#L154-L168)

```go
func failFirst(kind filesystem.OpKind, n int) filesystem.Interceptor {
	var mu sync.Mutex
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		mu.Lock()
		fail := op.Kind == kind && n > 0
		if fail {
			n--
		}
		mu.Unlock()
		if fail {
			return filesystem.Errorf(filesystem.CodeUnavailable, "injected outage")
		}
		return call(ctx)
	}
}
```
<!-- /ds:block hash=fecea7 -->

Put it inside a `layer.Retry` and the test shows both sides of the retry rule. A write of bytes can be rewound, so it is retried and lands. An upload arriving as a stream cannot be replayed, so it is not retried: it fails with `CodeUnavailable` and stores nothing. `RetryOptions.Sleep` replaces the backoff timer, so the test does not wait:

<!-- ds:block id=testing-retry-5yd7p44f -->
**TestRetryRidesOutOutage** · [`examples/start-testing/main_test.go:177-195`](../../examples/start-testing/main_test.go#L177-L195)

```go
func TestRetryRidesOutOutage(t *testing.T) {
	disk := filesystemtest.NewFake(t, "media", filesystem.WithInterceptors(
		layer.Retry(layer.RetryOptions{Attempts: 3, Sleep: noSleep}),
		failFirst(filesystem.OpWrite, 2),
	))
	if _, err := disk.WriteBytes(t.Context(), "a.txt", []byte("a"), filesystem.WriteOptions{}); err != nil {
		t.Fatalf("retried write: %v", err)
	}
	disk.AssertContent("a.txt", "a")

	streamed := filesystemtest.NewFake(t, "media", filesystem.WithInterceptors(
		layer.Retry(layer.RetryOptions{Attempts: 3, Sleep: noSleep}),
		failFirst(filesystem.OpWrite, 1),
	))
	if _, err := NewAvatars(streamed.Disk).Save(t.Context(), "u1", bytes.NewReader([]byte(gif))); !errors.Is(err, filesystem.ErrUnavailable) {
		t.Fatalf("streamed upload: err = %v, want CodeUnavailable", err)
	}
	streamed.AssertEmpty()
}
```
<!-- /ds:block hash=45863e -->

**A backend that stays down.** `layer.Guard` runs a check before every operation and fails it with the check's error. Return a `*filesystem.Error` with the code you want to simulate; any other error is reported as `CodePermission`:

<!-- ds:block id=testing-guard-6us897vb -->
**TestReadOutage** · [`examples/start-testing/main_test.go:200-214`](../../examples/start-testing/main_test.go#L200-L214)

```go
func TestReadOutage(t *testing.T) {
	down := layer.Guard(func(_ context.Context, op *filesystem.Op) error {
		if op.Kind == filesystem.OpRead {
			return filesystem.Errorf(filesystem.CodeUnavailable, "injected outage")
		}
		return nil
	})
	disk := filesystemtest.NewFake(t, "media", filesystem.WithInterceptors(down))
	if _, err := disk.WriteBytes(t.Context(), "a.txt", []byte("a"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := disk.ReadAll(t.Context(), "a.txt"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("err = %v", err)
	}
}
```
<!-- /ds:block hash=dc38ab -->

More faults worth a test in a real app, all built the same way:

- Fail `filesystem.OpDelete` after a purge commits, and check your `records.WithObjectErrorHandler` hears about the left-behind object.
- Fail `filesystem.OpRead` with `CodeNotFound`, and check your download handler answers `404` rather than `500` (`fshttp.StatusOf` maps it).
- Hand `upload.Policy.Inspect` a `scan.ScannerFunc` that reports an infection, or returns an error as a scanner that is down would, and check the upload is refused and nothing stays stored. The [production](production.md) example's `TestScanning` does exactly this.
- Return `filesystem.Errorf(filesystem.CodeQuotaExceeded, …)` from a `layer.Guard`, and check your UI path shows the right message.

## Step 5 — Move the clock instead of sleeping

Anything with a retention period or an expiry takes a clock. A small movable one is enough:

<!-- ds:block id=testing-clock-q9v8qeg7 -->
**clock** · [`examples/start-testing/main_test.go:218-221`](../../examples/start-testing/main_test.go#L218-L221)

```go
type clock struct {
	mu  sync.Mutex
	now time.Time
}
```
<!-- /ds:block hash=6e4372 -->

`records.WithClock` makes the store stamp trash times from it, so a sweep test runs in milliseconds:

<!-- ds:block id=testing-trash-fsxm6286 -->
**TestTrashRetention** · [`examples/start-testing/main_test.go:232-249`](../../examples/start-testing/main_test.go#L232-L249)

```go
func TestTrashRetention(t *testing.T) {
	const retention = 30 * 24 * time.Hour
	c := &clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	disk := filesystemtest.NewFake(t, mediaDisk)
	store := newStore(t, disk.Disk, records.WithClock(c.Now))
	f, err := NewGallery(store).Add(t.Context(), "u1", strings.NewReader(gif), "a.gif")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Trash(t.Context(), "u1", f.ID); err != nil {
		t.Fatal(err)
	}
	c.Advance(retention + 24*time.Hour)
	if n, err := store.SweepTrash(t.Context(), c.Now().Add(-retention)); err != nil || n != 1 {
		t.Fatalf("swept %d, %v", n, err)
	}
	disk.AssertEmpty()
}
```
<!-- /ds:block hash=f3cee9 -->

The records plugins (acl, folders, versions, locks, share, quota) and the `recordshttp` and share handlers follow the Store's clock unless given their own (`quota.WithClock`, `share.NewAttemptLimiterClock`), so one fake clock moves them all. The other clocks: `memory.WithClock` sets modification times (what `upload.Sweep` compares), `signedurl.WithClock` lets a test sign a link, move past its expiry and see `fshttp.Signed` answer `403`, `CacheOptions.Now` ages cached copies, `upload.ChunkedOptions.Now` and `upload.FetchOptions.Now` drive chunk takeover and `Retry-After` dates, and `layer.BandwidthOptions{Now, Sleep}` throttles without waiting.

## Step 6 — HTTP handlers

Handlers built on the library are tested with `net/http/httptest` like any other. Start the app behind an `httptest.Server` and use the server's URL as the base of signed links, so a redirect to a link lands back on the test server. The [first app](first-app.md)'s test does this end to end, from upload to purge:

<!-- ds:block id=quickstart-testserver-6kckt4sx -->
**testServer** · [`examples/quickstart/main_test.go:28-40`](../../examples/quickstart/main_test.go#L28-L40)

```go
func testServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	a, err := newApp(t.Context(), config{BaseURL: srv.URL, DataDir: dir, Secret: bytes.Repeat([]byte("s"), signedurl.MinKeyBytes)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	srv.Config.Handler = a.Handler()
	return srv, dir
}
```
<!-- /ds:block hash=e39512 -->

`upload.Handler`, `fshttp.Signed`, `fshttp.Public` and `recordshttp.Handler` are plain `http.Handler`s, so `httptest.NewRecorder` works for single requests too.

`filesystemtest` builds the requests and files an upload test needs, so a test does not hand-assemble multipart bodies or ship binary fixtures: `NewUploadRequest` makes the `multipart/form-data` POST a browser sends, `FakeImage` a real, decodable PNG, JPEG or GIF of the size you ask for, `FakeBytes` arbitrary bytes of no recognisable type, and `PadTo` an image padded to an exact byte count that still decodes.

The avatar service gets an HTTP handler: it reads the avatar from a form field, saves it through `Save`, and answers errors with the status and client-safe message `fshttp` derives from their code:

<!-- ds:block id=testing-servehttp-57hnvr2r -->
**Avatars.ServeHTTP** · [`examples/start-testing/http.go:26-45`](../../examples/start-testing/http.go#L26-L45)

```go
func (a *Avatars) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user := r.Header.Get(headerUser)
	if user == "" {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAvatarSize+formOverhead)
	file, _, err := r.FormFile(avatarField)
	if err != nil {
		http.Error(w, "send the image in the "+avatarField+" field", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()
	key, err := a.Save(r.Context(), user, file)
	if err != nil {
		http.Error(w, fshttp.ClientMessage(err), fshttp.StatusOf(err))
		return
	}
	_, _ = fmt.Fprintln(w, key)
}
```
<!-- /ds:block hash=b11fe5 -->

One helper posts a file through it as a browser would:

<!-- ds:block id=testing-postavatar-2tdsmydn -->
**postAvatar** · [`examples/start-testing/http_test.go:14-21`](../../examples/start-testing/http_test.go#L14-L21)

```go
func postAvatar(t *testing.T, a *Avatars, user string, file filesystemtest.UploadFile) *httptest.ResponseRecorder {
	t.Helper()
	req := filesystemtest.NewUploadRequest(t, "/avatar", nil, file)
	req.Header.Set(headerUser, user)
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	return rec
}
```
<!-- /ds:block hash=aacb90 -->

The happy path needs a real image, because the policy detects the type from the bytes; `FakeImage` draws one in code:

<!-- ds:block id=testing-uploadoverhttp-e85bbpak -->
**TestAvatarUploadOverHTTP** · [`examples/start-testing/http_test.go:26-34`](../../examples/start-testing/http_test.go#L26-L34)

```go
func TestAvatarUploadOverHTTP(t *testing.T) {
	disk := filesystemtest.NewFake(t, mediaDisk)
	img := filesystemtest.FakeImage(t, filesystemtest.PNG, 64, 64)
	rec := postAvatar(t, NewAvatars(disk.Disk), "u1", filesystemtest.UploadFile{Field: avatarField, Name: "me.png", Content: img})
	if rec.Code != http.StatusOK || rec.Body.String() != "avatars/u1\n" {
		t.Fatalf("got %d %q", rec.Code, rec.Body)
	}
	disk.AssertExists("avatars/u1")
}
```
<!-- /ds:block hash=e435e6 -->

Each refusal gets its status, and none of them leaves anything stored. `PadTo` makes a JPEG exactly one byte over the 2 MiB limit, which still decodes, so the test proves the size check rather than the type check; `FakeBytes` is never detected as an image:

<!-- ds:block id=testing-refusalsoverhttp-eauurr9s -->
**TestAvatarRefusalsOverHTTP** · [`examples/start-testing/http_test.go:39-56`](../../examples/start-testing/http_test.go#L39-L56)

```go
func TestAvatarRefusalsOverHTTP(t *testing.T) {
	disk := filesystemtest.NewFake(t, mediaDisk)
	a := NewAvatars(disk.Disk)
	tooBig := filesystemtest.PadTo(t, filesystemtest.FakeImage(t, filesystemtest.JPEG, 32, 32), maxAvatarSize+1)
	for name, tc := range map[string]struct {
		file filesystemtest.UploadFile
		want int
	}{
		"one byte over the limit": {filesystemtest.UploadFile{Field: avatarField, Name: "big.jpg", Content: tooBig}, http.StatusRequestEntityTooLarge},
		"not an image":            {filesystemtest.UploadFile{Field: avatarField, Name: "me.png", Content: filesystemtest.FakeBytes(1024)}, http.StatusUnsupportedMediaType},
		"wrong field":             {filesystemtest.UploadFile{Field: "file", Name: "me.png", Content: filesystemtest.FakeImage(t, filesystemtest.GIF, 8, 8)}, http.StatusBadRequest},
	} {
		if rec := postAvatar(t, a, "u1", tc.file); rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", name, rec.Code, tc.want, rec.Body)
		}
	}
	disk.AssertEmpty()
}
```
<!-- /ds:block hash=272bd7 -->

Check: `go test -run OverHTTP -v ./start-testing` in `examples/` prints `--- PASS` for both tests. The [filesystemtest README](../../filesystemtest/README.md) lists every builder.

### Golden image tests

Code that resizes, crops or watermarks images is tested by comparing its output with a reviewed PNG kept in `testdata/`. `imagingtest.Golden(t, got, path)` from `contrib/imaging/imagingtest` compares perceptually within a tolerance, so a re-encode that changes no visible pixel passes, saves the differing image for inspection when it fails, and writes the golden file when the test runs with `-update`. [Image transforms, step 9](../plugins/image-transforms.md#step-9--test-transforms-with-golden-images) builds one, run by [imaging-transforms](../../examples/imaging-transforms).

## Step 7 — The conformance suite, for a driver of your own

If you write a driver (for a backend the library does not cover, or a wrapper of your own), run the conformance suite against it. It is the same suite every built-in driver passes, and its own tests prove each case catches the bug it is meant to catch. The factory returns a new, empty disk for each case:

<!-- ds:block id=testing-conformance-584pkj8j -->
**TestConformance** · [`examples/start-testing/main_test.go:254-267`](../../examples/start-testing/main_test.go#L254-L267)

```go
func TestConformance(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		drv, err := local.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		disk, err := filesystem.NewDisk("conformance", drv)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = disk.Close() })
		return disk
	})
}
```
<!-- /ds:block hash=15957c -->

Here the factory builds a local disk in place of yours. The suite checks the driver contract and the `Disk`'s portable logic together: write then read, overwrite, empty and large objects, a failing body or an oversized one storing nothing, Unicode keys, not-found errors, ranges, listing order, pagination and directories, copy, move, prefix and batch deletes, concurrent writes, conditional writes, visibility and multipart. Cases for a capability the driver does not declare are skipped, and fail when it declares one it cannot deliver; on the local driver 33 cases pass and the three about stored content types, metadata and headers are skipped. `filesystemtest.Cases()` returns the cases, if you need to run them your own way. [Write a driver](../extending/write-a-driver.md) walks through a driver from the interface to a passing suite.

## What can go wrong

| Symptom | Fix |
|---|---|
| `no such table: fs_files` in a records test | The database is `:memory:`, or `Migrate` was not run. Use a file in `t.TempDir()` and migrate in the helper. |
| A refusal test passes although nothing was refused | It checked only `err != nil`. Check the code with `errors.Is(err, filesystem.ErrRejected)` and assert on the disk (`AssertEmpty`). |
| An injected failure is never retried | The body is a stream, which Retry never replays, or the code is not `CodeUnavailable`, the only code Retry retries by default (`RetryOptions.Retryable` changes that). |
| A retry test takes seconds | Pass `RetryOptions.Sleep` that returns at once. |
| Two `mem://` disks do not see each other's objects | Each `Open` of `mem://` is a new store. Open it once and share the `Disk`. |
| A signed link test gets `403` | The link was signed for another disk name, method or base URL, or it expired. Sign with the test server's URL and a clock under your control. |

## Next

- [Production](production.md): the same wiring, from the environment.
- [Errors](../concepts/errors.md): every code and its sentinel.
- [Interceptors](../concepts/interceptors.md) and [write an interceptor](../extending/write-an-interceptor.md): the mechanism the fault injection uses.
- [filesystemtest reference](../../filesystemtest/README.md).
