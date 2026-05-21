//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly || windows)

package tasklist

import (
	"context"
	"os"
)

func lockTaskFile(ctx context.Context, _ *os.File) error {
	return ctxErr(ctx)
}

func unlockTaskFile(_ *os.File) error {
	return nil
}

func syncTaskDir(_ string) error {
	return nil
}
