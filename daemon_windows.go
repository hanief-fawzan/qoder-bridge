//go:build windows

package main

import (
	"os"
	"syscall"
)

// processExists checks if a process with the given PID is running.
func processExists(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(os.Kill)
	return err == nil
}

// sendSignal sends a termination signal to a process.
func sendSignal(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(os.Kill)
}

// sysProcAttr returns platform-specific process attributes for daemonization.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}
