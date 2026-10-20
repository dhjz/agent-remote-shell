package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"cnb.cool/yyx946/tteist/internal/proto"
)

// exec 非零退出时，日志里必须有「它为什么失败」。
// 之前这条路径只写 exitCode，grep error 捞出来也看不出是命令不存在还是编译不过。
func TestExecAuditKeepsFailureReason(t *testing.T) {
	s, logPath := newLogServer(t)
	resp, err := s.Exec(context.Background(), proto.ExecRequest{Cmd: "echo 编译不过 >&2; exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ExitCode != 3 {
		t.Fatalf("退出码该是 3，实际 %d", resp.ExitCode)
	}
	e := lastEntry(t, logPath)
	if e["decision"] != "error" {
		t.Fatalf("非零退出该记 error，实际 %v", e["decision"])
	}
	if e["error"] != "编译不过" {
		t.Fatalf("审计里没留下失败原因：%v", e["error"])
	}
}

// 超时也要带原因：被杀之前吐的那几行，往往正是「卡在哪」的答案。
func TestExecAuditKeepsTimedOutReason(t *testing.T) {
	s, logPath := newLogServer(t)
	resp, err := s.Exec(context.Background(), proto.ExecRequest{Cmd: "echo 卡在下载依赖 >&2; sleep 5", TimeoutMS: 200})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.TimedOut {
		t.Fatal("该判超时")
	}
	e := lastEntry(t, logPath)
	if e["reason"] == nil || !strings.Contains(e["reason"].(string), "超时") {
		t.Fatalf("reason 该说超时：%v", e["reason"])
	}
	if e["error"] != "卡在下载依赖" {
		t.Fatalf("超时那行也该带命令输出：%v", e["error"])
	}
}

// 命令一行输出都没有的时候，字段干脆不出现——空字符串进日志只会让人以为
// 「它有输出但没记下来」。
func TestExecAuditOmitsEmptyReason(t *testing.T) {
	s, logPath := newLogServer(t)
	if _, err := s.Exec(context.Background(), proto.ExecRequest{Cmd: "exit 2"}); err != nil {
		t.Fatal(err)
	}
	e := lastEntry(t, logPath)
	if e["error"] != nil {
		t.Fatalf("没有输出就不该写 error：%v", e["error"])
	}
	if e["exitCode"] != float64(2) {
		t.Fatalf("退出码还是要留着：%v", e["exitCode"])
	}
}

// 后台任务「起了就没影」时，退出那行只有一个 exitCode 等于没记。
func TestJobExitKeepsFailureReason(t *testing.T) {
	s, logPath := newLogServer(t)
	info, err := s.StartJob(proto.JobStartReq{Cmd: "echo 端口已占用; exit 7"})
	if err != nil {
		t.Fatal(err)
	}
	waitExit(t, s, info.ID, 5*time.Second)
	e := lastEntry(t, logPath)
	if e["tool"] != "job_exit" || e["decision"] != "error" {
		t.Fatalf("该有 job_exit（error）：%v", e)
	}
	if e["error"] != "端口已占用" {
		t.Fatalf("job_exit 没带失败原因：%v", e["error"])
	}
}
