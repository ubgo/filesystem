# SaaS tenants: per-tenant files, quotas and buckets

The file storage of a multi-tenant SaaS product: every tenant's files are rows in its own `records` scope, stored under its own key prefix (`tenants/<id>/`), charged to its own plan's quota, optionally on a bucket of its own, and removed in one call when the tenant leaves. Another tenant's file ID is a `404`, never a `403` that would confirm it exists. It uses `records` with the `quota` plugin, `Disk.Scope` for per-tenant app data, `filesystem.Mux` and `OpenManager` to build disks from environment variables (local directories or S3 buckets), and `signedurl` with `fshttp.Signed` for downloads. The guide is [SaaS tenants](../../docs/recipes/saas-tenants.md).

## Run it

```sh
cd examples
go run ./recipe-saas-tenants
```

It creates a new temporary directory (printed at start-up) holding `app.db` (SQLite with the `fs_` tables) and two local disks, `shared/` (the `files` disk every tenant uses by default) and `globex/` (the `tenant-globex` disk only globex uses), and listens on `http://localhost:8113`. Stop it with Ctrl-C (or SIGTERM): it shuts down cleanly and removes the temporary directory.

The demo has two tenants, acme on the free plan (1 MiB, three files) and globex on pro (1 GiB), with the API keys `key-acme`, `key-globex` and the operator key `key-admin`. Setting `STORAGE_DISKS` switches from the demo directories to the real environment, for example `STORAGE_DISKS=files,tenant-globex` with `STORAGE_FILES_DSN=s3://…` and `STORAGE_TENANT_GLOBEX_DSN=s3://…` (the `file:` and `s3:` schemes are registered). The signing secret is a fixed demo value in `main`; replace it in a real app.

Run the tests with `go test ./recipe-saas-tenants`.

## Walkthrough

1. Upload as acme. The key carries the tenant prefix and the file lands on the shared disk:

```sh
printf 'Q3 plan\n' > plan.txt
curl -s -H "Authorization: Bearer key-acme" -F file=@plan.txt localhost:8113/files
```

```json
{"id":"fil_iwiw7vjhiv44v632nwxvymuuf4","filename":"plan.txt","content_type":"text/plain; charset=utf-8","size":8,"disk":"files","key":"tenants/acme/files/ws2xhx46rrruzm2sqxiulwn5lf.txt"}
```

2. acme downloads it through a five-minute signed link:

```sh
curl -s -i -H "Authorization: Bearer key-acme" localhost:8113/files/fil_iwiw7vjhiv44v632nwxvymuuf4
```

```text
HTTP/1.1 302 Found
Content-Type: text/html; charset=utf-8
Location: http://localhost:8113/dl/files/tenants/acme/files/ws2xhx46rrruzm2sqxiulwn5lf.txt?ct=text%2Fplain%3B+charset%3Dutf-8&exp=1791256805&kid=k1&m=GET&sig=3a9754eb48a4283006bb70af7ad86ae847429a3b5b6641ccbfd16438d35e98d3
Date: Tue, 06 Oct 2026 03:15:05 GMT
Content-Length: 251
```

Following the link (`curl -L`) returns `Q3 plan`; the same link with its signature altered answers `403 Forbidden`.

3. globex asks for acme's file ID, then lists its own files. It gets the same `404` an unknown ID would get, and an empty list:

```sh
curl -s -H "Authorization: Bearer key-globex" localhost:8113/files/fil_iwiw7vjhiv44v632nwxvymuuf4
curl -s -H "Authorization: Bearer key-globex" localhost:8113/files
```

```text
{"error":"Not Found","code":"not_found"}
[]
```

4. globex has a disk of its own in the configuration, so its upload goes there:

```sh
curl -s -H "Authorization: Bearer key-globex" -F file=@plan.txt localhost:8113/files
```

```json
{"id":"fil_wlhwo4jlsknz6bzil6kdz6bukr","filename":"plan.txt","content_type":"text/plain; charset=utf-8","size":8,"disk":"tenant-globex","key":"tenants/globex/files/3pyxnov77iqenh632akgmkou5t.txt"}
```

5. acme uploads twice more, reaching the free plan's three files. The fourth upload is refused with `507`, and usage shows the plan:

```sh
curl -s -i -H "Authorization: Bearer key-acme" -F file=@plan.txt localhost:8113/files
curl -s -H "Authorization: Bearer key-acme" localhost:8113/usage
```

```text
HTTP/1.1 507 Insufficient Storage
Content-Type: application/json
Date: Tue, 06 Oct 2026 03:15:05 GMT
Content-Length: 92

{"error":"quota: scope \"acme\" would exceed its limit of 3 files","code":"quota_exceeded"}
{"plan":"free","bytes":24,"files":3,"limit_bytes":1048576,"limit_files":3}
```

6. Raw app data goes through the tenant's scoped disk, where keys are relative to `tenants/acme` and cannot climb out (the third request answers `400`):

```sh
curl -s -X PUT -H "Authorization: Bearer key-acme" -d '{"color":"teal"}' localhost:8113/settings/theme
curl -s -H "Authorization: Bearer key-acme" localhost:8113/settings/theme
curl -s -X PUT -H "Authorization: Bearer key-acme" -d '{}' "localhost:8113/settings/..%2F..%2Fglobex%2Fx"
```

