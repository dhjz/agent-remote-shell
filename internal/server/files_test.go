package server

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"cnb.cool/yyx946/tteist/internal/proto"
)

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{"*.go", "a.go", true},
		{"*.go", "src/a.go", false},
		{"**/*.go", "src/a.go", true},
		{"**/*.go", "a.go", true},
		{"src/**/*.go", "src/x/y/a.go", true},
		{"a?c.go", "abc.go", true},
		{"*.{go,md}", "x.md", true},
	}
	for _, c := range cases {
		got, err := matchGlob(c.pat, c.name)
		if err != nil {
			t.Fatalf("%s/%s: %v", c.pat, c.name, err)
		}
		if got != c.want {
			t.Errorf("matchGlob(%q,%q)=%v want %v", c.pat, c.name, got, c.want)
		}
	}
}

func TestGlobMatchAny(t *testing.T) {
	// grep -glob 的语义：任意深度匹配文件名
	if !globMatchAny("*.go", "src/deep/a.go") {
		t.Fatal("*.go 应命中 src/deep/a.go")
	}
	if globMatchAny("src/*.go", "other/a.go") {
		t.Fatal("src/*.go 不应命中 other/a.go")
	}
}

// .gitignore 支持的核心价值：被忽略的目录根本不进去，不是「遍历完再筛」。
func TestGrepRespectsGitignore(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, ".gitignore", "dist/\n*.log\n!keep.log\nvendor-local\n")
	mustWrite(t, s, "main.go", "hit here\n")
	mustWrite(t, s, "a.log", "hit here\n")
	mustWrite(t, s, "keep.log", "hit here\n")
	mustWrite(t, s, "dist/bundle.js", "hit here\n")
	mustWrite(t, s, "sub/dist/bundle.js", "hit here\n")
	mustWrite(t, s, "vendor-local/x.go", "hit here\n")

	resp, err := s.Grep(proto.GrepReq{Pattern: "hit here", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range resp.Matches {
		got[m.File] = true
	}
	for _, want := range []string{"main.go", "keep.log"} {
		if !got[want] {
			t.Errorf("%s 应该被搜到；实际 %v", want, filesOf(resp.Matches))
		}
	}
	for _, no := range []string{"a.log", "dist/bundle.js", "sub/dist/bundle.js", "vendor-local/x.go"} {
		if got[no] {
			t.Errorf("%s 应按 .gitignore 排除", no)
		}
	}
}

// 显式关掉时，被忽略的也要能搜到——不然「dist 里到底有没有这个字符串」没法确认。
func TestGrepRespectIgnoreOff(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, ".gitignore", "dist/\n")
	mustWrite(t, s, "dist/a.js", "needle\n")
	off := false
	resp, err := s.Grep(proto.GrepReq{Pattern: "needle", RespectIgnore: &off})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Matches) != 1 || resp.Matches[0].File != "dist/a.js" {
		t.Fatalf("respect_ignore=false 时应该搜到 dist/a.js，实际 %v", filesOf(resp.Matches))
	}
}

// 嵌套 .gitignore 的作用域：子目录规则不外溢，父规则在子目录里仍生效。
func TestGrepNestedGitignore(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, ".gitignore", "*.tmp\n")
	mustWrite(t, s, "pkg/.gitignore", "keep.tmp\n")
	mustWrite(t, s, "pkg/keep.tmp", "needle\n")  // 子目录取反 → 保留
	mustWrite(t, s, "pkg/other.tmp", "needle\n") // 父规则 → 排除
	mustWrite(t, s, "other.tmp", "needle\n")     // 父规则 → 排除
	mustWrite(t, s, "pkg/code.go", "needle\n")

	resp, err := s.Grep(proto.GrepReq{Pattern: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	got := filesOf(resp.Matches)
	if len(got) != 1 || got[0] != "pkg/code.go" {
		t.Fatalf("实际命中 %v", got)
	}

	// 把 pkg/.gitignore 改成取反，验证子规则只在自己目录生效
	mustWrite(t, s, "pkg/.gitignore", "!keep.tmp\n")
	resp, _ = s.Grep(proto.GrepReq{Pattern: "needle"})
	got = filesOf(resp.Matches)
	if len(got) != 2 {
		t.Fatalf("应命中 pkg/code.go + pkg/keep.tmp，实际 %v", got)
	}
}

func TestGlobRespectsGitignore(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, ".gitignore", "dist/\n")
	mustWrite(t, s, "dist/app.js", "x")
	mustWrite(t, s, "src/app.js", "x")

	resp, err := s.Glob(proto.GlobReq{Pattern: "**/*.js"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Files) != 1 || resp.Files[0] != "src/app.js" {
		t.Fatalf("glob 结果 %v", resp.Files)
	}
	off := false
	resp, err = s.Glob(proto.GlobReq{Pattern: "**/*.js", RespectIgnore: &off})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Files) != 2 {
		t.Fatalf("respect_ignore=false 应拿到两个，实际 %v", resp.Files)
	}
}

