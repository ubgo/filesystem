package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/share"
	"github.com/ubgo/filesystem/upload"
)

// Decisions of the link-policy demo, not the library's.
const (
	// orgHeader stands for the app's session: the organisation the
	// visitor is signed in to.
	orgHeader = "X-Org"
	// portalMaxUpload caps one upload through a request link of the
	// client portal, below docPolicy's own cap.
	portalMaxUpload = 10
)

// downloadPage is the branded page a visitor gets instead of a redirect.
var downloadPage = template.Must(template.New("download").Parse(`<h1>{{.Name}}</h1><a href="{{.URL}}">Download</a>`))

// passwordPage is the form shown for a link that needs its password.
var passwordPage = template.Must(template.New("password").Parse(`<form method="post"><input name="password" type="password"></form>`))

// membersOnly lets a link be used only by visitors signed in to the
// organisation that owns it. It runs before the password and before any
// download is spent; its CodePermission answers 403.
// ds:def id=membersonly-z5h8c3qr
func membersOnly(r *http.Request, link share.Share) error {
	if r.Header.Get(orgHeader) != link.Scope {
		return filesystem.Errorf(filesystem.CodePermission, "sign in to %s", link.Scope)
	}
	return nil
}

// brandedPage hands the file over on a page of the app's own, with the
// signed URL the default would have redirected to. The download is
// already spent and reported when it runs.
// ds:def id=brandedpage-k2w9t6vn
func brandedPage(w http.ResponseWriter, _ *http.Request, _ share.Share, f records.File, url string) {
	_ = downloadPage.Execute(w, map[string]string{"Name": f.Filename, "URL": url})
}

// linkErrors renders the password form for share.ErrPassword and plain
// status text for everything else; every unusable link stays the same
// 404, so links cannot be probed.
// ds:def id=linkerrors-q8m3x7bd
func linkErrors(w http.ResponseWriter, _ *http.Request, status int, err error) {
	if errors.Is(err, share.ErrPassword) {
		w.WriteHeader(status)
		_ = passwordPage.Execute(w, nil)
		return
	}
	http.Error(w, http.StatusText(status), status)
}

// portalPolicy caps every upload through a request link at
// portalMaxUpload bytes, whatever WithUploads' base allows.
// ds:def id=portalpolicy-n7v4c2xs
func portalPolicy(_ *http.Request, _ share.Share, base upload.Policy) (upload.Policy, error) {
	base.MaxSize = portalMaxUpload
	return base, nil
}

// newPolicyHandler is the share handler with every policy of the app's own.
// ds:def id=newpolicyhandler-c6r2p9wf
func newPolicyHandler(s *records.Store, p *share.Plugin) http.Handler {
	return share.Handler(s, p,
		share.WithAccessCheck(membersOnly),
		share.WithDeliver(brandedPage),
		share.WithErrors(linkErrors),
		share.WithUploads(diskName, docPolicy),
		share.WithUploadPolicy(portalPolicy),
	)
}

// visit asks h for token as a member of org and returns the status and
// the first line of the body.
func visit(h http.Handler, method, token, org string, body io.Reader, contentType string) (int, string) {
	req := httptest.NewRequest(method, "/"+token, body)
	req.Header.Set(orgHeader, org)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	page, _, _ := bytes.Cut(rec.Body.Bytes(), []byte("<a"))
	return rec.Code, string(bytes.TrimSpace(page))
}

// fileForm is a multipart body with one "file" part.
func fileForm(name, content string) (*bytes.Buffer, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		return nil, "", err
	}
	if _, err := io.WriteString(part, content); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return &buf, mw.FormDataContentType(), nil
}

// policyDemo serves a file link, a password link and a request link
// through newPolicyHandler, on the folder Store of collections.go.
func policyDemo(ctx context.Context, w io.Writer, db *sql.DB, disks *filesystem.Manager) error {
	s, fp, sp, err := newFolderStore(ctx, db, disks)
	if err != nil {
		return err
	}
	h := newPolicyHandler(s, sp)
	f, err := uploadDoc(ctx, s, "invoice.txt", "amount due")
	if err != nil {
		return err
	}
	link, err := shareFile(ctx, s, sp, f.ID)
	if err != nil {
		return err
	}
	code, _ := visit(h, http.MethodGet, link.Token, "globex", nil, "")
	fmt.Fprintf(w, "policy: link opened from another organisation: %d\n", code)
	code, page := visit(h, http.MethodGet, link.Token, tenant, nil, "")
	fmt.Fprintf(w, "policy: link opened by a member: %d %s\n", code, page)
	locked, err := protect(ctx, s, sp, f.ID)
	if err != nil {
		return err
	}
	code, page = visit(h, http.MethodGet, locked.Token, tenant, nil, "")
	fmt.Fprintf(w, "policy: password link: %d %s\n", code, page)
	inbox, err := fp.Create(ctx, s, tenant, "", "Portal")
	if err != nil {
		return err
	}
	req, err := requestFiles(ctx, s, sp, inbox.ID, 5)
	if err != nil {
		return err
	}
	for _, content := range []string{"twelve bytes", "small"} {
		body, ct, err := fileForm("receipt.txt", content)
		if err != nil {
			return err
		}
		code, _ := visit(h, http.MethodPost, req.Token, tenant, body, ct)
		fmt.Fprintf(w, "policy: %d-byte upload through the request link: %d\n", len(content), code)
	}
	return nil
}
