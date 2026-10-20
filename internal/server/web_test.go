package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cnb.cool/yyx946/tteist/internal/audit"
	"cnb.cool/yyx946/tteist/internal/proto"
)

// 和 newTestServer 里那个 token 一致
const testServerToken = "0123456789abcdef0123"

func auditEntrySample() audit.Entry {
	return audit.Entry{
		Time: "2026-10-18T22:32:55.651+08:00", Decision: audit.Allowed, Tool: "exec",
		Command: "echo hi", Cwd: ".", Session: "s1", Reason: "r", Error: "exit 1", Path: "a.go",
		Remote: "10.1.2.3", Shell: "bash", ExitCode: audit.Int(0), Duration: audit.Duration(1),
		Bytes: audit.Bytes(3),
	}
}

// 页面必须从 embed 里出得来。单 exe 发行，页面丢了等于功能没了。
func TestLogPageServed(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/log", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/log 应该 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type 该是 text/html，实际 %q", ct)
	}
	body := rec.Body.String()
	// 页面结构：数据来自 /log/stream，页面上限 500 条
	if !strings.Contains(body, "/log/stream") {
		t.Error("页面里没提 /log/stream，说明打包的是旧版本")
	}
	if !strings.Contains(body, "const MAX=500") {
		t.Error("页面该把窗口钉在 500 条")
	}
	// 字段名必须跟着审计日志走，而不是某个第三方系统的旧名字
	for _, field := range []string{"exitCode", "durationMs", "sessionId", "remote", "decision", "tool", "shell"} {
		if !strings.Contains(body, field) {
			t.Errorf("页面没用到日志字段 %q", field)
		}
	}
}

// 页面本身不含数据，所以不鉴权；数据管道必须鉴权。
func TestLogPageOpenStreamGuarded(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/log", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/log 该免鉴权，实际 %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/log/stream", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/log/stream 没 token 该 401，实际 %d", rec.Code)
	}
}

