# Storage seats in a team workspace

Per-uploader limits of the quota plugin: a team workspace whose plan allows 100 bytes, where each member's seat may store 40 bytes, so one member cannot fill the plan for everyone. Each upload is charged to the workspace and to its uploader in one transaction; a refusal names the member; a replacement is charged to the file's uploader rather than to whoever replaced it; `Recount` rebuilds both kinds of counter. It uses `github.com/ubgo/filesystem/records` and `records/quota` on SQLite over an in-memory disk. The guide [Quota](../../docs/plugins/quota.md#per-uploader-limits-seats-in-a-workspace) explains it.

## Run it

```sh
cd examples
go run ./records-quota-seats
go test ./records-quota-seats
```

It needs no network or environment variables; the SQLite file lives in a temporary directory deleted on exit.

## What it does

```text
two uploads:             alice 30/40, bob 25/40, team 55/100 bytes
alice past her seat:     filesystem: quota_exceeded: quota: uploader "user:alice" in scope "team-acme" would exceed its limit of 40 bytes
bob grew alice's file:   alice 38/40, bob 25/40, team 63/100 bytes
recount:                 team 63 bytes, 2 files
after the recount:       alice 38/40, bob 25/40, team 63/100 bytes
```

The seat limit comes from `seatLimit`, the `quota.UploaderLimitFunc`; uploads made without an actor (uploader `""`) are limited only by the workspace.

<!-- ds:block id=seatlimit-r3vq8w2m -->
**seatLimit** · [`examples/records-quota-seats/main.go:61-66`](main.go#L61-L66)

```go
func seatLimit(_ context.Context, _, uploader string) (quota.Limit, error) {
	if uploader == "" {
		return quota.Limit{}, nil
	}
	return seatPlan, nil
}
```
<!-- /ds:block hash=2b0657 -->
