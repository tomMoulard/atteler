//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly

package main

import "os/exec"

func configureHeadlessRetryCommand(_ *exec.Cmd) {}
