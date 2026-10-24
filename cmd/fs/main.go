// Command fs works with any disk the library can open, from the shell:
// list, inspect, read, write, copy between backends, sign URLs and check
// that a disk works.
//
// A location is a DSN with the key in its fragment:
//
//	fs ls  'file:///var/app/uploads#avatars'
//	fs cp  'file:///var/app/uploads#a.png' 's3://media?region=auto&endpoint=https://….r2.cloudflarestorage.com#a.png'
//	fs url 's3://media?region=us-east-1#reports/q3.pdf' -expires 15m
//
// The schemes are file, mem, s3, gs, azblob, sftp, ftp, ftps,
// ftps-implicit, webdav and webdavs. Cloud credentials come from each
// SDK's usual environment; url on a backend that cannot sign its own URLs
// (file, mem, sftp, ftp, webdav) signs with FS_SIGNING_SECRET. Every
// command accepts -json for machine-readable output, and exits 0 on
// success, 1 on failure and 2 on a usage error.
package main

import (
	"context"
	"os"
	"os/signal"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
