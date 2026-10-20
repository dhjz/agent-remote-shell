// Package shellx 把「用哪个 shell 执行一段脚本」抽象成一个 profile。
// 关键取舍：wr 只做一次性执行，不做持久会话。持久会话在 Windows 上要靠
// marker 注入 + base64 喂命令，脆、慢、跨 shell 语义不一致；需要状态时
// 带 dir + env 一条命令解决，比会话可靠得多。
package shellx

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Profile 一个 shell 的执行方式。
type Profile struct {
	ID   string
	Path string
	Args func(script string) []string
	Note string
}

// IsWin 判断宿主平台。多 shell 的分支都挂在这上面。
func IsWin() bool { return runtime.GOOS == "windows" }

var registry = map[string]Profile{}

func add(p Profile) { registry[p.ID] = p }

func init() {
	add(Profile{
		ID:   "sh",
		Args: func(s string) []string { return []string{"-c", s} },
		Note: "POSIX sh，最通用",
	})
	add(Profile{
		ID:   "bash",
		Args: func(s string) []string { return []string{"--noprofile", "--norc", "-c", s} },
		Note: "bash，带 pipefail；Windows 上指 Git for Windows 的 bash（-c，不 source rc）",
	})
	add(Profile{
		ID: "pwsh",
		Args: func(s string) []string {
			return []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", s}
		},
		Note: "PowerShell 7+",
	})
	add(Profile{
		ID: "powershell",
		Args: func(s string) []string {
			return []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", s}
		},
		Note: "Windows PowerShell 5.1",
	})
	add(Profile{
		ID: "cmd",
		Args: func(s string) []string {
			return []string{"/d", "/q", "/c", s}
		},
		Note: "cmd.exe，一次性执行",
	})
	add(Profile{
		ID:   "gitbash",
		Args: func(s string) []string { return []string{"-lc", s} },
		Note: "Git for Windows 的登录式 bash（-lc，会 source rc，慢一个数量级）",
	})
	add(Profile{
		ID:   "wsl",
		Args: func(s string) []string { return []string{"--", "bash", "-lc", s} },
		Note: "WSL 默认发行版，文件系统与宿主隔离",
	})
}

// Resolve 按 ID 或直接路径拿到 Profile。未知 ID 当可执行文件路径处理，
// 这样 `-shell /usr/bin/zsh` 这种临时需求不用改代码。
func Resolve(id string) (Profile, error) {
	if id == "" {
		id = DefaultID()
	}
	if p, ok := registry[strings.ToLower(id)]; ok {
		p.Path = lookup(p)
		return p, nil
	}
	// 当成路径：用 sh -c 语义跑，最不容易踩参数坑
	if p, err := exec.LookPath(id); err == nil {
		base := strings.ToLower(filepath.Base(p))
		if prof, ok := registry[base]; ok {
			prof.Path = p
			return prof, nil
		}
		return Profile{
			ID:   id,
			Path: p,
			Args: func(s string) []string { return []string{"-c", s} },
			Note: "自定义可执行文件",
		}, nil
	}
	return Profile{}, fmt.Errorf("shell %q 不存在（试试 wr shells 看已装列表）", id)
}

// lookup 在常见安装位置找可执行文件。Windows 上 Git 自带的 bash
// 经常不在 PATH 里（即便在，也可能是 WSL/Cygwin 的那个），所以显式探一下。
func lookup(p Profile) string {
	var names []string
	switch p.ID {
	case "sh":
		names = []string{p.ID}
	case "bash", "gitbash":
		if IsWin() {
			// 两个 profile 是同一个 bash.exe，区别只在参数：bash 用 -c 不读 rc
			// （冷启动 ~105ms），gitbash 用 -lc 走登录流程（~2.6s）。PATH 里的
			// bash.exe 未必是 Git 自带的那个，所以先探固定安装位置。
			if fp := gitBash(); fp != "" {
				return fp
			}
			names = []string{"bash.exe"}
		} else {
			names = []string{p.ID}
		}
	case "pwsh":
		names = []string{"pwsh"}
		if IsWin() {
			names = []string{"pwsh.exe"}
		}
	case "powershell":
		names = []string{"powershell.exe", "powershell"}
	case "cmd":
		names = []string{"cmd.exe", "cmd"}
	case "wsl":
		names = []string{"wsl.exe", "wsl"}
	default:
		names = []string{p.ID}
	}
	for _, n := range names {
		if fp, err := exec.LookPath(n); err == nil {
			return fp
		}
	}
	// 找不到也返回个名字，让 exec 报错时带上原始命令
	return names[0]
}

