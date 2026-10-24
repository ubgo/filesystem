# Multi-tenancy: keeping tenants apart on shared storage

A SaaS app stores every tenant's files on one bucket, and one tenant must never read, overwrite or delete another's. This program shows the ways the library keeps them apart: a scoped disk per tenant (`Disk.Scope`) whose keys cannot reach outside its prefix, signed links issued from a scoped disk and why that disk must be in the Manager the handler resolves names with, a `layer.Guard` interceptor that checks every key against the request's tenant for code that builds keys itself, deleting a departing tenant with one prefix delete, the scope argument every `records` query filters by, and a disk per tenant opened from configuration in the tenant's region with the tenant's own domain. It uses the core `filesystem` package, `memory`, `signedurl`, `fshttp`, `layer`, `upload` and `records` (on SQLite). The guide [Multi-tenancy](../../docs/concepts/multi-tenancy.md) explains each step and how to choose a layout.

## Run it

```sh
cd examples
go run ./concepts-tenancy
go test ./concepts-tenancy
```

It needs no environment variables. The records step uses a private in-memory SQLite database (`file::memory:`), so no file is created. The signed-link step starts an in-process `httptest` server on a random loopback port and closes it before the program exits. The signing secret is fixed so the run is repeatable.

## What it does

```sh
go run ./concepts-tenancy
```

```text
acme reads logo.png: "acme logo"
shared disk holds: tenants/acme/logo.png tenants/globex/logo.png
acme reads ../globex/logo.png: invalid_key
scoped disk deletes its root: invalid_argument
after deleting globex: tenants/acme/logo.png
signed link /files/acme/report.pdf: 200
same link, scoped disk not in the Manager: 404
guard own key: <nil>
guard copy into another tenant: permission
globex asks for acme's file: not_found
acme: stored in files-eu-west-1 as tenants/acme/logo.png, served at https://files.acme.example/logo.png
globex: stored in files-us-east-1 as tenants/globex/logo.png, served at https://cdn.globex.example/logo.png
```

1. Two tenants write the same key, `logo.png`, through their scoped disks; the shared disk holds them under `tenants/acme/` and `tenants/globex/`, and `..` cannot climb out of a scope.
2. A scoped disk refuses to delete its own root, so removing a tenant is one `DeletePrefix` on the shared disk, done deliberately one level up.
3. A link signed by the `acme` scoped disk names that disk, so `fshttp.Signed` serves it when the disk is in its Manager and answers 404 when it is not.
4. A Guard on an unscoped disk lets a write under the request's tenant through and refuses a copy into another tenant with `permission`.
5. In `records`, a file uploaded under scope `acme` is `not_found` when asked for under `globex`.
6. Each tenant's disk is opened from configuration in the bucket of its own region, under its prefix, with its own domain for public links: `acme` lands in the EU bucket and is served from `files.acme.example`, `globex` in the US bucket from `cdn.globex.example`.

## The code

The disk one tenant's code receives: the shared disk seen through the tenant's prefix.

