# Production wiring from the environment

The program behind the production guide: the wiring a deployed app needs around the library, configured entirely from environment variables. Every disk is opened from `STORAGE_*` DSNs and checked at start-up; link signing keys come from one variable and rotate without breaking old links; every disk runs through metrics, logging, retry, timeout and concurrency-limit interceptors; quotas come from the scope's plan and changes go to an audit log; migrations run as a deploy step or print as SQL; a scheduled job empties the trash, sweeps abandoned uploads and prunes the audit log; and a readiness probe checks every disk. It runs offline on SQLite and local disks; in production the same code takes S3, GCS or Azure DSNs (Postgres or MySQL need that engine's driver and dialect in place of SQLite). It uses the core `github.com/ubgo/filesystem` package with `layer`, `fshttp`, `signedurl`, `upload`, `scan`, the `local`, `memory`, `s3`, `gcs` and `azure` drivers, and `records` with the `quota` and `audit` plugins. The guide [Production](../../docs/start/production.md) explains each step.

## Run it

The program takes one command: `serve`, `migrate`, `migrate-sql` or `maintain`. Every command reads the same environment and fails before doing anything if a variable is missing or a DSN is malformed. A local setup:

```sh
cd examples
export STORAGE_DISKS=media,backups
export STORAGE_DEFAULT_DISK=media
export STORAGE_MEDIA_DSN=file:data/media
export STORAGE_BACKUPS_DSN=file:data/backups
export FILES_SIGNING_KEYS='k1:0123456789abcdef0123456789abcdef'
export FILES_URL=http://localhost:8083/files
export DATABASE_PATH=app.db
export ADDR=localhost:8083

go run ./start-production migrate
go run ./start-production serve
go test ./start-production
```

| Variable | What it holds |
|---|---|
| `STORAGE_DISKS` | The disk names, comma-separated; each needs `STORAGE_<NAME>_DSN` |
| `STORAGE_DEFAULT_DISK` | The disk used when none is named |
| `FILES_SIGNING_KEYS` | Link signing keys, newest first, `id:secret,id:secret`; the first signs, all verify; each secret at least 32 bytes |
| `FILES_URL` | The absolute URL signed links point at; `serve` mounts the download handler at its path |
| `DATABASE_PATH` | The SQLite file |
| `ADDR` | Where `serve` listens |
| `CLAMD_ADDR` | A clamd address for virus scanning; read by `scannerFrom`, which only the tests exercise, since this program has no upload route |

