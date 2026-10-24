module github.com/ubgo/filesystem/adapters/sftp

go 1.26.0

require (
	github.com/pkg/sftp v1.13.11
	github.com/ubgo/filesystem v0.0.0-00010101000000-000000000000
	golang.org/x/crypto v0.57.0
)

require (
	github.com/kr/fs v0.1.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
)

replace github.com/ubgo/filesystem => ../..
