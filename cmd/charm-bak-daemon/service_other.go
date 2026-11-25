//go:build !windows

package main

// runAsService: only Windows has a service manager that calls in.
func runAsService() bool { return false }
