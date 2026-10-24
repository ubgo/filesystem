package s3

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
)

// TestEmptyLockDateMiddlewareRegistered proves withEmptyLockDateDropped
// installs dropEmptyLockDate in the deserialize step under its own ID, so
// it can be found (and is not added twice) by that name.
func TestEmptyLockDateMiddlewareRegistered(t *testing.T) {
	stack := middleware.NewStack("HeadObject", nil)
	var o s3.Options
	withEmptyLockDateDropped(&o)
	for _, apply := range o.APIOptions {
		if err := apply(stack); err != nil {
			t.Fatal(err)
		}
	}
	m, ok := stack.Deserialize.Get(emptyLockDateMiddlewareID)
	if !ok || m.ID() != emptyLockDateMiddlewareID {
		t.Fatalf("deserialize step = %v, want %s registered", stack.Deserialize.List(), emptyLockDateMiddlewareID)
	}
}
