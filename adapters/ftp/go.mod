module github.com/ubgo/filesystem/adapters/ftp

go 1.26.0

require (
	github.com/fclairamb/ftpserverlib v0.32.4
	github.com/jlaffaye/ftp v0.2.4
	github.com/spf13/afero v1.15.0
	github.com/ubgo/filesystem v0.0.0-00010101000000-000000000000
)

require (
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.28.0 // indirect
)

replace github.com/ubgo/filesystem => ../..
