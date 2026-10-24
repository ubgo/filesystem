// Module raster rasterises sanitised SVG for imaging, in pure Go. A
// separate module so the SVG renderer is compiled only by applications
// that turn SVG into pixels.
module github.com/ubgo/filesystem/contrib/svg/raster

go 1.26.0

require (
	github.com/srwiley/oksvg v0.0.0-20221011165216-be6e8873101c
	github.com/srwiley/rasterx v0.0.0-20220730225603-2ab79fcdd4ef
	github.com/ubgo/filesystem v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/contrib/imaging v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/contrib/svg v0.0.0-00010101000000-000000000000
)

require (
	golang.org/x/image v0.46.0 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace (
	github.com/ubgo/filesystem => ../../../
	github.com/ubgo/filesystem/contrib/imaging => ../../imaging
	github.com/ubgo/filesystem/contrib/svg => ../
	github.com/ubgo/filesystem/records => ../../../records
)
