# Testing code that stores files

The application code behind the testing guide, and its tests. `main.go` is a small avatar service and a gallery on the records layer; `main_test.go` is the interesting file. It tests that code with a `Fake` disk that records what was stored, a memory disk opened from a DSN, records on SQLite, injected faults (failed writes under a retry, a backend that is down), a movable clock for trash retention, and the conformance suite a custom driver should pass. Nothing needs a network, a container or a cloud account. It uses `github.com/ubgo/filesystem`, `filesystemtest`, `layer`, `memory`, `upload` and `records`. The guide [Test your app](../../docs/start/testing.md) explains each test.

## Run it

```sh
cd examples
go test ./start-testing
go test -v ./start-testing     # one line per test
go run ./start-testing         # the service once, on a memory disk
```

The tests create SQLite files in Go's per-test temporary directories, which `go test` deletes. The program writes nothing to disk.

## What it does

```sh
go test -count=1 -v ./start-testing
```

The top-level results (the conformance suite's subtests are left out here):

```text
--- PASS: TestAvatarUploadOverHTTP (0.00s)
--- PASS: TestAvatarRefusalsOverHTTP (0.01s)
--- PASS: TestAvatarSaved (0.00s)
--- PASS: TestAvatarRefused (0.00s)
--- PASS: TestAvatarRemoved (0.00s)
--- PASS: TestFromDSN (0.00s)
--- PASS: TestGallery (0.01s)
--- PASS: TestRolledBackUploadLeavesNoBytes (0.00s)
--- PASS: TestRetryRidesOutOutage (0.00s)
--- PASS: TestReadOutage (0.00s)
--- PASS: TestTrashRetention (0.00s)
--- PASS: TestConformance (0.50s)
PASS
ok  	github.com/ubgo/filesystem/examples/start-testing	1.103s
```

1. `TestAvatarSaved` hands the service a `Fake` disk and asserts on what it stored; `TestAvatarRefused` checks a refusal by its code, never its message, and that nothing was stored.
2. `TestFromDSN` opens the disk from a `mem://` DSN, so DSN parameters are under test too.
3. `TestGallery` checks rows and bytes together on a records store over SQLite; `TestRolledBackUploadLeavesNoBytes` makes the database step fail and checks the uploaded object was deleted again.
4. `TestRetryRidesOutOutage` injects two failed writes under a retry interceptor: a write of bytes is retried and lands, a streamed upload is not retried and stores nothing. `TestReadOutage` fails every read, as a backend that is down would.
5. `TestTrashRetention` moves the store's clock instead of sleeping: a file trashed 31 days ago is purged by a 30-day sweep.
6. `TestConformance` runs the suite every built-in driver passes; put your own driver in its factory.
7. `TestAvatarUploadOverHTTP` posts a multipart form built by `filesystemtest.NewUploadRequest`, carrying a PNG drawn by `FakeImage`, through the service's HTTP handler; `TestAvatarRefusalsOverHTTP` checks a JPEG padded one byte over the limit (413), bytes that are not an image (415) and a wrong form field (400), and that none of them stored anything.

The program itself saves one avatar and points at the tests:

```sh
go run ./start-testing
```

```text
saved avatars/u1 - run `go test ./start-testing` for the tests
```

## The code

The service takes its disk as a parameter, which is all a test needs to hand it a `Fake`:

<!-- ds:block id=testing-avatars-fkc7929x -->
**Avatars** · [`examples/start-testing/main.go:52-54`](main.go#L52-L54)

```go
type Avatars struct {
	disk *filesystem.Disk
}
```
<!-- /ds:block hash=5fae23 -->

<!-- ds:block id=testing-avatarpolicy-kycd9dze -->
**avatarPolicy** · [`examples/start-testing/main.go:64-70`](main.go#L64-L70)

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
**Avatars.Save** · [`examples/start-testing/main.go:75-81`](main.go#L75-L81)

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

<!-- ds:block id=testing-galleryadd-vx94ngb9 -->
**Gallery.Add** · [`examples/start-testing/main.go:106-117`](main.go#L106-L117)

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

Tests with a `Fake` disk and with a DSN:

<!-- ds:block id=testing-avatarsaved-fx8nkktf -->
**TestAvatarSaved** · [`examples/start-testing/main_test.go:32-41`](main_test.go#L32-L41)

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

<!-- ds:block id=testing-avatarrefused-pm2dhgbn -->
**TestAvatarRefused** · [`examples/start-testing/main_test.go:46-53`](main_test.go#L46-L53)

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

<!-- ds:block id=testing-fromdsn-jyf9ghyk -->
**TestFromDSN** · [`examples/start-testing/main_test.go:73-85`](main_test.go#L73-L85)

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

A records store per test on a SQLite file (not `:memory:`, where each connection is a separate empty database), and tests of rows and bytes together:

<!-- ds:block id=testing-newstore-6vkdpbv5 -->
**newStore** · [`examples/start-testing/main_test.go:92-111`](main_test.go#L92-L111)

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

<!-- ds:block id=testing-gallery-x5rhck7j -->
**TestGallery** · [`examples/start-testing/main_test.go:116-129`](main_test.go#L116-L129)

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

<!-- ds:block id=testing-rollback-w3zc6n3s -->
**TestRolledBackUploadLeavesNoBytes** · [`examples/start-testing/main_test.go:134-148`](main_test.go#L134-L148)

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

Fault injection with an interceptor and with `layer.Guard`:

<!-- ds:block id=testing-failfirst-gg62jgzr -->
**failFirst** · [`examples/start-testing/main_test.go:154-168`](main_test.go#L154-L168)

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

<!-- ds:block id=testing-retry-5yd7p44f -->
**TestRetryRidesOutOutage** · [`examples/start-testing/main_test.go:177-195`](main_test.go#L177-L195)

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

<!-- ds:block id=testing-guard-6us897vb -->
**TestReadOutage** · [`examples/start-testing/main_test.go:200-214`](main_test.go#L200-L214)

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

Time without sleeping:

<!-- ds:block id=testing-clock-q9v8qeg7 -->
**clock** · [`examples/start-testing/main_test.go:218-221`](main_test.go#L218-L221)

```go
type clock struct {
	mu  sync.Mutex
	now time.Time
}
```
<!-- /ds:block hash=6e4372 -->

<!-- ds:block id=testing-trash-fsxm6286 -->
**TestTrashRetention** · [`examples/start-testing/main_test.go:232-249`](main_test.go#L232-L249)

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

The conformance suite:

<!-- ds:block id=testing-conformance-584pkj8j -->
**TestConformance** · [`examples/start-testing/main_test.go:254-267`](main_test.go#L254-L267)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The code under test: the `Avatars` service, its upload policy, and the `Gallery` on the records layer |
| `main_test.go` | The tests: `Fake` disk, DSN, records on SQLite, rollback, retry, outage, clock and conformance |
| `http.go` | The avatar service's HTTP handler |
| `http_test.go` | The handler's tests, with fake multipart requests and images |

## Next

- The guide: [Test your app](../../docs/start/testing.md).
- Related examples: [quickstart](../quickstart), whose tests drive a whole app through `httptest`, [extending-driver](../extending-driver) for a driver that runs the conformance suite, [extending-interceptor](../extending-interceptor).
