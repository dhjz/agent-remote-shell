// Package policy 是命令黑名单。它拦的是「明显不该被 agent 敲」的东西，
// 不是安全边界——真正的边界是运行账户 + 目录 ACL。
package policy

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type Rule struct {
	Re  *regexp.Regexp
	Src string
}

type Policy struct {
	rules []Rule
	// noDefaults 把内置的两层（词表 + 模式）一起关掉，只留 -deny 的显式规则。
	// 只关一半比不关更让人困惑：文档说「关掉内置黑名单」，那就该全关。
	noDefaults bool
}

// Defaults 是内置的「模式」层：参数在哪出现都算数的动作。
// 另外两层（注册表、首段动作）靠词表，见 verbs / regVerbs / regAddr。
var Defaults = []string{
	// 格式化 / 删盘
	`(?i)\b(mkfs(\.\w+)?|format-volume|clear-disk|diskpart|remove-partition|wipefs)\b`,
	`(?i)\bdd\s+if=.+of=/dev/`,
	// 关机重启
	`(?i)\b(shutdown|reboot)\b\s+(-[a-z]+\s+)*(/r|/s|/p|/f|now|0)`,
	// 下载即执行、编码命令、fork 炸弹
	`(?i)(curl|wget|iwr|invoke-webrequest)[^|]*\|\s*(sh|bash|zsh|pwsh|powershell|iex|invoke-expression)\b`,
	`(?i)\b(bash|sh|pwsh|powershell)\s+(-[a-z]*-?enc|-enc|-encodedcommand)\b`,
	`(?i):\(\)\s*\{.*\};\s*:`,
}

// verbs 是「这条命令本身是干什么的」，只在首段命中时才算。
//
// 判据落在动词上而不是路径上：`nc -l 4444` 和 `go test -modfile ./x` 只差
// 一个词，路径一点忙都帮不上。删那一档不在这里——它看路径，见 pathVerbs。
var verbs = []string{
	// 提权与账户
	"sudo", "doas", "su", "runas", "setpriv", "pkexec",
	"useradd", "usermod", "userdel", "adduser", "deluser", "passwd", "chpasswd",
	// 服务、计划任务、防火墙
	"sc", "netsh", "bcdedit", "vssadmin", "wbadmin", "schtasks", "crontab", "systemctl", "service",
	// 账户与日志：net user/localgroup 改的是机器上的账户，wevtutil 清的是痕迹
	"net", "wevtutil",
	// 磁盘
	"dd", "mkfs", "fdisk", "parted", "diskpart", "format", "format-volume", "wipefs",
	// 关机重启。shutdown / reboot 由模式层兜（要看参数），这两个 cmdlet
	// 名字本身就是动作：`Restart-Computer -Force` 不带任何开关也照样重启。
	"restart-computer", "stop-computer",
	// 外带
	"nc", "ncat", "netcat", "socat", "telnet", "ssh",
}

// pathVerbs 是「删」这一档：和别的首段动作不同，它们不是一见就拦，
// 而是看路径判——删沙箱里的文件是日常，删沙箱外的是越界。
//
// 这条判据和 -root 是同一条。要让 agent 干活，`rm build/tmp.o` 就必须过；
// 只要有 VERB 一律拦（早先就是这样），agent 连清理产物都做不到，只能绕道
// `go clean` 或者求人。
var pathVerbs = []string{
	"rm", "unlink", "shred", "srm", "wipe", "del", "erase", "rmdir", "rd",
}

// modeVerbs 是「改权限 / 改属主」这一类：它们只看参数判，不看首段。
//
// 单独成一层是因为 chmod 在两个位置上长得很像，含义却相反：
//
//	chmod +x ./wr                   打包、跑测试的第一步，agent 天天要做
//	chmod -R 777 /                  机器就不是你的了
//
// 判据落在参数上：给谁开、开多少。只加执行位给工作区里的路径放行，
// 777/666 这类「谁都能写」、递归、以及指向系统目录的照样拦。
var modeVerbs = []string{
	"chmod", "chown", "chgrp", "chattr", "setfacl", "takeown", "icacls", "cacls", "attrib",
}

// regVerbs 是注册表动词：cmd 的 reg 工具、PowerShell 的 *-ItemProperty 系列、
// regedit。写和查都在表里——注册表读一眼就能知道这台机器装了什么、
// 跑在谁的账户下，对「只该动沙箱里文件」的 agent 来说已经是越界。
var regVerbs = []string{
	"add", "set", "delete", "query", "copy", "compare", "export", "import",
	"restore", "save", "load", "unload", "flags",
	"new", "remove", "clear", "get", "edit", "rename", "test", "append", "merge",
	"regedit",
	"get-itemproperty", "set-itemproperty", "new-itemproperty", "remove-itemproperty",
	"get-item", "set-item", "new-item", "remove-item", "get-childitem", "set-location",
}

