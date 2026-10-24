module github.com/ubgo/filesystem/jobs/river

go 1.26.0

replace (
	github.com/ubgo/filesystem => ../../
	github.com/ubgo/filesystem/jobs/diskjobs => ../diskjobs
)

require (
	github.com/jackc/pgx/v5 v5.11.0
	github.com/riverqueue/river v0.49.0
	github.com/riverqueue/river/riverdriver/riverpgxv5 v0.49.0
	github.com/riverqueue/river/rivertype v0.49.0
	github.com/ubgo/filesystem v0.0.0-00010101000000-000000000000
	github.com/ubgo/filesystem/jobs/diskjobs v0.0.0-00010101000000-000000000000
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/riverqueue/river/riverdriver v0.49.0 // indirect
	github.com/riverqueue/river/rivershared v0.49.0 // indirect
	github.com/tidwall/gjson v1.19.0 // indirect
	github.com/tidwall/match v1.2.0 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)
