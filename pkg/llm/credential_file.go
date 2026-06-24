package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type credentialFileCASMismatch struct {
	path string
}

func (e *credentialFileCASMismatch) Error() string {
	if e == nil {
		return ""
	}

	return "credential file changed during refresh: " + redactCredentialLocation(e.path)
}

func isCredentialFileCASMismatch(err error) bool {
	var mismatch *credentialFileCASMismatch

	return errors.As(err, &mismatch)
}

func withCredentialFileLock(path string, fn func() error) (err error) {
	if mkdirErr := os.MkdirAll(filepath.Dir(path), 0o700); mkdirErr != nil {
		return fmt.Errorf("credential lock mkdir: %s", redactCredentialPathError(mkdirErr))
	}

	lockPath := path + ".lock"

	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("credential lock open: %s", redactCredentialPathError(err))
	}
	defer file.Close()

	if lockErr := lockCredentialFile(file); lockErr != nil {
		return lockErr
	}

	defer func() {
		if unlockErr := unlockCredentialFile(file); unlockErr != nil && err == nil {
			err = unlockErr
		}
	}()

	return fn()
}

func digestCredentialFile(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func compareCredentialFileDigest(current []byte, expectedDigest string) bool {
	if expectedDigest == "" {
		return true
	}

	return digestCredentialFile(current) == expectedDigest
}

func atomicWriteCredentialFile(ctx context.Context, path string, data []byte, mode os.FileMode) error {
	if err := requireCredentialContext(ctx); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("credential file tempfile: %s", redactCredentialPathError(err))
	}

	tmpPath := tmp.Name()
	cleanup := true

	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := os.Chmod(tmpPath, mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("credential file chmod tempfile: %s", redactCredentialPathError(err))
	}

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("credential file write tempfile: %s", redactCredentialPathError(err))
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("credential file sync tempfile: %s", redactCredentialPathError(err))
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("credential file close tempfile: %s", redactCredentialPathError(err))
	}

	if err := requireCredentialContext(ctx); err != nil {
		return err
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("credential file rename: %s", redactCredentialPathError(err))
	}

	cleanup = false

	if err := syncCredentialDir(filepath.Dir(path)); err != nil {
		return err
	}

	return nil
}
