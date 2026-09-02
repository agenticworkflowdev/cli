//go:build windows

package process

import (
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

func configureProcessTree(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	return nil
}

func terminateProcessTree(pid int, force bool) {
	// taskkill's tree traversal requires the group leader to remain addressable,
	// so terminate the complete tree immediately on Windows for both passes.
	_ = exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
}
