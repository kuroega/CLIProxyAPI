//go:build windows

package cursor

import (
	"errors"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsShellProcessController struct {
	mu  sync.Mutex
	job windows.Handle
}

func newShellProcessController() (shellProcessController, error) {
	job, errJob := windows.CreateJobObject(nil, nil)
	if errJob != nil {
		return nil, errJob
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, errSet := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); errSet != nil {
		_ = windows.CloseHandle(job)
		return nil, errSet
	}
	return &windowsShellProcessController{job: job}, nil
}

func (c *windowsShellProcessController) Configure(_ *exec.Cmd) {}

func (c *windowsShellProcessController) Attach(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return errors.New("background shell process is unavailable")
	}
	c.mu.Lock()
	job := c.job
	c.mu.Unlock()
	if job == 0 {
		return errors.New("background shell job is closed")
	}
	process, errOpen := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(command.Process.Pid))
	if errOpen != nil {
		return errOpen
	}
	defer windows.CloseHandle(process)
	return windows.AssignProcessToJobObject(job, process)
}

func (c *windowsShellProcessController) Close() error {
	c.mu.Lock()
	job := c.job
	c.job = 0
	c.mu.Unlock()
	if job == 0 {
		return nil
	}
	return windows.CloseHandle(job)
}
