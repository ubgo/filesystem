// Module fsecho is the echo adapter. A separate module so echo reaches only the
// applications that use it.
module github.com/ubgo/filesystem/contrib/echo

go 1.26.0

// Development inside this repository builds against the working tree. A
// consumer's build ignores replace directives in dependencies.
replace (
	github.com/ubgo/filesystem => ../../
	github.com/ubgo/filesystem/contrib/internal/fwtest => ../internal/fwtest
	github.com/ubgo/filesystem/records => ../../records
)

require (
	github.com/labstack/echo/v4 v4.15.4
	github.com/ubgo/filesystem v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/contrib/internal/fwtest v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/records v0.0.0-00010101000000-000000000000
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/labstack/gommon v0.5.0 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/ubgo/sqlb v0.0.0-20260928141554-6bca77f855f6 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasttemplate v1.2.2 // indirect
	golang.org/x/crypto v0.53.0 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	modernc.org/libc v1.77.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	modernc.org/sqlite v1.60.1 // indirect
)
