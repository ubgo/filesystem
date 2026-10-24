module github.com/ubgo/filesystem/contrib/imaging/webp

go 1.26.0

require (
	github.com/gen2brain/webp v0.6.4
	github.com/ubgo/filesystem/contrib/imaging v0.0.0-00010101000000-000000000000
	golang.org/x/image v0.46.0
)

require (
	github.com/ebitengine/purego v0.10.1 // indirect
	github.com/ubgo/filesystem v0.0.0-00010101000000-000000000000 // indirect
)

replace (
	github.com/ubgo/filesystem => ../../../
	github.com/ubgo/filesystem/contrib/imaging => ../
	github.com/ubgo/filesystem/records => ../../../records
)
