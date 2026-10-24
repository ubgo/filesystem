package s3

import (
	"context"
	"testing"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/ubgo/filesystem"
)

// TestMapErrWithoutResponse proves response errors missing their HTTP
// response are classified without a panic. (A ResponseError with no inner
// smithy error at all is not covered: the SDK's own errors.As support
// panics on it before mapErr could look.)
func TestMapErrWithoutResponse(t *testing.T) {
	slowDown := &smithy.GenericAPIError{Code: errSlowDown}
	for name, c := range map[string]struct {
		err  error
		want filesystem.Code
	}{
		"no response":          {&awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{Err: slowDown}}, filesystem.CodeUnavailable},
		"no http.Response":     {&awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{Response: &smithyhttp.Response{}, Err: slowDown}}, filesystem.CodeUnavailable},
		"status, no slow down": {&awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{Response: &smithyhttp.Response{}}}, filesystem.CodeUnknown},
	} {
		if got := filesystem.CodeOf(mapErr(c.err)); got != c.want {
			t.Errorf("%s: code %q, want %q", name, got, c.want)
		}
		if _, ok := filesystem.RetryAfterOf(mapErr(c.err)); ok {
			t.Errorf("%s: a wait without a response", name)
		}
	}
}

// TestWithPayloadHashSwapsTheHasher proves the API option replaces the
// SDK's payload hasher under its own ID, so no other middleware hashes the
// body, and that the replacement signs the given digest.
func TestWithPayloadHashSwapsTheHasher(t *testing.T) {
	stack := middleware.NewStack("put", smithyhttp.NewStackRequest)
	if err := stack.Finalize.Add(&v4.ComputePayloadSHA256{}, middleware.After); err != nil {
		t.Fatal(err)
	}
	var o s3.Options
	withPayloadHash("abc")(&o)
	if err := o.APIOptions[0](stack); err != nil {
		t.Fatal(err)
	}
	m, ok := stack.Finalize.Get(payloadHashMiddlewareID)
	if !ok || m.ID() != payloadHashMiddlewareID {
		t.Fatalf("finalize step has %T", m)
	}
	var signed string
	_, _, _ = m.HandleFinalize(t.Context(), middleware.FinalizeInput{}, middleware.FinalizeHandlerFunc(func(ctx context.Context, _ middleware.FinalizeInput) (middleware.FinalizeOutput, middleware.Metadata, error) {
		signed = v4.GetPayloadHash(ctx)
		return middleware.FinalizeOutput{}, middleware.Metadata{}, nil
	}))
	if signed != "abc" {
		t.Errorf("payload hash = %q", signed)
	}
}
