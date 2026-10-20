package server

import (
	"os"
	"strings"
	"testing"
)

// PROMPT.md 是给三方 agent 的唯一说明书：它没写的东西，等于这个功能不存在
// （/fwd 就是这么漏掉的——README 完整、openapi 完整，agent 一个都不知道）。
// 所以这里守两条：端点清单不能漏，提示词里也没有写给读者看的话。

func readPrompt(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../PROMPT.md")
	if err != nil {
		t.Skipf("读不到 PROMPT.md: %v", err)
	}
	return string(data)
}

// 从 "POST /exec" 里取 /exec，从 "ANY /fwd/<name>/..." 里取 /fwd。
func endpointPath(entry string) string {
	f := strings.Fields(entry)
	if len(f) != 2 {
		return ""
	}
	p := f[1]
	if i := strings.IndexByte(p, '<'); i > 0 {
		p = p[:i] // /fwd/<name>/... → /fwd/
	}
	return strings.TrimRight(p, "/")
}

func TestPromptCoversEveryEndpoint(t *testing.T) {
	prompt := readPrompt(t)
	for _, e := range endpointList() {
		p := endpointPath(e)
		// /healthz 之类在「遇到问题」里是 curl $WR_URL/healthz，所以只认路径片段，
		// 不要求写成 /healthz 的完整端点表格。
		if p == "" || !strings.Contains(prompt, p) {
			t.Errorf("%s 挂在服务端，但 PROMPT.md 里没提 %s：agent 不会用一个没听说过的端点", e, p)
		}
	}
}

func TestPromptHasNoMetaTalk(t *testing.T) {
	prompt := readPrompt(t)
	for _, bad := range []string{"贴进", "投喂", "agent 系统提示", "整段"} {
		if strings.Contains(prompt, bad) {
			t.Errorf("PROMPT.md 出现了写给读者的话「%s」，那是 README 的事，不是提示词", bad)
		}
	}
}
