// Module crypt encrypts objects before they reach a backend. A separate
// module so the core never carries key handling, and so an application
// that does not encrypt never compiles it.
module github.com/ubgo/filesystem/contrib/crypt

go 1.26.0

// Development inside this repository builds against the working tree. A
// consumer's build ignores replace directives in dependencies.
replace github.com/ubgo/filesystem => ../../

require github.com/ubgo/filesystem v0.0.0-00010101000000-000000000000
