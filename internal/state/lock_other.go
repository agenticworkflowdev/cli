//go:build !unix && !windows

package state

import (
	"context"
	"errors"
)

func acquirePlatformLock(_ context.Context, _ string) (Lock, error) {
	return nil, errors.New("process-bound workflow locking is unsupported on this platform")
}
