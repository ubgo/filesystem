module github.com/ubgo/filesystem/contrib/imaging/icc

go 1.26.0

// Tests run the converter inside an imaging.Pipeline; the package itself
// imports only the standard library (deps_test.go).
require github.com/ubgo/filesystem/contrib/imaging v0.0.0-00010101000000-000000000000

require (
	github.com/ubgo/filesystem v0.0.0-00010101000000-000000000000 // indirect
	golang.org/x/image v0.46.0 // indirect
)

replace (
	github.com/ubgo/filesystem => ../../../
	github.com/ubgo/filesystem/contrib/imaging => ../
	github.com/ubgo/filesystem/records => ../../../records
)
