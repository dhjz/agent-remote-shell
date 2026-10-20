package shellx

import (
	"path/filepath"
	"strings"
	"testing"
)

// 中文 Windows 上的 mojibake 是这套东西存在的唯一理由：
// powershell / cmd 起手不把代码页钉成 UTF-8，Get-Content 出来的中文就是
// `鏃ュ織绯荤粺`。这里守两件事——该加的加上，不该加的别碰。
func TestEncodingPrefixOnlyTouchesWindowsShells(t *testing.T) {
	for _, id := range []string{"sh", "bash", "gitbash", "wsl"} {
		if got := EncodingPrefix(Profile{ID: id}); got != "" {
			t.Errorf("%s 不该有编码前缀（Unix 侧本来就 UTF-8），却给了 %q", id, got)
		}
	}
	for _, id := range []string{"powershell", "pwsh"} {
		got := EncodingPrefix(Profile{ID: id})
		if !strings.Contains(got, "[Console]::OutputEncoding=[Text.Encoding]::UTF8") {
			t.Errorf("%s 的前缀没钉控制台输出编码：%q", id, got)
		}
		if !strings.Contains(got, "$OutputEncoding=[Text.Encoding]::UTF8") {
			t.Errorf("%s 的前缀没钉 $OutputEncoding（不设的话非原生命令按 US-ASCII 吐，中文变 ?）：%q", id, got)
		}
	}
	if got := EncodingPrefix(Profile{ID: "cmd"}); !strings.HasPrefix(got, "chcp 65001") {
		t.Errorf("cmd 的前缀应该是 chcp 65001，实际 %q", got)
	}
}

// Wrap 的其它承诺不能因为多了一个前缀而破掉：exit code 兜底和 pipefail 都还在。
func TestWrapKeepsExitCodeHandling(t *testing.T) {
	script := "go test ./..."
	if got := Wrap(Profile{ID: "pwsh"}, script); !strings.HasPrefix(got, EncodingPrefix(Profile{ID: "pwsh"})) ||
		!strings.Contains(got, "exit $LASTEXITCODE") || !strings.Contains(got, script) {
		t.Errorf("pwsh 的 Wrap 丢了东西：%q", got)
	}
	if got := Wrap(Profile{ID: "bash"}, script); !strings.Contains(got, "set -o pipefail") ||
		!strings.HasSuffix(got, script) {
		t.Errorf("bash 的 Wrap 丢了东西：%q", got)
	}
	if got := Wrap(Profile{ID: "sh"}, script); !strings.Contains(got, "if (set -o pipefail)") {
		t.Errorf("sh 的 Wrap 丢了 pipefail 探测：%q", got)
	}
	if got := Wrap(Profile{ID: "cmd"}, script); got != EncodingPrefix(Profile{ID: "cmd"})+script {
		t.Errorf("cmd 的 Wrap 应该只有编码前缀：%q", got)
	}
}

// Windows 上的默认 shell 要挑 -c 的那个，不是 -lc 的。
// 同一个 bash.exe：bash 起一次 ~105ms，gitbash 要 ~2.6s（登录式，source rc），
// 而 powershell 是 ~455ms。默认值选错，代价落在每一条命令上。
func TestGitBashArgsStayNonLogin(t *testing.T) {
	git, ok := registry["gitbash"]
	if !ok {
		t.Fatal("gitbash profile 不见了")
	}
	if got := git.Args("true"); len(got) != 2 || got[0] != "-lc" {
		t.Errorf("gitbash 应该走 -lc（它存在的意义就是登录式 : %v", got)
	}
	bash := registry["bash"]
	got := bash.Args("true")
	for _, a := range got {
		if a == "-l" || a == "-lc" || a == "--login" {
			t.Errorf("bash 不该带登录参数，那会把启动成本拉到 gitbash 那档: %v", got)
		}
	}
	if len(got) == 0 || got[len(got)-1] != "true" {
		t.Errorf("bash 的脚本应该是最后一个参数: %v", got)
	}
}

// 装到哪块盘都跟得上：路径以 %ProgramFiles% 打头，不写死 C:，也不扫盘符。
//
// 断言用 filepath.Join 拼期望值，不硬编码 `\` 与 `/`：这条测试在 Linux 的 CI 上
// 也会跑，那里 filepath.Join 用 `/` 分隔，钉死分隔符会让这条测试只在 Windows 绿。
func TestGitBashCandsFollowProgramFiles(t *testing.T) {
	// Windows 环境变量在这台机器上没有，直接造一个装到 D: 的场景
	pf := `D:\Program Files`
	pf86 := `D:\Program Files (x86)`
	lad := `D:\Users\me\AppData\Local`
	t.Setenv("ProgramFiles", pf)
	t.Setenv("ProgramFiles(x86)", pf86)
	t.Setenv("ProgramW6432", pf)
	t.Setenv("LOCALAPPDATA", lad)

	cands := gitBashCands()
	for _, want := range []string{
		filepath.Join(pf, `Git`, `bin`, `bash.exe`),
		filepath.Join(pf, `Git`, `usr\bin`, `bash.exe`),
		filepath.Join(pf86, `Git`, `bin`, `bash.exe`),
		filepath.Join(lad, `Programs`, `Git`, `bin`, `bash.exe`),
	} {
		hit := false
		for _, c := range cands {
			if c == want {
				hit = true
				break
			}
		}
		if !hit {
			t.Errorf("候选里没有 %s，实际：%s", want, strings.Join(cands, "\n"))
		}
	}
	// 别再靠猜盘符：扫 A..Z 会白摸一百多次磁盘，没有益处
	for _, c := range cands {
		if strings.HasPrefix(c, `C:`) {
			t.Errorf("装到 D: 的机器上不该还有 C: 候选：%s", c)
		}
	}
	if strings.Contains(strings.Join(cands, "\n"), `bash.exe\bash.exe`) {
		t.Errorf("路径拼错了：%s", strings.Join(cands, "\n"))
	}
}

// Program Files 与 ProgramW6432 常指同一目录，候选不该重复
func TestGitBashCandsDedup(t *testing.T) {
	t.Setenv("ProgramFiles", `C:\Program Files`)
	t.Setenv("ProgramW6432", `C:\Program Files`)
	seen := map[string]bool{}
	for _, c := range gitBashCands() {
		k := strings.ToLower(c)
		if seen[k] {
			t.Errorf("候选重复：%s", c)
		}
		seen[k] = true
	}
}
