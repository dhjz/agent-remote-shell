package server

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"cnb.cool/yyx946/tteist/internal/shellx"
)

// 自描述端点挂在哪，横幅就指哪：写死两份路径迟早对不上。
const (
	apiDocPath   = "/api/openapi.json"
	apiIndexPath = "/api"
)

// banner 是 wr start 的输出：两条能直接点开/粘贴的地址，加上本机到底能用哪些终端。
// 单独抽出来是为了不依赖 net.Listen 也能测——端口算得对不对，测一下就知道。
//
// 为什么强调地址带 ?token=：这工具的客户端可能是人的浏览器，也可能是只会拼 URL 的
// 命令行。给一条完整地址，就等于把「token 怎么带」这个坑填掉了。
//
// 地址只打印 127.0.0.1：机器有几张网卡、对外 IP 是哪个，服务端自己猜不准，
// 猜出来还得挑一个当「主地址」，提示里塞一排 IP 只是噪音。要连远端就自己填。
func banner(addr, token, root, git string, fwd ...fwdTarget) string {
	return bannerWith(addr, token, root, git, "", fwd...)
}

// bannerWith 多一个审计日志落点。单独抽一层是因为落点是可选的：
// 关掉日志时那一行根本不该出现，而不是打一行「（无）」占位。
func bannerWith(addr, token, root, git, auditLog string, fwd ...fwdTarget) string {
	var b strings.Builder
	fmt.Fprintf(&b, "wr %s  沙箱目录 %s\n", Version, root)

	port := portOf(addr)
	fmt.Fprintf(&b, "  日志页面  %s\n", bannerLogURL(localHost(port), token))
	fmt.Fprintf(&b, "  接口清单  %s\n", withToken("http", localHost(port), apiDocPath, token))
	fmt.Fprintf(&b, "  接口目录  %s\n", withToken("http", localHost(port), apiIndexPath, token))

	if auditLog == "" {
		b.WriteString("审计日志 （未开启，加 -log 打开）\n")
	} else {
		fmt.Fprintf(&b, "审计日志 %s\n", auditLog)
	}

	if git == "" {
		b.WriteString("git 仓库 （未找到，exec 里的 git 会报 not a repository；用 -git-dir 指定）\n")
	} else {
		fmt.Fprintf(&b, "git 仓库 %s\n", git)
	}
	if len(fwd) > 0 {
		// 转发目标是「这台 wr 背后还有什么」，agent 起手 /info 之前就该看见。
		b.WriteString("转发目标（/fwd/<name>/... 剥前缀，/fwds/<name>/... 原样）：\n")
		for _, t := range fwd {
			// 简写（name=5173）补全后的地址一并给出：省得人去猜 127.0.0.1 从哪来的。
			if t.fromSpec != t.name+"="+t.base.Host {
				fmt.Fprintf(&b, "  %-11s → %s  (%s)\n", t.name, t.base.String(), t.fromSpec)
				continue
			}
			fmt.Fprintf(&b, "  %-11s → %s\n", t.name, t.base.String())
		}
	}
	b.WriteString(shellLines())
	b.WriteString("token 也可走 header：Authorization: Bearer <token> 或 X-WR-Token\n")
	return b.String()
}

// shellLines 列本机能用的终端：能用 / 不能用 / 推荐哪个。
// wr 的价值一半在「在远端跑命令」，选错 shell 是最常见的第一个坑（cmd 里没有 grep）。
func shellLines() string {
	def := shellx.DefaultID()
	var b strings.Builder
	b.WriteString("可用终端（-shell 取值）：\n")
	for _, p := range shellx.List() {
		mark, tag := "✗", "未安装"
		if shellx.Found(p.Path) {
			mark, tag = "✓", p.Path
		}
		note := p.Note
		if p.ID == def {
			note += "；默认"
		}
		fmt.Fprintf(&b, "  %s %-11s %-28s %s\n", mark, p.ID, shorten(tag, 28), note)
	}
	return b.String()
}

// shorten 按显示宽度粗剪：路径太长会把后面那列挤到屏幕外。
func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// portOf 从监听地址里拿端口。地址里没端口（或压根解析不了）时退回默认端口，
// 宁可给一条可能不对的地址，也别让横幅把这两行整个吞掉。
func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return port
	}
	return "7573"
}

// localHost 拼回环地址。绑 0.0.0.0 / :: 时本机照样能连 127.0.0.1，
// 显式绑了某张网卡也照样能从本机连——横幅只服务「在这台机器上点一下」。
func localHost(port string) string {
	return net.JoinHostPort("127.0.0.1", port)
}

// withToken 拼一条可直接用的地址。token 走 Escape：万一用户传了 base64 里带 +/ 的串，
// 不转义就会被当成空格，鉴权静默失败。
func withToken(scheme, hostport, path, token string) string {
	u := url.URL{Scheme: scheme, Host: hostport, Path: path}
	q := url.Values{}
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String()
}
