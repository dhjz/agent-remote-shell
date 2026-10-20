//go:build !windows

package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cnb.cool/yyx946/tteist/internal/proto"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Config{Token: "0123456789abcdef0123", Root: t.TempDir(), Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func waitExit(t *testing.T, s *Server, id string, d time.Duration) proto.JobLogsResp {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		resp, err := s.JobLogs(proto.JobLogsReq{ID: id, Since: 0})
		if err != nil {
			t.Fatal(err)
		}
		if !resp.Running {
			return resp
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s 在 %s 内没有退出", id, d)
	return proto.JobLogsResp{}
}

// 后台任务的立命之本：起得来、输出落盘、退出码能看见。
func TestJobStartLogsExit(t *testing.T) {
	s := newTestServer(t)
	info, err := s.StartJob(proto.JobStartReq{Cmd: "echo hello; exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if info.PID == 0 {
		t.Fatal("PID 应该被填上")
	}
	resp := waitExit(t, s, info.ID, 5*time.Second)
	if resp.ExitCode != 3 {
		t.Errorf("exit=%d want 3", resp.ExitCode)
	}
	if !strings.Contains(resp.Output, "hello") {
		t.Errorf("日志里没有 hello: %q", resp.Output)
	}
}

// since 只能拿到新增部分，否则跟日志会把整份文件重新刷一遍。
func TestJobLogsSinceIncremental(t *testing.T) {
	s := newTestServer(t)
	info, err := s.StartJob(proto.JobStartReq{Cmd: "echo a; sleep 0.3; echo b"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.JobLogs(proto.JobLogsReq{ID: info.ID, Wait: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Output, "a") {
		t.Fatalf("第一段应含 a: %q", first.Output)
	}
	second, err := s.JobLogs(proto.JobLogsReq{ID: info.ID, Since: first.Offset, Wait: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(second.Output, "\na\n") {
		t.Errorf("since 应该只回增量，实际回了: %q", second.Output)
	}
	waitExit(t, s, info.ID, 5*time.Second)
}

func TestJobTail(t *testing.T) {
	s := newTestServer(t)
	info, err := s.StartJob(proto.JobStartReq{Cmd: "printf 'l1\\nl2\\nl3\\n'"})
	if err != nil {
		t.Fatal(err)
	}
	waitExit(t, s, info.ID, 5*time.Second)
	resp, err := s.JobLogs(proto.JobLogsReq{ID: info.ID, Tail: 2})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resp.Output, "l1") || !strings.Contains(resp.Output, "l3") {
		t.Errorf("tail=2 结果不对: %q", resp.Output)
	}
}

// kill 要连子进程一起收掉，否则 `sh -c 'sleep &'` 会留孤儿。
func TestJobKill(t *testing.T) {
	s := newTestServer(t)
	info, err := s.StartJob(proto.JobStartReq{Cmd: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.KillJob(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Running {
		t.Error("kill 后不该还在 running")
	}
	if len(s.ListJobs().Jobs) != 1 {
		t.Error("kill 不该把 job 从列表里删掉，删要显式 drop")
	}
	if err := s.DropJob(info.ID); err != nil {
		t.Fatal(err)
	}
	if len(s.ListJobs().Jobs) != 0 {
		t.Error("drop 后应为空")
	}
}

func TestJobNotFound(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.JobLogs(proto.JobLogsReq{ID: "nope"}); err == nil {
		t.Fatal("不存在的 job 应报错")
	}
}

// 超时的 job 用 124，和 exec 的超时语义对齐。
func TestJobTimeout(t *testing.T) {
	s := newTestServer(t)
	info, err := s.StartJob(proto.JobStartReq{Cmd: "sleep 30", TimeoutMS: 300})
	if err != nil {
		t.Fatal(err)
	}
	resp := waitExit(t, s, info.ID, 10*time.Second)
	if resp.ExitCode != 124 {
		t.Errorf("超时 exit=%d want 124", resp.ExitCode)
	}
}

// until 是「等到这一行出现就返回」：起常驻服务时靠它拿首屏，不必拿 wait_ms 猜时长。
func TestJobLogsUntilPattern(t *testing.T) {
	s := newTestServer(t)
	info, err := s.StartJob(proto.JobStartReq{Cmd: "echo starting; sleep 0.2; echo 'Listening on :8002'; sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.DropJob(info.ID) }()
	// 先把已经写出来的那段消化掉：until 只看「本次新增」（since 之后），
	// 这是增量语义本来的样子，服务端不会把整份历史翻出来再匹配一遍。
	first, err := s.JobLogs(proto.JobLogsReq{ID: info.ID})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	resp, err := s.JobLogs(proto.JobLogsReq{ID: info.ID, Since: first.Offset, Until: `Listening on :8002`, Timeout: 10000})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Matched {
		t.Fatalf("应该命中 until，实际没命中：%q", resp.Output)
	}
	if !strings.Contains(resp.Output, "Listening on :8002") {
		t.Errorf("命中时该带着那一行一起回来：%q", resp.Output)
	}
	if !resp.Running {
		t.Error("进程还在跑，running 应为 true")
	}
	// 命中就该走，不能等到兜底时长
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("命中后应立即返回，实际等了 %s", d)
	}
}

// 一直等不到也要收场：进程既不起也不退时只靠兜底时长。
// 此时 matched=false——不能让人当成「服务起好了」。
func TestJobLogsUntilTimeout(t *testing.T) {
	s := newTestServer(t)
	info, err := s.StartJob(proto.JobStartReq{Cmd: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.DropJob(info.ID) }()

	start := time.Now()
	resp, err := s.JobLogs(proto.JobLogsReq{ID: info.ID, Until: "never-printed", Timeout: 300})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Matched {
		t.Error("没等到时 matched 必须是 false")
	}
	if d := time.Since(start); d < 300*time.Millisecond || d > 5*time.Second {
		t.Errorf("兜底时长不对：等了 %s", d)
	}
	if !resp.Running {
		t.Error("进程还活着，running 应为 true")
	}
}

// 进程退出优先于 until：服务起不来时不该陪它等满兜底时长。
func TestJobLogsUntilExitWins(t *testing.T) {
	s := newTestServer(t)
	info, err := s.StartJob(proto.JobStartReq{Cmd: "echo boom; exit 2"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := s.JobLogs(proto.JobLogsReq{ID: info.ID, Until: "Listening on", Timeout: 30000})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Running || resp.ExitCode != 2 {
		t.Errorf("该跟着进程退出返回：running=%v exit=%d", resp.Running, resp.ExitCode)
	}
	if resp.Matched {
		t.Error("退出那条日志里没有直到的行，matched 应为 false")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("进程退出后应立即返回，实际等了 %s", d)
	}
}

// 非法正则是 400 级错误，不是「悄悄不命中」。
func TestJobLogsUntilBadRegex(t *testing.T) {
	s := newTestServer(t)
	info, err := s.StartJob(proto.JobStartReq{Cmd: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.DropJob(info.ID) }()
	if _, err := s.JobLogs(proto.JobLogsReq{ID: info.ID, Until: "a("}); err == nil {
		t.Fatal("非法 until 正则应报错")
	}
}

// kill 带 drop_log：一条命令收干净，不留 .wr-jobs 里的残骸。
func TestJobKillDropLog(t *testing.T) {
	s := newTestServer(t)
	info, err := s.StartJob(proto.JobStartReq{Cmd: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	logPath, err := s.root.Resolve(info.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.KillJob(info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("不带 drop_log 时日志该留着: %v", err)
	}
	if _, err := s.killJob(info.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("drop_log 后日志该被删掉: %v", err)
	}
}

// 过期的日志在启动时清掉；还在写的（mtime 新）和 retain=0 时一律不动。
func TestReapJobLogs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, jobDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, age time.Duration) string {
		p := filepath.Join(root, jobDirName, name)
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stale := mk("0101-000000-aaaaaaaa.log", 72*time.Hour)
	fresh := mk("0101-000000-bbbbbbbb.log", time.Hour)

	if _, err := New(Config{Token: "0123456789abcdef0123", Root: root, Quiet: true, JobRetain: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("超过保留期的日志该被清掉: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("还新的日志不该动: %v", err)
	}

	// retain=0 = 不自动清，这是缺省
	keep := mk("0101-000000-cccccccc.log", 720*time.Hour)
	if _, err := New(Config{Token: "0123456789abcdef0123", Root: root, Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("没开 -job-retain 时不该清任何日志: %v", err)
	}
}