With the values above, it creates `data/media`, `data/backups` and `app.db` (plus SQLite's `-shm`/`-wal` files) in the directory you run it from; delete them when done. `serve` listens until you stop it with Ctrl-C. A file DSN with a relative path is written `file:data/media`; `file://./data/media` is refused because `.` would be a host. S3 credentials come from the AWS default chain, never from a DSN, so DSNs are safe to log.

## Walkthrough

Migrations, applied once and then up to date:

```sh
go run ./start-production migrate
go run ./start-production migrate
```

```text
applied records/0001_files_and_attachments
applied records/0002_files_uploaded_by
applied records/0003_files_status
applied records/0004_files_object
applied records/0005_files_source
applied records/0006_files_expiry
applied records/0007_files_checksums
applied quota/0001_usage
applied quota/0002_uploader_usage
applied quota/0003_reservations
applied quota/0004_dimension_usage
applied audit/0001_audit
applied audit/0002_audit_subject_detail
applied audit/0003_audit_chain
applied audit/0004_audit_keys_source
schema is up to date
```

The same changes as SQL for a team that applies schema changes with its own tool (first lines):

```sh
go run ./start-production migrate-sql
```

```text
-- fs_migrations (the ledger Migrate reads)
CREATE TABLE IF NOT EXISTS "fs_migrations" ("owner" TEXT NOT NULL, "name" TEXT NOT NULL, "checksum" TEXT NOT NULL, "applied_at" TIMESTAMP NOT NULL, PRIMARY KEY ("owner", "name"));
```

The scheduled clean-up, for cron or a Kubernetes CronJob:

```sh
go run ./start-production maintain
```

```text
purged 0 trashed files, swept 0 abandoned uploads, pruned 0 audit entries
```

A missing variable or a misspelt DSN parameter fails before anything runs, naming the variable but never printing the DSN:

```sh
(unset STORAGE_BACKUPS_DSN; go run ./start-production migrate)
STORAGE_BACKUPS_DSN='file:data/backups?visibilty=public' go run ./start-production migrate
```

```text
2026/10/03 16:27:06 filesystem: invalid_argument: STORAGE_BACKUPS_DSN is not set (disk "backups")
exit status 1
2026/10/03 16:27:08 filesystem: invalid_argument: STORAGE_BACKUPS_DSN: disk backups: unknown DSN parameter "visibilty" for scheme "file"
exit status 1
```

With `serve` running, the readiness probe writes, reads and deletes a small object on every disk:

```sh
curl -s -i localhost:8083/healthz
```

```text
HTTP/1.1 200 OK
Date: Sat, 03 Oct 2026 10:49:51 GMT
Content-Length: 3
Content-Type: text/plain; charset=utf-8

ok
```

Signed links are served under the path of `FILES_URL`; a link with a bad signature is refused:

```sh
curl -s -w ' %{http_code}\n' 'localhost:8083/files/media/x.txt?exp=1&kid=k1&m=GET&sig=00'
```

```text
Forbidden
 403
```

## The code

The environment prefix of every disk, the keys, and the signer every instance must share:

<!-- ds:block id=prod-env-scg2zs3w -->
**envStoragePrefix** · [`examples/start-production/main.go:53`](main.go#L53-L53)

```go
"STORAGE_"
```
<!-- /ds:block hash=d5d4cc -->

<!-- ds:block id=prod-parsekeys-8gx8j7ur -->
**parseSigningKeys** · [`examples/start-production/main.go:136-146`](main.go#L136-L146)

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
**newSigner** · [`examples/start-production/main.go:151-169`](main.go#L151-L169)

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

The interceptor chain, outermost first, and every backend a deployment might name:

<!-- ds:block id=prod-interceptors-th6rds2j -->
**interceptors** · [`examples/start-production/main.go:176-184`](main.go#L176-L184)

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

<!-- ds:block id=prod-newmux-fa8kysu7 -->
**newMux** · [`examples/start-production/main.go:188-197`](main.go#L188-L197)

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

Opening every disk from the environment and checking each before the app serves:

<!-- ds:block id=prod-opendisks-yfed2m8r -->
**openDisks** · [`examples/start-production/main.go:202-217`](main.go#L202-L217)

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

Plans, the quota lookup, and the records store with quota and audit:

<!-- ds:block id=prod-plans-9vyhv6qz -->
**planFree** · [`examples/start-production/main.go:223`](main.go#L223-L223)

```go
"free"
```
<!-- /ds:block hash=3e07f2 -->

<!-- ds:block id=prod-limitsfor-mtsswdmq -->
**limitsFor** · [`examples/start-production/main.go:244-256`](main.go#L244-L256)

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

<!-- ds:block id=prod-newstore-txwtyv4v -->
**newStore** · [`examples/start-production/main.go:262-276`](main.go#L262-L276)

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

The upload policy, with virus scanning when clamd is configured:

<!-- ds:block id=prod-uploadpolicy-qfzpymft -->
**uploadPolicy** · [`examples/start-production/main.go:282-293`](main.go#L282-L293)

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
**scannerFrom** · [`examples/start-production/main.go:297-302`](main.go#L297-L302)

```go
func scannerFrom(lookup filesystem.LookupFunc) scan.Scanner {
	if addr, ok := lookup(envClamd); ok && addr != "" {
		return scan.Clamd{Network: "tcp", Address: addr}
	}
	return nil
}
```
<!-- /ds:block hash=e3fb35 -->

The deploy and scheduled commands:

<!-- ds:block id=prod-migrate-bqqfw33y -->
**migrate** · [`examples/start-production/main.go:308-323`](main.go#L308-L323)

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

<!-- ds:block id=prod-migrationsql-udawhkdu -->
**migrationSQL** · [`examples/start-production/main.go:328-335`](main.go#L328-L335)

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

<!-- ds:block id=prod-maintain-9tanm49b -->
**maintain** · [`examples/start-production/main.go:342-348`](main.go#L342-L348)

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

The readiness probe, a mirrored disk, and the server:

<!-- ds:block id=prod-health-jnvrfde6 -->
**health** · [`examples/start-production/main.go:354-365`](main.go#L354-L365)

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

<!-- ds:block id=prod-mirrored-r5x49s84 -->
**mirrored** · [`examples/start-production/main.go:371-381`](main.go#L371-L381)

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

<!-- ds:block id=prod-serve-4e7aaa4b -->
**wiring.serve** · [`examples/start-production/main.go:441-453`](main.go#L441-L453)

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

Key rotation, tested: links signed with the old key verify until it is removed:

<!-- ds:block id=prod-keyrotation-test-dnhx53je -->
**TestKeyRotation** · [`examples/start-production/main_test.go:77-107`](main_test.go#L77-L107)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: configuration, signer, interceptors, disks, records store, policy, the four commands and the health probe |
| `main_test.go` | Wiring from a map with each missing variable named, key rotation, the interceptor chain, migrate commands, maintenance, scanning, health, the mirror and plan limits |

## Next

- The guide: [Production](../../docs/start/production.md), the [DSN reference](../../docs/reference/dsn.md) and [Troubleshooting](../../docs/reference/troubleshooting.md).
- Related examples: [quickstart](../quickstart) for the app this wiring grows from, [recipe-backups-and-mirrors](../recipe-backups-and-mirrors), [recipe-virus-scanning](../recipe-virus-scanning), [integrations-otel](../integrations-otel) for metrics.
