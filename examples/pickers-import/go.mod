// Module pickers-import is the example of contrib/pickers, separate so the
// main examples module does not depend on it.
module github.com/ubgo/filesystem/examples/pickers-import

go 1.26.0

replace (
	github.com/ubgo/filesystem => ../../
	github.com/ubgo/filesystem/contrib/pickers => ../../contrib/pickers
)

require (
	github.com/ubgo/filesystem v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/contrib/pickers v0.0.0-00010101000000-000000000000
)
