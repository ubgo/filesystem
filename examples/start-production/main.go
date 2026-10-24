// Command start-production is the program behind docs/start/production.md:
// the wiring a deployed app needs around the library, configured from the
// environment.
//
//	start-production serve         the HTTP server
//	start-production migrate       apply pending migrations, then exit
//	start-production migrate-sql   print every migration as SQL
//	start-production maintain      the scheduled clean-up, then exit
//
// It runs offline on SQLite and local disks; in production the same code
// takes Postgres or MySQL and S3, GCS or Azure DSNs.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
	"github.com/ubgo/filesystem/adapters/gcs"
	"github.com/ubgo/filesystem/adapters/s3"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/layer"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/audit"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/filesystem/scan"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Environment variables the app reads. The disks themselves come from
// STORAGE_DISKS and STORAGE_<NAME>_DSN (see filesystem.Mux.OpenManager).
const (
	// envStoragePrefix starts every storage variable.
	// ds:def id=prod-env-scg2zs3w
	envStoragePrefix = "STORAGE_"
	// envSigningKeys lists the link signing keys, newest first:
	// "k2:<secret>,k1:<secret>". The first signs; all verify.
	envSigningKeys = "FILES_SIGNING_KEYS"
	// envFilesURL is the absolute URL signed links point at, ideally on a
	// domain of its own ("https://files.example-usercontent.com/files").
	envFilesURL = "FILES_URL"
	// envClamd is the clamd address ("127.0.0.1:3310"). Unset, uploads
	// are not scanned.
	envClamd = "CLAMD_ADDR"
	// envDatabase is the SQLite file (a Postgres or MySQL DSN in a real
	// deployment, with that engine's driver and dialect).
	envDatabase = "DATABASE_PATH"
	// envAddr is where serve listens.
	envAddr = "ADDR"
)

