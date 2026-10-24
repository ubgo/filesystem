// Module media holds the examples of the media plugins that run external
// programs, call media platforms or parse untrusted formats
// (contrib/video, contrib/audio, contrib/videoplatform, contrib/pdf,
// contrib/svg, contrib/svg/raster), separate so the main examples module
// does not depend on them.
module github.com/ubgo/filesystem/examples/media

go 1.26.0

replace (
	github.com/ubgo/filesystem => ../../
	github.com/ubgo/filesystem/contrib/audio => ../../contrib/audio
	github.com/ubgo/filesystem/contrib/imaging => ../../contrib/imaging
	github.com/ubgo/filesystem/contrib/pdf => ../../contrib/pdf
	github.com/ubgo/filesystem/contrib/svg => ../../contrib/svg
	github.com/ubgo/filesystem/contrib/svg/raster => ../../contrib/svg/raster
	github.com/ubgo/filesystem/contrib/video => ../../contrib/video
	github.com/ubgo/filesystem/contrib/videoplatform => ../../contrib/videoplatform
	github.com/ubgo/filesystem/records => ../../records
)

require (
	github.com/ubgo/filesystem v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/contrib/audio v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/contrib/imaging v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/contrib/pdf v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/contrib/svg v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/contrib/svg/raster v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/contrib/video v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/contrib/videoplatform v0.0.0-00010101000000-000000000000
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
	github.com/srwiley/oksvg v0.0.0-20221011165216-be6e8873101c // indirect
	github.com/srwiley/rasterx v0.0.0-20220730225603-2ab79fcdd4ef // indirect
	golang.org/x/image v0.46.0 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	modernc.org/libc v1.77.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
