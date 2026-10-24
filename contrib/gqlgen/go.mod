// Module fsgql is the gqlgen adapter. A separate module so gqlgen reaches
// only the applications that use it.
module github.com/ubgo/filesystem/contrib/gqlgen

go 1.26.0

// Development inside this repository builds against the working tree. A
// consumer's build ignores replace directives in dependencies.
replace (
	github.com/ubgo/filesystem => ../../
	github.com/ubgo/filesystem/records => ../../records
)

require (
	github.com/99designs/gqlgen v0.17.94
	github.com/ubgo/filesystem v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/records v0.0.0-00010101000000-000000000000
	github.com/ubgo/sqlb v0.0.0-20260928141554-6bca77f855f6
	github.com/vektah/gqlparser/v2 v2.5.36
	modernc.org/sqlite v1.60.1
)

require (
	github.com/coder/websocket v1.8.15 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/sosodev/duration v1.4.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	modernc.org/libc v1.77.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