```text
{"key":"settings/theme.json"}
{"color":"teal"}{"error":"key contains a \"..\" segment","code":"invalid_key"}
```

7. A request without a known key is refused (`403`) before any handler runs:

```sh
curl -s localhost:8113/files
```

```json
{"error":"Forbidden","code":"permission"}
```

8. Offboard acme. A tenant key cannot; the operator key can, and acme's usage drops to zero:

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

Afterwards the data directory holds `app.db` and one object, globex's file under `globex/tenants/globex/files/`. acme's three files and its settings document are gone.

## The code

`planLimits` maps each billing plan to a quota; zero in a field is unlimited.

<!-- ds:block id=planlimits-wfzvqwrx -->
**planLimits** · [`examples/recipe-saas-tenants/main.go:90-93`](main.go#L90-L93)

```go
var planLimits = map[Plan]quota.Limit{
	PlanFree: {Bytes: 1 << 20, Files: 3},
	PlanPro:  {Bytes: 1 << 30},
}
```
<!-- /ds:block hash=213ca3 -->

`demoEnv` is the storage configuration used when `STORAGE_DISKS` is unset: the same variable names a production deployment sets, pointing at two local directories.

<!-- ds:block id=demoenv-t3e4nky5 -->
**demoEnv** · [`examples/recipe-saas-tenants/main.go:130-137`](main.go#L130-L137)

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

`openDisks` builds every configured disk through a `Mux` that knows the `file:` and `s3:` schemes, so the same code opens directories in development and buckets in production.

<!-- ds:block id=opendisks-jkxkrcu6 -->
**openDisks** · [`examples/recipe-saas-tenants/main.go:144-153`](main.go#L144-L153)

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

`newQuota` charges every tenant against its plan, read when a file is recorded, so an upgrade applies to the next upload. The charge is one conditional `UPDATE` in the upload's transaction.

<!-- ds:block id=newquota-2stc2hye -->
**newQuota** · [`examples/recipe-saas-tenants/main.go:158-166`](main.go#L158-L166)

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

`authenticate` resolves the API key to a tenant once and puts it in the context; handlers never read a tenant from the URL or a header the client picks.

<!-- ds:block id=app-authenticate-m82ras65 -->
**App.authenticate** · [`examples/recipe-saas-tenants/main.go:237-246`](main.go#L237-L246)

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

`diskFor` picks the tenant's dedicated disk when one is configured, else the shared one; `policyFor` builds the upload policy per tenant so keys carry the tenant prefix.

<!-- ds:block id=app-diskfor-puec8wbc -->
**App.diskFor** · [`examples/recipe-saas-tenants/main.go:260-265`](main.go#L260-L265)

```go
func (a *App) diskFor(tenant string) string {
	if _, err := a.Disks.Disk(dedicatedPrefix + tenant); err == nil {
		return dedicatedPrefix + tenant
	}
	return sharedDisk
}
```
<!-- /ds:block hash=db8931 -->

<!-- ds:block id=policyfor-p2nyj2hu -->
**policyFor** · [`examples/recipe-saas-tenants/main.go:274-280`](main.go#L274-L280)

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

`uploadFile` stores and records the file in the tenant's scope; `download` looks the file up in that scope only and redirects to a signed link.

<!-- ds:block id=app-uploadfile-v7gas2px -->
**App.uploadFile** · [`examples/recipe-saas-tenants/main.go:301-320`](main.go#L301-L320)

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

<!-- ds:block id=app-download-8j5huwey -->
**App.download** · [`examples/recipe-saas-tenants/main.go:346-366`](main.go#L346-L366)

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

`tenantDisk` is the tenant's view of its disk (`Disk.Scope`), and `putSettings` writes app data through it.

<!-- ds:block id=app-tenantdisk-6tdetehg -->
**App.tenantDisk** · [`examples/recipe-saas-tenants/main.go:372-378`](main.go#L372-L378)

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
**App.putSettings** · [`examples/recipe-saas-tenants/main.go:386-404`](main.go#L386-L404)

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

`purgeFiles` pages through the tenant's scope and purges each file (records has no single "delete a scope" call), and `offboard` runs it, then deletes whatever is left under the prefix. An interrupted run is safe to repeat.

<!-- ds:block id=app-purgefiles-ee7ughab -->
**App.purgeFiles** · [`examples/recipe-saas-tenants/main.go:458-478`](main.go#L458-L478)

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
**App.offboard** · [`examples/recipe-saas-tenants/main.go:492-516`](main.go#L492-L516)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | Plans, the demo tenant directory and keys, disks from the environment, the quota, the routes and handlers, and offboarding. |
| `main_test.go` | Tenant isolation (another tenant's ID is `404`, lists are separate), quota per plan, and the scoped disk plus offboarding. |

## Next

- The guide: [SaaS tenants](../../docs/recipes/saas-tenants.md), with edge cases and production notes (offboarding as a job, audit retention, a dedicated bucket as configuration).
- [Multi-tenancy](../../docs/concepts/multi-tenancy.md), [Quota](../../docs/plugins/quota.md) and [Configuration](../../docs/concepts/configuration.md).
- The [concepts-tenancy](../concepts-tenancy) and [records-quota](../records-quota) examples.
- Related recipes: [recipe-team-drive](../recipe-team-drive) (a fuller product on the same scopes), [recipe-account-deletion](../recipe-account-deletion), [recipe-exports](../recipe-exports).
