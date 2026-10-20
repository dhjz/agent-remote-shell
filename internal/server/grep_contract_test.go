package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"fmt"
	"os"
	"strings"
	"testing"

	"cnb.cool/yyx946/tteist/internal/proto"
)

// 空结果的形状必须和有结果时一致：matches / files 是 []，不是 null。
// 下游每处都写 (j.matches or []) 的成本，比这里多给几个字节贵得多。
func TestGrepEmptyShapeIsStable(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, "a.go", "nothing here\n")

	resp, err := s.Grep(proto.GrepReq{Pattern: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(resp)
	got := string(b)
	for _, want := range []string{`"matches":[]`, `"files":[]`, `"total_matches":0`, `"total_files":0`, `"skipped":{"binary":0,"too_large":0,"ignored":0}`} {
		if !strings.Contains(got, want) {
			t.Errorf("空结果里应当有 %s，实际 %s", want, got)
		}
	}
}

// truncated 只说「还有更多」是不够的：得同时给出总数，
// 否则 300 个文件命中 200 条会被当成「只有 200 个文件命中」。
func TestGrepReportsTotalsBeyondLimit(t *testing.T) {
	s := newTestServer(t)
	for i := 0; i < 12; i++ {
		mustWrite(t, s, fmt.Sprintf("f%02d.txt", i), "needle\n")
	}
	resp, err := s.Grep(proto.GrepReq{Pattern: "needle", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Matches) != 5 || !resp.Truncated {
		t.Fatalf("matches=%d truncated=%v，期望 5/true", len(resp.Matches), resp.Truncated)
	}
	if resp.TotalMatches != 12 || resp.TotalFiles != 12 {
		t.Fatalf("total_matches=%d total_files=%d，期望 12/12", resp.TotalMatches, resp.TotalFiles)
	}
	if len(resp.Files) != 12 {
		t.Fatalf("files=%d，期望全量 12（files 不受 limit 截断）", len(resp.Files))
	}
}

// limit 只有一种缺省：没传 = 50。0 和负数都在 decode 那一层被拦，
// 这里验的是绕过 HTTP 直接调 Grep 时的兜底。
func TestGrepLimitSemantics(t *testing.T) {
	s := newTestServer(t)
	for i := 0; i < 60; i++ {
		mustWrite(t, s, fmt.Sprintf("f%02d.txt", i), "needle\n")
	}
	resp, err := s.Grep(proto.GrepReq{Pattern: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Matches) != defaultGrepLimit || !resp.Truncated {
		t.Fatalf("缺省 limit 应给 %d 条，实际 %d（truncated=%v）", defaultGrepLimit, len(resp.Matches), resp.Truncated)
	}
	if _, err := s.Grep(proto.GrepReq{Pattern: "needle", Limit: -1}); err == nil {
		t.Fatal("limit=-1 应当报错，而不是当成不限")
	}

	// 走一遍 HTTP：错误得是 400 且说清怎么改。
	if code, msg := post(t, s, "/grep", `{"pattern":"needle","limit":-1}`); code != 400 || !strings.Contains(msg, "limit") {
		t.Fatalf("limit=-1 走 HTTP 应 400 且提到 limit，实际 %d %q", code, msg)
	}
}

// 并发扫描 + 按完成顺序 append ⇒ 每次顺序不同，limit 截掉谁全看运气。
// 排序后同一查询必须给出同一份结果。
func TestGrepOrderIsStable(t *testing.T) {
	s := newTestServer(t)
	for i := 0; i < 40; i++ {
		mustWrite(t, s, fmt.Sprintf("f%02d.txt", i), "needle\nneedle again\n")
	}
	first, err := s.Grep(proto.GrepReq{Pattern: "needle", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := s.Grep(proto.GrepReq{Pattern: "needle", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if toJSON(t, first.Matches) != toJSON(t, again.Matches) {
			t.Fatalf("第 %d 次结果不同：%v vs %v", i, first.Matches, again.Matches)
		}
	}
	// 截断发生前排序，所以拿到的应该是字典序最前的 10 条。
	if first.Matches[0].File != "f00.txt" || first.Matches[9].File != "f04.txt" {
		t.Fatalf("排序后应取 f00..f04 的 10 行，实际首尾 %s / %s", first.Matches[0].File, first.Matches[9].File)
	}
}

// 二进制文件静默消失 = 「世界上没有这个文件」。至少要计数，开关打开就得能搜。
func TestGrepBinaryIsReportedAndSearchable(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, "probe.dat", "needle\x00binary\n")
	mustWrite(t, s, "a.txt", "needle\n")

	resp, err := s.Grep(proto.GrepReq{Pattern: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Skipped.Binary != 1 {
		t.Fatalf("skipped.binary=%d，期望 1", resp.Skipped.Binary)
	}
	if resp.TotalFiles != 1 {
		t.Fatalf("total_files=%d，期望 1（二进制默认不搜）", resp.TotalFiles)
	}

	resp, err = s.Grep(proto.GrepReq{Pattern: "needle", SearchBinary: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.TotalFiles != 2 || resp.Skipped.Binary != 0 {
		t.Fatalf("search_binary=true 时应搜到 2 个文件，实际 total_files=%d skipped.binary=%d", resp.TotalFiles, resp.Skipped.Binary)
	}
}

// 非 UTF-8 的文件照样按字节扫（能搜到），但返回的行里 U+FFFD 是替换出来的，
// 得显式标出来，不然调用方会拿它当原文。
func TestGrepMarksLossyEncoding(t *testing.T) {
	s := newTestServer(t)
	// 一段非法 UTF-8 字节 + 命中串
	mustWriteRaw(t, s, "gbk.txt", append([]byte("prefix \xd6\xd0\xce\xc4 "), []byte("needle\n")...))

	resp, err := s.Grep(proto.GrepReq{Pattern: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Matches) != 1 {
		t.Fatalf("应命中 1 行，实际 %d", len(resp.Matches))
	}
	if resp.Matches[0].Encoding != "lossy" {
		t.Fatalf("encoding=%q，期望 lossy", resp.Matches[0].Encoding)
	}
	// Write 走的是 proto.WriteReq.Content（string），非法字节在 JSON 里会被换成 U+FFFD，
	// 三个坏字节变成 4 个替换字符，所以这里只校验 col 落在命中词上。
	if resp.Matches[0].Col < 1 {
		t.Fatalf("col=%d，期望按字节算的命中位置", resp.Matches[0].Col)
	}

	mustWrite(t, s, "ok.txt", "clean needle\n")
	resp, _ = s.Grep(proto.GrepReq{Pattern: "clean"})
	if resp.Matches[0].Encoding != "" {
		t.Fatal("干净的 UTF-8 不该带 encoding 字段")
	}
}

// 长行截断是对的，但要给出原文长度和命中列，否则「在行内哪儿」全靠猜。
func TestGrepLongLineKeepsColAndLength(t *testing.T) {
	s := newTestServer(t)
	long := strings.Repeat("x", 5000) + "needle" + strings.Repeat("y", 5000)
	mustWrite(t, s, "long.txt", long+"\n")

	resp, err := s.Grep(proto.GrepReq{Pattern: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	m := resp.Matches[0]
	if !m.TextTruncated || m.Bytes != len(long) {
		t.Fatalf("text_truncated=%v bytes=%d，期望 true/%d", m.TextTruncated, m.Bytes, len(long))
	}
	if m.Col != 5001 {
		t.Fatalf("col=%d，期望 5001", m.Col)
	}
	if len(m.Text) > maxGrepLineBytes+len("…") {
		t.Fatalf("text 长度 %d，超过 %d", len(m.Text), maxGrepLineBytes)
	}
}

// context 拆成 before/after 之后，只要上文就不该把下文一起买回来。
func TestGrepBeforeAfterAndContextCap(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, "a.txt", "l1\nl2\nHIT\nl4\nl5\n")

	resp, err := s.Grep(proto.GrepReq{Pattern: "HIT", Before: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := linesOf(resp.Matches); fmt.Sprint(got) != "[2 3]" {
		t.Fatalf("before=1 应只带 l2+HIT，实际 %v", got)
	}
	if resp.TotalMatches != 1 {
		t.Fatalf("total_matches=%d，期望 1（上下文行不算命中）", resp.TotalMatches)
	}

	if _, err := s.Grep(proto.GrepReq{Pattern: "HIT", Context: maxGrepContext + 1}); err == nil {
		t.Fatal("context 超过上限应报错，而不是照单全收")
	}
	if code, msg := post(t, s, "/grep", `{"pattern":"HIT","context":-1}`); code != 400 || !strings.Contains(msg, "context") {
		t.Fatalf("context=-1 应 400，实际 %d %q", code, msg)
	}
}

// exclude 是「除了 *.min.js」这种最常写的排除，缺了它只能搜完再自己筛。
func TestGrepExclude(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, "app.js", "needle\n")
	mustWrite(t, s, "dist/app.min.js", "needle\n")
	mustWrite(t, s, "web/lib.min.js", "needle\n")

	resp, err := s.Grep(proto.GrepReq{Pattern: "needle", RespectIgnore: boolPtr(false), Exclude: "*.min.js"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(resp.Files) != "[app.js]" {
		t.Fatalf("exclude=*.min.js 后应只剩 app.js，实际 %v", resp.Files)
	}
}

// .wr-jobs 是引擎自己的日志目录，剪不剪不该由项目的 .gitignore 决定：
// respect_ignore=false 时也不能把日志当代码搜出来。
func TestGrepNeverSearchesJobLogs(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, jobDirName+"/1000-x.log", "needle\n")
	mustWrite(t, s, "a.txt", "needle\n")

	resp, err := s.Grep(proto.GrepReq{Pattern: "needle", RespectIgnore: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(resp.Files) != "[a.txt]" {
		t.Fatalf("不应搜到 .wr-jobs 里的日志，实际 %v", resp.Files)
	}
	if resp.Skipped.Ignored == 0 {
		t.Fatal("硬剪枝的数量要报出来，否则「搜过整棵树」是假的")
	}
}

// files_only 是省 token 的正确打开方式：只给文件，不给行，计数照旧全量。
func TestGrepFilesOnly(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, "a.txt", "needle\nneedle\nneedle\n")
	mustWrite(t, s, "b.txt", "needle\n")

	resp, err := s.Grep(proto.GrepReq{Pattern: "needle", FilesOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Matches) != 0 {
		t.Fatalf("files_only 不该带行，实际 %d 条", len(resp.Matches))
	}
	if len(resp.Files) != 2 || resp.TotalMatches != 4 || resp.TotalFiles != 2 {
		t.Fatalf("files=%v total_matches=%d total_files=%d", resp.Files, resp.TotalMatches, resp.TotalFiles)
	}
	b, _ := json.Marshal(resp)
	if !strings.Contains(string(b), `"matches":[]`) {
		t.Fatalf("files_only 的 matches 也必须是 []，实际 %s", b)
	}
}

func boolPtr(v bool) *bool { return &v }

// mustWriteRaw 直接落原始字节：编码相关的用例需要非 UTF-8 的内容，
// 走 Write 的 string 会被换成 U+FFFD。
func mustWriteRaw(t *testing.T, s *Server, rel string, data []byte) {
	t.Helper()
	abs, err := s.root.Resolve(rel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func linesOf(ms []proto.GrepMatch) []int {
	out := make([]int, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Line)
	}
	return out
}

// 形状只有在「真的走一遍 HTTP」时才算数：直接调 s.Grep 测不到 handler 的
// 编码、写头、以及那条被 deny 掉的错误路径。合并两次改动后这里尤其值得钉住。
func TestGrepHTTPShapeAndRejection(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, "a.go", "hit\n")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	do := func(body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/grep", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer 0123456789abcdef0123")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var m map[string]any
		if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
			t.Fatalf("返回体不是 JSON: %v", err)
		}
		return res.StatusCode, m
	}

	code, m := do(`{"pattern":"hit"}`)
	if code != 200 {
		t.Fatalf("正常查询该是 200，实际 %d：%v", code, m)
	}
	if _, ok := m["matches"].([]any); !ok {
		t.Errorf("matches 必须是数组，实际 %T", m["matches"])
	}
	if _, ok := m["skipped"].(map[string]any); !ok {
		t.Errorf("skipped 必须恒在，实际 %T", m["skipped"])
	}

	// 负数 limit 要当场拒掉：悄悄当成「不限」比报错更贵。
	code, m = do(`{"pattern":"hit","limit":-1}`)
	if code != 400 {
		t.Fatalf("limit=-1 该是 400，实际 %d：%v", code, m)
	}
	if _, ok := m["error"].(string); !ok {
		t.Errorf("400 的返回体该是 {\"error\":\"...\"}，实际 %v", m)
	}
}
