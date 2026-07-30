//go:build !windows

package main

import "syscall"

// processExists checks if a process with the given PID is running.
func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil
}

// sendSignal sends SIGTERM to a process.
func sendSignal(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// sysProcAttr returns platform-specific process attributes for daemonization.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
