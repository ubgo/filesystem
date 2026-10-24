<!-- Thanks for contributing to filesystem! -->

## What & why

<!-- What does this change and why? Link the issue it closes. -->
Closes #

## Type of change

- [ ] Bug fix
- [ ] Security fix (tagged **[security]** in CHANGELOG.md)
- [ ] New feature, driver or plugin
- [ ] Refactor / internal
- [ ] Docs

## Checklist

- [ ] `task check` is green; a change touching SQL is also green on `task test:engines`, and a driver change on its real-backend task.
- [ ] New logic has a test that fails without it. Database error paths are covered with `recordstest.OpenFaulty`.
- [ ] Keys are cleaned before a driver sees them, signed URLs expire, and no client-visible error names a disk or a storage key.
- [ ] No new third-party dependency in the core module; a package that needs one is its own module.
- [ ] A driver declares only the capabilities it really honours, and the conformance suite passes.
- [ ] Every new closed-set value is a named constant; every new export has a doc comment saying why it exists and what stays true.
- [ ] No suppressed diagnostics: no `//nolint`, no skipped test, no loosened assertion.
- [ ] README, package README, guide page and CHANGELOG.md updated for anything user-facing.
