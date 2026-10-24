module github.com/ubgo/filesystem/contrib/cloudfront

go 1.26.0

replace github.com/ubgo/filesystem => ../../

require (
	github.com/aws/aws-sdk-go-v2/feature/cloudfront/sign v1.12.4
	github.com/ubgo/filesystem v0.0.0-00010101000000-000000000000
)
