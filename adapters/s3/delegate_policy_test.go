package s3_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// sentPolicy delegates o through a fake STS and returns the session
// policy it sent.
func sentPolicy(t *testing.T, o fss3.DelegateOptions) iamPolicy {
	t.Helper()
	f := &fakeSTS{}
	if _, err := fss3.DelegatePrefix(t.Context(), f, o); err != nil {
		t.Fatal(err)
	}
	var doc iamPolicy
	if err := json.Unmarshal([]byte(aws.ToString(f.last.Policy)), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// TestDelegatePartition proves every ARN of the session policy names the
// bucket's partition: the role's by default, PartitionAWS for a role that
// is not an ARN, an explicit Partition when it agrees with the role, and
// that a malformed or contradicting Partition is refused.
func TestDelegatePartition(t *testing.T) {
	base := fss3.DelegateOptions{SessionName: "user-42", Bucket: "media", Prefix: "users/42", Duration: time.Hour}
	for name, tc := range map[string]struct{ role, partition, want string }{
		"aws from role":       {"arn:aws:iam::1:role/up", "", fss3.PartitionAWS},
		"govcloud from role":  {"arn:aws-us-gov:iam::1:role/up", "", fss3.PartitionGovCloud},
		"china from role":     {"arn:aws-cn:iam::1:role/up", "", fss3.PartitionChina},
		"iso from role":       {"arn:aws-iso:iam::1:role/up", "", "aws-iso"},
		"role not an ARN":     {"uploader", "", fss3.PartitionAWS},
		"ARN, no partition":   {"arn::iam::1:role/up", "", fss3.PartitionAWS},
		"explicit, no ARN":    {"uploader", fss3.PartitionGovCloud, fss3.PartitionGovCloud},
		"explicit, agreeing":  {"arn:aws-cn:iam::1:role/up", fss3.PartitionChina, fss3.PartitionChina},
		"explicit, aws match": {"arn:aws:iam::1:role/up", fss3.PartitionAWS, fss3.PartitionAWS},
	} {
		o := base
		o.Role, o.Partition = tc.role, tc.partition
		doc := sentPolicy(t, o)
		want := "arn:" + tc.want + ":s3:::media"
		if !slices.Equal(doc.Statement[0].Resource, []string{want + "/users/42/*"}) || !slices.Equal(doc.Statement[1].Resource, []string{want}) {
			t.Errorf("%s: resources %v and %v, want partition %s", name, doc.Statement[0].Resource, doc.Statement[1].Resource, tc.want)
		}
	}
	for name, tc := range map[string]struct{ role, partition string }{
		"contradicts role": {"arn:aws:iam::1:role/up", fss3.PartitionGovCloud},
		"wildcard":         {"uploader", "aws*"},
		"separator":        {"uploader", "aws:s3"},
		"upper case":       {"uploader", "AWS"},
	} {
		o := base
		o.Role, o.Partition = tc.role, tc.partition
		if _, err := fss3.DelegatePrefix(t.Context(), &fakeSTS{}, o); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	// PrefixPolicy keeps its old meaning: the aws partition.
	if p, err := fss3.PrefixPolicy("media", "users/42", false); err != nil || !json.Valid([]byte(p)) || !strings.Contains(p, `"arn:aws:s3:::media"`) {
		t.Errorf("PrefixPolicy = %s, %v", p, err)
	}
}

// TestDelegateKMSKeys proves KMSKeyARNs adds one statement scoped to
// exactly those keys, with kms:Decrypt for read-only credentials and
// kms:GenerateDataKey added only with Write; that no statement is added
// without keys; and that a key outside the partition, an alias, a
// wildcard or a malformed ARN is refused.
func TestDelegateKMSKeys(t *testing.T) {
	keys := []string{"arn:aws:kms:us-east-1:1:key/1234abcd-12ab-34cd-56ef-1234567890ab", "arn:aws:kms:eu-west-1:1:key/mrk-1"}
	o := fss3.DelegateOptions{Role: "arn:aws:iam::1:role/up", SessionName: "user-42", Bucket: "media", Prefix: "users/42", Duration: time.Hour}
	if doc := sentPolicy(t, o); len(doc.Statement) != 2 {
		t.Fatalf("no keys: %d statements, want 2", len(doc.Statement))
	}

	o.KMSKeyARNs = keys
	doc := sentPolicy(t, o)
	if len(doc.Statement) != 3 {
		t.Fatalf("with keys: %d statements, want 3", len(doc.Statement))
	}
	kms := doc.Statement[2]
	if kms.Effect != "Allow" || !slices.Equal(kms.Resource, keys) || !slices.Equal(kms.Action, []string{"kms:Decrypt"}) || kms.Condition != nil {
		t.Errorf("read-only KMS statement %+v", kms)
	}

	o.Write = true
	kms = sentPolicy(t, o).Statement[2]
	if !slices.Equal(kms.Resource, keys) || !slices.Equal(kms.Action, []string{"kms:Decrypt", "kms:GenerateDataKey"}) {
		t.Errorf("write KMS statement %+v", kms)
	}

	gov := o
	gov.Role, gov.KMSKeyARNs = "arn:aws-us-gov:iam::1:role/up", []string{"arn:aws-us-gov:kms:us-gov-west-1:1:key/k1"}
	if kms := sentPolicy(t, gov).Statement[2]; !slices.Equal(kms.Resource, gov.KMSKeyARNs) {
		t.Errorf("GovCloud KMS statement %+v", kms)
	}

	for name, key := range map[string]string{
		"other partition": "arn:aws-us-gov:kms:us-gov-west-1:1:key/k1",
		"alias":           "arn:aws:kms:us-east-1:1:alias/media",
		"wildcard key":    "arn:aws:kms:us-east-1:1:key/*",
		"any region":      "arn:aws:kms:*:1:key/k1",
		"bare key id":     "1234abcd-12ab-34cd-56ef-1234567890ab",
		"empty":           "",
		"no key id":       "arn:aws:kms:us-east-1:1:key/",
		"no account":      "arn:aws:kms:us-east-1::key/k1",
		"extra field":     "arn:aws:kms:us-east-1:1:key/k1:x",
		"nested path":     "arn:aws:kms:us-east-1:1:key/k1/k2",
		"not kms":         "arn:aws:s3:::media",
	} {
		bad := o
		bad.KMSKeyARNs = []string{keys[0], key}
		if _, err := fss3.DelegatePrefix(t.Context(), &fakeSTS{}, bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}