// gitBash 找 Git for Windows 自带的 bash.exe。
// go、git 这些要用的工具都在它的 MSYS2 环境里，PATH 里那些 bash.exe
// （WSL 的 System32\bash.exe、Cygwin 的、MSYS2 的）不一定带得全。
func gitBash() string {
	for _, c := range gitBashCands() {
		if fileExists(c) {
			return c
		}
	}
	for _, d := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if strings.Contains(strings.ToLower(d), "git") && fileExists(filepath.Join(d, "bash.exe")) {
			return filepath.Join(d, "bash.exe")
		}
	}
	return ""
}

// gitBashCands 是 Git for Windows 的安装位置候选。
//
// 不扫盘符也不猜 C:。Git 默认装到 %ProgramFiles%，而这个变量本身就是系统
// 给的答案（装在 D: 的机器上它就是 D:\），跟着它走比遍历 A..Z 再挨个 os.Stat
// 既准又便宜——后者在一个没装 Git 的机器上要白摸一百多次磁盘。
// 剩下几个只覆盖「装到了安装器默认位置以外」的常见情形。
func gitBashCands() []string {
	roots := []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramW6432"), os.Getenv("ProgramFiles(x86)")}
	// 环境变量缺失时的兜底（正常 Windows 上不会走到）
	if roots[0] == "" {
		roots[0] = `C:\Program Files`
	}
	if roots[2] == "" {
		roots[2] = `C:\Program Files (x86)`
	}
	var cands []string
	for _, root := range roots {
		if root == "" {
			continue
		}
		for _, bin := range []string{`bin`, `usr\bin`} {
			cands = append(cands, filepath.Join(root, `Git`, bin, `bash.exe`))
		}
	}
	// Git 安装器的 per-user 选项（免管理员权限），这类路径通常不在 PATH 里
	for _, local := range []string{os.Getenv("LOCALAPPDATA"), filepath.Join(userHome(), `AppData`, `Local`)} {
		if local != "" {
			cands = append(cands, filepath.Join(local, `Programs`, `Git`, `bin`, `bash.exe`))
		}
	}
	if home := userHome(); home != "" {
		cands = append(cands, filepath.Join(home, `scoop`, `apps`, `git`, `current`, `bin`, `bash.exe`))
	}
	// Windows 路径不分大小写，Program Files 与 ProgramW6432 常常就是同一个目录
	seen := map[string]bool{}
	var out []string
	for _, c := range cands {
		if k := strings.ToLower(c); !seen[k] {
			seen[k] = true
			out = append(out, c)
		}
	}
	return out
}

func userHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// DefaultID 选宿主上最可能存在的那个。
//
// Windows 上装过 Git 就用 bash：同一个 bash.exe，非登录式起一次 ~105ms，
// powershell 是 ~455ms，gitbash（-lc）要到 ~2.6s。省下来的不是几十毫秒，
// 是每条命令。
func DefaultID() string {
	if IsWin() {
		if gitBash() != "" {
			return "bash"
		}
		if fp, err := exec.LookPath("pwsh.exe"); err == nil && fp != "" {
			return "pwsh"
		}
		return "powershell"
	}
	if _, err := exec.LookPath("bash"); err == nil {
		return "bash"
	}
	return "sh"
}

