//go:build !windows

// 本文件负责Unix 进程组的启动与清理。
package process

import (
	"errors"
	"os/exec"
	"syscall"
)

// Tree 用进程组标识追踪子进程及其后代。
type Tree struct{ pid int }

// Start 在新进程组中启动命令，以便整组清理。
func Start(cmd *exec.Cmd) (*Tree, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Tree{cmd.Process.Pid}, nil
}

// Kill 终止进程组；进程组已不存在时视为清理完成。
func (p *Tree) Kill() error {
	err := syscall.Kill(-p.pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
func (p *Tree) Close() error { return p.Kill() }
