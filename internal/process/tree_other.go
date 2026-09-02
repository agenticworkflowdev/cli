//go:build !unix && !windows

package process

import (
	"errors"
	"os/exec"
)

func configureProcessTree(_ *exec.Cmd) error {
	return errors.New("process-tree termination is unsupported on this platform")
}

func terminateProcessTree(_ int, _ bool) {}
