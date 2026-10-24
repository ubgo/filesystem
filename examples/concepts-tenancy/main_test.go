package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun checks every isolation rule the multi-tenancy page states.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`acme reads logo.png: "acme logo"`,
		"shared disk holds: tenants/acme/logo.png tenants/globex/logo.png",
		"acme reads ../globex/logo.png: invalid_key",
		"scoped disk deletes its root: invalid_argument",
		"after deleting globex: tenants/acme/logo.png",
		"signed link /files/acme/report.pdf: 200",
		"same link, scoped disk not in the Manager: 404",
		"guard own key: <nil>",
		"guard copy into another tenant: permission",
		"globex asks for acme's file: not_found",
		"acme: stored in files-eu-west-1 as tenants/acme/logo.png, served at https://files.acme.example/logo.png",
		"globex: stored in files-us-east-1 as tenants/globex/logo.png, served at https://cdn.globex.example/logo.png",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
