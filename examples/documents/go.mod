// Module documents holds the examples of the document plugins
// (contrib/pdf text, contrib/office, contrib/ocr, contrib/vision),
// separate so no other examples module depends on them.
module github.com/ubgo/filesystem/examples/documents

go 1.26.0

replace (
	github.com/ubgo/filesystem => ../../
	github.com/ubgo/filesystem/contrib/ocr => ../../contrib/ocr
	github.com/ubgo/filesystem/contrib/office => ../../contrib/office
	github.com/ubgo/filesystem/contrib/pdf => ../../contrib/pdf
	github.com/ubgo/filesystem/contrib/vision => ../../contrib/vision
	github.com/ubgo/filesystem/records => ../../records
)

require (
	github.com/ubgo/filesystem v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/contrib/ocr v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/contrib/office v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/contrib/pdf v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/contrib/vision v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/records v0.0.0-00010101000000-000000000000
	github.com/ubgo/sqlb v0.0.0-20260928141554-6bca77f855f6
	modernc.org/sqlite v1.60.1
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.48.0 // indirect
	modernc.org/libc v1.77.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
