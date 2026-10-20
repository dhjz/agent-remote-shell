package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// post 打一次真实 handler：这几个用例要验的正是 HTTP 层的行为，
// 直接调 s.Grep() 会跳过解码，等于没测。
func post(t *testing.T, s *Server, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return rec.Code, e.Error
}

// postBody 和 post 打同一层 handler，但把整个响应体交出来：
// 有的用例要看返回结构，不只看 error。
func postBody(t *testing.T, s *Server, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// 拼错字段名过去是静默降级：返回一份看起来正常的空结果，
// 调用方会得出「这仓库里没这个词」的错误结论。现在必须 400 并指名候选。
func TestUnknownFieldIsRejected(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, "a.go", "PLAYER = 1\n")

	cases := []struct{ path, body, wantHint string }{
		{"/grep", `{"pattern":"player","ignorecase":true}`, "ignore_case"},
		{"/list", `{"dir":".","limt":10}`, "limit"},
		{"/exec", `{"cmd":"echo hi","path":"src"}`, "（字段名见 /api/openapi.json）"},
	}
	for _, c := range cases {
		code, msg := post(t, s, c.path, c.body)
		if code != 400 {
			t.Fatalf("%s %s: code=%d msg=%s，未知字段必须 400", c.path, c.body, code, msg)
		}
		if !strings.Contains(msg, "未知字段") || !strings.Contains(msg, c.wantHint) {
			t.Fatalf("%s %s: msg=%q 应含 %q", c.path, c.body, msg, c.wantHint)
		}
	}

	// 正确写法仍然能搜到，说明 400 是因为字段名，不是因为搜索本身。
	code, msg := post(t, s, "/grep", `{"pattern":"player","ignore_case":true}`)
	if code != 200 {
		t.Fatalf("正确字段名应通过：%d %s", code, msg)
	}
}

// required 字段缺失过去返回「什么都没发生」的 200，
// sync 场景下客户端会误以为「已经同步完了」，这是最贵的误判。
func TestMissingRequiredIsRejected(t *testing.T) {
	s := newTestServer(t)

	cases := []struct{ path, body, wantField string }{
		{"/list", `{}`, `"dir"`},
		{"/sync/plan", `{"dir":"tmp_probe"}`, `"want"`},
		{"/exec", `{"dir":"src"}`, `"cmd"`},
		{"/grep", `{}`, `"pattern"`},
		{"/read", `{"path":""}`, `"path"`},
	}
	for _, c := range cases {
		code, msg := post(t, s, c.path, c.body)
		if code != 400 {
			t.Fatalf("%s %s: code=%d msg=%s，缺必填必须 400", c.path, c.body, code, msg)
		}
		if !strings.Contains(msg, "缺少必填字段") || !strings.Contains(msg, c.wantField) {
			t.Fatalf("%s %s: msg=%q 应指出 %s", c.path, c.body, msg, c.wantField)
		}
	}
}

// 可选字段缺失不能被误判成必填：overwrite / respect_ignore 是指针，
// 缺省有各自语义，不能因为「没写」就拒。
func TestOptionalFieldsStillAccepted(t *testing.T) {
	s := newTestServer(t)
	for _, c := range []struct{ path, body string }{
		{"/list", `{"dir":"."}`},
		{"/glob", `{"pattern":"*.go"}`},
		{"/grep", `{"pattern":"x","glob":"*.go","respect_ignore":false}`},
		{"/exec", `{"cmd":"echo hi"}`},
		{"/sync/plan", `{"dir":".","want":[]}`},
	} {
		if code, msg := post(t, s, c.path, c.body); code != 200 {
			t.Fatalf("%s %s: code=%d msg=%s", c.path, c.body, code, msg)
		}
	}
}

// openapi.json 的 required 必须和 decode 的校验同源：文档说必填，服务端就得拦。
func TestOpenAPIRequiredMatchesValidation(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	// 文档要 token 了：测试照旧按「有 token 的调用方」拿。
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/openapi.json?token=0123456789abcdef0123", nil))

	var doc struct {
		Paths map[string]map[string]struct {
			RequestBody struct {
				Content map[string]struct {
					Schema map[string]any `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Required []string `json:"required"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}

	// $ref 到具体定义，取它的 required 列表。
	requiredOf := func(path string) []string {
		body := doc.Paths[path]["post"].RequestBody.Content["application/json"].Schema
		ref, _ := body["$ref"].(string)
		name := ref[strings.LastIndex(ref, "/")+1:]
		return doc.Components.Schemas[name].Required
	}

	if got := strings.Join(requiredOf("/list"), ","); got != "dir" {
		t.Fatalf("/list required=%q，期望 dir", got)
	}
	if got := strings.Join(requiredOf("/sync/plan"), ","); got != "dir,want" {
		t.Fatalf("/sync/plan required=%q，期望 dir,want", got)
	}
	if got := requiredOf("/glob"); len(got) != 1 || got[0] != "pattern" {
		t.Fatalf("/glob required=%v，期望只有 pattern", got)
	}
}

// /list 的字段名是 dir，不是 path 也不是 folder。文档（openapi.md、PROMPT.md）
// 写错过一次，agent 照着写就吃 400——所以在这里钉死，改字段名得先改这条断言。
func TestListFieldNameIsDir(t *testing.T) {
	s := newTestServer(t)

	// 提示语要给得出来：字段名错了，服务端得指向 openapi.json，而不是干说一句「参数非法」。
	if code, msg := post(t, s, "/list", `{"path":"."}`); code != 400 ||
		!strings.Contains(msg, "/api/openapi.json") {
		t.Fatalf("用 path 调 /list 应该 400 并指路 openapi.json，实际 code=%d msg=%s", code, msg)
	}

	// dir 能用，返回体自己也用 dir（entries 里的 dir 是「是不是目录」，另一个东西）。
	code, body := postBody(t, s, "/list", `{"dir":".","limit":5}`)
	if code != 200 {
		t.Fatalf("dir 调 /list 应该 200，实际 code=%d body=%s", code, body)
	}
	var resp struct {
		Dir     string `json:"dir"`
		Entries []struct {
			Name string `json:"name"`
			Dir  bool   `json:"dir"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("解析 /list 响应失败: %v (%s)", err, body)
	}
	if resp.Dir != "." {
		t.Fatalf("/list 回显的 dir=%q，期望 .", resp.Dir)
	}
}

// auditRemote 认 X-Forwarded-For 的最左一项，没有就回落到对端地址。
func TestAuditRemoteUsesForwardedFor(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/exec", nil)
	r.RemoteAddr = "10.9.9.1:1234"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.9.9.1")
	if got := auditRemote(r); got != "1.2.3.4" {
		t.Fatalf("auditRemote = %q, want 1.2.3.4", got)
	}
	r.Header.Del("X-Forwarded-For")
	if got := auditRemote(r); got != "10.9.9.1" {
		t.Fatalf("auditRemote = %q, want 10.9.9.1", got)
	}
}
