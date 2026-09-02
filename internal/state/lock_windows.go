//go:build windows

package state

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func acquirePlatformLock(ctx context.Context, lockPath string) (Lock, error) {
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open workflow lock: %w", err)
	}
	overlapped := new(windows.Overlapped)
	for {
		err = windows.LockFileEx(
			windows.Handle(file.Fd()),
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0,
			1,
			0,
			overlapped,
		)
		if err == nil {
			return &windowsFileLock{file: file, overlapped: overlapped}, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			_ = file.Close()
			return nil, fmt.Errorf("acquire workflow lock: %w", err)
		}
		if err := waitForLockRetry(ctx); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("acquire workflow lock: %w", err)
		}
	}
}

type windowsFileLock struct {
	file       *os.File
	overlapped *windows.Overlapped
}

func (lock *windowsFileLock) Release() error {
	if lock.file == nil {
		return nil
	}
	err := windows.UnlockFileEx(windows.Handle(lock.file.Fd()), 0, 1, 0, lock.overlapped)
	closeErr := lock.file.Close()
	lock.file = nil
	if err != nil {
		return fmt.Errorf("unlock workflow: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close workflow lock: %w", closeErr)
	}
	return nil
}
