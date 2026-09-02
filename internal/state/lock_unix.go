//go:build unix

package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
)

func acquirePlatformLock(ctx context.Context, lockPath string) (Lock, error) {
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open workflow lock: %w", err)
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &fileLock{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("acquire workflow lock: %w", err)
		}
		if err := waitForLockRetry(ctx); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("acquire workflow lock: %w", err)
		}
	}
}

type fileLock struct {
	file *os.File
}

func (lock *fileLock) Release() error {
	if lock.file == nil {
		return nil
	}
	err := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
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
