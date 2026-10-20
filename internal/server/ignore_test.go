package server

import "testing"

// 逐条对照 git 的行为：这些断言都是 `git check-ignore` 的实测结果。
func TestIgnoreBasic(t *testing.T) {
	ctx := IgnoreCtx{owner: []string{"."}, rules: [][]ignoreRule{parseIgnore(`
# 注释
*.log
!keep.log
/dist/
build
**/tmp
/rootonly.txt
docs/*.md
`)}}
	cases := []struct {
		rel  string
		dir  bool
		want bool
	}{
		{"a.log", false, true},
		{"sub/a.log", false, true},
		{"keep.log", false, false},
		{"sub/keep.log", false, false},
		{"dist", true, true},
		{"dist", false, false}, // 带 / 的规则只匹配目录
		{"dist/a.js", false, false},
		{"build", true, true},
		{"a/build", true, true},
		{"a/tmp", true, true},
		{"a/tmp", true, true},
		// 父目录已忽略，遍历根本不会落到它下面；Match 对深层路径不做推断
		{"a/tmp/x", true, false},
		{"rootonly.txt", false, true},
		{"sub/rootonly.txt", false, false},
		{"docs/a.md", false, true},
		{"docs/deep/a.md", false, false},
	}
	for _, c := range cases {
		if got := ctx.Match(c.rel, c.dir); got != c.want {
			t.Errorf("Match(%q, dir=%v)=%v want %v", c.rel, c.dir, got, c.want)
		}
	}
}

func TestIgnoreNestedScope(t *testing.T) {
	root := IgnoreCtx{}
	root = loadIgnoreInMem(root, ".", parseIgnore("*.log\n/gen\n"))
	sub := loadIgnoreInMem(root, "pkg", parseIgnore("!app.log\n*.tmp\n"))
	cases := []struct {
		ctx  IgnoreCtx
		rel  string
		want bool
	}{
		{root, "a.log", true},
		{root, "gen", true},
		{sub, "pkg/app.log", false},  // 子目录规则取反覆盖父规则
		{sub, "pkg/other.log", true}, // 父规则仍然生效
		{sub, "pkg/x.tmp", true},
		{root, "x.tmp", false}, // 子目录规则不外溢
	}
	for _, c := range cases {
		if got := c.ctx.Match(c.rel, false); got != c.want {
			t.Errorf("Match(%q)=%v want %v", c.rel, got, c.want)
		}
	}
}

func loadIgnoreInMem(parent IgnoreCtx, rel string, rules []ignoreRule) IgnoreCtx {
	out := IgnoreCtx{
		owner:       append([]string{}, parent.owner...),
		rules:       append([][]ignoreRule{}, parent.rules...),
		files:       parent.files,
		strict:      parent.strict,
		noGitignore: false,
	}
	out.owner = append(out.owner, rel)
	out.rules = append(out.rules, rules)
	return out
}
