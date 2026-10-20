package server

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ignoreRule 是 .gitignore 里的一行，编译成「可以逐层走」的形式。
//
// 遍历时对每个文件跑一遍所有规则是 O(文件数 × 规则数)；把规则按所在目录挂到树上之后，
// 进目录只带下去该目录作用域内的规则，出目录就丢——和 git 的栈式语义一致，
// monorepo 根上的 .gitignore 也就不会变成全局负担。
type ignoreRule struct {
	pat      globPattern
	negate   bool // ! 开头：重新纳入
	dirOnly  bool // 尾部 /：只匹配目录
	anchored bool // 含 / 或以 / 开头：只在规则目录下按相对路径匹配
}

// IgnoreCtx 是「走到某个目录时」所有生效规则，从根到当前目录依次排列。
// files 是叠在 .gitignore 之上的「内置忽略」（node_modules、dist 之类）：
// 它们不给用户的取反规则翻案——`!node_modules` 也救不回来，
// 否则一次搜索就会掉进几十万文件里，而这是 agent 最不需要的惊喜。
// IgnoreCtx 的零值就是「什么都不过滤」——调用方需要时再 WithBuiltin / Push 规则。
type IgnoreCtx struct {
	owner []string // 每层规则所属目录（相对 root，根为空串），与 rules 一一对应
	rules [][]ignoreRule
	files []string
	// strict 是「.gitignore 也救不回来」的硬名单：.git、node_modules 这类。
	// 用户写 `!node_modules` 是给自己找麻烦，这里不接。
	strict []string
	// noGitignore 表示这一路走下来一条 .gitignore 都没读到。
	// 只有这种时候才轮到软名单（dist/build/target…）兜底。
	noGitignore bool
}

// withBuiltin 把内置忽略集挂进来。strict 只放真正「进去了就会拖死搜索」的目录。
func WithBuiltin(names, strict []string) IgnoreCtx {
	return IgnoreCtx{}.WithBuiltin(names, strict)
}

func (c IgnoreCtx) WithBuiltin(names, strict []string) IgnoreCtx {
	c.files = append([]string{}, names...)
	c.strict = append([]string{}, strict...)
	c.noGitignore = true // 读到 .gitignore 就会被翻成 false
	return c
}

// 规则数再多也就几百条，缓存只为省掉重复 split，
// 不改语义：每次遍历都从磁盘重新读，改完 .gitignore 下一次搜索就生效。
func loadIgnoreRules(dir, rel string, ctx IgnoreCtx) IgnoreCtx {
	entries, err := readDirNames(dir)
	if err != nil {
		return ctx
	}
	var mine []ignoreRule
	found := false
	for _, name := range entries {
		// .gitignore 与 .git/info/exclude 等价，前者是主要用法。
		// 这里只认前者：exclude 的落地位置受 GIT_DIR 影响，读错了比不读更糟。
		if name != ".gitignore" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return ctx
		}
		found = true
		mine = append(mine, parseIgnore(string(data))...)
	}
	// 哪怕文件是空的，只要它存在，这个仓库的可见性就归 .gitignore 说了算。
	// 派生时把内置忽略集一起带上，否则父子链一断，node_modules 就拦不住了。
	out := IgnoreCtx{
		owner: append([]string{}, ctx.owner...), rules: append([][]ignoreRule{}, ctx.rules...),
		files: ctx.files, strict: ctx.strict, noGitignore: !found,
	}
	if len(mine) > 0 {
		out.owner = append(out.owner, rel)
		out.rules = append(out.rules, mine)
	}
	return out
}

func readDirNames(dir string) ([]string, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

// parseIgnore 逐行解析。注释、空行、行尾空白（未转义）按 git 的规矩处理。
func parseIgnore(src string) []ignoreRule {
	var out []ignoreRule
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSuffix(line, "\r")
		line = strings.TrimRight(line, " \t")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negate := strings.HasPrefix(line, "!")
		if negate {
			line = line[1:]
		}
		dirOnly := strings.HasSuffix(line, "/")
		line = strings.TrimSuffix(line, "/")
		if line == "" {
			continue
		}
		// `**/x` 记成非锚定：** 本身就表示任意层级，走到 hit 里按 basename 比对。
		anchored := strings.Contains(strings.TrimPrefix(line, "**/"), "/") || strings.HasPrefix(line, "/")
		line = strings.TrimPrefix(line, "/")
		if line == "" {
			continue
		}
		out = append(out, ignoreRule{
			pat:    globPattern{segs: strings.Split(line, "/")},
			negate: negate, dirOnly: dirOnly, anchored: anchored,
		})
	}
	return out
}

