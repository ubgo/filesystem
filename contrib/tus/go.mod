module github.com/ubgo/filesystem/contrib/tus

go 1.25.14

require github.com/ubgo/filesystem v0.0.0-00010101000000-000000000000

require (
	github.com/tus/tusd/v2 v2.10.1
	golang.org/x/exp v0.0.0-20260611194520-c48552f49976
)

require github.com/tus/lockfile v1.2.0 // indirect

replace github.com/ubgo/filesystem => ../../