// List 列出所有内置 profile 及其在本机的可用性。
func List() []Profile {
	ids := make([]string, 0, len(registry))
	for id := range registry {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Profile, 0, len(ids))
	for _, id := range ids {
		p := registry[id]
		p.Path = lookup(p)
		out = append(out, p)
	}
	return out
}

// Found 用 LookPath 真实性判断，而不是看路径字符串。
func Found(path string) bool {
	if path == "" {
		return false
	}
	if filepath.IsAbs(path) {
		return fileExists(path)
	}
	_, err := exec.LookPath(path)
	return err == nil
}

// Wrap 给脚本打上统一的前缀，让失败能反映到 exit code 上。
// PowerShell 的 $? 对原生命令不上报，必须自己兜 $LASTEXITCODE。
// 前缀还有一件事：把控制台的输入输出编码钉在 UTF-8，见 EncodingPrefix。
func Wrap(p Profile, script string) string {
	switch p.ID {
	case "bash", "gitbash", "wsl", "sh":
		return EncodingPrefix(p) + wrapPosix(p, script)
	case "powershell", "pwsh":
		return EncodingPrefix(p) + script + `
; $wr_ok = $?
; if ($LASTEXITCODE -ne $null -and $LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
; if (-not $wr_ok) { exit 1 }`
	default:
		return EncodingPrefix(p) + script
	}
}

func wrapPosix(p Profile, script string) string {
	if p.ID == "sh" {
		// dash（Debian/Ubuntu 的 /bin/sh）不认 pipefail，且是致命错误——加
		// 2>/dev/null 也拦不住，整条命令直接 exit 2。所以 sh 只在真支持时才试。
		return "if (set -o pipefail) 2>/dev/null; then set -o pipefail; fi; " + script
	}
	return "set -o pipefail 2>/dev/null; " + script
}

// EncodingPrefix 把 Windows 两个 shell 的控制台编码换成 UTF-8（CodePage 65001），
// 其余 shell 返回空串。
//
// 不换会怎样：中文 Windows 的管道默认是 ANSI 代码页（936 = GBK），于是
//   - powershell 5.1 的 Get-Content / Select-String 按 GBK 解码 UTF-8 文件，
//     吐出去的是 `鏃ュ織绯荤粺` 这种 mojibake；$OutputEncoding 还是 US-ASCII
//   - cmd 里 `type x.go` 同理
//
// 而 wr 这头按 UTF-8 收字节，两头一对不上，用户就会以为远端文件坏了。
// git / go 这类直接吐字节的原生命令不受影响——它们不经过 shell 的字符串管道，
// 所以只有 PowerShell 原生命令才现形。
//
// 两个取值都不能省：
//   - [Console]::OutputEncoding 决定「PowerShell 往管道写」的编码。它默认取控制台
//     代码页；只设它、不设 $OutputEncoding，非原生命令按 US-ASCII 编码，中文变 `?`。
//   - $OutputEncoding 决定「PowerShell 喂给原生命令」的编码，反方向同一个坑。
//
// 顺带把 Get-Content / Set-Content 的默认编码也钉成 UTF-8（5.1 里默认是 ANSI）：
// 用户写 Get-Content main.go 时不该再补 -Encoding UTF8。
//
// $PROFILE 已经被不加载了，这几句也不进日志：它们不是用户命令的一部分，
// 用户看到的 stdout/stderr 里不该多出它们。失败一律不管——老系统的 conhost
// 偶尔会拒绝改代码页，但把用户命令拦下来更糟，所以静默兜住。
func EncodingPrefix(p Profile) string {
	switch p.ID {
	case "powershell", "pwsh":
		return "[Console]::InputEncoding=[Console]::OutputEncoding=[Text.Encoding]::UTF8;" +
			"$OutputEncoding=[Text.Encoding]::UTF8;" +
			"$PSDefaultParameterValues['*:Encoding']='utf8';"
	case "cmd":
		return "chcp 65001>nul & "
	}
	return ""
}
