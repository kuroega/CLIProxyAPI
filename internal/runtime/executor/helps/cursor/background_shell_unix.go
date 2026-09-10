//go:build !windows

package cursor

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
)

type unixShellProcessController struct {
	mu  sync.Mutex
	pid int
}

func newShellProcessController() (shellProcessController, error) {
	return &unixShellProcessController{}, nil
}

func (c *unixShellProcessController) Configure(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func (c *unixShellProcessController) Attach(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return errors.New("background shell process is unavailable")
	}
	c.mu.Lock()
	c.pid = command.Process.Pid
	c.mu.Unlock()
	return nil
}

func (c *unixShellProcessController) Close() error {
	c.mu.Lock()
	pid := c.pid
	c.pid = 0
	c.mu.Unlock()
	if pid == 0 {
		return nil
	}
	if errKill := syscall.Kill(-pid, syscall.SIGKILL); errKill != nil && !errors.Is(errKill, syscall.ESRCH) {
		return errKill
	}
	return nil
}
