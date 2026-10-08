//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// detachTTY starts cmd in its own session, which has no controlling terminal.
func detachTTY(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
