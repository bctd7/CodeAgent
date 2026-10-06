//go:build !windows

package shell

import (
	"fmt"
	"os/exec"
	"syscall"
)

// setSysProcAttr 为 Unix 进程设置新的进程组，以便整体终止。
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree 在 Unix 上通过进程组负 PID 发送 SIGKILL 杀死子进程及其所有子进程。
func killProcessTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		if killErr := syscall.Kill(pid, syscall.SIGKILL); killErr != nil {
			return fmt.Errorf("kill 终止进程失败 (PID %d): %w", pid, killErr)
		}
	}
	return nil
}

// resolvePlatformShell 解析 Unix 平台默认的 Shell 工具（使用 bash）。
func resolvePlatformShell() (shellName, shellPath string, args []string, isPowerShell bool, err error) {
	shellName = "bash"
	isPowerShell = false
	args = []string{"-c"}

	p, err := exec.LookPath("bash")
	if err != nil {
		return shellName, "", nil, false, fmt.Errorf("未找到 bash 执行程序: %w", err)
	}

	return shellName, p, args, false, nil
}
