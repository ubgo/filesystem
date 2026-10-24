// Module svg sanitises user-uploaded SVG in pure Go. A separate module so
// the core's sniffing never grows an XML rewriter, and so an application
// that does not accept SVG never compiles one.
module github.com/ubgo/filesystem/contrib/svg

go 1.26.0

// Development inside this repository builds against the working tree. A
// consumer's build ignores replace directives in dependencies.
replace github.com/ubgo/filesystem => ../../

require github.com/ubgo/filesystem v0.0.0-00010101000000-000000000000