const (
	// mediaDisk is the disk uploads go to.
	mediaDisk = "media"
	// incomingDir is where unconfirmed direct uploads land; maintain
	// sweeps it.
	incomingDir = "incoming"
	// keyListSeparator separates keys in envSigningKeys.
	keyListSeparator = ","
	// keyIDSeparator separates a key's ID from its secret.
	keyIDSeparator = ":"
	// opTimeout bounds each attempt of one storage operation.
	opTimeout = 10 * time.Second
	// maxConcurrentOps caps storage operations in flight per disk.
	maxConcurrentOps = 64
	// checkTimeout bounds a health check of every disk.
	checkTimeout = 5 * time.Second
	// trashRetention is how long a trashed file can still be restored.
	trashRetention = 30 * 24 * time.Hour
	// abandonedUploadAge is when an unconfirmed direct upload is swept.
	abandonedUploadAge = 24 * time.Hour
	// auditRetention is how long the audit log keeps an entry.
	auditRetention = 365 * 24 * time.Hour
	// readHeaderTimeout bounds how long a client may take to send headers.
	readHeaderTimeout = 5 * time.Second
	// ownerUser is the owner type files attach to.
	ownerUser = "user"
	// maxUploadSize is the largest upload accepted: 25 MiB.
	maxUploadSize = 25 << 20
	// uploadDir is where accepted uploads are stored.
	uploadDir = "files"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: start-production serve|migrate|migrate-sql|maintain")
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(context.Background(), os.Args[1], os.LookupEnv, logger, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run executes one command with configuration from lookup.
func run(ctx context.Context, cmd string, lookup filesystem.LookupFunc, logger *slog.Logger, out io.Writer) error {
	w, err := wire(ctx, lookup, logger)
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()
	switch cmd {
	case "serve":
		return w.serve(ctx, lookup)
	case "migrate":
		return migrate(ctx, w.store, out)
	case "migrate-sql":
		return migrationSQL(w.store, out)
	case "maintain":
		return maintain(ctx, w.store, w.audit, w.media, time.Now(), out)
	}
	return fmt.Errorf("unknown command %q", cmd)
}

// parseSigningKeys reads "id:secret,id:secret", newest first. Secrets are
// used as their bytes, so any string of signedurl.MinKeyBytes or more
// works, such as the output of `openssl rand -base64 48`.
// ds:def id=prod-parsekeys-8gx8j7ur
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

// newSigner builds the link signer from the environment. Every instance
// must get the same keys, or a link one instance signs fails on another.
// ds:def id=prod-newsigner-sdqgdhyz
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

// interceptors is the chain every disk runs its operations through, first
// outermost: metrics see the whole operation, the log one line per
// operation, Retry repeats transient failures, Timeout bounds each try,
// Limit caps concurrency.
// ds:def id=prod-interceptors-th6rds2j
func interceptors(logger *slog.Logger, metrics func(context.Context, layer.Event)) []filesystem.Interceptor {
	return []filesystem.Interceptor{
		layer.Observe(metrics),
		layer.Log(logger),
		layer.Retry(layer.RetryOptions{}),
		layer.Timeout(opTimeout),
		layer.Limit(maxConcurrentOps),
	}
}

// newMux knows every backend the deployment might name in a DSN.
// ds:def id=prod-newmux-fa8kysu7
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

// openDisks opens every disk STORAGE_DISKS lists, each with the signer and
// the interceptor chain, and proves each works before the app serves.
// ds:def id=prod-opendisks-yfed2m8r
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

// The plans a scope can be on, and what each may store. Zero means
// unlimited.
const (
	// ds:def id=prod-plans-9vyhv6qz
	planFree      = "free"
	planFreeBytes = 1 << 30
	planFreeFiles = 10_000
	planPro       = "pro"
	planProBytes  = 100 << 30
)

// planLimit returns what a scope on plan may store.
func planLimit(plan string) (quota.Limit, bool) {
	switch plan {
	case planFree:
		return quota.Limit{Bytes: planFreeBytes, Files: planFreeFiles}, true
	case planPro:
		return quota.Limit{Bytes: planProBytes}, true
	}
	return quota.Limit{}, false
}

// limitsFor returns the quota of a scope from its plan. planOf is the
// app's own lookup (a column on the workspace row, a billing API).
// ds:def id=prod-limitsfor-mtsswdmq
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

// newStore builds the records store with quotas and an audit log. An
// object deletion that fails after a commit (the row is already gone) is
// logged, so an operator can remove the orphan; a sweep would too.
// ds:def id=prod-newstore-txwtyv4v
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

// uploadPolicy is the policy of every upload path. With a scanner, each
// stored upload is scanned before it is reported stored, and an infected
// one is deleted; a scanner that is down refuses uploads.
// ds:def id=prod-uploadpolicy-qfzpymft
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

// scannerFrom returns a clamd scanner when CLAMD_ADDR is set.
// ds:def id=prod-scannerfrom-7pbngzb2
func scannerFrom(lookup filesystem.LookupFunc) scan.Scanner {
	if addr, ok := lookup(envClamd); ok && addr != "" {
		return scan.Clamd{Network: "tcp", Address: addr}
	}
	return nil
}

// migrate applies the pending migrations and prints them. Run it once per
// deploy, before the new version serves; Migrate takes a lock, so two
// deploy jobs racing still apply each migration once.
// ds:def id=prod-migrate-bqqfw33y
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

// migrationSQL prints every migration as one script, for a review or for
// a team that applies schema changes with its own tool.
// ds:def id=prod-migrationsql-udawhkdu
func migrationSQL(store *records.Store, out io.Writer) error {
	script, err := store.MigrationSQL()
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, script)
	return err
}

// maintain is the scheduled clean-up: purge the trash past its retention,
// sweep direct uploads nobody confirmed, prune old audit entries. Each
// step runs even when an earlier one failed; every step can run
// again, continuing where it stopped.
// ds:def id=prod-maintain-9tanm49b
func maintain(ctx context.Context, store *records.Store, auditLog *audit.Plugin, media *filesystem.Disk, now time.Time, out io.Writer) error {
	purged, errTrash := store.SweepTrash(ctx, now.Add(-trashRetention))
	swept, errUploads := upload.Sweep(ctx, media, incomingDir, now.Add(-abandonedUploadAge))
	pruned, errAudit := auditLog.Prune(ctx, store, now.Add(-auditRetention))
	_, errOut := fmt.Fprintf(out, "purged %d trashed files, swept %d abandoned uploads, pruned %d audit entries\n", purged, swept, pruned)
	return errors.Join(errTrash, errUploads, errAudit, errOut)
}

// health answers 200 when every disk passes Check and 503 otherwise. Check
// writes, reads and deletes a small object on each disk, so use it for a
// readiness probe every few seconds, not for every request.
// ds:def id=prod-health-jnvrfde6
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

// mirrored returns a disk named name that writes to the primary disk and
// then to the backup, and reads from the primary. Records rows name the
// disk, so give the mirror the name the rows use.
// ds:def id=prod-mirrored-r5x49s84
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

// wiring is everything a command needs.
type wiring struct {
	db     *sql.DB
	disks  *filesystem.Manager
	media  *filesystem.Disk
	signer *signedurl.Signer
	store  *records.Store
	audit  *audit.Plugin
	logger *slog.Logger
}

// wire builds the wiring from the environment. Any missing or wrong
// setting fails here, before the process serves or migrates anything.
func wire(ctx context.Context, lookup filesystem.LookupFunc, logger *slog.Logger) (*wiring, error) {
	signer, err := newSigner(lookup)
	if err != nil {
		return nil, err
	}
	disks, err := openDisks(ctx, lookup, signer, interceptors(logger, recordMetric))
	if err != nil {
		return nil, err
	}
	w := &wiring{disks: disks, signer: signer, logger: logger}
	if w.media, err = disks.Disk(mediaDisk); err != nil {
		return nil, errors.Join(err, w.Close())
	}
	path, ok := lookup(envDatabase)
	if !ok || path == "" {
		return nil, errors.Join(fmt.Errorf("%s is not set", envDatabase), w.Close())
	}
	if w.db, err = sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)"); err != nil {
		return nil, errors.Join(err, w.Close())
	}
	if w.store, w.audit, err = newStore(w.db, disks, limitsFor(everyoneFree), logger); err != nil {
		return nil, errors.Join(err, w.Close())
	}
	return w, nil
}

// Close releases the database and the disks.
func (w *wiring) Close() error {
	var errDB error
	if w.db != nil {
		errDB = w.db.Close()
	}
	return errors.Join(errDB, w.disks.Close())
}

// everyoneFree stands in for the app's plan lookup.
func everyoneFree(context.Context, string) (string, error) { return planFree, nil }

// recordMetric stands in for a metrics client: contrib/otel builds on the
// same layer.Event.
func recordMetric(context.Context, layer.Event) {}

// serve mounts signed links at the path of FILES_URL and a health probe,
// and listens on ADDR.
// ds:def id=prod-serve-4e7aaa4b
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
