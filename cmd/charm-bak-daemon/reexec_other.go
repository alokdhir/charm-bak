//go:build !unix

package main

// reexec isn't possible here; main exits non-zero and the service
// manager starts the new executable.
func reexec() {}