<!-- ds:block id=tenantdisk-c9d3jqwx -->
**tenantDisk** · [`examples/concepts-tenancy/main.go:76-78`](main.go#L76-L78)

```go
func tenantDisk(shared *filesystem.Disk, tenant string) (*filesystem.Disk, error) {
	return shared.Scope(tenant, filesystem.JoinKey(tenantsDir, tenant))
}
```
<!-- /ds:block hash=7f4361 -->

The same key for two tenants, and a traversal attempt refused.

<!-- ds:block id=scopeddisks-y4w23nt2 -->
**scopedDisks** · [`examples/concepts-tenancy/main.go:83-106`](main.go#L83-L106)

```go
func scopedDisks(ctx context.Context, shared *filesystem.Disk, out io.Writer) error {
	for _, tenant := range []string{"acme", "globex"} {
		d, err := tenantDisk(shared, tenant)
		if err != nil {
			return err
		}
		if _, err := d.WriteBytes(ctx, "logo.png", []byte(tenant+" logo"), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	acme, err := tenantDisk(shared, "acme")
	if err != nil {
		return err
	}
	b, err := acme.ReadAll(ctx, "logo.png")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "acme reads logo.png: %q\n", b)
	fmt.Fprintf(out, "shared disk holds: %s\n", strings.Join(keys(ctx, shared, ""), " "))
	_, err = acme.ReadAll(ctx, "../globex/logo.png")
	fmt.Fprintf(out, "acme reads ../globex/logo.png: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=249f78 -->

Deleting a tenant from the shared disk.

<!-- ds:block id=deletetenant-cvsd5bmn -->
**deleteTenant** · [`examples/concepts-tenancy/main.go:124-136`](main.go#L124-L136)

```go
func deleteTenant(ctx context.Context, shared *filesystem.Disk, out io.Writer) error {
	globex, err := tenantDisk(shared, "globex")
	if err != nil {
		return err
	}
	err = globex.DeletePrefix(ctx, "")
	fmt.Fprintf(out, "scoped disk deletes its root: %s\n", filesystem.CodeOf(err))
	if err := shared.DeletePrefix(ctx, filesystem.JoinKey(tenantsDir, "globex")); err != nil {
		return err
	}
	fmt.Fprintf(out, "after deleting globex: %s\n", strings.Join(keys(ctx, shared, ""), " "))
	return nil
}
```
<!-- /ds:block hash=e56b5f -->

Signed links from a scoped disk, served by a handler whose Manager holds it, then by one whose Manager does not.

<!-- ds:block id=signedlinks-vm866gh6 -->
**signedLinks** · [`examples/concepts-tenancy/more.go:27-63`](more.go#L27-L63)

```go
func signedLinks(ctx context.Context, out io.Writer) error {
	routes := http.NewServeMux()
	srv := httptest.NewServer(routes)
	defer srv.Close()
	signer, err := signedurl.New(srv.URL+signedPrefix, []signedurl.Key{{ID: "k1", Secret: demoSecret}})
	if err != nil {
		return err
	}
	shared, err := filesystem.NewDisk("shared", memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return err
	}
	acme, err := tenantDisk(shared, "acme")
	if err != nil {
		return err
	}
	if _, err := acme.WriteBytes(ctx, "report.pdf", []byte("%PDF-1.7 acme"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	disks, err := filesystem.NewManager(shared, acme)
	if err != nil {
		return err
	}
	routes.Handle(signedPrefix+"/", http.StripPrefix(signedPrefix, fshttp.Signed(disks, signer)))
	link, err := acme.SignedURL(ctx, "report.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	resp, err := http.Get(link.URL)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	path := strings.TrimPrefix(link.URL[:strings.IndexByte(link.URL, '?')], srv.URL)
	fmt.Fprintf(out, "signed link %s: %d\n", path, resp.StatusCode)
	return withoutScopedDisk(ctx, shared, signer, link.URL, srv.URL, out)
}
```
<!-- /ds:block hash=688f2a -->

<!-- ds:block id=withoutscopeddisk-w2gz9zvj -->
**withoutScopedDisk** · [`examples/concepts-tenancy/more.go:68-79`](more.go#L68-L79)

```go
func withoutScopedDisk(ctx context.Context, shared *filesystem.Disk, signer *signedurl.Signer, link, base string, out io.Writer) error {
	disks, err := filesystem.NewManager(shared)
	if err != nil {
		return err
	}
	h := http.StripPrefix(signedPrefix, fshttp.Signed(disks, signer))
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, strings.TrimPrefix(link, base), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	fmt.Fprintf(out, "same link, scoped disk not in the Manager: %d\n", rec.Code)
	return nil
}
```
<!-- /ds:block hash=91d0c9 -->

The Guard check and the disk it protects: every key, and the target of a copy or move, must lie under the request's tenant.

<!-- ds:block id=sametenant-4fket7z9 -->
**sameTenant** · [`examples/concepts-tenancy/more.go:89-101`](more.go#L89-L101)

```go
func sameTenant(ctx context.Context, op *filesystem.Op) error {
	tenant, _ := ctx.Value(tenantKey{}).(string)
	if tenant == "" {
		return errors.New("no tenant in context")
	}
	dir := filesystem.JoinKey(tenantsDir, tenant)
	for _, k := range append([]string{op.Key, op.Target}, op.Keys...) {
		if k != "" && !filesystem.KeyInDir(k, dir) {
			return fmt.Errorf("key %q is not in %s", k, dir)
		}
	}
	return nil
}
```
<!-- /ds:block hash=8ddba3 -->

<!-- ds:block id=guardkeys-u2scjnm4 -->
**guardKeys** · [`examples/concepts-tenancy/more.go:107-118`](more.go#L107-L118)

```go
func guardKeys(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("shared", memory.New(), filesystem.WithInterceptors(layer.Guard(sameTenant)))
	if err != nil {
		return err
	}
	ctx = context.WithValue(ctx, tenantKey{}, "acme")
	_, err = disk.WriteBytes(ctx, "tenants/acme/a.txt", []byte("x"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "guard own key: %v\n", err)
	_, err = disk.Copy(ctx, "tenants/acme/a.txt", "tenants/globex/a.txt")
	fmt.Fprintf(out, "guard copy into another tenant: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=7d4911 -->

The records scope: a file ID from one tenant is not found in another.

<!-- ds:block id=recordscopes-r5r3w6ux -->
**recordScopes** · [`examples/concepts-tenancy/more.go:123-153`](more.go#L123-L153)

```go
func recordScopes(ctx context.Context, out io.Writer) error {
	db, err := sql.Open("sqlite", memoryDB)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	disk, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		return err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return err
	}
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks)
	if err != nil {
		return err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return err
	}
	p := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"application/pdf"}}
	f, err := store.Upload(ctx, "acme", "media", strings.NewReader("%PDF-1.7 acme"), "plan.pdf", p)
	if err != nil {
		return err
	}
	_, err = store.File(ctx, "globex", f.ID)
	fmt.Fprintf(out, "globex asks for acme's file: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=041b13 -->

A disk per tenant from configuration: the tenant's region, prefix and domain.

<!-- ds:block id=opentenant-wujwfu92 -->
**openTenant** · [`examples/concepts-tenancy/regions.go:59-65`](regions.go#L59-L65)

```go
func openTenant(ctx context.Context, mux *filesystem.Mux, t tenantConfig) (*filesystem.Disk, error) {
	q := url.Values{filesystem.ParamPrefix: {filesystem.JoinKey(tenantsDir, t.Name)}}
	dsn := bucketScheme + "://" + t.Bucket + "?" + q.Encode()
	return mux.Open(ctx, t.Name, dsn,
		filesystem.WithPublicURL(t.Domain),
		filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
}
```
<!-- /ds:block hash=cc4050 -->

<!-- ds:block id=regionsanddomains-mf34m22t -->
**regionsAndDomains** · [`examples/concepts-tenancy/regions.go:71-109`](regions.go#L71-L109)

```go
func regionsAndDomains(ctx context.Context, out io.Writer) error {
	buckets := map[string]*memory.Driver{}
	mux, err := regionalMux(buckets)
	if err != nil {
		return err
	}
	disks, err := filesystem.NewManager()
	if err != nil {
		return err
	}
	for _, t := range tenantConfigs {
		d, err := openTenant(ctx, mux, t)
		if err != nil {
			return err
		}
		if err := disks.Add(d); err != nil {
			return err
		}
	}
	for _, t := range tenantConfigs {
		d, err := disks.Disk(t.Name)
		if err != nil {
			return err
		}
		if _, err := d.WriteBytes(ctx, "logo.png", []byte(t.Name), filesystem.WriteOptions{}); err != nil {
			return err
		}
		link, err := d.PublicURL(ctx, "logo.png")
		if err != nil {
			return err
		}
		stored, err := buckets[t.Bucket].Stat(ctx, filesystem.JoinKey(tenantsDir, t.Name, "logo.png"))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: stored in %s as %s, served at %s\n", t.Name, t.Bucket, stored.Key, link)
	}
	return nil
}
```
<!-- /ds:block hash=4e8202 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The layout constants, the program, scoped disks and deleting a tenant |
| `more.go` | Signed links from a scoped disk, the tenant Guard and the records scope |
| `regions.go` | A disk per tenant from configuration: data residency and a custom domain per tenant |
| `main_test.go` | Runs the program and checks the lines the guide quotes |

## Next

- The guide: [Multi-tenancy](../../docs/concepts/multi-tenancy.md), including quotas per tenant and per-tenant disks from configuration.
- Related examples: [concepts-keys](../concepts-keys), [concepts-serving](../concepts-serving), [concepts-interceptors](../concepts-interceptors), [recipe-saas-tenants](../recipe-saas-tenants), [records-quota](../records-quota).