// push 读 dir 下的 .gitignore 并把规则压到栈顶（rel 是 dir 相对遍历根的路径）。
// 服务端的 walk 走的是函数递归版本（loadIgnoreRules），客户端同步目录时没有
// 那套 walk/skip 语义，只需要「逐层进出」——共用同一份解析和匹配逻辑，
// 免得两边对 .gitignore 的理解慢慢漂开。
func (c *IgnoreCtx) Push(dir, rel string) {
	*c = loadIgnoreRules(dir, rel, *c)
}

// pop 出目录时丢掉这一层的规则，兄弟目录不该看见它。
func (c *IgnoreCtx) Pop() {
	if n := len(c.owner); n > 0 {
		c.owner = c.owner[:n-1]
		c.rules = c.rules[:n-1]
	}
}

// ignores 只看内置忽略集，给遍历侧判断「这个名字是不是死目录」用。
func (c IgnoreCtx) ignores(rel string) bool {
	return contains(c.files, baseName(rel))
}

func baseName(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[i+1:]
	}
	return rel
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Match 判断相对路径是否被忽略。isDir 只影响 dirOnly 规则：
// git 里 `dist/` 只是「命中后不往下走」，`dist` 目录本身仍算被忽略。
// 目录一旦命中忽略，它下面的内容自然不会被遍历到，不需要在这里往下推。
//
// 内置忽略（files）不在这里判：它们是「没有规则时的默认视野」，会被用户的
// 取反翻案；硬名单（strict）才是不容商量的那条，所以只在 strict 上直接返回。
func (c IgnoreCtx) Match(rel string, isDir bool) bool {
	// 硬名单先判、直接返回：用户的取反规则后面也翻不了案。
	if contains(c.strict, baseName(rel)) {
		return true
	}
	ignored := false
	for i := range c.owner {
		scoped, ok := relScope(c.owner[i], rel)
		if !ok {
			continue
		}
		for _, r := range c.rules[i] {
			if !r.hit(scoped, isDir) {
				continue
			}
			ignored = !r.negate // 后出现的规则覆盖前面的，所以不能看到命中就返回
		}
	}
	return ignored
}

// hit 的 isDir 只用于 dirOnly：`dist/` 不匹配同名普通文件。
func (r ignoreRule) hit(rel string, isDir bool) bool {
	if rel == "." || (r.dirOnly && !isDir) {
		return false
	}
	if r.anchored {
		return r.pat.match(rel)
	}
	// 不含 / 的规则按「任意层级的一段」匹配；`**/x` 的 ** 也归到这里，
	// 因为它的最后一段就是那条 basename 规则。
	if r.pat.segs[0] == "**" {
		return matchSegs(r.pat.segs[1:], lastSeg(rel))
	}
	for _, seg := range strings.Split(rel, "/") {
		if ok, err := path.Match(r.pat.segs[0], seg); err == nil && ok {
			return true
		}
	}
	return false
}

// lastSeg 给 `**/x` 用：x 必须落在最后一段上。
func lastSeg(rel string) []string {
	segs := strings.Split(rel, "/")
	return segs[len(segs)-1:]
}

func relScope(ownerRel, rel string) (string, bool) {
	if ownerRel == "." || ownerRel == "" {
		return rel, true
	}
	if rel == ownerRel {
		return ".", true
	}
	prefix := ownerRel + "/"
	if !strings.HasPrefix(rel, prefix) {
		return "", false
	}
	return strings.TrimPrefix(rel, prefix), true
}

// globPattern 用 path.Match 逐段匹配，** 单独处理。
// 自己写而不是转 regexp：.gitignore 的模式集就这么点，引 regexp 反而多一层缓存和转义。
type globPattern struct {
	segs []string
}

func (g globPattern) match(rel string) bool {
	return matchSegs(g.segs, strings.Split(rel, "/"))
}

func matchSegs(pat, name []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(name); i++ {
			if matchSegs(pat[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], name[0])
	if err != nil || !ok {
		return false
	}
	return matchSegs(pat[1:], name[1:])
}
