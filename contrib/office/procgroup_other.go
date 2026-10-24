//go:build !unix

package office

import "os/exec"

// ownGroup does nothing where process groups are not available: a
// cancelled conversion kills the program it started, and a process that
// program started may outlive it.
func ownGroup(*exec.Cmd) {}

// killGroup does nothing where process groups are not available.
func killGroup(*exec.Cmd) {}
