package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/acl"
	"github.com/ubgo/filesystem/records/folders"
)

// Subjects and the audit window of the deny step.
const (
	team       = "group:team"
	contractor = "user:contractor"
	// auditWindow is how long the contractor is kept out of Finance.
	auditWindow = 30 * 24 * time.Hour
)

// newDenyStore wires the acl plugin with WithDeny, so a grant may take a
// level away (Effect: acl.Deny) as well as give one. A deny that applies
// wins over every allow, wherever each sits.
// ds:def id=newdenystore-r7k3w9mf
func newDenyStore(ctx context.Context, db *sql.DB) (*records.Store, *folders.Plugin, *acl.Plugin, error) {
	fp := folders.New()
	ap := acl.New(acl.WithHierarchy(fp), acl.WithDeny())
	s, err := storeWith(ctx, db, fp, ap)
	if err != nil {
		return nil, nil, nil, err
	}
	return s, fp, ap, nil
}

// everyoneButContractor lets the whole team write in the team folder and
// keeps the contractor, a member of the team, out of Finance inside it
// until the audit ends.
// ds:def id=everyonebutcontractor-p2x8v5nk
func everyoneButContractor(ctx context.Context, s *records.Store, ap *acl.Plugin, teamFolder, finance string, now time.Time) error {
	if _, err := ap.Grant(ctx, s, tenant, acl.Grant{Subject: team, Kind: acl.OnFolder, Resource: teamFolder, Level: acl.Write}); err != nil {
		return err
	}
	until := now.Add(auditWindow)
	_, err := ap.Grant(ctx, s, tenant, acl.Grant{Subject: contractor, Kind: acl.OnFolder, Resource: finance, Level: acl.Read, Effect: acl.Deny, ExpiresAt: &until})
	return err
}

// denyDemo checks what the contractor and a colleague may do under the
// team grant and the Finance deny.
func denyDemo(ctx context.Context, w io.Writer, dir string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "deny.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	s, fp, ap, err := newDenyStore(ctx, db)
	if err != nil {
		return err
	}
	teamFolder, err := fp.Create(ctx, s, tenant, "", "Team")
	if err != nil {
		return err
	}
	finance, err := fp.Create(ctx, s, tenant, teamFolder.ID, "Finance")
	if err != nil {
		return err
	}
	budget, err := record(ctx, s, "budget.txt", "q4 budget")
	if err != nil {
		return err
	}
	if err := fp.Put(ctx, s, tenant, budget.ID, finance.ID); err != nil {
		return err
	}
	roadmap, err := record(ctx, s, "roadmap.txt", "next year")
	if err != nil {
		return err
	}
	if err := fp.Put(ctx, s, tenant, roadmap.ID, teamFolder.ID); err != nil {
		return err
	}
	if err := everyoneButContractor(ctx, s, ap, teamFolder.ID, finance.ID, time.Now()); err != nil {
		return err
	}
	// A file grant to the contractor on the budget cannot reopen the deny
	// on its folder.
	if _, err := ap.Grant(ctx, s, tenant, acl.Grant{Subject: contractor, Kind: acl.OnFile, Resource: budget.ID, Level: acl.Read}); err != nil {
		return err
	}
	for _, c := range []struct {
		who  []string
		file records.File
	}{
		{[]string{"user:dana", team}, budget}, {[]string{contractor, team}, roadmap}, {[]string{contractor, team}, budget},
	} {
		ok, err := ap.Can(ctx, s, tenant, c.who, c.file.ID, acl.Read)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "deny: %s reads %s: %t\n", c.who[0], c.file.Filename, ok)
	}
	plain := acl.New(acl.WithHierarchy(fp))
	_, err = plain.Grant(ctx, s, tenant, acl.Grant{Subject: contractor, Kind: acl.OnFile, Resource: roadmap.ID, Level: acl.Read, Effect: acl.Deny})
	fmt.Fprintf(w, "deny: a deny without WithDeny: %s\n", filesystem.CodeOf(err))
	ok, err := plain.Can(ctx, s, tenant, []string{contractor, team}, budget.ID, acl.Read)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "deny: stored denies still refuse without WithDeny: %t\n", !ok)
	return nil
}