// regAddr 是「这段话在说注册表」的标记：
//
//	reg / regedit / reged              Windows 那几个工具名
//	registry / registrykey / pshk      PowerShell 的注册表驱动器名
//	hklm / hkcu / hkey_local_machine…  根键缩写（HKLM:\、HKCU: 都算）
var regAddr = []string{
	"reg", "regedit", "reged", "registry", "registrykey", "pshk",
	"hklm", "hkcu", "hkcr", "hku", "hkcc",
	"hkey_local_machine", "hkey_current_user", "hkey_classes_root", "hkey_users", "hkey_current_config",
}

// hiveRoot 是注册表根键在字符串里的样子。出现在命令里就是一条注册表路径，
// 光有工具名（reg）还不够——`grep -rn "reg query"` 里的 reg 是搜索词。
var hiveRoot = []string{"hklm", "hkcu", "hkcr", "hku", "hkcc", "hkey_"}

var (
	verbsRe   = mustWord(verbs)
	modeRe    = mustWord(modeVerbs)
	pathRe    = mustWord(pathVerbs)
	regVerbRe = mustWord(regVerbs)
	regAddrRe = mustWord(regAddr)
)

// mustWord 把词表编成「整词」正则。词尾只认这些边界，免得 rm 借走 rmdir、
// reg 借走 registry——两边都在表里，长短共存才是想要的效果。
//
// 边界里特意不含反斜杠：用 [^0-9a-z_-] 这种「非标识符字符」的写法，
// 既覆盖空格引号斜杠冒号，也不用在正则里写转义（写错一个就少拦一大片）。
func mustWord(words []string) *regexp.Regexp {
	alts := make([]string, len(words))
	for i, w := range words {
		alts[i] = regexp.QuoteMeta(w)
	}
	sort.SliceStable(alts, func(i, j int) bool { return len(alts[i]) > len(alts[j]) })
	return regexp.MustCompile(`(?i)(^|[^0-9a-z_.-])[:]?(` + strings.Join(alts, "|") + `)([^0-9a-z_-]|$)`)
}

func New(extra []string, noDefaults bool) (*Policy, error) {
	p := &Policy{noDefaults: noDefaults}
	if !noDefaults {
		for _, s := range Defaults {
			re, err := regexp.Compile(s)
			if err != nil {
				return nil, fmt.Errorf("内置规则编译失败 %q: %w", s, err)
			}
			p.rules = append(p.rules, Rule{Re: re, Src: s})
		}
	}
	for _, s := range extra {
		if strings.TrimSpace(s) == "" {
			continue
		}
		re, err := regexp.Compile(s)
		if err != nil {
			return nil, fmt.Errorf("非法 -deny 规则 %q: %w", s, err)
		}
		p.rules = append(p.rules, Rule{Re: re, Src: s})
	}
	return p, nil
}

// Check 命中即拒。返回命中的规则原文方便排查。
//
// cmd 和 args 分开看，不先拼成一行：拼出来的「首段」未必是真正执行的那条命令，
// 拦下来的东西和日志里记的对不上，排查时最难查。
func (p *Policy) Check(cmd string, args []string) error {
	for _, t := range append([]string{cmd}, args...) {
		if err := p.match(t); err != nil {
			return err
		}
	}
	return nil
}

// match 判一段命令：命令本体一遍，引号里那段再一遍。
//
// 分两遍是因为同一个字符串在两种位置上含义不同：
//
//	grep -rn 'reg query' docs/   引号里是搜索词，不该拦
//	cmd /c "reg query HKLM\SAM"  引号里是命令，该拦
//
// 所以引号里那段要自己站得住才判：有根键路径、有注册表动词，才当命令看。
func (p *Policy) match(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	if err := p.matchOne(s); err != nil {
		return err
	}
	for _, q := range quoted(s) {
		if err := p.matchQuoted(q); err != nil {
			return err
		}
	}
	return nil
}

// matchOne 判一条命令。三层，顺序有意：注册表的提示最具体，排最前。
func (p *Policy) matchOne(s string) error {
	if p.noDefaults {
		return p.matchPatterns(s)
	}
	head := commandHead(s)
	// 首段为空不等于没事：`bash -enc <base64>` 的命令藏在开关后面，
	// 交给模式层收拾，别在这里直接放过。
	if head != "" {
		if isRegCtx(s) {
			return fmt.Errorf("命令被策略拒绝：注册表操作（%q），wr 只该动沙箱里的文件", head)
		}
		// 首段动作看引号外的部分：`echo 'rm -rf /' >> notes.md` 是记笔记，
		// 不是删盘——引号里那串在 shell 眼里只是字符串。
		// `echo x && rm -rf /` 里的 rm 是真会跑的第二段，所以按段挨个看，不只第一段。
		// 注意这里不能用 stripQuoted：引号里的路径（`rm -rf "dir with space"`）
		// 正是这一层要判的东西，抹掉就成了「没有参数」。命令结构由 pathFields 认。
		for _, seg := range segments(s) {
			h := commandHead(seg)
			if h == "" {
				continue
			}
			if verbsRe.MatchString(h) && !pathRe.MatchString(h) {
				return fmt.Errorf("命令被策略拒绝：%q 属于黑名单动作，真要做请人工执行", h)
			}
			if modeRe.MatchString(h) {
				if why := modeRisk(h, seg); why != "" {
					return fmt.Errorf("命令被策略拒绝：%s（%q）", why, h)
				}
			}
			// 沙箱边界：跨出去的增删改查一律拦，仓库里的随便。
			// 判据落在路径上，不在动词上——`rm build/tmp.o` 是日常，`rm -rf /` 不是。
			if c, args, ok := commandCall(seg); ok {
				if bad, why := touchesOutside(c, args); why != "" {
					return fmt.Errorf("命令被策略拒绝：%s（%q），wr 只该动沙箱里的文件", why, bad)
				}
				if why := pathRisk(c, args); why != "" {
					return fmt.Errorf("命令被策略拒绝：%s（%q）", why, c)
				}
			}
		}
	}
	return p.matchPatterns(s)
}

