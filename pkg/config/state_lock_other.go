//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly || windows)

package config

import "os"

func lockStateFile(_ *os.File) error {
	return nil
}

func unlockStateFile(_ *os.File) error {
	return nil
}

func syncDir(_ string) error {
	return nil
}
