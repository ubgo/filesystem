# Supabase Storage

Supabase Storage speaks the S3 protocol, so the S3 driver stores files in it. This program builds the DSN of a bucket in a Supabase project (the project's S3 endpoint `/storage/v1/s3`, path-style addressing, the access key pair read from two environment variables), opens it, and stores, lists, reads and deletes a file. It uses the core `filesystem` package, `github.com/ubgo/filesystem/adapters/s3`, and `github.com/johannesboyne/gofakes3` for the stand-in server only. The guide [Supabase Storage](../../docs/adapters/supabase.md) explains each step.

## Run it

```sh
cd examples
go run ./adapters-supabase
go test ./adapters-supabase
```

It needs no Supabase project and no credentials. An in-process S3 (gofakes3) answers under `/storage/v1/s3` and returns 404 for any other path, so the program proves the DSN reaches the path Supabase's endpoint has. It was not run against a real Supabase project: what Supabase itself accepts (its upload limits, which S3 features it honours) is not exercised here.

## What it does

```text
capabilities: [copy,delete-prefix,delete-keys,presign,multipart,presign-part,presign-post,metadata,content-type,headers,presign-headers,checksum-sha256]
stored: users/42/profile.json 14 bytes application/json
listed: users/42/profile.json
read back: {"name":"Ada"}
exists after delete: false
first request: PUT /storage/v1/s3/avatars/users/42/profile.json
if-not-exists: unsupported
```

The capabilities are what the S3 driver declares for this DSN, not what Supabase was checked to support.

## The code

<!-- ds:block id=supabasedsn-cqjtcxsy -->
**supabaseDSN** · [`examples/adapters-supabase/main.go:79-87`](main.go#L79-L87)

```go
func supabaseDSN(projectURL string) string {
	q := url.Values{}
	q.Set(fss3.ParamRegion, region)
	q.Set(fss3.ParamEndpoint, projectURL+s3Path)
	q.Set(fss3.ParamPathStyle, "true")
	q.Set(fss3.ParamAccessKeyEnv, accessKeyEnv)
	q.Set(fss3.ParamSecretKeyEnv, secretKeyEnv)
	return fss3.Scheme + "://" + bucket + "?" + q.Encode()
}
```
<!-- /ds:block hash=4b7734 -->