// segments 按 shell 的命令分隔符切段：`a && b; c | d` 是四条独立命令。
// 只看第一段会漏掉 `echo x && rm -rf /` 里真正危险的那半截。
func segments(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ';', '|', '&', '\n':
			return true
		}
		return false
	})
}

// matchQuoted 判引号里的内容——这里拿到的每一段都已经确认过「执行面在这段引号里」：
// `sh -c "…"` 是命令，`echo "…" > f` 只是写字，后者进不来。
//
// 所以直接按整条命令判一遍：`sh -c "rm -rf /"` 跟敲 `rm -rf /` 是同一件事，
// 不该因为中间隔了层引号就少拦一层。
func (p *Policy) matchQuoted(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	// 引号里必须有根键路径：`reg query` 单独出现就是搜索词，
	// `reg query HKLM\SAM` 才是一条注册表命令。
	if keyPath(s) && regVerbRe.MatchString(" "+s+" ") {
		return fmt.Errorf("命令被策略拒绝：注册表操作（%q），wr 只该动沙箱里的文件", commandHead(s))
	}
	return p.matchOne(s)
}

// matchPatterns 跑「模式」层：参数在哪出现都算数的那些动作。
func (p *Policy) matchPatterns(s string) error {
	for _, r := range p.rules {
		if r.Re.MatchString(s) {
			return fmt.Errorf("命令被策略拒绝：命中 /%s/", r.Src)
		}
	}
	return nil
}

// sysDirs 是「这台机器本身」的目录。绝对路径落在这里面，也当越界看：
// `rd /s /q C:\Windows` 被拦是因为这个前缀，而不是因为 rd 这个词。
var sysDirs = []string{
	`c:\windows`, `c:\program files`, `c:\program files (x86)`, `c:\programdata`, `c:\users`,
	"/etc", "/bin", "/sbin", "/usr", "/lib", "/lib64", "/boot", "/sys", "/proc", "/dev", "/var",
	"/root", "/home", "/opt", "/private/etc",
}

