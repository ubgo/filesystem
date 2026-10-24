# SaaS tenants

A B2B product stores files for many customers in one system, and the one bug it cannot afford is customer A seeing customer B's invoice. This recipe is for any multi-tenant SaaS (workspaces, organizations, accounts) that needs every file read, listed, counted and deleted per tenant, with plan-based storage limits and, for the customers who ask, a bucket of their own.

The program is [examples/recipe-saas-tenants](../../examples/recipe-saas-tenants). Every code block below is copied from it, and every transcript is from running it.

## What you get

- Every file row belongs to one tenant's scope, and every query filters by it: another tenant's file ID is a `404`, never a `403` that would confirm it exists.
- The tenant comes from the credential (an API key here, a session in your app), never from the URL or a header the client picks.
- Every object of a tenant lives under `tenants/<id>/`, so it can be listed, copied or deleted as one directory.
- A per-tenant view of the disk (`Disk.Scope`) for app data that is not a user file, from which no key can climb out.
- Storage limits per billing plan, charged in the upload's transaction, refused with `507`.
- Disks from the environment: a shared bucket for most tenants, a dedicated one for any tenant whose disk is configured.
- Offboarding: one call removes a tenant's rows, quota usage and objects.

## The design

| Piece | Why |
|---|---|
| `records` scope = tenant ID | Every `Store` method takes the scope and filters by it in SQL. `Store.File(ctx, "globex", acmeFileID)` finds nothing. |
| Authentication middleware | Resolves the credential to a tenant once and puts it in the context. Handlers never read a tenant from the request. |
| `upload.RandomKey("tenants/<id>/files")` per request | Keys carry the tenant prefix. The policy is built per tenant because a `KeyFunc` sees only the file, not the request. |
| `Disk.Scope(name, "tenants/<id>")` | A disk whose keys are relative to the tenant prefix. Keys are cleaned before the prefix is added, so `../globex/x` is `CodeInvalidKey`. |
| `quota` plugin with a `LimitFunc` over plans | Limits come from the app's plan table; usage is charged with one conditional `UPDATE` in the upload's transaction, so concurrent uploads cannot both pass the limit. |
| `Mux.OpenManager("STORAGE_", …)` | Disks come from configuration. A tenant whose `tenant-<id>` disk is configured gets it; everyone else shares `files`. |
| `Store.Purge` per file, then `Disk.DeletePrefix` | Records offers no "delete a whole scope" call; the app pages through the scope and purges each file, then removes whatever is left under the prefix. |

## Step 1: Plans and the directory

The plan table is the app's. The quota plugin only needs a function from a scope to a `quota.Limit`.

