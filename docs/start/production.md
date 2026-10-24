# Production

You will have the app from [your first app](first-app.md) ready to deploy: its storage chosen by environment variables (S3, R2 or GCS instead of a local directory), its signing keys in a secret store and rotatable, every storage call retried, bounded, logged and measured, its schema migrated by the deploy, its trash and abandoned uploads swept on a schedule, quotas, virus scanning, a health check, a backup copy, and the rules for running several instances.

The program behind this page is [examples/start-production](../../examples/start-production). It runs offline on SQLite and local disks so its test can run anywhere; every snippet takes a Postgres database and S3 DSNs unchanged apart from the driver and dialect.

## Before you start

- [Your first app](first-app.md) running locally.
- The library starts no goroutines, holds no global state, and ships no scheduler, secret store or metrics backend. Each of those is one call or one function value for the app to provide; this page is the list.

## Step 1 — Storage from the environment

The program reads every setting through a lookup function (`os.LookupEnv` in `main`, a map in tests), so nothing reads the process environment behind its back:

<!-- ds:block id=prod-env-scg2zs3w -->
**envStoragePrefix** · [`examples/start-production/main.go:53`](../../examples/start-production/main.go#L53-L53)

```go
"STORAGE_"
```
<!-- /ds:block hash=d5d4cc -->

The `Mux` knows every backend the deployment might name; registering an adapter costs nothing until a DSN uses it:

<!-- ds:block id=prod-newmux-fa8kysu7 -->
**newMux** · [`examples/start-production/main.go:188-197`](../../examples/start-production/main.go#L188-L197)

```go
func newMux() (*filesystem.Mux, error) {
	mux := filesystem.NewMux()
	return mux, errors.Join(
		mux.Register(local.Scheme, local.Opener),
		mux.Register(memory.Scheme, memory.Opener),
		mux.Register(s3.Scheme, s3.Opener),
		mux.Register(gcs.Scheme, gcs.Opener),
		mux.Register(azure.Scheme, azure.Opener),
	)
}
```
<!-- /ds:block hash=9284b0 -->

`OpenManager` builds every disk from `STORAGE_DISKS` and one `STORAGE_<NAME>_DSN` per disk, with the options (the signer, the interceptors) applied to each. The program then runs `Check` on every disk under a timeout, so a wrong credential or a missing bucket fails the deploy, not the first upload:

<!-- ds:block id=prod-opendisks-yfed2m8r -->
**openDisks** · [`examples/start-production/main.go:202-217`](../../examples/start-production/main.go#L202-L217)

```go
func openDisks(ctx context.Context, lookup filesystem.LookupFunc, signer *signedurl.Signer, chain []filesystem.Interceptor) (*filesystem.Manager, error) {
	mux, err := newMux()
	if err != nil {
		return nil, err
	}
	disks, err := mux.OpenManager(ctx, envStoragePrefix, lookup, filesystem.WithURLSigner(signer), filesystem.WithInterceptors(chain...))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	if err := disks.Check(ctx); err != nil {
		return nil, errors.Join(err, disks.Close())
	}
	return disks, nil
}
```
<!-- /ds:block hash=7c7d65 -->

A production environment on Cloudflare R2 with a CDN in front of public files, and a second bucket for backups:

```sh
STORAGE_DISKS=media,backups
STORAGE_DEFAULT_DISK=media
STORAGE_MEDIA_DSN='s3://acme-media?region=auto&endpoint=https://<account>.r2.cloudflarestorage.com&public_url=https://cdn.example.com'
STORAGE_BACKUPS_DSN='s3://acme-backups?region=auto&endpoint=https://<account>.r2.cloudflarestorage.com'
AWS_ACCESS_KEY_ID=…
AWS_SECRET_ACCESS_KEY=…
```

S3 credentials come from the AWS default chain, never from a DSN, so DSNs are safe to log. GCS uses Application Default Credentials (`gs://bucket`), and Azure a connection string from the environment (`azblob://container`); the [adapters](../adapters/README.md) pages have the details and the [DSN reference](../reference/dsn.md) every parameter. Four parameters work on every scheme: `prefix` (scope the disk to a key prefix), `public_url` (the base `PublicURL` returns, typically a CDN), `visibility` (`public` or `private`, the default for writes) and `readonly`.

A missing variable is an error that names it and never prints a DSN, which may carry secrets:

```text
filesystem: invalid_argument: STORAGE_BACKUPS_DSN is not set (disk "backups")
```

A malformed DSN fails too, and its error names the variable and the disk, still without the DSN:

```text
filesystem: invalid_argument: STORAGE_BACKUPS_DSN: disk backups: unknown DSN parameter "visibilty" for scheme "file"
```

Check: `start-production migrate` with one DSN variable unset exits non-zero and names the variable.

## Step 2 — Signing keys and rotation

S3, GCS and Azure sign their own links (SigV4, V4, SAS) with the backend's credentials. A local or memory disk cannot, and gets its links from `signedurl` through `WithURLSigner`; the `Disk` uses the backend's signing when it has the `Presign` capability and the signer otherwise. Production apps on an object store may still need the signer for a local disk, a mirror, or an SFTP server, so the program always builds it.

The keys come from one variable, newest first. The first key signs; every key verifies:

<!-- ds:block id=prod-parsekeys-8gx8j7ur -->
**parseSigningKeys** · [`examples/start-production/main.go:136-146`](../../examples/start-production/main.go#L136-L146)

```go
func parseSigningKeys(raw string) ([]signedurl.Key, error) {
	var keys []signedurl.Key
	for item := range strings.SplitSeq(raw, keyListSeparator) {
		id, secret, ok := strings.Cut(strings.TrimSpace(item), keyIDSeparator)
		if !ok || id == "" || secret == "" {
			return nil, fmt.Errorf("%s: each key is id%ssecret", envSigningKeys, keyIDSeparator)
		}
		keys = append(keys, signedurl.Key{ID: id, Secret: []byte(secret)})
	}
	return keys, nil
}
```
<!-- /ds:block hash=ddc838 -->

<!-- ds:block id=prod-newsigner-sdqgdhyz -->
**newSigner** · [`examples/start-production/main.go:151-169`](../../examples/start-production/main.go#L151-L169)

```go
func newSigner(lookup filesystem.LookupFunc) (*signedurl.Signer, error) {
	base, ok := lookup(envFilesURL)
	if !ok || base == "" {
		return nil, fmt.Errorf("%s is not set", envFilesURL)
	}
	raw, ok := lookup(envSigningKeys)
	if !ok || raw == "" {
		return nil, fmt.Errorf("%s is not set", envSigningKeys)
	}
	keys, err := parseSigningKeys(raw)
	if err != nil {
		return nil, err
	}
	signer, err := signedurl.New(base, keys)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envSigningKeys, err) // the error names no secret
	}
	return signer, nil
}
```
<!-- /ds:block hash=aec5c0 -->

```sh
FILES_SIGNING_KEYS="k1:$(openssl rand -base64 48)"
```

Each secret must be at least 32 bytes (`signedurl.MinKeyBytes`); a shorter one stops the start-up with `signing key "k1" is 5 bytes, shorter than 32`. The key ID travels in every link (`kid=k1`), so a verifier picks the right key directly. To rotate:

1. Put the new key first and keep the old one: `FILES_SIGNING_KEYS=k2:<new>,k1:<old>`. Deploy. New links are signed with `k2`; links already handed out still verify with `k1`.
2. Wait for the longest link lifetime you issue to pass (at most seven days, `filesystem.MaxPresignExpiry`).
3. Remove the old key: `FILES_SIGNING_KEYS=k2:<new>`. Links signed with `k1` now fail with `403`.

If a key leaks, skip the wait: remove it at once, and every link signed with it stops working. The repository's test walks the three states:

<!-- ds:block id=prod-keyrotation-test-dnhx53je -->
**TestKeyRotation** · [`examples/start-production/main_test.go:77-107`](../../examples/start-production/main_test.go#L77-L107)

```go
func TestKeyRotation(t *testing.T) {
	before, err := newSigner(env(map[string]string{envFilesURL: "https://f.example/files", envSigningKeys: "k1:" + oldSecret}))
	if err != nil {
		t.Fatal(err)
	}
	link, err := before.SignURL(t.Context(), "media", "a.png", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link.URL)
	if err != nil {
		t.Fatal(err)
	}
	during, err := newSigner(env(map[string]string{envFilesURL: "https://f.example/files", envSigningKeys: "k2:" + newSecret + ",k1:" + oldSecret}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := during.Verify("media", "a.png", filesystem.MethodGet, u.Query()); err != nil {
		t.Errorf("old link during rotation: %v", err)
	}
	after, err := newSigner(env(map[string]string{envFilesURL: "https://f.example/files", envSigningKeys: "k2:" + newSecret}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := after.Verify("media", "a.png", filesystem.MethodGet, u.Query()); !errors.Is(err, filesystem.ErrPermission) {
		t.Errorf("old link after rotation: %v", err)
	}
	if _, err := parseSigningKeys("no-separator"); err == nil {
		t.Error("malformed key list accepted")
	}
}
```
<!-- /ds:block hash=653e63 -->

Rotating AWS credentials has the same effect on links S3 signed: a presigned URL dies with the key that signed it.

## Step 3 — Retry, timeouts, limits, logs and metrics

Cross-cutting behaviour runs as interceptors around every storage operation. They wrap the operation, not the driver, so they can never hide a backend's capabilities. The first one listed runs outermost:

<!-- ds:block id=prod-interceptors-th6rds2j -->
**interceptors** · [`examples/start-production/main.go:176-184`](../../examples/start-production/main.go#L176-L184)

```go
func interceptors(logger *slog.Logger, metrics func(context.Context, layer.Event)) []filesystem.Interceptor {
	return []filesystem.Interceptor{
		layer.Observe(metrics),
		layer.Log(logger),
		layer.Retry(layer.RetryOptions{}),
		layer.Timeout(opTimeout),
		layer.Limit(maxConcurrentOps),
	}
}
```
<!-- /ds:block hash=5eee90 -->

- `layer.Observe` sees each whole operation, retries included, as a `layer.Event` (kind, disk, key, attempts, bytes, duration, error code). It is the hook metrics and tracing build on; [contrib/otel](../integrations/otel.md) gives OpenTelemetry spans and metrics on top of it. An event carries no payload, URL or signature.
- `layer.Log` writes one record per operation to your `*slog.Logger`, with the attributes `disk`, `op`, `key`, `attempts`, `bytes`, `duration`, `code` and `error`. Successes log at Debug and failures at Warn, except not-found from a read, which is an answer rather than a failure and logs at Debug. It logs keys, never signed URLs or credentials.
- `layer.Retry` retries only errors with `CodeUnavailable` (timeouts, throttling, dropped connections), three attempts by default with jittered backoff from 100 ms to 2 s. It never retries an operation that is unsafe to repeat: a write whose body is a one-shot stream (an upload arriving over HTTP), a move, a multipart completion. A write from bytes or any `io.Seeker` that can seek (a regular file, not a pipe) is rewound and retried.
- `layer.Timeout` bounds each attempt. It leaves reads alone, because a read returns a stream that outlives the call; bound downloads with the request's context.
- `layer.Limit` caps operations in flight per disk in this process and makes the rest wait, so a burst cannot open thousands of connections to the backend.

`layer.Guard` adds a policy check before every operation (a maintenance window, a tenant check), and `filesystem.ReadOnly()`, or `readonly=true` in a DSN, refuses every write. The [interceptors](../concepts/interceptors.md) page covers writing your own.

## Step 4 — Migrations in deploys

Migrating at start-up is fine for one process. With several instances or a release pipeline, migrate once, from the deploy, before the new version serves:

<!-- ds:block id=prod-migrate-bqqfw33y -->
**migrate** · [`examples/start-production/main.go:308-323`](../../examples/start-production/main.go#L308-L323)

```go
func migrate(ctx context.Context, store *records.Store, out io.Writer) error {
	applied, err := store.Migrate(ctx)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		_, err = fmt.Fprintln(out, "schema is up to date")
		return err
	}
	for _, m := range applied {
		if _, err := fmt.Fprintln(out, "applied", m); err != nil {
			return err
		}
	}
	return nil
}
```
<!-- /ds:block hash=503474 -->

```sh
start-production migrate
start-production migrate
```

```text
applied records/0001_files_and_attachments
applied records/0002_files_uploaded_by
applied quota/0001_usage
applied quota/0002_uploader_usage
applied audit/0001_audit
applied audit/0002_audit_subject_detail
schema is up to date
```

The core's migrations come first, then each plugin's in the order it was installed. `Migrate` takes a lock across processes (a Postgres advisory lock; MySQL `GET_LOCK`, waiting up to 60 seconds by default, `records.WithMigrationLockWait`; SQLite has one writer and needs none), so two deploy jobs started together apply each migration once. A migration whose statements changed after it ran stops `Migrate` with `CodeInvalidArgument` instead of skipping or re-running it.

Teams that apply schema changes with their own tool (Atlas, Flyway, a DBA's review) print the SQL instead:

<!-- ds:block id=prod-migrationsql-udawhkdu -->
**migrationSQL** · [`examples/start-production/main.go:328-335`](../../examples/start-production/main.go#L328-L335)

```go
func migrationSQL(store *records.Store, out io.Writer) error {
	script, err := store.MigrationSQL()
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, script)
	return err
}
```
<!-- /ds:block hash=122bd9 -->

```text
-- records 0001_files_and_attachments
CREATE TABLE IF NOT EXISTS fs_files (id TEXT NOT NULL PRIMARY KEY, scope TEXT NOT NULL, disk TEXT NOT NULL, storage_key TEXT NOT NULL, filename TEXT NOT NULL, content_type TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL, visibility TEXT NOT NULL, metadata TEXT NOT NULL, created_at TIMESTAMP NOT NULL, deleted_at TIMESTAMP NULL);
CREATE TABLE IF NOT EXISTS fs_attachments (id TEXT NOT NULL PRIMARY KEY, scope TEXT NOT NULL, file_id TEXT NOT NULL, owner_type TEXT NOT NULL, owner_id TEXT NOT NULL, role TEXT NOT NULL, position INTEGER NOT NULL, created_at TIMESTAMP NOT NULL);
…
```

The script is rendered for the store's dialect; run the command against a Postgres store to get Postgres SQL. It holds the schema changes only, not the rows `Migrate` writes to its ledger (`fs_migrations`). Pick one way per database: if you apply the script yourself, a later `Migrate` on that database sees an empty ledger and runs every migration again, and any statement that cannot run twice fails (on SQLite, the `ALTER TABLE … ADD COLUMN` of `records/0002_files_uploaded_by`). The [migrations](../records/migrations.md) page has the details.

## Step 5 — Scheduled clean-up

Three things grow forever unless something removes them: files in the trash, uploads that were started and never confirmed, and audit entries. The library deletes none of them on its own; the app runs one job on a schedule (cron, a Kubernetes CronJob, your job runner):

<!-- ds:block id=prod-maintain-9tanm49b -->
**maintain** · [`examples/start-production/main.go:342-348`](../../examples/start-production/main.go#L342-L348)

```go
func maintain(ctx context.Context, store *records.Store, auditLog *audit.Plugin, media *filesystem.Disk, now time.Time, out io.Writer) error {
	purged, errTrash := store.SweepTrash(ctx, now.Add(-trashRetention))
	swept, errUploads := upload.Sweep(ctx, media, incomingDir, now.Add(-abandonedUploadAge))
	pruned, errAudit := auditLog.Prune(ctx, store, now.Add(-auditRetention))
	_, errOut := fmt.Fprintf(out, "purged %d trashed files, swept %d abandoned uploads, pruned %d audit entries\n", purged, swept, pruned)
	return errors.Join(errTrash, errUploads, errAudit, errOut)
}
```
<!-- /ds:block hash=845a5b -->

```sh
start-production maintain
```

```text
purged 0 trashed files, swept 0 abandoned uploads, pruned 0 audit entries
```

- `store.SweepTrash` purges every file, in every scope, trashed before the cutoff: rows in a transaction, then the objects after the commit. A file it cannot purge does not stop it; the error names each one. `store.SweepOrphans` can join the job to delete objects whose row is gone (a deletion that failed after a commit), checking every object against the rows first.
- `upload.Sweep` deletes every object under a prefix older than the cutoff. Point it at the prefix where direct uploads land before `upload.Confirm` records them (here `incoming/`), never at your live files. It refuses the root of a disk.
- `audit.Plugin.Prune` deletes log entries older than the cutoff.

Each step continues where it stopped when it runs again, so a job that died half-way is fixed by the next run. Run the job from one scheduler, once an hour or once a day; nothing is gained by running it on every instance.

One more thing can be left behind: when an object deletion fails after its row was already deleted and committed (the backend was down at that moment), the bytes stay. The row never comes back, so the records stay correct, but the object costs storage. `records.WithObjectErrorHandler` hears about every such failure; the program logs the disk and key so an operator, or a bucket lifecycle rule on old objects, can remove it:

<!-- ds:block id=prod-newstore-txwtyv4v -->
**newStore** · [`examples/start-production/main.go:262-276`](../../examples/start-production/main.go#L262-L276)

```go
func newStore(db *sql.DB, disks *filesystem.Manager, limits quota.LimitFunc, logger *slog.Logger) (*records.Store, *audit.Plugin, error) {
	q, err := quota.New(limits)
	if err != nil {
		return nil, nil, err
	}
	auditLog := audit.New()
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithOwnerTypes(ownerUser),
		records.WithPlugins(q, auditLog),
		records.WithObjectErrorHandler(func(ctx context.Context, f records.File, err error) {
			logger.ErrorContext(ctx, "object left behind", "disk", f.Disk, "key", f.Key, "error", err)
		}),
	)
	return store, auditLog, err
}
```
<!-- /ds:block hash=4a707d -->

## Step 6 — Quotas

The `quota` plugin limits each scope's bytes and file count. The limits come from your app, usually from the scope's billing plan:

<!-- ds:block id=prod-plans-9vyhv6qz -->
**planFree** · [`examples/start-production/main.go:223`](../../examples/start-production/main.go#L223-L223)

```go
"free"
```
<!-- /ds:block hash=3e07f2 -->

<!-- ds:block id=prod-limitsfor-mtsswdmq -->
**limitsFor** · [`examples/start-production/main.go:244-256`](../../examples/start-production/main.go#L244-L256)

```go
func limitsFor(planOf func(ctx context.Context, scope string) (string, error)) quota.LimitFunc {
	return func(ctx context.Context, scope string) (quota.Limit, error) {
		plan, err := planOf(ctx, scope)
		if err != nil {
			return quota.Limit{}, err
		}
		limit, ok := planLimit(plan)
		if !ok {
			return quota.Limit{}, fmt.Errorf("scope %q has unknown plan %q", scope, plan)
		}
		return limit, nil
	}
}
```
<!-- /ds:block hash=e85cc3 -->

Usage is charged inside the transaction that records the file, with one conditional `UPDATE`, so two concurrent uploads on two instances can never both pass the limit, and a rolled-back upload un-charges itself. An upload over the limit fails with `CodeQuotaExceeded` (HTTP `507` through `fshttp.StatusOf`) and its object is deleted. A trashed file still counts, because its bytes are still stored; purging releases it. The [quota](../plugins/quota.md) page covers usage reports and `Recount`.

## Step 7 — Virus scanning

`Policy.Inspect` runs on every upload once it is stored and before the upload reports success; an error deletes the object and fails the upload. `scan.Inspect` plugs a scanner into it, and `scan.Clamd` talks to a ClamAV daemon:

<!-- ds:block id=prod-uploadpolicy-qfzpymft -->
**uploadPolicy** · [`examples/start-production/main.go:282-293`](../../examples/start-production/main.go#L282-L293)

```go
func uploadPolicy(scanner scan.Scanner) upload.Policy {
	p := upload.Policy{
		MaxSize:        maxUploadSize,
		Allowed:        []string{"image/*", "application/pdf"},
		MatchExtension: true,
		Key:            upload.RandomKey(uploadDir),
	}
	if scanner != nil {
		p.Inspect = scan.Inspect(scanner)
	}
	return p
}
```
<!-- /ds:block hash=e3bee6 -->

<!-- ds:block id=prod-scannerfrom-7pbngzb2 -->
**scannerFrom** · [`examples/start-production/main.go:297-302`](../../examples/start-production/main.go#L297-L302)

```go
func scannerFrom(lookup filesystem.LookupFunc) scan.Scanner {
	if addr, ok := lookup(envClamd); ok && addr != "" {
		return scan.Clamd{Network: "tcp", Address: addr}
	}
	return nil
}
```
<!-- /ds:block hash=e3fb35 -->

- Scanning fails closed. A scanner that is down or times out refuses the upload with `CodeUnavailable`; nothing unscanned is ever reported stored. An app that prefers to accept uploads while the scanner is down wraps its `Scanner` and decides there.
- `Inspect` runs in `upload.Receive` (form uploads, imports from a URL, tus) and in `upload.Confirm` (presigned and Uppy uploads, after the bytes reached the bucket), so it covers every upload path of the library.
- clamd refuses streams over its `StreamMaxLength` (100 MiB by default in current ClamAV, 25 MB in old releases). Keep it at least as large as `MaxSize`, or large uploads fail with `CodeTooLarge`.
- `scan.Clamd.Ping` checks the daemon, for a readiness probe.

This program's `serve` mode has no upload route of its own: `uploadPolicy(scannerFrom(lookup))` is the policy to hand the upload handlers of [your first app](first-app.md), and the program's tests run it against a scanner that refuses a file. Without `CLAMD_ADDR`, `scannerFrom` returns nil and the policy does not scan. Decide that on purpose: [virus scanning](../recipes/virus-scanning.md) covers running clamd next to the app.

## Step 8 — A separate origin for user content

Files your users upload must never run as your site. `fshttp` already serves every file with `X-Content-Type-Options: nosniff` and a sandboxing `Content-Security-Policy`, serves active content (HTML, SVG, XML) as a download, and upload policies refuse active content by default. The remaining defence is the domain: serve user files from a domain that shares no cookies with the app, as GitHub does with `githubusercontent.com`.

- **Signed links from fshttp**: set `FILES_URL` to a URL on that domain (`https://files.example-usercontent.com/files`) and route that host to the same program; `serve` mounts `fshttp.Signed` at the URL's path.
- **Public files**: set `public_url` in the disk's DSN to a CDN on that domain. `PublicURL` then builds links without a backend call.
- **Presigned S3, GCS or Azure links** already point at the provider's domain.

<!-- ds:block id=prod-serve-4e7aaa4b -->
**wiring.serve** · [`examples/start-production/main.go:441-453`](../../examples/start-production/main.go#L441-L453)

```go
func (w *wiring) serve(ctx context.Context, lookup filesystem.LookupFunc) error {
	base, _ := lookup(envFilesURL)
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("%s: %w", envFilesURL, err)
	}
	mux := http.NewServeMux()
	mux.Handle(u.Path+"/", http.StripPrefix(u.Path, fshttp.Signed(w.disks, w.signer)))
	mux.Handle("/healthz", health(w.disks, w.logger))
	addr, _ := lookup(envAddr)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: readHeaderTimeout, BaseContext: func(net.Listener) context.Context { return ctx }}
	return srv.ListenAndServe()
}
```
<!-- /ds:block hash=ccaa43 -->

## Step 9 — Backups and mirrors

`filesystem.Mirror` returns a disk that reads from a primary and writes to the primary and then to every mirror. It keeps a warm copy in a second bucket or region, and it is also how a store moves to a new backend without downtime: mirror the old one to the new one, backfill old objects, then make the new one primary.

<!-- ds:block id=prod-mirrored-r5x49s84 -->
**mirrored** · [`examples/start-production/main.go:371-381`](../../examples/start-production/main.go#L371-L381)

```go
func mirrored(disks *filesystem.Manager, name, primary, backup string) (*filesystem.Disk, error) {
	p, err := disks.Disk(primary)
	if err != nil {
		return nil, err
	}
	b, err := disks.Disk(backup)
	if err != nil {
		return nil, err
	}
	return filesystem.Mirror(name, p, b)
}
```
<!-- /ds:block hash=3f31a2 -->

The semantics, which you must accept before relying on it:

- Not atomic across disks. A write lands on the primary first; if a mirror then fails, the write returns `CodeUnavailable` and the object exists on the primary only. Writes are overwrites, so retrying converges.
- Deletes go to every disk, so a mirror is a copy, not a backup against deletion. For that, turn on object versioning or replication at the provider.
- Signed upload URLs and presigned parts are refused, because a client uploading straight to the primary would bypass the mirrors. Uploads through the server, including server-side multipart, are replicated.
- Records rows store the disk name, so give the mirror the name the rows already use (`media`), and open the underlying disks under other names.

The program builds the mirror (`media` onto `backups`) in its tests rather than in `serve`, which only serves signed links and writes nothing; in production, call `mirrored` while wiring and use the disk it returns.

## Step 10 — Health checks

`Check` proves a disk works end to end. The program runs it on every disk at start-up (step 1) and behind a readiness endpoint:

<!-- ds:block id=prod-health-jnvrfde6 -->
**health** · [`examples/start-production/main.go:354-365`](../../examples/start-production/main.go#L354-L365)

```go
func health(disks *filesystem.Manager, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
		defer cancel()
		if err := disks.Check(ctx); err != nil {
			logger.WarnContext(ctx, "storage check failed", "error", err)
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n") // a probe that hung up cannot be told
	})
}
```
<!-- /ds:block hash=02d506 -->

```sh
curl -s -i localhost:8083/healthz
```

```text
HTTP/1.1 200 OK
Date: Sat, 03 Oct 2026 09:20:31 GMT
Content-Length: 3
Content-Type: text/plain; charset=utf-8

ok
```

Each check writes, reads and deletes a small object under `.filesystem-check/` on every disk. That is one write per disk per probe: fine every ten seconds, wasteful on every request. A read-only disk cannot be checked this way and reports `CodeReadOnly`, so leave read-only disks out of the probe. If a crash ever leaves a probe object behind, `.filesystem-check/` is safe to delete.

## Step 11 — More than one instance

The library keeps no state in the process that instances would need to share, but your configuration must agree:

- **Same signing keys everywhere.** A link signed by one instance is verified by whichever instance receives it.
- **Shared storage.** A `file:` disk is a directory on one machine; two instances behind a load balancer each see their own. Use an object store, or a volume every instance mounts at the same path.
- **One migration per deploy.** Run `migrate` from the deploy job. If instances also migrate at start-up, the lock makes it safe, not fast.
- **One scheduler for clean-up.** Run `maintain` from one place.
- **Per-process limits.** `layer.Limit(64)` allows 64 operations per disk per instance; ten instances make 640 against the backend.
- **Quotas are already safe**: they are charged in the database, not in memory.
- **Uppy multipart**: set `UppyOptions.Authorize`. Upload IDs and keys come from the client, and without the check one user could complete or abort another's upload.

## Step 12 — The checklist

| Item | Where |
|---|---|
| Every disk opened from the environment and checked at start-up | step 1 |
| Signing keys from a secret store, 32 bytes or more, the same on every instance, with a rotation plan | step 2 |
| Retry, timeout and concurrency limit on every disk; failures logged; metrics observed | step 3 |
| Migrations applied by the deploy, or reviewed as SQL | step 4 |
| Trash, abandoned uploads and audit entries swept on a schedule; failed object deletions logged | step 5 |
| Quotas per scope, if users pay by usage | step 6 |
| Uploads scanned, or a written decision not to | step 7 |
| User files on a domain without your cookies; policies left refusing active content | step 8 |
| A copy elsewhere, and versioning against deletion | step 9 |
| A readiness probe on `Check` | step 10 |
| Signed link lifetimes as short as the product allows; never a public link for a private file | [security](../concepts/security.md) |

## What can go wrong

| Symptom | Fix |
|---|---|
| `filesystem: invalid_argument: STORAGE_X_DSN is not set (disk "x")` | A disk is listed in `STORAGE_DISKS` without its DSN variable. Disk names are upper-cased, with `-` and `.` as `_`: disk `media-cdn` reads `STORAGE_MEDIA_CDN_DSN`. |
| Links work on one instance and `403` on another | The instances have different `FILES_SIGNING_KEYS`. |
| Links stop working right after a deploy | The signing key changed without keeping the old one second in the list. |
| Uploads fail with `unavailable` while the backend is fine | The scanner is down, and scanning fails closed. Check `scan.Clamd.Ping`. |
| A migration fails with `invalid_argument` naming a changed migration | Someone edited a migration that already ran. Restore it and add a new migration instead. |
| Storage costs grow although users delete files | The clean-up job is not running, or `upload.Sweep` points at the wrong prefix. Run `maintain` and read its counts. |
| Requests take several times `opTimeout` during a backend outage | Each attempt gets its own timeout and Retry makes three. Lower `RetryOptions.Attempts` or the `Timeout` for request paths that must answer fast. |

## Next

- [Test your app](testing.md): fakes, injected failures and clocks.
- [Configuration](../concepts/configuration.md), [interceptors](../concepts/interceptors.md), [security](../concepts/security.md) and [consistency](../concepts/consistency.md) for the reasoning behind each step.
- [Public CDN](../recipes/public-cdn.md), [SaaS tenants](../recipes/saas-tenants.md) and [moving to S3](../recipes/move-to-s3.md) for complete setups.
