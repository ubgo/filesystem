// Module remote caches media from other servers: link-preview images,
// an HMAC-signed image proxy and a federated media cache. A separate
// module so the core never grows an HTML parser.
module github.com/ubgo/filesystem/contrib/remote

go 1.26.0

// Development inside this repository builds against the working tree. A
// consumer's build ignores replace directives in dependencies.
replace github.com/ubgo/filesystem => ../../

require (
	github.com/ubgo/filesystem v0.0.0-00010101000000-000000000000
	golang.org/x/net v0.59.0
)
