package server

import (
	"strings"
	"testing"

	"cnb.cool/yyx946/tteist/internal/shellx"
)

// 启动横幅是给人看的，但它算错了会把人带沟里：
// 地址里少个 token、端口对不上、把没装的 shell 标成能用——都值得测。
func TestBannerHasTokenizedURLs(t *testing.T) {
	out := banner("0.0.0.0:7573", "s3cr3t", "/tmp/repo", "")
	if n := strings.Count(out, "/api/openapi.json?token=s3cr3t"); n == 0 {
		t.Fatalf("横幅里没有带 token 的接口清单地址:\n%s", out)
	}
	if !strings.Contains(out, "/api?token=s3cr3t") {
		t.Fatalf("横幅里没有带 token 的入口页地址:\n%s", out)
	}
	if !strings.Contains(out, "http://127.0.0.1:7573") {
		t.Fatalf("横幅该给本机回环地址:\n%s", out)
	}
}

// 只打印 127.0.0.1：网卡枚举会把一排 IP 塞进控制台，用户还得自己挑一条，
// 真正的用法是从本机点开或者自己替换成远端 IP。
func TestBannerOnlyLoopback(t *testing.T) {
	out := banner("0.0.0.0:7573", "tk", "/tmp/repo", "")
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "http://") {
			continue
		}
		if !strings.Contains(line, "http://127.0.0.1:7573") {
			t.Errorf("横幅里的地址只该是 127.0.0.1，这条不是: %s", line)
		}
	}
	if strings.Contains(out, "另一张网卡") {
		t.Errorf("横幅不该再列其它网卡:\n%s", out)
	}
}

// token 里的特殊字符必须转义：base64 常见 + 和 /，不转义会被服务端当空格。
func TestBannerEscapesToken(t *testing.T) {
	out := banner("127.0.0.1:7573", "a+b/c=", "/tmp/repo", "")
	if !strings.Contains(out, "token=a%2Bb%2Fc%3D") {
		t.Fatalf("token 没有转义:\n%s", out)
	}
}

// 显式绑了别的网卡也照样从本机连得通，端口得跟着走。
func TestBannerKeepsPort(t *testing.T) {
	out := banner("192.168.1.7:8080", "tk", "/tmp/repo", "")
	if !strings.Contains(out, "http://127.0.0.1:8080/api/openapi.json?token=tk") {
		t.Fatalf("横幅没按监听端口回显回环地址:\n%s", out)
	}
	// 解析不了的地址也别把两行整个吞掉，退回默认端口至少给条能改的。
	out = banner("oops", "tk", "/tmp/repo", "")
	if !strings.Contains(out, "http://127.0.0.1:7573/api?token=tk") {
		t.Fatalf("地址解析失败时该退回默认端口:\n%s", out)
	}
}

func TestBannerShellsMarkedByAttachment(t *testing.T) {
	out := banner("127.0.0.1:7573", "abcdef", "/tmp/repo", "")
	if !strings.Contains(out, "可用终端") {
		t.Fatalf("横幅没列可用终端:\n%s", out)
	}
	// 每个内置 shell 都得有一行，否则等于「没告诉你还能用哪个」。
	for _, id := range []string{"pwsh", "gitbash", "cmd", "wsl", "bash"} {
		if !strings.Contains(out, " "+id+" ") {
			t.Errorf("横幅漏了 shell %s:\n%s", id, out)
		}
	}
}

// 横幅里「默认」这个标签得跟着 DefaultID 走，不能钉死在某一行上：
// Windows 上装过 Git 就是 bash（~105ms），不是 pwsh（~455ms），更不是 gitbash（~2.6s）。
func TestBannerDefaultFollowsDefaultID(t *testing.T) {
	out := banner("127.0.0.1:7573", "tk", "/tmp/repo", "")
	def := shellx.DefaultID()
	hit := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "；默认") {
			hit++
			if !strings.Contains(line, " "+def+" ") {
				t.Errorf("「默认」标在了 %q 上，但 DefaultID 是 %s", line, def)
			}
		}
	}
	if hit != 1 {
		t.Errorf("「默认」应该只出现一次，实际 %d 次:\n%s", hit, out)
	}
}
