//go:build windows

package server

import (
	"os/exec"
	"strconv"
	"syscall"
)

// setProcAttr 让超时能杀掉整棵进程树。
// 单纯 cmd.Process.Kill() 只杀直接子进程，mvn / gradle 这种会留下孙子进程继续跑。
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
	cmd.Cancel = func() error { return killTree(cmd) }
}

func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	// taskkill /T 是 Windows 上唯一可靠的树杀；/F 强制
	tk := exec.Command("taskkill", "/F", "/T", "/PID", pid)
	tk.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	_ = tk.Run()
	return cmd.Process.Kill()
}
