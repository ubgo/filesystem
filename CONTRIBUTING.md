# Contributing to filesystem

## The gate

```sh
task check
```

That runs gofmt, vet (on the host and for linux, windows and freebsd), the tests of every module, the Markdown wrap check and docsync's drift check. Everything must be green before a change is proposed.

A change that touches SQL, a driver or a handler also runs against the real backends. Each of these tasks starts a throwaway container from `compose.yml`, runs, and removes it with its data afterwards, even when a test fails; run them one at a time:

```sh
task test:engines   # records and the imaging plugins on SQLite, Postgres and MySQL
task cover:records  # records coverage merged across the three engines
task test:rustfs    # the S3 conformance suite against RustFS
task test:s3        # the same against SeaweedFS, with prune_prefixes=true
task test:azure     # the Azure driver against Azurite
task test:clamav    # the scan package against a real clamd
```

`task --list` shows every other command. There is no CI that runs on push: `task check` is the real gate.

## The rules a change is reviewed against

**Storage code is held to a security bar.** Every key is cleaned and checked for traversal in the `Disk` before a driver sees it, signed URLs always expire, upload size and type limits are enforced on the server from the bytes rather than the client's claim, an error a client sees never names a disk or a storage key (`fshttp.ClientMessage`), and no feature ships without the test that would catch its absence.

**The core module depends only on the standard library.** `deps_test.go` fails otherwise. A package that needs a third-party module (a cloud SDK, an image codec, a SQL driver) is its own module with a `replace` directive back to the root, added to the Taskfile's `MODULES` list. The `records` module depends only on the core and `github.com/ubgo/sqlb`, and its own guard fails otherwise.

**Capabilities are authoritative.** A driver declares what it can do in `Capabilities()`; `NewDisk` refuses a driver that declares an interface it does not implement, the `Disk` calls an optional method only when its bit is true, and the conformance suite (`filesystemtest`) exercises every declared capability. A driver that cannot do something leaves the bit off rather than faking it.

**Cross-cutting behaviour is an interceptor, never a wrapper driver.** Retry, timeouts, logging and metrics wrap operations (`Interceptor` over `*Op`) so capabilities are never hidden. A wrapper driver exists only where keys or targets change (scoped, mirrored disks) and declares what it forwards.

**Records keep the object-first ordering.** Object first, row second when a file is created; row first, object last (after the commit) when it goes; an upload whose transaction rolls back deletes its object. Plugins reach the tables through the records hook interfaces, never by writing another plugin's rows.

**Every closed-set value is a named constant, every export has a doc comment** that says why it exists and what stays true, and no diagnostic is suppressed: no `//nolint`, no skipped test, no loosened assertion.

**Guide code is copied, never typed.** A code block in `docs/` or an example README sits between `<!-- ds:block id=… -->` markers and is written by `ds refresh` from an example `task check` compiles and tests (docsync repo mode). `ds check` fails when a copy is edited by hand or its source changes; run `ds refresh` after changing the example, and re-run a walkthrough for real when its output changes.

**A test for every behaviour.** New logic comes with the test that fails without it; prove it by breaking the code and watching the test fail. Database error paths are tested with the fault-injecting driver (`recordstest.OpenFaulty`), which fails each database step of an operation in turn; storage error paths with an interceptor that fails the Nth operation.

## Where a contribution lives

| You are adding | Where it goes | What it ships with |
|---|---|---|
| A storage driver | `adapters/<name>/`, its own module; implement the six-method `filesystem.Driver` plus the optional interfaces your backend really supports | the conformance suite passing against a fake or a container, a DSN opener, a README with its capabilities and limits, and a guide under `docs/adapters/`; [write a driver](docs/extending/write-a-driver.md) is the checklist |
| An interceptor | `layer/` when it needs only the standard library, its own module otherwise | tests for the operations it changes and the ones it must leave alone; [write an interceptor](docs/extending/write-an-interceptor.md) |
| A records plugin | `records/<name>/` when it needs only sqlb, its own module otherwise; it implements the hook interfaces it needs and owns its tables and migrations | tests on the three engines, a fault-sweep case for every database step, a README and a guide under `docs/plugins/`; [write a records plugin](docs/extending/write-a-records-plugin.md) |
| An integration (a framework, tracing, a resumable-upload protocol) | `contrib/<name>/`, its own module | an example under `examples/` that its test drives end to end, and a guide under `docs/integrations/` |

Open an issue before starting a new driver, plugin or module so the scope is agreed first; a fix or a documentation change needs no discussion beyond the pull request.

## Pull requests

Keep a pull request to one change. Update the README, the package README, the guide page and CHANGELOG.md for anything a user can see; tag a security fix **[security]** and a breaking change **[breaking]**.
