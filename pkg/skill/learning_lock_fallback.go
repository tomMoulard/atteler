//go:build !windows && !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly

package skill

import "os"

func lockLearningFile(_ *os.File) error {
	return nil
}

func unlockLearningFile(_ *os.File) error {
	return nil
}
