//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

// reexec replaces this process with the executable now at selfPath.
func reexec() {
	err := syscall.Exec(selfPath, os.Args, os.Environ())
	fmt.Fprintln(os.Stderr, "charm-bak-daemon: restarting after update:", err)
}
