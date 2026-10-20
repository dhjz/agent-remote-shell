package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// 落点解析是这个功能最容易被误解的一环：相对路径到底相对谁。
// 答案是「执行 wr start 的目录」，不是 -root、不是 home。
func TestResolveKeepsRelativeToBase(t *testing.T) {
	base := t.TempDir()
	cases := map[string]string{
		".wr.log":         filepath.Join(base, ".wr.log"),
		"logs/wr.log":     filepath.Join(base, "logs", "wr.log"),
		"./logs/wr.jsonl": filepath.Join(base, "logs", "wr.jsonl"),
	}
	for in, want := range cases {
		got, err := Resolve(base, in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Errorf("%s → %s，想要 %s", in, got, want)
		}
	}
	// 绝对路径原样保留：有人就是要把日志放到沙箱外
	abs := filepath.Join(t.TempDir(), "a.log")
	if got, _ := Resolve(base, abs); got != abs {
		t.Errorf("绝对路径被改写了: %s → %s", abs, got)
	}
}

func TestResolveRejectsEmpty(t *testing.T) {
	if _, err := Resolve(t.TempDir(), "  "); err == nil {
		t.Fatal("空文件名该报错")
	}
}

// 一行一个 JSON 对象，字段名要和 winreach-mcp 对得上。
func TestLogWritesJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "audit.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	l.Log(Entry{Decision: Allowed, Tool: "exec", Command: "echo hi", ExitCode: Int(0)})
	l.Log(Entry{Decision: Blocked, Tool: "write", Path: ".git/x", Reason: "拒了"})

	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("该写 2 行，实际 %d 行: %q", len(lines), lines)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("不是合法 JSON: %v (%s)", err, lines[0])
	}
	for _, k := range []string{"time", "decision", "tool", "command"} {
		if _, ok := first[k]; !ok {
			t.Errorf("缺字段 %s: %s", k, lines[0])
		}
	}
	if first["decision"] != Allowed {
		t.Errorf("decision 不对: %v", first["decision"])
	}
	// exitCode=0 必须写出来：0 恰恰是最有意义的退出码，不能被 omitempty 吞掉。
	if v, ok := first["exitCode"]; !ok || v.(float64) != 0 {
		t.Errorf("exitCode 0 丢了: %s", lines[0])
	}

	var second map[string]any
	_ = json.Unmarshal([]byte(lines[1]), &second)
	if second["reason"] != "拒了" || second["path"] != ".git/x" {
		t.Errorf("blocked 那行字段不对: %s", lines[1])
	}
}

// 不带 exitCode / duration 的时候不该写 null 占位，日志要短。
func TestLogOmitsEmptyFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	l, _ := Open(path)
	defer l.Close()
	l.Log(Entry{Decision: Allowed, Tool: "read", Path: "a.go"})
	line := readLines(t, path)[0]
	for _, k := range []string{"exitCode", "durationMs", "bytes", "command", "remote"} {
		if strings.Contains(line, `"`+k+`"`) {
			t.Errorf("没赋值的 %s 不该出现: %s", k, line)
		}
	}
}

// 并发写不能出现半行：一行一次 Write，任何时刻都是完整 JSON。
func TestLogConcurrentNoTornLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	l, _ := Open(path)
	defer l.Close()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l.Log(Entry{Decision: Allowed, Tool: "exec", Command: strings.Repeat("x", 200)})
		}(i)
	}
	wg.Wait()

	lines := readLines(t, path)
	if len(lines) != 50 {
		t.Fatalf("该写 50 行，实际 %d 行", len(lines))
	}
	for _, ln := range lines {
		if !json.Valid([]byte(ln)) {
			t.Fatalf("有半行: %s", ln)
		}
	}
}

// 重启不能丢历史：O_APPEND 追加而不是覆盖。
func TestOpenAppendsAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	l, _ := Open(path)
	l.Log(Entry{Decision: Allowed, Tool: "exec"})
	l.Close()

	l2, _ := Open(path)
	defer l2.Close()
	l2.Log(Entry{Decision: Allowed, Tool: "exec"})

	if n := len(readLines(t, path)); n != 2 {
		t.Fatalf("重启后该是追加，实际 %d 行", n)
	}
}

func TestTruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	l, _ := Open(path)
	defer l.Close()
	l.Log(Entry{Decision: Allowed, Tool: "exec"})
	if err := l.Truncate(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != 0 {
		t.Fatalf("清空后该是 0 字节: %v %v", fi.Size(), err)
	}
	// 清空之后还能继续写
	l.Log(Entry{Decision: Allowed, Tool: "exec"})
	if n := len(readLines(t, path)); n != 1 {
		t.Fatalf("清空后可续写，实际 %d 行", n)
	}
}

// nil logger 是「没开日志」的正常形态，所有方法都得能安全调用。
func TestNilLoggerIsSafe(t *testing.T) {
	var l *Logger
	l.Log(Entry{Decision: Allowed, Tool: "exec"})
	if l.Path() != "" {
		t.Error("nil 的 Path 该是空串")
	}
	if err := l.Close(); err != nil {
		t.Errorf("nil 的 Close 该成功: %v", err)
	}
	if err := l.Truncate(); err == nil {
		t.Error("nil 的 Truncate 该报「未开启」")
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if s := sc.Text(); s != "" {
			out = append(out, s)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
