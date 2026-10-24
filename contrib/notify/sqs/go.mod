module github.com/ubgo/filesystem/contrib/notify/sqs

go 1.26.0

require (
	github.com/aws/aws-sdk-go-v2 v1.47.1
	github.com/aws/aws-sdk-go-v2/credentials v1.20.7
	github.com/aws/aws-sdk-go-v2/service/sqs v1.52.2
	github.com/ubgo/filesystem/contrib/notify v0.0.0-00010101000000-000000000000
)

require (
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.4 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.4 // indirect
	github.com/aws/smithy-go v1.28.1 // indirect
)

replace github.com/ubgo/filesystem/contrib/notify => ../
