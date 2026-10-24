package main

import (
	"net/http"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordshttp"
	"github.com/ubgo/filesystem/signedurl"
)

// tenantDisks maps a tenant that brought its own bucket to the disk
// mounted for it. Decision of this demo app: globex has a bucket of its
// own, every other tenant has none on this route.
var tenantDisks = map[string]string{"globex": "globex-bucket"}

// addTenantDisks mounts one disk per entry of tenantDisks in the
// manager, signed like the shared disk so its links lead to bytes. A
// real app opens each tenant's bucket from its settings.
func addTenantDisks(disks *filesystem.Manager, signer *signedurl.Signer) error {
	for _, name := range tenantDisks {
		disk, err := filesystem.NewDisk(name, memory.New(), filesystem.WithURLSigner(signer))
		if err != nil {
			return err
		}
		if err := disks.Add(disk); err != nil {
			return err
		}
	}
	return nil
}

// diskFor chooses the disk of each upload: the tenant's own bucket, and a
// refusal before any byte is read for a tenant without one.
// ds:def id=diskfor-h3v8q2nw
func diskFor(_ *http.Request, t recordshttp.Target) (string, error) {
	name, ok := tenantDisks[t.Scope]
	if !ok {
		return "", filesystem.Errorf(filesystem.CodePermission, "no storage configured for this workspace")
	}
	return name, nil
}

// newBucketHandler serves the same records routes, with uploads on the
// disk diskFor names instead of one fixed Disk; it is mounted under /byo.
// ds:def id=newbuckethandler-k6t9z4mr
func newBucketHandler(s *records.Store) (http.Handler, error) {
	return recordshttp.Handler(recordshttp.Config{
		Store:        s,
		DiskFor:      diskFor,
		Policy:       policy,
		MaxFiles:     maxFilesPerRequest,
		Scope:        scopeOf,
		Authorize:    authorizer(),
		LinkLifetime: linkLifetime,
	})
}
