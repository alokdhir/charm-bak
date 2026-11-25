//go:build windows

package main

import (
	"golang.org/x/sys/windows/svc"
)

// serviceName is the Windows service the installer creates.
const serviceName = "charm-bak"

// runAsService runs the daemon under the Service Control Manager when
// that's what started it, and reports whether it did.
func runAsService() bool {
	is, err := svc.IsWindowsService()
	if err != nil || !is {
		return false
	}
	svc.Run(serviceName, handler{})
	return true
}

type handler struct{}

// Execute runs the daemon until it stops or Windows asks it to. Stopping
// to restart into an update ends with a service-specific error code: the
// service is set to restart on such a failure (the installer's
// sc.exe failure and failureflag), which starts the new executable.
func (handler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	done := make(chan bool, 1)
	go func() { done <- run() }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case restart := <-done:
			if restart {
				return true, 75
			}
			return false, 0
		case req := <-requests:
			switch req.Cmd {
			case svc.Interrogate:
				status <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				stopDaemon()
			}
		}
	}
}
