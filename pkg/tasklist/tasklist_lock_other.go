//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly || windows)

package tasklist

import (
	"context"
	"errors"
	"os"
)

func lockTaskFile(ctx context.Context, _ *os.File) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}

	return errors.New("tasklist: interprocess file locking is not supported on this platform")
}

func unlockTaskFile(_ *os.File) error {
	return nil
}

func syncTaskDir(_ string) error {
	return nil
}
