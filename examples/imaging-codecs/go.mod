// Module imaging-codecs holds the example of the image codec modules
// (contrib/imaging/webp, avif, icc), separate so the main examples module
// does not depend on their WebAssembly codecs.
module github.com/ubgo/filesystem/examples/imaging-codecs

go 1.26.0

replace (
	github.com/ubgo/filesystem => ../../
	github.com/ubgo/filesystem/contrib/imaging => ../../contrib/imaging
	github.com/ubgo/filesystem/contrib/imaging/avif => ../../contrib/imaging/avif
	github.com/ubgo/filesystem/contrib/imaging/icc => ../../contrib/imaging/icc
	github.com/ubgo/filesystem/contrib/imaging/webp => ../../contrib/imaging/webp
	github.com/ubgo/filesystem/records => ../../records
)

require (
	github.com/ubgo/filesystem/contrib/imaging v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/contrib/imaging/avif v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/contrib/imaging/icc v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/contrib/imaging/webp v0.0.0-00010101000000-000000000000
)

require (
	github.com/ebitengine/purego v0.10.1 // indirect
	github.com/gen2brain/avif v0.6.0 // indirect
	github.com/gen2brain/webp v0.6.4 // indirect
	github.com/tetratelabs/wazero v1.12.0 // indirect
	github.com/ubgo/filesystem v0.0.0-00010101000000-000000000000 // indirect
	golang.org/x/image v0.46.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
)
