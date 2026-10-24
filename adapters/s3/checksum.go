package s3

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// checksumAlgorithm is the checksum algorithm of calls on key made with
// ctx: RequestOptions.ChecksumAlgorithm, else Config.ChecksumAlgorithm,
// else none (the SDK default for writes, no checksum asked for on Stat).
func (d *Driver) checksumAlgorithm(ctx context.Context, key string) types.ChecksumAlgorithm {
	if a := requestOptions(ctx, key).ChecksumAlgorithm; a != "" {
		return a
	}
	return d.cfg.ChecksumAlgorithm
}

// storedChecksum is the base64 checksum of algorithm alg that HeadObject
// returned, "" when the object has none of that algorithm.
func storedChecksum(out *s3.HeadObjectOutput, alg types.ChecksumAlgorithm) string {
	switch alg {
	case types.ChecksumAlgorithmCrc32:
		return aws.ToString(out.ChecksumCRC32)
	case types.ChecksumAlgorithmCrc32c:
		return aws.ToString(out.ChecksumCRC32C)
	case types.ChecksumAlgorithmCrc64nvme:
		return aws.ToString(out.ChecksumCRC64NVME)
	case types.ChecksumAlgorithmSha1:
		return aws.ToString(out.ChecksumSHA1)
	case types.ChecksumAlgorithmSha256:
		return aws.ToString(out.ChecksumSHA256)
	}
	return ""
}
