// Module archive reads user-uploaded zip and tar archives under limits. A
// separate module so an application that never accepts archives never
// compiles an archive walker, and so the core stays free of it.
module github.com/ubgo/filesystem/contrib/archive

go 1.26.0

// Development inside this repository builds against the working tree. A
// consumer's build ignores replace directives in dependencies.
replace github.com/ubgo/filesystem => ../../

require github.com/ubgo/filesystem v0.0.0-00010101000000-000000000000