// touchesOutside 判这条命令会不会碰到工作区外面的东西。
// 命中的那个词和一个说法一起返回；空串表示全在沙箱里，想怎么做都行。
//
// 只看路径，不看动词：路径落到沙箱外就是越界，落在里面就是日常。
// `rm build/tmp.o`、`mkdir -p src/a`、`cp a.go b.go` 都是后者。
// 动词该不该存在是另一层的事（verbs / pathVerbs / modeVerbs 各管各的），
// 混在一起判的话，`rm` 永远见不到这一层。
//
func touchesOutside(cmd string, args []string) (string, string) {
	// 通配先展开：`rm -f ./NU*` 在 shell 眼里就是那几个具体文件，按通配整块跳过
	// 等于给 `rm -rf ../*` 也开了后门。展开后逐个判，撑破边界的自然露出来。
	// 删除不动这一步——删的东西要认得越具体越好，通配整块交给 pathRisk。
	if !isDelete(cmd) {
		args = expandAfterExceptions(args)
	}
	for _, a := range args {
		if a == "-" || isWinSwitch(a) && !strings.ContainsAny(a, `\`) {
			continue // stdin/stdout，或者 `/s` `/f` 这类开关
		}
		if isRedirTarget(a) && !isDelete(cmd) {
			// 输出目的地：/dev/null、NUL 是空设备，写进去等于扔掉，不算「动文件」。
			// 删除不放行——`rm /dev/null` 在 Linux 上真会把设备节点删掉。
			continue
		}
		if isTmpPath(a) && !deletesTmpRoot(cmd, a) {
			// 根 /tmp 是临时文件的公用地：写测试产物、`go build -o /tmp/x` 都算日常，
			// 连它的子文件一起删也一样——写了不收拾只会越攒越多。
			// `/tmp/*` 也在这儿：收拾自己扔的产物最常见的写法，拦了只能一个个写全名。
			// 整个 /tmp 本身不在此列，那不是收拾自己的产物，是端走公用地。
			continue
		}
		if outOfSandbox(a) {
			return a, "工作区外的路径"
		}
	}
	if inSysDir(cmd) {
		return cmd, "系统目录"
	}
	return "", ""
}

// expandAfterExceptions 展开通配，但放过「本来就有豁免」的那两个名字：
// 空设备和根 /tmp。它们得原样留到循环里，由 isRedirTarget / isTmpPath 判——
// 先展开的话 `/tmp/*` 会变成一串 `/tmp` 下的绝对路径，反而撞上「工作区外的路径」。
func expandAfterExceptions(args []string) []string {
	const maxExpanded = 2048
	out := make([]string, 0, len(args))
	for _, a := range args {
		if !strings.ContainsAny(a, "*?[") {
			out = append(out, a)
			continue
		}
		if isRedirTarget(a) || isTmpPath(a) {
			out = append(out, a)
			continue
		}
		g := expandGlob(a)
		if len(g) > maxExpanded {
			out = append(out, a) // 太大了就按原模式看，宁可多拦
			continue
		}
		out = append(out, g...)
	}
	if len(out) > maxExpanded {
		return args
	}
	return out
}

// redirTargets 是「写到这儿等于扔掉」的那几个名字。
// 比较前会把反斜杠归一、大小写拉平，所以这里只管名字本身。
var redirTargets = []string{
	"/dev/null", "/dev/stdout", "/dev/stderr",
	"nul", `\\.\nul`, "/nul",
}

// isRedirTarget 判一段是不是「重定向到空设备」的写法。
//
// 这一条是实测踩出来的：`curl -s -o /dev/null http://x` 在 Windows 沙箱里被
// 「工作区外的路径 /dev/null」拦掉，可它一个字节都没写到沙箱外——真要写的话
// Windows 上根本没有 /dev/null 这个路径。判据落在「写不写得出去」上，
// 而不是「字符串里有没有斜杠」上。
//
// 管两种写法：`> /dev/null` 这种重定向符后面的目标（落在参数内部），
// 和 `-o /dev/null` 这种「开关的下一个词」（本身就是参数）。
// 只认空设备。根 /tmp 是另一条（见 isTmpPath）：它不在这个函数里，因为
// 它的判据多看一层「是不是在删」。/proc、~ 这些实打实是别人的东西，仍然拦。
func isRedirTarget(t string) bool {
	for _, w := range strings.FieldsFunc(t, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '>'
	}) {
		tok := strings.ToLower(strings.Trim(strings.ReplaceAll(w, `\`, "/"), "'\"`"))
		if hasWord(redirTargets, tok) {
			return true
		}
	}
	return false
}

// tmpRoots 是「临时的东西扔这儿」的那几个目录，只认根下的。
//
// 放行的理由是它们本来就没人认领：写进去迟早被清，写坏了也不牵连谁。
// 判据和空设备同一个路子——不看字符串里有没有斜杠，看「碰了会不会弄坏别人的东西」。
// 不加 `~/tmp`、`/var/tmp`：前者是家目录，后者留着给跨重启的东西，都不是公用地。
var tmpRoots = []string{"/tmp", `\temp`}

// isTmpRoot 判一个参数是不是「整个根 /tmp」本身，而不是它底下的某个文件。
//
// 只给删这一档用：`rm -rf /tmp/x` 是收拾自己刚扔的产物，`rm -rf /tmp` 是把
// 公用地整个端走——连别人正在用的东西一起。
func isTmpRoot(t string) bool {
	p := strings.ToLower(strings.ReplaceAll(t, `\`, "/"))
	for _, r := range tmpRoots {
		r = strings.ReplaceAll(r, `\`, "/")
		if p == r || p == r+"/" || p == r+"/." {
			return true
		}
	}
	return false
}

// deletesTmpRoot 判这条删除是不是在端走整个根 /tmp。
//
// 只有「删」这一档要看它：`cat /tmp`、`ls /tmp` 看一眼没什么，`rm -rf /tmp`
// 删的是所有人放在那儿的文件。
func deletesTmpRoot(cmd, arg string) bool {
	return isDelete(cmd) && isTmpRoot(arg)
}

// isTmpPath 判一个参数是不是落在根 /tmp 底下：
//
//	/tmp、/tmp/x、/tmp/a/b        是
//	~/tmp、/root/tmp、/var/tmp    不是——家目录和 /var 里的东西有人认领，名字像不代表是
//	/tmpx、/tmp2、TMP/x、./tmp    不是——按整段比，不做前缀匹配；相对路径归沙箱管
func isTmpPath(t string) bool {
	p := strings.ToLower(strings.ReplaceAll(t, `\`, "/"))
	if !strings.HasPrefix(p, "/") {
		return false // 相对路径（./tmp、tmp/x）落在沙箱里，归沙箱管
	}
	// `..` 先拦在外面：`/tmp/../etc` 前缀看着像 /tmp，走出去就是 /etc。
	if p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "/../") || strings.HasSuffix(p, "/..") {
		return false
	}
	for _, r := range tmpRoots {
		r = strings.ReplaceAll(r, `\`, "/")
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

// isDelete 判这条命令是不是在删东西。
func isDelete(cmd string) bool {
	return hasWord(pathVerbs, strings.TrimSuffix(strings.ToLower(cmd), ".exe"))
}

// hasWildcard 判这串参数里有没有通配符（开关里的不算：`/f` 不是通配）。
func hasWildcard(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") || isWinSwitch(a) {
			continue
		}
		if strings.ContainsAny(a, "*?") {
			return true
		}
	}
	return false
}

// outOfSandbox 判一个路径参数是不是出了沙箱。
//
// Windows 的反斜杠先归一成正斜杠再判：agent 写 `C:\Windows` 还是 `C:/Windows`
// 说的是同一件事，判据不该因为换个斜杠就漏一条。
func outOfSandbox(t string) bool {
	p := strings.ToLower(strings.ReplaceAll(t, `\`, "/"))
	if strings.HasPrefix(p, "//") { // UNC 共享 \\host\share
		return true
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.Contains(p, "$home") || strings.Contains(p, "%userprofile%") {
		return true
	}
	// 绝对路径：POSIX 的 /x、Windows 的 D:/x
	if strings.HasPrefix(p, "/") || isDriveAbs(p) {
		return true
	}
	return p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "/../") || strings.HasSuffix(p, "/..")
}

// pathRisk 判「删」这一档该不该拦，返回拒绝理由，空串表示放行。
//
// 判据落在参数上，跟 chmod 那一档同一个路子：
//
//	rm build/tmp.o、rm -rf build、rd /s /q build    工作区里的，放行
//	rm -rf *、rm -rf .、del /f /s /q *              没写清删什么，说不准就拦
//	rm -rf /、del C:\Windows\*、rd /s /q D:\        路径出了沙箱，拦
//
// 一个路径参数都没有（`rm -rf`、`rd /s /q`）也拦：没见过这样删东西的，
// 十有八九是命令被切坏了，放过它比拦下来更糟。
func pathRisk(cmd string, args []string) string {
	if !hasWord(pathVerbs, strings.TrimSuffix(strings.ToLower(cmd), ".exe")) {
		return ""
	}
	n := 0
	wild := 0 // 这次删除里带通配的参数个数
	for _, a := range args {
		if strings.HasPrefix(a, "-") || isWinSwitch(a) {
			if isWinWildcard(a) {
				return "没写清删什么（通配）"
			}
			continue
		}
		n++
		if isWinWildcard(a) {
			if isTmpPath(a) {
				continue // `/tmp/*` 收在公用地里，不参与「光秃秃一个通配」的计数
			}
			wild++
			// 静态前缀落在工作区外就是明摆着的越界，报越界比报通配有用。
			// 前缀还站在工作区里，这条通配才有可能只碰到自己的文件：
			// `./NU*`、`build/*.o`、`src/**/*.go` 收得住；`*`、`*.o`、`../*` 收不住。
			// 判据是「撑不撑破边界」，不是「有没有星号」——通配是 shell 的日常写法。
			if !globStaysInside(a) {
				return "没写清删什么（通配）"
			}
			continue
		}
		if a == "." {
			return "没写清删什么（通配）"
		}
	}
	if n == 0 {
		return "没写清删什么"
	}
	// 光秃秃一个 `*` / `*.o` 还是拦：通配收得进当前目录，但它说的是「这一层的全都算」，
	// 仓库根上敲下去就是整棵树，跟 `rm -rf .` 是一句话。要写就写清挑哪些：
	// `./NU*`、`*.log`、`src/*.go` —— 这些都是「挑名字」，不是「这一层全算」。
	if wild > 0 && wild == n && !namesPattern(args) {
		return "没写清删什么（通配）"
	}
	return ""
}

// namesPattern 判这次删除带不带「挑名字」的部分：通配之前还有字面量
// （`./NU*` 的 `./NU`、`*.log` 的 `.log`、`src/*.go` 的 `src/`）。
//
// 只有 `*`、`*.*`、`**` 这种一个字母都不带的，才是「这一层全算」。
func namesPattern(args []string) bool {
	for _, a := range args {
		if !isWinWildcard(a) {
			continue
		}
		t := strings.Map(func(r rune) rune {
			if r == '*' || r == '?' || r == ' ' || r == '.' || r == '/' || r == '\\' {
				return -1
			}
			return r
		}, a)
		if t != "" {
			return true
		}
	}
	return false
}

// globStaysInside 判一条通配删的是不是「当前这层目录里的东西」。
//
// 静态前缀在第一层就已经用完了：`./NU*` 的前缀 `./` 只在当前目录里挑名字，
// 挑出来的跑不出沙箱；`../*`、`/etc/*` 的前缀本身就在外面。前缀是空串或 `.`
// 时同理——`*`、`*.o` 挑的是当前目录，`rm -rf *` 里没有通配之前那么广。
//
// 这条只管「前缀在不在工作区里」；「光秃秃一个通配」另算，见 pathRisk 里的 wild 计数。
func globStaysInside(pat string) bool {
	return !outOfSandbox(globStaticPrefix(pat))
}

// isWinWildcard 判这个参数是不是「什么都能匹配上」：`*`、`C:\x\*`、`*.o` 都算。
// `del *.o` 这种看着收敛，实际还是靠通配删文件，宁可让人写全名。
func isWinWildcard(t string) bool {
	return strings.ContainsAny(t, "*?")
}

// inSysDir 判这个路径是不是已经站到系统目录里了。
func inSysDir(p string) bool {
	p = strings.ToLower(strings.ReplaceAll(strings.TrimRight(p, `\/`+"'\"`"), `\`, "/"))
	for _, d := range sysDirs {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// isDriveAbs 判 `D:` / `D:/x` 这类盘符开头的绝对路径。
func isDriveAbs(p string) bool {
	if len(p) < 2 || p[1] != ':' {
		return false
	}
	c := p[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isWinSwitch 判 cmd 风格的开关：`/s`、`/f`、`/q`、`/t 0`。
// 只认「斜杠 + 短词」——`/etc`、`/home/me` 这种绝对路径长得像但不一样。
func isWinSwitch(t string) bool {
	if !strings.HasPrefix(t, "/") || strings.HasPrefix(t, "//") {
		return false
	}
	body := strings.TrimLeft(t, "-/")
	if body == "" {
		return false
	}
	for _, c := range body {
		if c == '/' || c == '\\' || c == '.' {
			return false
		}
	}
	return len(body) <= 6
}

// modeRisk 判一条 chmod/chown 类的命令该不该拦，返回拒绝理由，空串表示放行。
//
// 四条判据，任一命中即拦：
//
//	-R / --recursive     递归改一整棵树，误伤面收不住
//	777 / 666 / a+w / o+w  「谁都能写」；+s 是 setuid，等于换个人跑
//	/ 开头、~、$HOME、..  出了工作区就是在动系统的权限
//	chown / 接管类工具     改属主就是把文件交给另一个账户，工作区里也没有这个需求
//
// 剩下的放行：`chmod +x ./wr`、`chmod 755 build/run.sh`、`chmod u+x bin/*`、
// `attrib -r x`（Windows 上给只读文件解只读，打包日常）。
// 这条边界和工作区沙箱是同一条：仓库里的文件，agent 想怎么定权限都行；
// 仓库外的，一个字都别碰。
func modeRisk(cmd, s string) string {
	if why := modeBits(cmd, s); why != "" {
		return why
	}
	if cmd, args, ok := commandCall(stripQuoted(s)); ok {
		if _, why := touchesOutside(cmd, args); why != "" {
			return "改工作区外的权限"
		}
	}
	return ""
}

// modeBits 判权限位本身：递归、谁都能写、setuid、改属主。
// 这几条跟路径无关，落在仓库里也一样危险——`chmod -R 777 .` 是仓库自己的权限，
// 但递归 + 777 一起给出去，误伤面收不住。
func modeBits(cmd, s string) string {
	verb := strings.TrimSuffix(strings.ToLower(cmd), ".exe")
	switch verb {
	case "chown", "chgrp", "setfacl", "takeown", "icacls", "cacls":
		return "改属主或接管"
	}
	args := commandArgs(stripQuoted(s), verb)
	for _, t := range args {
		if chmodRecursive(t) {
			return "递归改权限"
		}
	}
	for _, t := range args {
		switch {
		case strings.Contains(t, "777"), strings.Contains(t, "666"),
			strings.Contains(t, "a+w"), strings.Contains(t, "o+w"), strings.Contains(t, "+s"):
			return "给所有人写权限"
		}
	}
	return ""
}

// commandCall 从一条命令里切出「命令本体 + 它后面的参数」。
//
// pathFields 按「词」切命令，供边界判断用：引号里的东西算一个词，空格不算分隔符。
//
// 边界判断要的是「这条命令会碰哪些路径」，不是「shell 会怎么分词」。
// shell 是在分词之后才去掉引号的，两件事不等价：
//
//	rm -rf "dir with space"   shell 眼里是一个参数；按空格切就成了 dir / with / space
//	rm -f ./NU*               shell 会先把 glob 展开成真名，展出来的都是工作区里的文件
//
// 按空格硬切的话，前者切出一个不存在的 with、后者留下一个带通配的 ./NU*，
// 两条都被判成「没写清删什么」——拦的不是风险，是自己没读懂。风险跟写法无关，
// 所以这里统一按词切；通配另由 expandGlob 展开看边界。
func pathFields(s string) []string {
	var out []string
	var buf strings.Builder
	var q rune
	flush := func() {
		if t := strings.TrimSpace(buf.String()); t != "" {
			out = append(out, t)
		}
		buf.Reset()
	}
	for _, c := range s {
		switch {
		case q != 0:
			if c == q {
				q = 0
			} else {
				buf.WriteRune(c)
			}
		case c == '"' || c == '\'':
			q = c
			flush() // 引号前那半截自成一个词
		case c == ' ' || c == '\t' || c == '\n' || c == ';' || c == '|' || c == '&' ||
			c == '(' || c == ')' || c == '{' || c == '}':
			flush()
		default:
			buf.WriteRune(c)
		}
	}
	flush()
	return out
}

// globStaticPrefix 取通配符之前那段不含元字符的目录前缀：
// `./build/*.o` → `./build`，`../x/*` → `../x`，`*` → 空串。
func globStaticPrefix(p string) string {
	if i := strings.IndexAny(p, "*?[{"); i >= 0 {
		p = p[:i]
	}
	if p == "" {
		return "." // 「就在当前目录」，跟写全名时一个意思
	}
	return p
}

// expandGlob 把带通配的参数换成它可能碰到的路径，供边界判断。
//
// 判据只有一条：这条通配能不能把边界撑破。`./NU*`、`build/*.o`、`src/**/*.go`
// 展出来的都在工作区里，删起来跟写全名是一回事；`../*`、`/etc/*` 就说不准了，
// 照旧按通配拦。目录不存在、glob 空手而归时看静态前缀，够判断——判目的是
// 「这条通配有多大」，不是「现在还存不存在」。
func expandGlob(pat string) []string {
	out := []string{globStaticPrefix(pat)}
	m, err := filepath.Glob(pat)
	if err != nil {
		return out // 非法模式（Windows 上 `[` 未闭合之类）：只按静态前缀判
	}
	for _, h := range m {
		if s := strings.ReplaceAll(h, `\`, "/"); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// 给 touchesOutside 用，所以切得比 commandArgs 狠：前缀赋值（`FOO=1 rm …`）、
// 透传词（`sudo rm …`）、shell 与开关（`cmd /c del …`、`bash -c "…"`）都剥掉，
// 留下的第一个词才是真动作，后面的词才是它真正会碰的东西。
func commandCall(s string) (string, []string, bool) {
	fs := pathFields(s)
	var out []string
	keepNext := false
	for _, raw := range fs {
		t := strings.Trim(raw, "\"'`")
		low := strings.ToLower(t)
		if keepNext {
			// 紧跟 -o / -O 的那个词是输出目的地，不是命令的一部分：
			// 留着让上层判它是工作区内的文件还是空设备。
			out = append(out, t)
			keepNext = false
			continue
		}
		if low == "-o" || low == "-O" || low == "--output" {
			keepNext = true
			continue
		}
		if t == "" || strings.HasPrefix(low, "-") || isWinSwitch(low) {
			continue // 开关：`-Recurse`、`/s`、`/f`
		}
		if j := strings.IndexByte(low, '='); j > 0 && !strings.ContainsAny(low[:j], `/\`) {
			continue // 前缀赋值
		}
		if len(out) == 0 {
			if isShellPrelude(low) || hasWord(passthrough, strings.TrimSuffix(low, ".exe")) {
				continue
			}
			out = append(out, strings.TrimSuffix(low, ".exe"))
			continue
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return "", nil, false
	}
	return out[0], out[1:], true
}

// chmodRecursive 判这个开关是不是「递归」。
//
// `-R`、`-Rv`、`--recursive` 都算，但 `attrib -r`（去掉只读）和 `chmod g+r`
// 不算——同一个 r 在大小写和位置上差一点点，意思就从「整棵树」变成「这一位」。
// 所以只看大写 R，外加 GNU 的长选项。
func chmodRecursive(t string) bool {
	if t == "--recursive" {
		return true
	}
	return strings.HasPrefix(t, "-") && !strings.HasPrefix(t, "--") && strings.Contains(t, "R")
}

// fields 按 shell 的分隔符切词，和 commandHead 用同一把刀。
func fields(s string) []string {
	raw := pathFields(s)
	out := make([]string, 0, len(raw))
	for _, f := range raw {
		if t := strings.ToLower(strings.Trim(f, "\"'`")); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// commandArgs 取 cmd 它自己后面的参数。
//
// 从原串里切，不从 fields 的结果里切：fields 会把词统一小写，而 -R 和 -r
// 在 chmod 里是两个意思，切完再看就分不清了。
func commandArgs(s, cmd string) []string {
	cmd = strings.TrimSuffix(strings.ToLower(cmd), ".exe")
	raw := pathFields(s)
	for i, f := range raw {
		if strings.TrimSuffix(strings.ToLower(strings.Trim(f, "\"'`")), ".exe") == cmd {
			raw = raw[i+1:]
			break
		}
	}
	out := make([]string, 0, len(raw))
	for _, f := range raw {
		if t := strings.Trim(f, "\"'`"); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// isRegCtx 判这条命令是不是在动注册表。
//
// 判据是「命令位置上有注册表动词 + 整串里有注册表地址」，两样都凑齐才算：
// 只看动词，`go test -run Query` 会中枪；只看地址，`git log --grep hklm` 也中枪。
// 地址可以落在引号里（`Get-ItemProperty -Path 'HKLM:\SOFTWARE'` 是 PS 的常态），
// 但动词必须在引号外面——`grep -rn 'reg query'` 里连动词都只是搜索词。
func isRegCtx(s string) bool {
	if !regAddrRe.MatchString(s) {
		return false
	}
	out := stripQuoted(s)
	return regVerbRe.MatchString(out) || regVerbRe.MatchString(commandHead(out))
}

// keyPath 判这段里有没有一条注册表路径：根键（HKLM\…）或 PS 的驱动器
// （HKLM:\…）。`reg query` 过不了，`reg query HKLM\SAM` 能过。
func keyPath(s string) bool {
	low := strings.ToLower(s)
	for _, h := range hiveRoot {
		if strings.Contains(low, h) {
			return true
		}
	}
	return false
}

// stripQuoted 抹掉引号里的内容，留下命令结构：
//
//	grep -rn 'reg query' docs/  →  grep -rn   docs/
//
// 引号里的是数据，不该让外面的命令替它挨刀。
// 被抹掉的那段换成两个空格，不是直接删：删了 `echo "a;b" && go build ./...`
// 会拼出一条根本不存在的 `echo && go build`，后面判首段就被带偏了。
func stripQuoted(s string) string {
	var b strings.Builder
	var quote rune
	for _, c := range s {
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
				b.WriteByte(' ')
			}
		case c == '"' || c == '\'':
			quote = c
			b.WriteByte(' ')
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// scriptVerbs 是「它后面那段引号里是要执行的脚本」的词。
// 同样的引号，跟在 sh 后面是命令，跟在 echo / cat / grep 后面是数据。
var scriptVerbs = []string{
	"sh", "bash", "zsh", "dash", "ksh", "fish", "pwsh", "powershell", "cmd",
	"iex", "invoke-expression", "eval",
}

// passthrough 自己不执行，只把后头的命令原样交给别人跑。
var passthrough = []string{"env", "sudo", "doas", "nohup", "time", "timeout", "nice", "stdbuf", "xargs"}

// quoted 把「执行面在引号里」的那几段摘出来。别的引号一律当数据：
//
//	sh -c "rm -rf /"            引号里是要执行的脚本 → 摘
//	echo "rm -rf /" > notes.md  引号里是笔记，只写文件 → 不摘
//
// 早先是「凡是引号都摘」，于是 `echo "rm -rf /" > notes.md` 被当成删盘拦下来——
// 拦的不是执行，是往文件里写字。
func quoted(s string) []string {
	var out []string
	var buf, pre strings.Builder // pre 是引号之前那段，用来判谁带着这段引号
	var q rune
	exec := false
	flush := func() {
		if exec {
			out = append(out, buf.String())
		}
		buf.Reset()
		exec = false
	}
	for _, c := range s {
		switch {
		case q != 0 && c == q:
			flush()
			q = 0
		case q != 0:
			buf.WriteRune(c)
		case c == '"' || c == '\'':
			q, exec = c, runsScript(pre.String())
			pre.Reset()
		default:
			pre.WriteRune(c)
		}
	}
	return out
}

// runsScript 判引号之前那段的命令是不是真在带脚本：`./run.sh "…"`、`bash -c "…"`、
// `env A=1 bash -c "…"` 都算；`echo "…"` 不算。
func runsScript(pre string) bool {
	fields := strings.FieldsFunc(pre, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', ';', '|', '&', '(', ')', '{', '}':
			return true
		}
		return false
	})
	if len(fields) == 0 {
		return false
	}
	head := strings.ToLower(strings.Trim(fields[0], "\"'`"))
	if hasWord(passthrough, head) {
		// 透传词后面第一个不带开关、不带赋值的词才是真命令
		for _, f := range fields[1:] {
			w := strings.ToLower(strings.Trim(f, "\"'`"))
			if strings.Contains(w, "=") || strings.HasPrefix(w, "-") {
				continue
			}
			return isShell(w) || strings.ContainsAny(w, `/\`)
		}
		return false
	}
	if isShell(head) || strings.ContainsAny(head, `/\`) {
		return true
	}
	// powerShell 的 -File x.ps1 之外，`x.ps1 "…"` 也靠关联程序执行
	return strings.HasSuffix(head, ".ps1")
}

func isShell(w string) bool {
	return hasWord(scriptVerbs, strings.TrimSuffix(w, ".exe"))
}

func hasWord(words []string, w string) bool {
	for _, x := range words {
		if x == w {
			return true
		}
	}
	return false
}

// commandHead 取「真正要执行的那个词」。
//
// 按 shell 的分隔符切段，每段只看第一个词。三种情况往后多看一个词：
// 前缀赋值（`FOO=1 go test`）、解释器开关（`cmd /c reg …`）、`cd` 的参数
// （`cd /tmp && rm …` 的判据在 rm 上）。参数里的词一个都不算——
// `grep chmod` 里的 chmod 是搜索词，不是命令。
func commandHead(s string) string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', ';', '|', '&', '(', ')', '{', '}':
			return true
		}
		return false
	})
	skip := 0
	for i, f := range fields {
		t := strings.ToLower(strings.Trim(f, "\"'`"))
		if skip > 0 {
			skip--
			continue
		}
		if t == "" {
			continue
		}
		if j := strings.IndexByte(t, '='); j > 0 && !strings.ContainsAny(t[:j], `/\`) {
			continue
		}
		if isShellPrelude(t) && i+1 < len(fields) {
			skip = 1 // cd 的参数、解释器的开关
			continue
		}
		return t
	}
	return ""
}

// isShellPrelude 是那些后面还会跟一条真命令的词：cd、env、各 shell 与 cmd。
// 单独出现的（就一句 cd）不算，所以只在还有下一个词时才跳过。
func isShellPrelude(t string) bool {
	switch strings.TrimSuffix(strings.ToLower(t), ".exe") {
	case "cd", "env", "cmd", "sh", "bash", "zsh", "pwsh", "powershell", "exec", "command":
		return true
	}
	return false
}
