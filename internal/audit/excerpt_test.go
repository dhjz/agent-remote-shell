package audit

import "testing"

// 一次失败的根因基本都在第一行有内容的地方。早先 exec 的 error 分支
// 一个字都不往日志里写，事后只有一个裸的 exitCode，等于把唯一的线索扔了。
func TestExcerptTakesFirstMeaningfulLine(t *testing.T) {
	cases := []struct {
		name      string
		stderr    string
		stdout    string
		want      string
		wantEmpty bool
	}{
		{"stderr 优先", "go: 找不到包\n后续行", "这是 stdout", "go: 找不到包", false},
		{"跳过分隔空行", "\n\n  \npy: 语法错\n", "", "py: 语法错", false},
		{"stderr 空则退回 stdout", "", "npm ERR! missing script\n", "npm ERR! missing script", false},
		{"CRLF 不留 \\r", "cmd 错误\r\n第二行", "", "cmd 错误", false},
		{"两边都空就没有", "", "  \n", "", true},
		{"只留行内内容，不拼多行", "E1\nE2\nE3", "", "E1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Excerpt(c.stderr, c.stdout)
			if c.wantEmpty {
				if got != "" {
					t.Fatalf("该是空串，实际 %q", got)
				}
				return
			}
			if got != c.want {
				t.Fatalf("摘出来 %q，期望 %q", got, c.want)
			}
		})
	}
}

// 超长的一行要掐断：日志不是第二份 stderr，但也不要让某一行把预算全占了。
// 按 rune 切——中文命令行的报错按字节切会切出半个字。
func TestExcerptCapsLongLine(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "错"
	}
	got := Excerpt(long, "")
	r := []rune(got)
	if len(r) != excerptMax+1 || r[len(r)-1] != '…' {
		t.Fatalf("该是 %d 个字符加省略号，实际 %d 个", excerptMax, len(r))
	}
}
