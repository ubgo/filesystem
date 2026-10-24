package azure

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

func TestHelpers(t *testing.T) {
	if val[string](nil) != "" || val(new(int64)) != 0 {
		t.Error("val of nil or zero")
	}
	e := azcore.ETag(`"abc"`)
	if etag(nil) != "" || etag(&e) != "abc" {
		t.Error("etag")
	}
	if metadata(nil) != nil {
		t.Error("empty metadata")
	}
	if got := parseConnectionString("A=1; B = x=y ;broken;"); got["A"] != "1" || got["B"] != "x=y" || len(got) != 2 {
		t.Errorf("parse = %v", got)
	}
	if blockID("abc", 7) != "YWJjLTAwMDA3" {
		t.Errorf("blockID = %s", blockID("abc", 7))
	}
}
