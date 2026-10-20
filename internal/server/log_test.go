package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cnb.cool/yyx946/tteist/internal/proto"
)

// newLogServer 起一个开了审计日志的测试服务，返回服务端和日志落点。
func newLogServer(t *testing.T) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	s, err := New(Config{
		Token: "0123456789abcdef0123", Root: t.TempDir(), Quiet: true,
		LogPath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// entries 把日志读成结构体切片。测试只关心「写了什么」，不关心行长。
func entries(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("日志里有半行或非 JSON: %v (%s)", err, line)
		}
		out = append(out, m)
	}
	return out
}

func findEntry(t *testing.T, all []map[string]any, tool string) map[string]any {
	t.Helper()
	for _, e := range all {
		if e["tool"] == tool {
			return e
		}
	}
	t.Fatalf("日志里没有 tool=%s 的记录: %v", tool, all)
	return nil
}

// exec 是审计的主战场：命令本身、工作目录、退出码、耗时都得留下。
func TestAuditExec(t *testing.T) {
	s, path := newLogServer(t)
	resp, err := s.Exec(context.Background(), proto.ExecRequest{Cmd: "echo hi", Shell: "sh"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("命令没跑成: %+v", resp)
	}
	e := findEntry(t, entries(t, path), "exec")
	if e["decision"] != "allowed" {
		t.Errorf("decision 该是 allowed: %v", e)
	}
	if e["command"] != "echo hi" {
		t.Errorf("command 没记: %v", e["command"])
	}
	if _, ok := e["exitCode"]; !ok {
		t.Error("exitCode 没记")
	}
	if _, ok := e["durationMs"]; !ok {
		t.Error("durationMs 没记")
	}
	if e["time"] == "" {
		t.Error("time 没记")
	}
	// 用的是哪个 shell 必须记：同一条命令在 bash / pwsh / cmd 里不是一回事，
	// 事后翻日志看到 command 却不知道谁跑的，等于只记了一半。
	if e["shell"] != "sh" {
		t.Errorf("shell 没记: %v", e["shell"])
	}
}

// 命令非零退出记 error，不是 allowed：error 一把能捞出所有「没按预期完成」的动作。
func TestAuditExecNonZeroIsError(t *testing.T) {
	s, path := newLogServer(t)
	resp, err := s.Exec(context.Background(), proto.ExecRequest{Cmd: "exit 7", Shell: "sh"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ExitCode != 7 {
		t.Fatalf("exit code: %d", resp.ExitCode)
	}
	e := findEntry(t, entries(t, path), "exec")
	if e["decision"] != "error" {
		t.Errorf("非零退出该记 error: %v", e)
	}
	if e["exitCode"].(float64) != 7 {
		t.Errorf("exitCode 该是 7: %v", e["exitCode"])
	}
}

// 策略拒绝也要留痕：这才是一份「谁试过什么」的日志。
func TestAuditBlockedExec(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	s, err := New(Config{
		Token: "0123456789abcdef0123", Root: t.TempDir(), Quiet: true,
		LogPath: path, Deny: []string{`^rm -rf /$`},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.Exec(context.Background(), proto.ExecRequest{Cmd: "rm -rf /", Shell: "sh"}); err == nil {
		t.Fatal("该被黑名单拦下")
	}
	e := findEntry(t, entries(t, path), "exec")
	if e["decision"] != "blocked" {
		t.Errorf("该记 blocked: %v", e)
	}
	if e["reason"] == nil || e["reason"] == "" {
		t.Errorf("blocked 该带 reason: %v", e)
	}
	if e["command"] != "rm -rf /" {
		t.Errorf("被拒的命令也要记下来: %v", e["command"])
	}
	// 被拒的尝试最该知道「他打算用哪个 shell 干」，所以这里也带上请求里的值。
	if e["shell"] != "sh" {
		t.Errorf("blocked 那行该带请求的 shell: %v", e["shell"])
	}
}

// 文件写操作要能被追：写入、编辑、删除、搬移、建目录。
func TestAuditFileOps(t *testing.T) {
	s, path := newLogServer(t)
	if _, err := s.Write(proto.WriteReq{Path: "a.txt", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Edit(proto.EditReq{Path: "a.txt", Edits: []proto.Edit{{Old: "hello", New: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mkdir(proto.MkdirReq{Path: "sub"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(proto.MoveReq{From: "a.txt", To: "sub/b.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete(proto.DeleteReq{Path: "sub", Recursive: true}); err != nil {
		t.Fatal(err)
	}

	all := entries(t, path)
	for _, tool := range []string{"write", "edit", "mkdir", "move", "delete"} {
		e := findEntry(t, all, tool)
		if e["decision"] != "allowed" {
			t.Errorf("%s 该是 allowed: %v", tool, e)
		}
	}
	if e := findEntry(t, all, "write"); e["path"] != "a.txt" || e["bytes"].(float64) != 5 {
		t.Errorf("write 该记 path 和 bytes: %v", e)
	}
	if e := findEntry(t, all, "move"); e["path"] != "a.txt" {
		t.Errorf("move 的 path 该是来源: %v", e)
	}
}

// .git 下的写入被拒，也要留痕——「有人试过改 .git」本身就是重要信号。
func TestAuditGitBlocked(t *testing.T) {
	s, path := newLogServer(t)
	if _, err := s.Write(proto.WriteReq{Path: ".git/config", Content: "x"}); err == nil {
		t.Fatal("该拒绝写 .git")
	}
	all := entries(t, path)
	e := findEntry(t, all, "write")
	if e["decision"] != "blocked" {
		t.Errorf("该记 blocked: %v", e)
	}
	if !strings.Contains(e["reason"].(string), ".git") {
		t.Errorf("reason 该说明是 .git: %v", e["reason"])
	}
	// 一次拒绝只该留一行：两行会让人以为有人试了两次。
	if len(all) != 1 {
		t.Fatalf("一次被拒该只留 1 行，实际 %d 行: %v", len(all), all)
	}
}

// 同一个 .git 拒绝走 HTTP 时也只能一行，而且要带上来源地址。
func TestAuditGitBlockedOverHTTPOnce(t *testing.T) {
	s, path := newLogServer(t)
	body := `{"path":".git/config","content":"x"}`
	req := httptest.NewRequest("POST", "/write?token="+s.cfg.Token, strings.NewReader(body))
	req.RemoteAddr = "10.9.8.7:1234"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("该 400，实际 %d", w.Code)
	}
	all := entries(t, path)
	if len(all) != 1 {
		t.Fatalf("一次被拒该只留 1 行，实际 %d 行: %v", len(all), all)
	}
	if all[0]["decision"] != "blocked" || all[0]["remote"] != "10.9.8.7" {
		t.Errorf("该是带 remote 的 blocked: %v", all[0])
	}
}

// 只读动作也要留痕：审计回答的是「这个 token 被谁动过什么」，
// 只记写动作时，读过一遍全部源码和没发生过在日志里长得一模一样。
// 但只记规模不记内容——所以这里同时钉住「有这行」和「没有正文」。
func TestAuditReadOnlyRecorded(t *testing.T) {
	s, path := newLogServer(t)
	if _, err := s.Write(proto.WriteReq{Path: "a.txt", Content: "hello secret"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(proto.ReadReq{Path: "a.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(proto.ListReq{Dir: "."}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Glob(proto.GlobReq{Pattern: "*.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grep(proto.GrepReq{Pattern: "secret"}); err != nil {
		t.Fatal(err)
	}

	all := entries(t, path)
	for _, tool := range []string{"read", "list", "glob", "grep"} {
		e := findEntry(t, all, tool)
		if e["decision"] != "allowed" {
			t.Errorf("%s 该记 allowed: %v", tool, e)
		}
		if _, ok := e["count"]; !ok {
			t.Errorf("%s 该带 count（结果规模）: %v", tool, e)
		}
	}

	if e := findEntry(t, all, "read"); e["path"] != "a.txt" {
		t.Errorf("read 该记路径: %v", e)
	}
	if e := findEntry(t, all, "grep"); e["pattern"] != "secret" {
		t.Errorf("grep 该记正则: %v", e)
	}
	if e := findEntry(t, all, "glob"); e["pattern"] != "*.txt" {
		t.Errorf("glob 该记模式: %v", e)
	}
	if e := findEntry(t, all, "list"); e["path"] != "." {
		t.Errorf("list 该记目录（根写成 .）: %v", e)
	}

	// count 只说「几条」，说不出「多长」：glob 命中 1 个也好、命中 200 个也好，
	// 只有 count 时看不出这次到底把多少字节塞进了调用方的上下文。
	// 所以每个读动作都要带上返回体大小，且和真实响应序列化后一致。
	readResp, err := s.Read(proto.ReadReq{Path: "a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	listResp, err := s.List(proto.ListReq{Dir: "."})
	if err != nil {
		t.Fatal(err)
	}
	globResp, err := s.Glob(proto.GlobReq{Pattern: "*.txt"})
	if err != nil {
		t.Fatal(err)
	}
	grepResp, err := s.Grep(proto.GrepReq{Pattern: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	for tool, resp := range map[string]any{
		"read": readResp, "list": listResp, "glob": globResp, "grep": grepResp,
	} {
		e := findEntry(t, all, tool)
		got, ok := e["bytes"].(float64)
		if !ok {
			t.Errorf("%s 该带 bytes（返回体大小）: %v", tool, e)
			continue
		}
		raw, _ := json.Marshal(resp)
		if int(got) != len(raw) {
			t.Errorf("%s 的 bytes=%v，响应实际 %d 字节", tool, got, len(raw))
		}
	}

	// 内容不许落进日志：审计里出现文件正文就等于又存了一份代码。
	for _, e := range all {
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), "hello secret") {
			t.Fatalf("审计日志里出现了文件内容: %s", raw)
		}
		if _, ok := e["content"]; ok {
			t.Fatalf("审计条目不该有 content 字段: %v", e)
		}
	}
}

// 只读失败也记一行 error：读越界、路径逃逸这些「试了但没成」，
// 恰恰是排查误用时最有价值的那几条。
func TestAuditReadError(t *testing.T) {
	s, path := newLogServer(t)
	if _, err := s.Read(proto.ReadReq{Path: "../etc/passwd"}); err == nil {
		t.Fatal("该拒绝沙箱外路径")
	}
	e := findEntry(t, entries(t, path), "read")
	if e["decision"] != "error" {
		t.Errorf("该记 error: %v", e)
	}
	// 失败的行数必须是 0：记成 1 会让「读越界」看起来像读到了东西。
	if e["count"] != float64(0) {
		t.Errorf("失败时 count 该是 0: %v", e)
	}
	// 什么都没返回时不该有 bytes：0 字节和「没这个字段」都得说得清楚，
	// 写个 0 反而像「返回了空内容」，那是另一回事。
	if _, ok := e["bytes"]; ok {
		t.Errorf("失败时不该记 bytes: %v", e)
	}
}

// 走 HTTP 的只读也要带上来源地址，和写动作一样能追到人。
func TestAuditReadOverHTTP(t *testing.T) {
	s, path := newLogServer(t)
	if _, err := s.Write(proto.WriteReq{Path: "a.txt", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/read?token="+s.cfg.Token, strings.NewReader(`{"path":"a.txt"}`))
	req.RemoteAddr = "10.9.8.7:1234"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("该 200，实际 %d: %s", w.Code, w.Body.String())
	}
	e := findEntry(t, entries(t, path), "read")
	if e["remote"] != "10.9.8.7" {
		t.Errorf("read 该带 remote: %v", e)
	}
}

// 后台任务成对留痕：起一行、退一行，同一个 sessionId。
func TestAuditJobLifecycle(t *testing.T) {
	s, path := newLogServer(t)
	info, err := s.StartJob(proto.JobStartReq{Cmd: "echo out", Shell: "sh"})
	if err != nil {
		t.Fatal(err)
	}
	waitExit(t, s, info.ID, 10*time.Second)

	all := entries(t, path)
	start := findEntry(t, all, "job_start")
	end := findEntry(t, all, "job_exit")
	if start["sessionId"] != info.ID || end["sessionId"] != info.ID {
		t.Errorf("两行该用同一个 sessionId=%s: %v / %v", info.ID, start["sessionId"], end["sessionId"])
	}
	if end["exitCode"] == nil {
		t.Errorf("job_exit 该带 exitCode: %v", end)
	}
	// 后台任务最容易「起了就没影」：退出那行必须自己说明白是谁跑的，
	// 别让人回头再去 grep 启动那行。
	if start["shell"] != "sh" || end["shell"] != "sh" {
		t.Errorf("起止两行都要带 shell: %v / %v", start["shell"], end["shell"])
	}
}

func TestAuditJobKill(t *testing.T) {
	s, path := newLogServer(t)
	info, err := s.StartJob(proto.JobStartReq{Cmd: "sleep 30", Shell: "sh"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.KillJob(info.ID); err != nil {
		t.Fatal(err)
	}
	findEntry(t, entries(t, path), "job_kill")
}

// /log 三种取法：从头、tail、after。after 是跟日志的关键，不能重复也不能漏。
func TestServerLogRead(t *testing.T) {
	s, path := newLogServer(t)
	for i := 0; i < 3; i++ {
		if _, err := s.Write(proto.WriteReq{Path: "a.txt", Content: "x"}); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := s.Log(proto.LogReq{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Path != path {
		t.Errorf("path 该是绝对路径: %s", resp.Path)
	}
	if n := strings.Count(strings.TrimSpace(resp.Output), "\n") + 1; n != 3 {
		t.Errorf("该读到 3 行，实际 %d:\n%s", n, resp.Output)
	}

	tail, err := s.Log(proto.LogReq{Tail: 1})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.TrimSpace(tail.Output), "\n") + 1; n != 1 {
		t.Errorf("tail=1 该只回 1 行，实际 %d:\n%s", n, tail.Output)
	}

	// after=末尾偏移：没有新增时回空
	empty, err := s.Log(proto.LogReq{After: tail.Offset})
	if err != nil {
		t.Fatal(err)
	}
	if empty.Output != "" {
		t.Errorf("没有新增时该回空: %q", empty.Output)
	}

	// 再写一条，after 应该只看得到它
	if _, err := s.Write(proto.WriteReq{Path: "b.txt", Content: "y"}); err != nil {
		t.Fatal(err)
	}
	more, err := s.Log(proto.LogReq{After: tail.Offset})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(more.Output, "b.txt") || strings.Contains(more.Output, "a.txt") {
		t.Errorf("after 该只回新增那条: %q", more.Output)
	}
}

// 清空要留痕：先写一行再截断，之后补回这一行。
func TestServerLogClear(t *testing.T) {
	s, path := newLogServer(t)
	if _, err := s.Write(proto.WriteReq{Path: "a.txt", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	resp, err := s.ClearLog()
	if err != nil {
		t.Fatal(err)
	}
	if resp.Cleared == 0 {
		t.Error("cleared 该是清掉的字节数")
	}
	all := entries(t, path)
	findEntry(t, all, "log_clear")
	// 清空动作本身还在，说明是「先留痕再截断」
	if len(all) != 1 {
		t.Errorf("清空后该只剩 log_clear 一行，实际 %d 行: %v", len(all), all)
	}
}

// 没开日志时 /log 要给出能照着做的报错，而不是空响应。
func TestServerLogDisabled(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.Log(proto.LogReq{}); err == nil {
		t.Fatal("没开日志该报错")
	}
	if _, err := s.ClearLog(); err == nil {
		t.Fatal("没开日志时清空也该报错")
	}
}

// /info 要告诉客户端日志落在哪儿：它可能在沙箱外，路径只有服务端知道。
func TestInfoExposesAuditPath(t *testing.T) {
	s, path := newLogServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/info?token="+s.cfg.Token, nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var info proto.InfoResp
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.AuditLog != path {
		t.Errorf("info.audit_log 该是 %s，实际 %q", path, info.AuditLog)
	}
}

// HTTP 层：写操作要带上来源地址，token 不能被写进日志。
func TestAuditHTTPRemoteAndNoToken(t *testing.T) {
	s, path := newLogServer(t)
	h := s.Handler()
	body := `{"path":"a.txt","content":"x"}`
	req := httptest.NewRequest("POST", "/write?token="+s.cfg.Token, strings.NewReader(body))
	req.RemoteAddr = "10.1.2.3:5555"
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), s.cfg.Token) {
		t.Fatalf("token 被写进日志了:\n%s", raw)
	}
	e := findEntry(t, entries(t, path), "write")
	if e["remote"] != "10.1.2.3" {
		t.Errorf("remote 该是裸 IP: %v", e["remote"])
	}
}

// 401 不该产生审计行：没通过鉴权的请求连「动作」都算不上。
func TestAuditNoEntryOn401(t *testing.T) {
	s, path := newLogServer(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/exec", strings.NewReader(`{"cmd":"echo hi"}`)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("该 401，实际 %d", w.Code)
	}
	if all := entries(t, path); len(all) != 0 {
		t.Fatalf("401 不该写审计: %v", all)
	}
}

// 横幅要把落点打出来，否则默认路径只能靠猜。
func TestBannerShowsAuditPath(t *testing.T) {
	out := bannerWith("127.0.0.1:7573", "tk", "/tmp/repo", "", "/tmp/repo/.wr.log")
	if !strings.Contains(out, "审计日志 /tmp/repo/.wr.log") {
		t.Fatalf("横幅没打审计路径:\n%s", out)
	}
	out = bannerWith("127.0.0.1:7573", "tk", "/tmp/repo", "", "")
	if !strings.Contains(out, "未开启") {
		t.Fatalf("关掉日志时该明说:\n%s", out)
	}
}

// /log 的参数全是可选的：{} 要能正常读（wr log 的默认形态就是它），
// 拼错字段名要 400 并按新规矩给出候选，不能静默当成「从头上读」。
func TestLogEndpointDecoding(t *testing.T) {
	s, _ := newLogServer(t)
	code, msg := post(t, s, "/log", `{}`)
	if code != 200 {
		t.Fatalf("空 body 该能读日志：%d %s", code, msg)
	}
	code, msg = post(t, s, "/log", `{"tail":10,"oflset":5}`)
	if code != 400 || !strings.Contains(msg, "未知字段") {
		t.Fatalf("拼错字段名该 400 并指名：%d %s", code, msg)
	}
	// /log/clear 没有参数，客户端发的是 {}
	code, msg = post(t, s, "/log/clear", `{}`)
	if code != 200 {
		t.Fatalf("清空该成功：%d %s", code, msg)
	}
}

// lastEntry 取最后一行审计：失败埋点都在动作结束时写，最后一条就是它的结论。
func lastEntry(t *testing.T, path string) map[string]any {
	t.Helper()
	all := entries(t, path)
	if len(all) == 0 {
		t.Fatalf("审计日志 %s 里一行都没有", path)
	}
	return all[len(all)-1]
}
