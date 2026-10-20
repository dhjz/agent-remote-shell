//go:build !windows

package server

import (
	"os/exec"
	"syscall"
)

// setProcAttr 放进独立进程组，超时时按组 SIGKILL。
// 实测发现：只 kill 主进程时，`sleep 300 & wait` 会留下孤儿进程，
// 所以必须按 pgid 打整组，并且等 goroutine 收尾，不能提前返回。
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		pgid := cmd.Process.Pid
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
