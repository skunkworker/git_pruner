//go:build !unix

package main

import "os/exec"

// detachTTY is a no-op where sessions do not exist; netTimeout still bounds the call.
func detachTTY(*exec.Cmd) {}