<!-- ds:block id=planlimits-wfzvqwrx -->
**planLimits** · [`examples/recipe-saas-tenants/main.go:90-93`](../../examples/recipe-saas-tenants/main.go#L90-L93)

```go
var planLimits = map[Plan]quota.Limit{
	PlanFree: {Bytes: 1 << 20, Files: 3},
	PlanPro:  {Bytes: 1 << 30},
}
```
<!-- /ds:block hash=213ca3 -->

<!-- ds:block id=newquota-2stc2hye -->
**newQuota** · [`examples/recipe-saas-tenants/main.go:158-166`](../../examples/recipe-saas-tenants/main.go#L158-L166)

```go
func newQuota(dir Directory) (*quota.Plugin, error) {
	return quota.New(func(_ context.Context, scope string) (quota.Limit, error) {
		t, ok := dir.Tenants[scope]
		if !ok {
			return quota.Limit{}, filesystem.Errorf(filesystem.CodePermission, "unknown tenant %q", scope)
		}
		return planLimits[t.Plan], nil
	})
}
```
<!-- /ds:block hash=29ecab -->

The function runs whenever a file is recorded, so a plan upgrade applies to the next upload. An unknown tenant is an error, which refuses the upload rather than letting it through with no limit.

## Step 2: Disks from the environment

<!-- ds:block id=opendisks-jkxkrcu6 -->
**openDisks** · [`examples/recipe-saas-tenants/main.go:144-153`](../../examples/recipe-saas-tenants/main.go#L144-L153)

```go
func openDisks(ctx context.Context, lookup filesystem.LookupFunc, signer *signedurl.Signer) (*filesystem.Manager, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(local.Scheme, local.Opener); err != nil {
		return nil, err
	}
	if err := mux.Register(s3.Scheme, s3.Opener); err != nil {
		return nil, err
	}
	return mux.OpenManager(ctx, envPrefix, lookup, filesystem.WithURLSigner(signer))
}
```
<!-- /ds:block hash=5abe05 -->

In production the configuration is the environment:

```sh
STORAGE_DISKS=files,tenant-globex
STORAGE_FILES_DSN='s3://acme-saas-files?region=eu-west-1'
STORAGE_TENANT_GLOBEX_DSN='s3://globex-dedicated?region=us-east-1'
```

S3 credentials come from the AWS default chain, never from the DSN. Without `STORAGE_DISKS` the demo uses two local directories:

<!-- ds:block id=demoenv-t3e4nky5 -->
**demoEnv** · [`examples/recipe-saas-tenants/main.go:130-137`](../../examples/recipe-saas-tenants/main.go#L130-L137)

```go
func demoEnv(dataDir string) filesystem.LookupFunc {
	env := map[string]string{
		envPrefix + filesystem.EnvDisks:                         sharedDisk + "," + dedicatedPrefix + "globex",
		filesystem.EnvName(envPrefix, sharedDisk):               "file:" + filepath.Join(dataDir, "shared"),
		filesystem.EnvName(envPrefix, dedicatedPrefix+"globex"): "file:" + filepath.Join(dataDir, "globex"),
	}
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}
```
<!-- /ds:block hash=6804ea -->

`filesystem.EnvName` builds the variable name from the disk name (`tenant-globex` becomes `STORAGE_TENANT_GLOBEX_DSN`). A missing or malformed variable stops start-up with an error that names the variable, never its value. The [configuration](../concepts/configuration.md) page lists every Mux parameter, including `prefix=`, which scopes a whole disk to a prefix from the DSN.

Choosing the disk is one lookup:

<!-- ds:block id=app-diskfor-puec8wbc -->
**App.diskFor** · [`examples/recipe-saas-tenants/main.go:260-265`](../../examples/recipe-saas-tenants/main.go#L260-L265)

```go
func (a *App) diskFor(tenant string) string {
	if _, err := a.Disks.Disk(dedicatedPrefix + tenant); err == nil {
		return dedicatedPrefix + tenant
	}
	return sharedDisk
}
```
<!-- /ds:block hash=db8931 -->

The file row records which disk holds its object, so moving a tenant to its own bucket later affects new uploads only; existing files keep working from the shared disk until you copy them ([move to S3](move-to-s3.md) shows the copy).

## Step 3: The tenant comes from the credential

<!-- ds:block id=app-authenticate-m82ras65 -->
**App.authenticate** · [`examples/recipe-saas-tenants/main.go:237-246`](../../examples/recipe-saas-tenants/main.go#L237-L246)

```go
func (a *App) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.Dir.Keys[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			writeError(w, filesystem.Errorf(filesystem.CodePermission, "unknown API key"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}
```
<!-- /ds:block hash=c82253 -->

Everything after this middleware reads the tenant from the context. Never accept `?tenant=` or `X-Tenant`: a client can send any value.

## Step 4: Upload into the tenant

<!-- ds:block id=policyfor-p2nyj2hu -->
**policyFor** · [`examples/recipe-saas-tenants/main.go:274-280`](../../examples/recipe-saas-tenants/main.go#L274-L280)

```go
func policyFor(tenant string) upload.Policy {
	return upload.Policy{
		MaxSize: maxFileSize,
		Allowed: []string{"image/*", "application/pdf", "text/plain", "text/csv"},
		Key:     upload.RandomKey(prefixOf(tenant) + "/files"),
	}
}
```
<!-- /ds:block hash=b4af7a -->

<!-- ds:block id=app-uploadfile-v7gas2px -->
**App.uploadFile** · [`examples/recipe-saas-tenants/main.go:301-320`](../../examples/recipe-saas-tenants/main.go#L301-L320)

```go
func (a *App) uploadFile(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenantOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFileSize+64<<10)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "send the file in the form field \"file\""))
		return
	}
	defer func() { _ = file.Close() }()
	f, err := a.Store.Upload(r.Context(), tenant, a.diskFor(tenant), file, header.Filename, policyFor(tenant))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toResponse(f))
}
```
<!-- /ds:block hash=068338 -->

`Store.Upload` takes the scope, the disk and the policy. The quota plugin's `BeforeCreate` runs inside the same transaction as the row insert; past the limit it fails with `CodeQuotaExceeded`, the transaction rolls back, and the stored object is deleted again.

## Step 5: Read only within the tenant

<!-- ds:block id=app-download-8j5huwey -->
**App.download** · [`examples/recipe-saas-tenants/main.go:346-366`](../../examples/recipe-saas-tenants/main.go#L346-L366)

```go
func (a *App) download(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenantOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	f, err := a.Store.File(r.Context(), tenant, r.PathValue("id"))
	if err == nil && f.DeletedAt != nil {
		err = filesystem.Errorf(filesystem.CodeNotFound, "file is in the trash")
	}
	if err != nil {
		writeError(w, err)
		return
	}
	link, err := a.Store.URL(r.Context(), f, linkLifetime)
	if err != nil {
		writeError(w, err)
		return
	}
	http.Redirect(w, r, link, http.StatusFound)
}
```
<!-- /ds:block hash=62fd02 -->

`Store.File` includes trashed files (so an admin screen can show them); the handler treats a trashed file as missing. The redirect goes to a signed link on the disk; anyone holding the link can download until it expires, which is the point of a link, so keep the lifetime short ([private downloads](private-downloads.md)).

## Step 6: A tenant's own view of the disk

Some tenant data is not a user file: settings, generated reports, imported assets. It does not need a row, but it must stay inside the tenant's prefix.

<!-- ds:block id=app-tenantdisk-6tdetehg -->
**App.tenantDisk** · [`examples/recipe-saas-tenants/main.go:372-378`](../../examples/recipe-saas-tenants/main.go#L372-L378)

```go
func (a *App) tenantDisk(tenant string) (*filesystem.Disk, error) {
	disk, err := a.Disks.Disk(a.diskFor(tenant))
	if err != nil {
		return nil, err
	}
	return disk.Scope(disk.Name()+"."+tenant, prefixOf(tenant))
}
```
<!-- /ds:block hash=b72be5 -->

<!-- ds:block id=app-putsettings-tcq5z7vn -->
**App.putSettings** · [`examples/recipe-saas-tenants/main.go:386-404`](../../examples/recipe-saas-tenants/main.go#L386-L404)

```go
func (a *App) putSettings(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenantOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	disk, err := a.tenantDisk(tenant)
	if err != nil {
		writeError(w, err)
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxSettingsSize)
	e, err := disk.Write(r.Context(), settingsKey(r.PathValue("name")), body, filesystem.WriteOptions{ContentType: "application/json", MaxSize: maxSettingsSize})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": e.Key})
}
```
<!-- /ds:block hash=280ad1 -->

The scoped disk shares the driver, interceptors and signer of its parent. Its name (`files.acme`) appears in logs and errors. It is not in the Manager, so a signed URL for it would not resolve in `fshttp.Signed`; add scoped disks to the Manager (`Manager.Add`) if you sign links to them.

## Step 7: Offboarding

<!-- ds:block id=app-purgefiles-ee7ughab -->
**App.purgeFiles** · [`examples/recipe-saas-tenants/main.go:458-478`](../../examples/recipe-saas-tenants/main.go#L458-L478)

```go
func (a *App) purgeFiles(ctx context.Context, tenant string) (int, error) {
	purged := 0
	for _, trashed := range []bool{false, true} {
		for {
			page, err := a.Store.Files(ctx, records.FileQuery{Scope: tenant, Trashed: trashed})
			if err != nil {
				return purged, err
			}
			if len(page.Files) == 0 {
				break
			}
			for _, f := range page.Files {
				if err := a.Store.Purge(ctx, tenant, f.ID); err != nil {
					return purged, err
				}
				purged++
			}
		}
	}
	return purged, nil
}
```
<!-- /ds:block hash=2ce806 -->

<!-- ds:block id=app-offboard-htwpfehk -->
**App.offboard** · [`examples/recipe-saas-tenants/main.go:492-516`](../../examples/recipe-saas-tenants/main.go#L492-L516)

```go
func (a *App) offboard(w http.ResponseWriter, r *http.Request) {
	if p, _ := r.Context().Value(principalKey{}).(Principal); !p.Admin {
		writeError(w, filesystem.Errorf(filesystem.CodePermission, "operator key required"))
		return
	}
	tenant := r.PathValue("id")
	if _, ok := a.Dir.Tenants[tenant]; !ok {
		writeError(w, filesystem.Errorf(filesystem.CodeNotFound, "unknown tenant %q", tenant))
		return
	}
	n, err := a.purgeFiles(r.Context(), tenant)
	if err != nil {
		writeError(w, err)
		return
	}
	disk, err := a.Disks.Disk(a.diskFor(tenant))
	if err == nil {
		err = disk.DeletePrefix(r.Context(), prefixOf(tenant))
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, offboardResponse{Tenant: tenant, Purged: n})
}
```
<!-- /ds:block hash=72d441 -->

`Store.Purge` runs each plugin's `BeforePurge` hook, so quota usage is released and plugin rows (variants, versions, share links, tags) go with the file. `DeletePrefix` then removes raw objects and any orphan a failed deletion left. Both steps are idempotent, so rerunning after a failure finishes the job.

## Try it

```sh
cd examples
go run ./recipe-saas-tenants
```

```text
listening on http://localhost:8113 data in /var/folders/sk/.../T/recipe-saas-tenants-2696435957
```

Upload as acme, then try to read it as globex:

```sh
printf 'Q3 plan\n' > plan.txt
curl -s -H "Authorization: Bearer key-acme" -F file=@plan.txt localhost:8113/files
```

```json
{"id":"fil_bosct44ea6dkqkyj7whri5oijz","filename":"plan.txt","content_type":"text/plain; charset=utf-8","size":8,"disk":"files","key":"tenants/acme/files/alef6vrhgjoemyome3qa2ui3un.txt"}
```

```sh
curl -s -i -H "Authorization: Bearer key-acme" localhost:8113/files/fil_bosct44ea6dkqkyj7whri5oijz
```

```text
HTTP/1.1 302 Found
Location: http://localhost:8113/dl/files/tenants/acme/files/alef6vrhgjoemyome3qa2ui3un.txt?ct=text%2Fplain%3B+charset%3Dutf-8&exp=1791256762&kid=k1&m=GET&sig=f74bbbb760983b1e517f0d44c21bf4f76d1ced063452b35e58c6e2ec3443a04d
```

```sh
curl -s -H "Authorization: Bearer key-globex" localhost:8113/files/fil_bosct44ea6dkqkyj7whri5oijz
curl -s -H "Authorization: Bearer key-globex" localhost:8113/files
```

```text
{"error":"Not Found","code":"not_found"}
[]
```

Check: globex gets the same `404` an unknown ID would get, and an empty listing.

globex has a disk of its own in the configuration:

```sh
curl -s -H "Authorization: Bearer key-globex" -F file=@plan.txt localhost:8113/files
```

```json
{"id":"fil_7kifyt6iijvuhcrvepgp2vepal","filename":"plan.txt","content_type":"text/plain; charset=utf-8","size":8,"disk":"tenant-globex","key":"tenants/globex/files/xqsr2zwmbz5tzs2m3r47uoms6r.txt"}
```

acme is on the free plan (three files). The fourth upload is refused:

```text
HTTP/1.1 507 Insufficient Storage
Content-Type: application/json

{"error":"quota: scope \"acme\" would exceed its limit of 3 files","code":"quota_exceeded"}
```

```sh
curl -s -H "Authorization: Bearer key-acme" localhost:8113/usage
```

```json
{"plan":"free","bytes":24,"files":3,"limit_bytes":1048576,"limit_files":3}
```

Raw data through the tenant disk; the key is relative to the tenant prefix:

```sh
curl -s -X PUT -H "Authorization: Bearer key-acme" -d '{"color":"teal"}' localhost:8113/settings/theme
curl -s -H "Authorization: Bearer key-acme" localhost:8113/settings/theme
```

```text
{"key":"settings/theme.json"}
{"color":"teal"}
```

Offboard acme. A tenant key cannot; the operator key can:

```sh
curl -s -X DELETE -H "Authorization: Bearer key-acme" localhost:8113/admin/tenants/acme
curl -s -X DELETE -H "Authorization: Bearer key-admin" localhost:8113/admin/tenants/acme
curl -s -H "Authorization: Bearer key-acme" localhost:8113/usage
```

```text
{"error":"Forbidden","code":"permission"}
{"tenant":"acme","files_purged":3}
{"plan":"free","bytes":0,"files":0,"limit_bytes":1048576,"limit_files":3}
```

Check: the only object left in the data directory is globex's, under `globex/tenants/globex/files/`. acme's three files and its settings document are gone.

## Edge cases this handles

- A guessed or leaked file ID from another tenant: `404`, the same as an unknown ID.
- Concurrent uploads near the limit: the quota charge is one conditional `UPDATE`, so at most one of them gets the last slot; the others are `507` and leave no object.
- A rolled-back upload un-charges itself, because the charge is in the same transaction.
- A trashed file still counts against the quota (its bytes are still stored) until it is purged.
- A key with `..` on the tenant disk: `CodeInvalidKey`, before any driver call.
- Offboarding interrupted part-way: rows already purged stay purged; a rerun purges the rest and deletes the prefix.

## For production

- Replace the API-key map with your session or token lookup; keep the rule that the tenant comes from the credential.
- Run offboarding as a background job with an audit record, after the retention period your contract promises; offer an export first ([exports](exports.md)).
- The `audit` plugin's log is append-only and is not purged with the files. `audit.Plugin.Prune` applies a retention period across every scope; it has no per-scope delete, so a departed tenant's entries age out with everyone else's ([audit](../plugins/audit.md)).
- Records has no single call that purges a whole scope; the loop above is the supported way, and it runs every plugin hook. On very large tenants, run it in batches and let the job resume.
- A dedicated bucket per tenant is configuration, not code: add `tenant-<id>` to `STORAGE_DISKS` with its DSN. Existing files stay where their rows say until you copy them and update `fs_files.disk`.
- A tenant on a dedicated disk can have its own encryption key: S3's `sse=aws:kms` DSN parameter ([S3](../adapters/s3.md)).
- Put `layer.Limit` or a per-tenant rate limit in front of uploads so one tenant cannot saturate the storage backend for the others.

## Related

- [Multi-tenancy](../concepts/multi-tenancy.md): scopes, scoped disks and per-tenant disks in depth.
- [Quota](../plugins/quota.md): limits, usage, `Recount`.
- [Configuration](../concepts/configuration.md): Manager, Mux, DSNs and `OpenManager`.
- [Account deletion](account-deletion.md): removing one user's files rather than a whole tenant.
- [Team drive](team-drive.md): a fuller product on the same scopes.