// 并发扫描不能把同一个文件算两遍、也不能因为 limit 提前退就把已收集的丢掉。
func TestGrepConcurrentLimit(t *testing.T) {
	s := newTestServer(t)
	const n = 200
	for i := 0; i < n; i++ {
		mustWrite(t, s, fmt.Sprintf("d%02d/f%d.go", i%20, i), "needle\n")
	}
	resp, err := s.Grep(proto.GrepReq{Pattern: "needle", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Matches) != 10 {
		t.Fatalf("matches=%d want 10", len(resp.Matches))
	}
	if !resp.Truncated {
		t.Error("到 limit 应标记 truncated")
	}
	seen := map[string]bool{}
	for _, m := range resp.Matches {
		if seen[m.File] {
			t.Errorf("重复命中 %s", m.File)
		}
		seen[m.File] = true
	}

	all, err := s.Grep(proto.GrepReq{Pattern: "needle", Limit: n + 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Matches) != n {
		t.Fatalf("全量 matches=%d want %d", len(all.Matches), n)
	}
}

// context 行必须留在同一个文件的连续段里，不能被并发切成碎片。
func TestGrepContextCoherent(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, "a.go", "1\n2\nneedle\n4\n5\n")
	resp, err := s.Grep(proto.GrepReq{Pattern: "needle", Context: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Matches) != 3 {
		t.Fatalf("matches=%+v", resp.Matches)
	}
	for i, m := range resp.Matches {
		if m.Line != i+2 {
			t.Errorf("第 %d 个命中行号=%d，应为 %d", i, m.Line, i+2)
		}
	}
}

func filesOf(ms []proto.GrepMatch) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range ms {
		if !seen[m.File] {
			seen[m.File] = true
			out = append(out, m.File)
		}
	}
	return out
}

// .git、node_modules 是硬名单：.gitignore 里写取反也救不回来，
// 否则一次搜索会掉进几万个对象文件。
func TestGrepHardSkipNotNegotiable(t *testing.T) {
	s := newTestServer(t)
	// 必须配一条「忽略一切」的规则，否则 git 的语义是「被父目录忽略的子文件
	// 无法用 ! 单独救回」——这正是我们要对 dist 的软名单做的事。
	mustWrite(t, s, ".gitignore", "*\n!dist/\n!dist/app.js\n!node_modules\n")
	mustWrite(t, s, "node_modules/x/i.js", "needle\n")
	mustWrite(t, s, "dist/app.js", "needle\n") // dist 是软名单，取反能救
	resp, err := s.Grep(proto.GrepReq{Pattern: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	got := filesOf(resp.Matches)
	if len(got) != 1 || got[0] != "dist/app.js" {
		t.Fatalf("实际 %v：node_modules 应永远排除，dist 取反后应可见", got)
	}
}

// 没有 .gitignore 的老仓库行为不变：node_modules/.git 这类照旧默认跳过。
func TestGrepNoGitignoreKeepsDefaultSkips(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, "node_modules/lib/a.js", "needle\n")
	mustWrite(t, s, "src/app.js", "needle\n")
	resp, err := s.Grep(proto.GrepReq{Pattern: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	got := filesOf(resp.Matches)
	if len(got) != 1 || got[0] != "src/app.js" {
		t.Fatalf("实际 %v", got)
	}
}

// 内置软名单（dist/build/target…）只在用户没表态时才生效：
// 写了 .gitignore 就以 .gitignore 为准，不然「我明明没忽略 dist」会变成玄学。
func TestGrepGitignoreOverridesSoftSkips(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, ".gitignore", "*.log\n")
	mustWrite(t, s, "dist/app.js", "needle\n")
	resp, err := s.Grep(proto.GrepReq{Pattern: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if got := filesOf(resp.Matches); len(got) != 1 || got[0] != "dist/app.js" {
		t.Fatalf("仓库有 .gitignore 时，dist 不该再被内置规则吃掉：%v", got)
	}
}

// 用户写了 !dist 就该能搜到 dist：内置软名单只兜「没规则」的情况。
func TestGrepNegateSoftSkip(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, ".gitignore", "!dist\n")
	mustWrite(t, s, "dist/app.js", "needle\n")
	mustWrite(t, s, "node_modules/x/i.js", "needle\n")
	resp, err := s.Grep(proto.GrepReq{Pattern: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if got := filesOf(resp.Matches); len(got) != 1 || got[0] != "dist/app.js" {
		t.Fatalf("实际 %v", got)
	}
}

// 超时是「没搜完」，不是「没有」：结果要标出来，别让调用方据此下结论。
// 用一个不可能的负数确认参数校验，用 1ns 确认截断标记。
func TestGrepTimeoutMarksIncomplete(t *testing.T) {
	s := newTestServer(t)
	for i := 0; i < 20; i++ {
		mustWrite(t, s, filepath.Join("d", strconv.Itoa(i)+".go"), "needle\n")
	}

	if _, err := s.Grep(proto.GrepReq{Pattern: "needle", TimeoutMS: -1}); err == nil {
		t.Fatal("负超时应被拒")
	}

	resp, err := s.Grep(proto.GrepReq{Pattern: "needle", Limit: 100, TimeoutMS: 1})
	if err != nil {
		t.Fatal(err)
	}
	// 1ns 在 20 个文件上几乎必然先超时；真跑完了也不算错，只要求标记自洽。
	if resp.TimedOut && !resp.Truncated {
		t.Fatal("timed_out 时 truncated 必须为 true")
	}
}

// 读到什么就是文件里有什么：CRLF 的 \r 不能被 /read 吞掉。
// 实测踩过：/write 写进去 CRLF 的 x.sh，/read 看着正常，跑起来 shell 报
// `x: command not found`——\r 还在文件里，只是被读取一侧悄悄抹掉了。
func TestReadKeepsCRLF(t *testing.T) {
	s := newTestServer(t)
	mustWriteRaw(t, s, "x.sh", []byte("#!/bin/sh\r\necho hi\r\n"))
	resp, err := s.Read(proto.ReadReq{Path: "x.sh"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.CRLF {
		t.Error("CRLF 文件应带 crlf:true")
	}
	if !strings.Contains(resp.Content, "\r\n") {
		t.Fatalf("行尾的 \\r 不该被抹掉：%q", resp.Content)
	}
	if resp.Total != 2 {
		t.Errorf("行数应还是 2，得到 %d", resp.Total)
	}
	// LF 文件不该被误标
	mustWrite(t, s, "y.sh", "#!/bin/sh\necho hi\n")
	if resp, err = s.Read(proto.ReadReq{Path: "y.sh"}); err != nil {
		t.Fatal(err)
	}
	if resp.CRLF || strings.Contains(resp.Content, "\r") {
		t.Errorf("LF 文件不该带 \\r 或 crlf:true：%v %q", resp.CRLF, resp.Content)
	}
}

// grep 同理：返回的 Text 是原文，CRLF 的行里那个 \r 还在。
func TestGrepKeepsCRLF(t *testing.T) {
	s := newTestServer(t)
	mustWriteRaw(t, s, "win.go", []byte("package a\r\nfunc hit() {}\r\n"))
	resp, err := s.Grep(proto.GrepReq{Pattern: "func hit", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Matches) != 1 {
		t.Fatalf("应命中 1 行，得到 %d", len(resp.Matches))
	}
	if !strings.HasSuffix(resp.Matches[0].Text, "\r") {
		t.Errorf("命中行的 \\r 不该被抹掉：%q", resp.Matches[0].Text)
	}
	if resp.Matches[0].Line != 2 {
		t.Errorf("行号应为 2，得到 %d", resp.Matches[0].Line)
	}
}
