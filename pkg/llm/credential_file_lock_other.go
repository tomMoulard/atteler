//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly || windows)

package llm

import "os"

func lockCredentialFile(_ *os.File) error {
	return nil
}

func unlockCredentialFile(_ *os.File) error {
	return nil
}

func syncCredentialDir(_ string) error {
	return nil
}
