//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly || windows)

package session

import "os"

func lockHeadlessFile(_ *os.File) error {
	return nil
}

func unlockHeadlessFile(_ *os.File) error {
	return nil
}

func headlessProcessAlive(_ int) bool {
	return false
}
