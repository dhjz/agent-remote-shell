//go:build windows

package server

import "os/exec"

// reapOrphans 在 Windows 上由 taskkill /F /T 在取消时整树处理，
// 这里不额外动作，避免误伤 PID 复用后的其它进程。
func reapOrphans(cmd *exec.Cmd) {}
