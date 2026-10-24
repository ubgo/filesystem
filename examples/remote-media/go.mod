// Module remote-media is the example of contrib/remote, separate so the
// main examples module does not depend on an HTML parser.
module github.com/ubgo/filesystem/examples/remote-media

go 1.26.0

replace (
	github.com/ubgo/filesystem => ../../
	github.com/ubgo/filesystem/contrib/remote => ../../contrib/remote
)

require (
	github.com/ubgo/filesystem v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/contrib/remote v0.0.0-00010101000000-000000000000
)

require golang.org/x/net v0.59.0 // indirect
