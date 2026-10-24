---
name: Bug report
about: A driver, upload, handler or records operation did the wrong thing
title: "[bug] "
labels: bug
---

**Security issue?** Stop here and report it privately instead: see SECURITY.md.

**What happened**

A clear description of the bug, and what you expected instead.

**How to reproduce**

The DSN (with secrets removed) or the driver and options you use, reduced as far as it still reproduces, and the call or request that misbehaves:

```go
disk, err := mux.Open(ctx, "media", "s3://my-bucket?region=us-east-1")
```

**Environment**

- filesystem version (commit or tag):
- Driver and backend (local, memory, S3 provider, GCS, Azure, SFTP server):
- Records engine and version, if records are involved:
- Go version and OS:
