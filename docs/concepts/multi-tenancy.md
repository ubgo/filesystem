# Multi-tenancy

A SaaS application keeps many customers' files on shared storage: one bucket, one database. The worst storage bug such an application can have is one tenant reading or overwriting another's files. This page shows the tools the library gives for keeping tenants apart (key prefixes, scoped disks, a guard on every operation, the records scope, per-tenant quotas) and how to delete everything a departing tenant stored. Read it before your second customer signs up.

The snippets come from `examples/concepts-tenancy`. Run it with `go run ./concepts-tenancy` from the `examples` directory.

## Choosing a layout

| Layout | How | Isolation | When |
|---|---|---|---|
| Prefix per tenant on a shared disk | `tenants/<id>/…` keys, a scoped disk per request | by the library: a scoped disk cannot address another prefix | the default for most SaaS products |
| Disk per tenant from configuration | a DSN with `prefix=tenants/<id>`, or a bucket per tenant | by configuration, and by the provider's IAM for separate buckets | a few large tenants, contractual separation, data residency |
| Shared keys with a guard | the application builds keys, `layer.Guard` checks each one | by a check on every operation | code that must see several tenants' keys at once (admin tools, jobs) |

The database side is the same in every layout: the records module filters every query by a `scope` string you choose, normally the tenant ID.

## Step 1 — A scoped disk per tenant

`disk.Scope(name, prefix)` returns a Disk that sees only the keys under `prefix`: key `logo.png` on the scoped disk is `tenants/acme/logo.png` on the shared one. It shares the shared disk's driver, interceptors, URL signer and settings, and a public URL base gains the prefix. Keys are cleaned before the prefix is added, so no key, `..` included, can leave it. Give the code that serves a request the tenant's scoped disk, never the shared one:

<!-- ds:block id=tenantdisk-c9d3jqwx -->
**tenantDisk** · [`examples/concepts-tenancy/main.go:76-78`](../../examples/concepts-tenancy/main.go#L76-L78)

```go
func tenantDisk(shared *filesystem.Disk, tenant string) (*filesystem.Disk, error) {
	return shared.Scope(tenant, filesystem.JoinKey(tenantsDir, tenant))
}
```
<!-- /ds:block hash=7f4361 -->

<!-- ds:block id=scopeddisks-y4w23nt2 -->
**scopedDisks** · [`examples/concepts-tenancy/main.go:83-106`](../../examples/concepts-tenancy/main.go#L83-L106)

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

Check: `acme reads logo.png: "acme logo"`, `shared disk holds: tenants/acme/logo.png tenants/globex/logo.png`, and `acme reads ../globex/logo.png: invalid_key`.

The scoped disk's name appears in its errors, logs and signed URLs, so name it after the tenant. Creating one is cheap (a small struct; no backend call), so creating it per request is fine.

## Step 2 — Signed links from a scoped disk

A signed link names the disk that issued it. `fshttp.Signed` resolves that name through the Manager you give it, so a scoped disk that issues links must be in that Manager. Without it the link answers 404:

<!-- ds:block id=signedlinks-vm866gh6 -->
**signedLinks** · [`examples/concepts-tenancy/more.go:27-63`](../../examples/concepts-tenancy/more.go#L27-L63)

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
**withoutScopedDisk** · [`examples/concepts-tenancy/more.go:68-79`](../../examples/concepts-tenancy/more.go#L68-L79)

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

Check: `signed link /files/acme/report.pdf: 200`, then `same link, scoped disk not in the Manager: 404`.

With many tenants, registering a scoped disk per tenant in a Manager at start-up does not scale. `fshttp.Signed` takes any `fshttp.Disks`, an interface with one method, `Disk(name string) (*filesystem.Disk, error)`, so an application can implement it to build the tenant's scoped disk on demand from the name in the link. Backends that sign their own URLs (S3, GCS, Azure) do not go through `fshttp` at all: the scoped key is signed directly.

## Step 3 — A guard for code that builds keys

When the application builds full keys itself, `layer.Guard` can check every operation against the tenant the request belongs to. A check that covers the key, the target of a copy or move, and every key of a batch delete:

<!-- ds:block id=sametenant-4fket7z9 -->
**sameTenant** · [`examples/concepts-tenancy/more.go:89-101`](../../examples/concepts-tenancy/more.go#L89-L101)

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
**guardKeys** · [`examples/concepts-tenancy/more.go:107-118`](../../examples/concepts-tenancy/more.go#L107-L118)

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

Check: `guard own key: <nil>` and `guard copy into another tenant: permission`. The guard runs before the driver, so a refused operation never reaches storage. Put the tenant in the context in your authentication middleware, the same place you resolve the user.

## Step 4 — The records scope

Every records operation takes a `scope` argument, and every query filters by it: a file ID from one tenant is `not_found` in another, attachments and listings never cross scopes, and the plugins (folders, tags, quota, share, audit) key their own rows by it too. The library treats the scope as opaque; use the tenant, workspace or user ID your application already has.

<!-- ds:block id=recordscopes-r5r3w6ux -->
**recordScopes** · [`examples/concepts-tenancy/more.go:123-153`](../../examples/concepts-tenancy/more.go#L123-L153)

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

Check: `globex asks for acme's file: not_found`.

The scope does not change object keys: `Store.Upload` stores under whatever key the policy's `KeyFunc` chooses. To keep bytes and rows aligned, upload through the tenant's scoped disk, or give the policy a `KeyFunc` that puts the tenant first. Then a tenant's rows and objects can each be found by one value. In `recordshttp`, the required `Scope` hook maps each request to its scope; see [HTTP endpoints](../records/http-endpoints.md).

## Step 5 — Quotas per tenant

The records `quota` plugin limits how many bytes and files each scope may store. Usage is charged inside the upload's transaction with one conditional update, so two concurrent uploads can never both pass a limit, and a rolled-back upload un-charges itself. An upload over the limit fails with `quota_exceeded` (HTTP 507). Limits come from your application, typically the tenant's plan. See [Quota](../plugins/quota.md).

## Step 6 — Deleting a tenant

A tenant that leaves takes its files with it. On a prefix layout that is one `DeletePrefix` on the shared disk, which S3 and local disk do in bulk and other backends by listing. A scoped disk refuses to delete its own root, like any disk, so this is done one level up, deliberately:

<!-- ds:block id=deletetenant-cvsd5bmn -->
**deleteTenant** · [`examples/concepts-tenancy/main.go:124-136`](../../examples/concepts-tenancy/main.go#L124-L136)

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

Check: `scoped disk deletes its root: invalid_argument`, then `after deleting globex: tenants/acme/logo.png`.

With records, delete the rows too. `Store.PurgeOwner` purges what one owner (a user, a post) leaves behind; for a whole tenant, list the scope's files with `Store.Files` (once for live files, once with `Trashed: true`) and `Purge` them, then delete the prefix for anything unrecorded. The object-last rule applies: rows go in the transaction, objects after it commits. See [Account deletion](../recipes/account-deletion.md).

## Per-tenant disks from configuration

A tenant with its own bucket, region or provider gets its own DSN, opened with the same `Mux`. The `prefix` parameter gives a configured disk the same confinement as `Scope`:

```sh
STORAGE_DISKS=shared,acme
STORAGE_SHARED_DSN=s3://app-files?region=eu-west-1
STORAGE_ACME_DSN=s3://acme-files?region=us-east-1&prefix=files
```

See [Configuration](configuration.md). The full walk-through of a tenant-aware application is [SaaS tenants](../recipes/saas-tenants.md).

### A region per tenant (data residency)

Some customers require that their files never leave a region: an EU customer under the GDPR, a public-sector customer under national rules. The library has no region setting of its own, because a region is a property of the bucket: keep one bucket per region, record each tenant's region in your tenants table, and open the tenant's disk on the bucket of its region. Every write through that disk then lands in that region, and nothing in your upload or download code changes.

The program stands in for S3 with a Mux scheme whose host is a bucket, so two DSNs naming one bucket reach the same bytes, as they would on S3. Each tenant's configuration names its bucket and its domain:

<!-- ds:block id=opentenant-wujwfu92 -->
**openTenant** · [`examples/concepts-tenancy/regions.go:59-65`](../../examples/concepts-tenancy/regions.go#L59-L65)

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

With a real backend the DSN is `s3://files-eu-west-1?region=eu-west-1&prefix=tenants/acme`; the rest is the same. Open the disks once at start-up (or the first time a tenant is seen) into a `Manager`, and resolve them by name per request:

<!-- ds:block id=regionsanddomains-mf34m22t -->
**regionsAndDomains** · [`examples/concepts-tenancy/regions.go:71-109`](../../examples/concepts-tenancy/regions.go#L71-L109)

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

Check: `acme: stored in files-eu-west-1 as tenants/acme/logo.png, served at https://files.acme.example/logo.png`, and `globex` in `files-us-east-1`. Moving a tenant to another region is a copy between two disks (`filesystem.CopyAcross`, or the `fs` tool's `sync`) followed by a change of its configuration; see [Move to S3](../recipes/move-to-s3.md) for the pattern.

### A domain per tenant

A tenant on a plan with white-labelling wants its links on its own domain: `files.acme.example/logo.png`, not your CDN's. Point a CDN distribution for that domain at the tenant's files and give the tenant's disk that domain with `WithPublicURL`. `PublicURL` builds links from it without a backend call, so links on the tenant's domain cost nothing extra.

Where the prefix goes decides which way to set it:

- **The CDN serves the tenant's prefix** (its origin is `bucket/tenants/acme`): pass `filesystem.WithPublicURL("https://files.acme.example")` as a code option to `Mux.Open`, as the program does. The URL is the domain plus the key inside the tenant's disk, with no prefix.
- **The CDN serves the bucket root**: put the domain in the DSN instead, `public_url=https://files.acme.example`. The Mux then appends the prefix itself, giving `https://files.acme.example/tenants/acme/logo.png`, because a DSN's public URL always names the bucket's root.

A scoped disk from `Disk.Scope` inherits its parent's public base with the prefix appended, the second case; open the tenant's disk through the Mux (or `NewDisk` over its own driver) when it needs a domain of its own. Signed links are unaffected by either: they point at your `fshttp.Signed` handler, or at the backend's own presigned URL.

## Pitfalls

- Handing request code the shared disk "just for this one call": every call it makes can address every tenant. Pass the scoped disk.
- Building keys from a tenant ID taken from the request body or URL without checking it belongs to the signed-in user. A scoped disk confines keys to a prefix; choosing the right prefix is your authorisation.
- Forgetting the scoped disk in the Manager given to `fshttp.Signed` (Step 2): links answer 404.
- Using `""` as the records scope in a multi-tenant application: every tenant then shares one scope.
- Listing the shared disk's root in an admin page: it walks every tenant. Page with `ListPage` and a prefix.

## Next

- [Keys](keys.md): why a prefix cannot be escaped.
- [Interceptors](interceptors.md): Guard and the other built-in interceptors.
- [SaaS tenants](../recipes/saas-tenants.md): a complete tenant-aware set-up.
