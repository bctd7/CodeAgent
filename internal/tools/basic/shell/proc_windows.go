//go:build windows

package shell

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// setSysProcAttr 为 Windows 进程设置隐藏窗口等属性。
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

// killProcessTree 在 Windows 上调用 taskkill.exe /F /T /PID 强行杀死整棵进程树。
// 若 taskkill 执行失败，尝试强制终止主进程，并返回实际清理错误。
func killProcessTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = "C:\\Windows"
	}
	taskkill := filepath.Join(systemRoot, "System32", "taskkill.exe")
	killCmd := exec.Command(taskkill, "/F", "/T", "/PID", strconv.Itoa(pid))
	setSysProcAttr(killCmd)

	if err := killCmd.Run(); err != nil {
		if p, findErr := os.FindProcess(pid); findErr == nil {
			_ = p.Kill()
		}
		return fmt.Errorf("taskkill 终止进程树失败 (PID %d): %w", pid, err)
	}
	return nil
}

// resolvePlatformShell 解析 Windows 平台默认的 Shell 工具（优先 pwsh，其次 powershell）。
func resolvePlatformShell() (shellName, shellPath string, args []string, isPowerShell bool, err error) {
	shellName = "powershell"
	isPowerShell = true
	args = []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command"}

	// 优先寻找 pwsh (PowerShell 7+)
	if p, err := exec.LookPath("pwsh.exe"); err == nil {
		return shellName, p, args, true, nil
	}
	if p, err := exec.LookPath("pwsh"); err == nil {
		return shellName, p, args, true, nil
	}

	// 其次寻找 Windows PowerShell (powershell.exe)
	if p, err := exec.LookPath("powershell.exe"); err == nil {
		return shellName, p, args, true, nil
	}
	if p, err := exec.LookPath("powershell"); err == nil {
		return shellName, p, args, true, nil
	}

	return shellName, "", nil, true, fmt.Errorf("未找到 PowerShell 执行程序 (pwsh/powershell)")
}
