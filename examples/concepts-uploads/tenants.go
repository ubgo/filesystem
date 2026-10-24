package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/upload"
)

// tenantHeader stands in for the app's authentication, which would put
// the signed-in tenant on the context.
const tenantHeader = "X-Tenant"

// tenantKey is the context key of the demo's tenant middleware.
type tenantKey struct{}

// withTenant is the app's middleware: it puts the caller's tenant on the
// request's context.
func withTenant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t := r.Header.Get(tenantHeader); t != "" {
			r = r.WithContext(context.WithValue(r.Context(), tenantKey{}, t))
		}
		next.ServeHTTP(w, r)
	})
}

// tenantFrom reads the tenant withTenant stored.
func tenantFrom(ctx context.Context) (string, bool) {
	t, ok := ctx.Value(tenantKey{}).(string)
	return t, ok && t != ""
}

// tenantPolicy keys every upload under the uploading tenant's prefix,
// read from the request's context: one policy for every tenant, no
// Resolver building a policy per request. A request without a tenant is
// refused before anything is stored.
// ds:def id=tenantpolicy-w6k2r9md
func tenantPolicy() upload.Policy {
	p := policy
	p.Key = nil // Key or KeyContext, not both
	p.KeyContext = func(ctx context.Context, info upload.Info) (string, error) {
		tenant, ok := tenantFrom(ctx)
		if !ok {
			return "", filesystem.Errorf(filesystem.CodePermission, "no tenant")
		}
		return upload.RandomKey("tenants/" + tenant)(info)
	}
	return p
}

// keyFromRequest posts one PNG through the form handler as two tenants
// and once without one.
// ds:def id=keyfromrequest-t3x8c5vb
func keyFromRequest(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	h := withTenant(upload.Handler(disk, tenantPolicy(), upload.HandlerOptions{}))
	for _, tenant := range []string{"acme", "globex", ""} {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, err := mw.CreateFormFile(upload.DefaultField, "logo.png")
		if err != nil {
			return err
		}
		if _, err := io.WriteString(fw, pngFile); err != nil {
			return err
		}
		if err := mw.Close(); err != nil {
			return err
		}
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/uploads", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set(tenantHeader, tenant)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		fmt.Fprintf(out, "key from the request, tenant %q: %d %s\n", tenant, rec.Code, keyDirs(ctx, disk, tenant))
	}
	return nil
}

// keyDirs is the directory of the object stored for tenant, or the
// number of objects when the tenant is empty.
func keyDirs(ctx context.Context, disk *filesystem.Disk, tenant string) string {
	if tenant == "" {
		return fmt.Sprintf("(objects stored in all: %d)", count(ctx, disk))
	}
	for e, err := range disk.List(ctx, "tenants/"+tenant, filesystem.ListOptions{Recursive: true}) {
		if err == nil {
			return path.Dir(e.Key) + "/<random>" + path.Ext(e.Key)
		}
	}
	return "-"
}
