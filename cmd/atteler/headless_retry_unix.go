//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"os/exec"
	"syscall"
)

func configureHeadlessRetryCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
