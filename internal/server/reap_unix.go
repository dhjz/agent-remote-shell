//go:build !windows

package server

import (
	"os/exec"
	"syscall"
	"time"
)

// reapOrphans 清掉命令留在进程组里的孙进程。
//
// 为什么需要它：bash -c 'a & b & wait' 里 a/b 是 bash 自己 fork 的，
// 它们 join 进程组有延迟。cmd.Cancel 打 SIGKILL 时它们可能还没进来，
// 主进程一死就成孤儿继续跑。实测 1s 超时后能留下 2 个 sleep 300。
//
// 做法：主进程退出后反复对整组发 SIGKILL，直到 kill(2) 报 ESRCH（组已空）。
// 注意判断条件是「组不存在」而不是「返回值为 0」：组里只剩僵尸时
// kill 仍会成功，靠固定次数上限收尾。
func reapOrphans(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(-pgid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
