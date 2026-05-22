//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly || windows)

package session

import "os"

func lockHeadlessFile(_ *os.File) error {
	return nil
}

func unlockHeadlessFile(_ *os.File) error {
	return nil
}

func lockSessionFile(_ *os.File, _ string) error {
	return nil
}

func unlockSessionFile(_ *os.File, _ string) error {
	return nil
}

func headlessProcessAlive(_ int) bool {
	return false
}

func headlessProcessGroupID(_ int) int {
	return 0
}

func headlessProcessGroupAlive(_ int) bool {
	return false
}

func signalHeadlessProcess(_ int) error {
	return nil
}

func signalHeadlessProcessGroup(pid int, _ int) error {
	return signalHeadlessProcess(pid)
}
