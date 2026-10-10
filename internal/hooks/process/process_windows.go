//go:build windows

// 本文件负责Windows 作业对象的启动与清理。
package process

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 进程以挂起状态启动，加入作业对象后才恢复，防止提前派生后代。
// 关闭作业对象会终止所有后代，父进程已经退出时也一样。
type Tree struct{ job windows.Handle }

var resumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// Start 创建作业对象，将挂起进程加入作业后恢复执行。
func Start(cmd *exec.Cmd) (*Tree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_SUSPENDED}
	if err = cmd.Start(); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, process)
		if err == nil {
			status, _, _ := resumeProcess.Call(uintptr(process))
			if status != 0 {
				err = fmt.Errorf("NtResumeProcess: NTSTATUS %#x", status)
			}
		}
		_ = windows.CloseHandle(process)
	}
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = windows.CloseHandle(job)
		return nil, err
	}
	return &Tree{job}, nil
}

// Kill 终止作业对象中的整个进程树。
func (p *Tree) Kill() error { return windows.TerminateJobObject(p.job, 1) }

// Close 终止进程树并释放作业句柄，同时保留两步的错误。
func (p *Tree) Close() error { return errors.Join(p.Kill(), windows.CloseHandle(p.job)) }