// 历史 + 增量：连上先给 tail 那几行，之后新写的继续推。
func TestLogStreamTailsThenFollows(t *testing.T) {
	s, logPath := newLogServer(t)
	for i := 1; i <= 3; i++ {
		appendLine(t, logPath, fmt.Sprintf(`{"tool":"exec","command":"echo %d"}`, i))
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/log/stream?tail=2", nil)
	req.Header.Set("Authorization", "Bearer "+testServerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("该回 text/plain 的裸流，实际 %q", ct)
	}

	// 先读历史：tail=2 只要最后两行
	head := readLines(t, resp.Body, 2)
	if len(head) != 2 || !strings.Contains(head[1], "echo 3") {
		t.Fatalf("历史不对：%v", head)
	}

	// 再追加一行，应该在同一个连接里出现
	appendLine(t, logPath, `{"tool":"write","path":"a.go"}`)
	next := readLines(t, resp.Body, 1)
	if len(next) != 1 || !strings.Contains(next[0], `"path":"a.go"`) {
		t.Fatalf("增量没推过来：%v", next)
	}
}

// 半行不许推：写到一半的行要等换行，否则浏览器只能看到坏行。
func TestReadLogChunkWaitsForNewline(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	// 第一行完整、第二行才写了一半：这是追日志时最常见的中间态
	if err := os.WriteFile(p, []byte("aaa\nbb"), 0o644); err != nil {
		t.Fatal(err)
	}

	data, next, err := readLogChunk(p, 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "aaa\n" {
		t.Fatalf("只该推到最后一个换行：%q", data)
	}
	if next != 4 {
		t.Fatalf("offset 该停在 4，实际 %d", next)
	}
	// 补上后半截和换行，剩下那行才出来
	if err := os.WriteFile(p, []byte("aaa\nbbb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	appendLine(t, p, "ccc")
	data, next, err = readLogChunk(p, next, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "bbb\nccc\n" {
		t.Fatalf("补齐后该拿到整两行，实际 %q", data)
	}
	if next != int64(len("aaa\nbbb\nccc\n")) {
		t.Fatalf("offset 不对：%d", next)
	}

	// 停在同一位置再读：没有新内容就不该有任何输出
	if data, _, _ := readLogChunk(p, next, 1<<20); len(data) != 0 {
		t.Fatalf("没新增还读出了 %q", data)
	}
}

// 日志被清空后 offset 会大于文件大小，必须回到 0 重来，
// 否则页面从此再也收不到东西（/log/clear 是个日常操作）。
func TestReadLogChunkResetsAfterTruncate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	appendLine(t, p, `{"tool":"exec"}`)
	appendLine(t, p, `{"tool":"write"}`)

	if _, next, _ := readLogChunk(p, 0, 1<<20); next == 0 {
		t.Fatal("第一次该读到内容")
	} else if err := os.Truncate(p, 0); err != nil {
		t.Fatal(err)
	}
	appendLine(t, p, `{"tool":"mkdir"}`)

	data, next, err := readLogChunk(p, 9999, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "mkdir") {
		t.Fatalf("清空后该从头读，实际 %q", data)
	}
	if next != int64(len(`{"tool":"mkdir"}`)+1) {
		t.Fatalf("offset 该重置后重算，实际 %d", next)
	}
}

// 没开审计日志时，流端点要给一句能看懂的话，而不是空连接。
func TestLogStreamWithoutAuditLog(t *testing.T) {
	s := newTestServer(t) // newTestServer 不开 -log
	if s.aud != nil {
		t.Skip("这个测试要求未开启审计日志")
	}
	req := httptest.NewRequest(http.MethodGet, "/log/stream?token="+testServerToken, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未开日志该 400，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "审计日志") {
		t.Errorf("错误信息该说清楚原因：%s", rec.Body.String())
	}
}

// ---------- 助手 ----------

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// readLines 从流里凑够 n 行。流是不封口的，所以只能按行数停，
// 配合 http 客户端的超时兜底（测试里靠 t.Deadline）。
func readLines(t *testing.T, r io.Reader, n int) []string {
	t.Helper()
	type res struct {
		lines []string
		err   error
	}
	ch := make(chan res, 1)
	go func() {
		var out []string
		buf := make([]byte, 4096)
		var rest strings.Builder
		for len(out) < n {
			k, err := r.Read(buf)
			if k > 0 {
				rest.Write(buf[:k])
				s := rest.String()
				for {
					i := strings.IndexByte(s, '\n')
					if i < 0 {
						break
					}
					out = append(out, s[:i])
					s = s[i+1:]
				}
				rest.Reset()
				rest.WriteString(s)
			}
			if err != nil {
				ch <- res{out, err}
				return
			}
		}
		ch <- res{out, nil}
	}()
	select {
	case got := <-ch:
		if len(got.lines) < n {
			t.Fatalf("只读到 %d 行（err=%v）：%v", len(got.lines), got.err, got.lines)
		}
		return got.lines
	case <-time.After(5 * time.Second):
		t.Fatalf("等 %d 行超时", n)
		return nil
	}
}

// 横幅里要有那一行，且带 token：人点一下就能打开实时页。
func TestBannerShowsLogPage(t *testing.T) {
	out := banner("0.0.0.0:7573", "s3cr3t", "/tmp/repo", "")
	if !strings.Contains(out, "http://127.0.0.1:7573/log?token=s3cr3t") {
		t.Fatalf("横幅没给日志页面地址：\n%s", out)
	}
}

// 页面里的 JSON 一定要能被页面解析：顺手验一下审计字段名没漂。
func TestAuditEntryFieldsMatchPage(t *testing.T) {
	e := auditEntrySample()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"time"`, `"decision"`, `"tool"`, `"command"`, `"shell"`, `"cwd"`,
		`"path"`, `"sessionId"`, `"reason"`, `"error"`, `"exitCode"`, `"durationMs"`, `"bytes"`, `"remote"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("审计行少了字段 %s：%s", k, b)
		}
	}
}

// tail 装不下时必须留最近的那一段，而不是「从第 tail 行往后」那一段。
// 早先的写法让一份 1.7MiB 的日志在 tail=500 时只回 296 行，且回的既不是
// 最近 296 行、开头还是半行——页面上看起来就是流断了。
func TestLogTailTruncatesFromTheHead(t *testing.T) {
	s, logPath := newLogServer(t)
	pad := strings.Repeat("x", 3000)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		fmt.Fprintf(f, `{"tool":"exec","i":%d,"command":%q}`+"\n", i, pad)
	}
	f.Close()

	resp, err := s.Log(proto.LogReq{Tail: 500})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Truncated {
		t.Fatal("1MiB 装不下 600 行，该标 truncated")
	}
	if int64(len(resp.Output)) > maxLogChunk {
		t.Fatalf("回得比上限还多：%d", len(resp.Output))
	}
	// 每一行都得是完整 JSON，尤其第一行——半行到了浏览器只会被当坏行
	lines := strings.Split(strings.TrimSuffix(resp.Output, "\n"), "\n")
	for _, l := range lines {
		var v map[string]any
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatalf("出现半行：%q", l)
		}
	}
	// 要的是最近的，所以末行必须是最后写进去的那条
	if !strings.Contains(lines[len(lines)-1], `"i":599`) {
		t.Fatalf("末行该是最新的一条，实际 %q", lines[len(lines)-1])
	}
	// offset 指向末尾，紧跟其后的增量能接上
	if resp.Offset != resp.Size {
		t.Fatalf("offset 该停在文件末尾：offset=%d size=%d", resp.Offset, resp.Size)
	}
	appendLine(t, logPath, `{"tool":"write","path":"after"}`)
	data, _, err := readLogChunk(logPath, resp.Offset, streamChunkLimit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"after"`) {
		t.Fatalf("offset 之后该读到新增行，实际 %q", data)
	}
}

// tail 掐中半行时，起点要挪到下一个换行之后：宁少一行，不给坏行。
func TestLogTailSkipsPartialFirstLine(t *testing.T) {
	s, logPath := newLogServer(t)
	var buf bytes.Buffer
	for i := 0; i < 6; i++ {
		buf.WriteString(fmt.Sprintf(`{"tool":"exec","i":%d}`+"\n", i))
	}
	if err := os.WriteFile(logPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	// limit 故意给一个落在行中间的值，逼出「起点是半行」这种情况
	resp, err := s.Log(proto.LogReq{Tail: 6, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(strings.TrimSuffix(resp.Output, "\n"), "\n") {
		if l == "" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatalf("出现半行：%q", l)
		}
	}
}

// 存活探测只回 "ok" 两个字符：探活脚本和 keepalive 只关心「还活着」，
// 版本号不算存活信息，顺手递给外人没好处。
func TestHealthzIsBareOK(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz 应该是 200，实际 %d", rec.Code)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Errorf("GET /healthz 的 body 该是 %q，实际 %q", "ok", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("GET /healthz 的 Content-Type 该是 text/plain，实际 %q", ct)
	}
}

// 转发的配置只有带 token 的 /info 和 /fwd 会说。/fwd 那条路免 token 且谁都能敲，
// 所以「配了什么」必须从 /info 起手就能拿到，不能靠走错名字去试。
func TestInfoCarriesFwdTargets(t *testing.T) {
	s, err := New(Config{
		Token: "0123456789abcdef0123", Root: t.TempDir(), Quiet: true,
		Fwd: []string{"web=http://127.0.0.1:1", "api=5173"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/info?token=0123456789abcdef0123", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /info 应该是 200，实际 %d", rec.Code)
	}
	var info proto.InfoResp
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if len(info.Fwd) != 2 {
		t.Fatalf("/info 该带上两条转发目标，实际 %d 条：%s", len(info.Fwd), rec.Body.String())
	}
	byName := map[string]proto.FwdHop{}
	for _, h := range info.Fwd {
		byName[h.Name] = h
	}
	if got := byName["api"].URL; got != "http://127.0.0.1:5173" {
		t.Errorf("简写的转发目标该补成 http://127.0.0.1:5173，实际 %q", got)
	}
	if got := byName["web"].Spec; got != "web=http://127.0.0.1:1" {
		t.Errorf("spec 该回显启动参数的原始写法，实际 %q", got)
	}
}

// 没配 -fwd 时是空数组而不是 null：客户端看见 null 会当字段不存在。
func TestInfoFwdEmptyIsArray(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/info?token=0123456789abcdef0123", nil))
	if !strings.Contains(rec.Body.String(), `"fwd":[]`) {
		t.Errorf("/info 里没配转发时 fwd 该是空数组：%s", rec.Body.String())
	}
}
