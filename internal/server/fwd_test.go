package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"cnb.cool/yyx946/tteist/internal/proto"
)

// newFwdServer 起一台带转发配置的 wr，外加一台假上游。
func newFwdServer(t *testing.T, upstream string) (*Server, *httptest.Server) {
	t.Helper()
	s, err := New(Config{
		Token: "0123456789abcdef0123", Root: t.TempDir(), Quiet: true,
		Fwd: []string{"web=" + upstream},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func TestFwdPassesPathAndQuery(t *testing.T) {
	var gotPath, gotQuery, gotAuth, gotWrToken string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 只有真正的业务请求才记：wr 还会在后台探一次上游的根，
		// 那次探活和「这次转发带了什么」无关。
		if r.URL.Path == "/api/items" {
			gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
			gotAuth, gotWrToken = r.Header.Get("Authorization"), r.Header.Get("X-Wr-Token")
		}
		_, _ = io.WriteString(w, "hello from upstream")
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	resp, err := http.Get(ts.URL + "/fwd/web/api/items?x=1&y=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 || string(body) != "hello from upstream" {
		t.Fatalf("转发失败: %d %q", resp.StatusCode, body)
	}
	if gotPath != "/api/items" {
		t.Errorf("路径没剥对: %q", gotPath)
	}
	// 转发这条路不鉴权，所以 query 里也不该被塞进 wr 的 token。
	if gotQuery != "x=1&y=2" {
		t.Errorf("query 丢了: %q", gotQuery)
	}
	if strings.Contains(gotQuery, "token") {
		t.Errorf("wr 的 token 不该漏给上游: %q", gotQuery)
	}
	// 这次请求没带凭据，所以上游看到的就是「什么都没有」：
	// wr 不替客户端编一条，也不把 wr 的口令顺手塞进 Authorization。
	if gotAuth != "" {
		t.Errorf("没带凭据的请求，上游不该收到 Authorization: %q", gotAuth)
	}
	if gotWrToken != "0123456789abcdef0123" {
		t.Errorf("上游需要同一个 token 时应能拿到: %q", gotWrToken)
	}
}

// 只摘「值等于 wr 口令」的那条凭据：上游自己的 Authorization 得活着到上游手里。
// 一刀切删名字会把上游的鉴权一起关掉，那才是真正误删。
func TestFwdKeepsUpstreamOwnAuth(t *testing.T) {
	var gotAuth, gotWrToken, gotQuery []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/items" {
			gotAuth = append(gotAuth, r.Header.Get("Authorization"))
			gotWrToken = append(gotWrToken, r.Header.Get("X-WR-Token"))
			gotQuery = append(gotQuery, r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	const wrToken = "0123456789abcdef0123"
	cases := []struct {
		name, auth, wrToken, query string
		wantAuth, wantWrToken      string
		wantQuery                  string
	}{
		{
			name: "上游自己的 Bearer 原样过去",
			auth: "Bearer upstream-secret", wantAuth: "Bearer upstream-secret",
			wantWrToken: wrToken, // wr 的口令只以 X-WR-Token 这一份出现
		},
		{
			name: "上游的 basic 原样过去",
			auth: "Basic dXA6cHc=", wantAuth: "Basic dXA6cHc=", wantWrToken: wrToken,
		},
		{
			name: "wr 的口令在 Authorization 里被摘掉",
			auth: "Bearer " + wrToken, wantAuth: "", wantWrToken: wrToken,
		},
		{
			name: "没写 scheme 的 wr 口令也被摘掉",
			auth: wrToken, wantAuth: "", wantWrToken: wrToken,
		},
		{
			name:    "X-WR-Token 是上游自己的就留着（和 wr 的同名头合并）",
			wrToken: "upstream-own", wantWrToken: wrToken,
		},
		{
			name:  "query 里上游自己的 token 保留",
			query: "token=upstream-param&x=1", wantQuery: "token=upstream-param&x=1",
			wantWrToken: wrToken,
		},
		{
			name:  "query 里 wr 的口令被摘掉",
			query: "token=" + wrToken + "&x=1", wantQuery: "x=1",
			wantWrToken: wrToken,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotAuth, gotWrToken, gotQuery = nil, nil, nil
			u := ts.URL + "/fwd/web/api/items"
			if c.query != "" {
				u += "?" + c.query
			}
			req, err := http.NewRequest("GET", u, nil)
			if err != nil {
				t.Fatal(err)
			}
			if c.auth != "" {
				req.Header.Set("Authorization", c.auth)
			}
			if c.wrToken != "" {
				req.Header.Set("X-WR-Token", c.wrToken)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if len(gotAuth) != 1 {
				t.Fatalf("上游该收到正好一次业务请求，实际 %d 次", len(gotAuth))
			}
			// X-WR-Token 的绝对值不重要（wr 会顶一份自己的），这里只区分
			// 「上游自己的值活着」和「被 wr 的顶掉了」。
			if c.wrToken == "upstream-own" {
				if gotWrToken[0] != "upstream-own" {
					t.Errorf("上游自己的 X-WR-Token 被顶掉了: %q", gotWrToken[0])
				}
			} else if gotWrToken[0] != c.wantWrToken {
				t.Errorf("X-WR-Token = %q，想要 %q", gotWrToken[0], c.wantWrToken)
			}
			if gotAuth[0] != c.wantAuth {
				t.Errorf("Authorization = %q，想要 %q", gotAuth[0], c.wantAuth)
			}
			if c.wantQuery != "" && gotQuery[0] != c.wantQuery {
				t.Errorf("query = %q，想要 %q", gotQuery[0], c.wantQuery)
			}
		})
	}
}

func TestFwdKeepsUpstreamPrefix(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.URL.Path)
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL+"/app")
	resp, err := http.Get(ts.URL + "/fwd/web/x/y?token=0123456789abcdef0123")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "/app/x/y" {
		t.Errorf("上游前缀丢了: %q", body)
	}
}

// 名字匹配不上就什么都不说：这条路径免 token，谁都能敲一次。
// 回「已配置: web」等于把转发名清单白送出去，客户端该自己用 token 走 /info 或 /fwd。
func TestFwdUnknownName(t *testing.T) {
	_, ts := newFwdServer(t, "http://127.0.0.1:1")
	for _, p := range []string{"/fwd/nope/x", "/fwds/nope/x", "/fwd/", "/fwds/"} {
		resp, err := http.Get(ts.URL + p + "?token=0123456789abcdef0123")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s 没匹配到名字该回空 200，实际 %d", p, resp.StatusCode)
		}
		if len(body) != 0 {
			t.Errorf("%s 的 body 该是空的，实际 %q", p, body)
		}
	}
}

// 免 token 的错名请求也不能漏出配置：上面那条不带 token 再验一次。
func TestFwdUnknownNameLeaksNothing(t *testing.T) {
	s, ts := newFwdServer(t, "http://127.0.0.1:1")
	resp, err := http.Get(ts.URL + "/fwd/nope/x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "已配置") || strings.Contains(string(body), "127.0.0.1:1") {
		t.Errorf("错名请求漏了转发配置：%q", body)
	}
	if len(s.fwd) == 0 {
		t.Fatal("测试前提：这次服务端该配了转发目标")
	}
}

func TestFwdUpstreamDown(t *testing.T) {
	// 端口 1 必然连不上，回 502 且正文说清是哪台上游。
	_, ts := newFwdServer(t, "http://127.0.0.1:1")
	resp, err := http.Get(ts.URL + "/fwd/web/x?token=0123456789abcdef0123")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("上游不可达该回 502，实际 %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "127.0.0.1:1") {
		t.Errorf("502 正文没指出上游地址: %s", body)
	}
}

func TestFwdList(t *testing.T) {
	s, _ := newFwdServer(t, "http://127.0.0.1:5173")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/fwd", nil)
	req.Header.Set("Authorization", "Bearer 0123456789abcdef0123")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /fwd 该回 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	var got proto.FwdResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Targets) != 1 || got.Targets[0].Name != "web" || !strings.Contains(got.Targets[0].URL, "5173") {
		t.Errorf("目标清单不对: %+v", got)
	}
}

// 转发不再校验 wr 的 token：浏览器里点开页面、HMR 的 websocket 都拿不到 header，
// 而能打到的只有 -fwd 点名过的那几台上游，不是开放代理。
func TestFwdNoToken(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "no token needed")
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	resp, err := http.Get(ts.URL + "/fwd/web/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "no token needed" {
		t.Fatalf("裸转发不该要 token: %d %q", resp.StatusCode, body)
	}
}

// 清单回显的是 wr 自己的配置，还得要 token。
func TestFwdListRequiresToken(t *testing.T) {
	s, _ := newFwdServer(t, "http://127.0.0.1:5173")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/fwd", nil))
	if rec.Code != 401 {
		t.Fatalf("GET /fwd 还是要 token，实际 %d", rec.Code)
	}
}

func TestNewFwdTargetsRejectsBadSpec(t *testing.T) {
	bad := [][]string{
		{"nourl"},              // 缺 =
		{"=http://x"},          // 缺名字
		{"web="},               // 缺 url
		{"a/b=http://x"},       // 名字里带 /
		{"web=ftp://x"},        // 非 http
		{"web=0"},              // 端口为 0
		{"web=70000"},          // 端口超界
		{"web=127.0.0.1:5173"}, // 简写只认端口，别写主机名
		{"web=localhost:5173"}, // 同上：要写主机就把 http:// 带上
		{"web=http://x", "x=http://y", "web=http://z"}, // 重名
	}
	for _, specs := range bad {
		if _, err := newFwdTargets(specs, "tk"); err == nil {
			t.Errorf("该拒绝的配置没拒: %v", specs)
		}
	}
	if _, err := newFwdTargets([]string{"web=http://127.0.0.1:5173"}, "tk"); err != nil {
		t.Errorf("合法配置被拒: %v", err)
	}
	if _, err := newFwdTargets([]string{"web=5173", "pw=8931"}, "tk"); err != nil {
		t.Errorf("简写被拒: %v", err)
	}
}

// 简写只写端口时补成 127.0.0.1 的 http：这是本地 dev server 的全部情况，
// 也是 -fwd 最常见的一行。
func TestFwdPortShortcut(t *testing.T) {
	targets, err := newFwdTargets([]string{"web=5173", " pw = 8931 "}, "tk")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"web": "http://127.0.0.1:5173", "pw": "http://127.0.0.1:8931"} {
		got, ok := targets[name]
		if !ok {
			t.Fatalf("少了目标 %s", name)
		}
		if got.base.String() != want {
			t.Errorf("%s 补全后是 %s，期望 %s", name, got.base.String(), want)
		}
	}
	// 原始写法原样留着：横幅和 /fwd 要回显用户自己写的那行。
	if targets["web"].fromSpec != "web=5173" || targets["pw"].fromSpec != "pw = 8931" {
		t.Errorf("原始写法没保住: %q / %q", targets["web"].fromSpec, targets["pw"].fromSpec)
	}
}

// 简写补全后的地址要真的能打通，并且 /fwd 回包带上原始写法。
func TestFwdPortShortcutReachesUpstream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "port-ok")
	}))
	defer up.Close()

	// 上游是 httptest 的随机端口，这里把 host 换成 127.0.0.1 后取端口当简写。
	port := up.URL[strings.LastIndexByte(up.URL, ':')+1:]
	s, err := New(Config{
		Token: "0123456789abcdef0123", Root: t.TempDir(), Quiet: true,
		Fwd: []string{"web=" + port},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/fwd/web/x?token=0123456789abcdef0123")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "port-ok" {
		t.Fatalf("简写没转发成功: %d %q", resp.StatusCode, body)
	}

	rec := httptest.NewRecorder()
	s.handleFwdList(rec, httptest.NewRequest("GET", "/fwd", nil))
	var got proto.FwdResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Targets) != 1 || got.Targets[0].Spec != "web="+port {
		t.Errorf("/fwd 该回显原始写法: %+v", got.Targets)
	}
}

func TestFwdCallJSON(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/x" || r.URL.RawQuery != "q=1" {
			t.Errorf("上游收到 %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()

	s, _ := newFwdServer(t, up.URL)
	res, err := s.fwdCall(nil, proto.FwdReq{Target: "web", Method: "POST", Path: "api/x?q=1", Body: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != 200 || res.Body != `{"ok":true}` {
		t.Fatalf("回包不对: %+v", res)
	}
	if res.Bytes != 11 {
		t.Errorf("bytes 不对: %d", res.Bytes)
	}
}

func TestFwdCallBinaryBody(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{0x89, 0x50, 0x4e, 0x47, 0xff, 0x00})
	}))
	defer up.Close()

	s, _ := newFwdServer(t, up.URL)
	res, err := s.fwdCall(nil, proto.FwdReq{Target: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Body != "" || res.BodyB64 == "" {
		t.Errorf("非 UTF-8 该走 body_b64: %+v", res)
	}
}

func TestFwdCallUnknownTarget(t *testing.T) {
	s, _ := newFwdServer(t, "http://127.0.0.1:1")
	if _, err := s.fwdCall(nil, proto.FwdReq{Target: "nope"}); err == nil {
		t.Fatal("不存在的目标该报错")
	}
}

func TestFwdCallUpstreamDown(t *testing.T) {
	s, _ := newFwdServer(t, "http://127.0.0.1:1")
	_, err := s.fwdCall(nil, proto.FwdReq{Target: "web", TimeoutMS: 2000})
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("连不上该报出上游地址: %v", err)
	}
}

// 上游是 base=/fwd/web/ 的 dev server 时（vite 就是这么干活的）：
// 转发根路径拿到的是 Location: /fwd/web/，但那不是「送回 wr 入口」——
// 相对路径的宿主是上游自己。wr 该跟着它走一遍，而不是判回环回 502。
//
// 这里照 vite 的真实语义模拟：/ 不在 base 里就 302 到 base，base 下的路径一律 200。
// 页面里引用的是带 base 的绝对路径，浏览器因此会请求 /fwd/web/src/main.js。
func TestFwdUpstreamBasePrefixIsFollowed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/fwd/web/") {
			w.Header().Set("Location", "/fwd/web/")
			w.WriteHeader(http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "html:"+r.URL.Path)
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	for _, p := range []string{"/fwd/web/", "/fwd/web/src/main.js", "/fwd/web/@vite/client"} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s 该 200，实际 %d %s", p, resp.StatusCode, body)
		}
		if resp.Header.Get("Location") != "" {
			t.Errorf("%s 已经是终态，不该还带 Location: %q", p, resp.Header.Get("Location"))
		}
	}
}

// 每跳都换到新路径上还绕回来（/ → /fwd/web/ → /）才是真死循环：那时才 502。
func TestFwdRealLoopStillReported(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Location", "/fwd/web/")
		} else {
			w.Header().Set("Location", "/")
		}
		w.WriteHeader(http.StatusFound)
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(ts.URL + "/fwd/web/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("绕回来的跳转该拦成 502，实际 %d", resp.StatusCode)
	}
	if resp.Header.Get("Location") != "" {
		t.Errorf("拦下来就不该再带 Location: %q", resp.Header.Get("Location"))
	}
	if !strings.Contains(string(body), "转发回环") || !strings.Contains(string(body), "302") {
		t.Errorf("正文要说清是上游在自跳: %s", body)
	}
}

// 上游把 /fwd/web/preview/ 当自己的挂载点：跟着走过去，别把 302 扔给浏览器。
func TestFwdEntryRedirectWithPathIsFollowed(t *testing.T) {
	var got []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path)
		if r.URL.Path == "/preview/" {
			_, _ = io.WriteString(w, "preview page")
			return
		}
		w.Header().Set("Location", "/fwd/web/preview/")
		w.WriteHeader(http.StatusFound)
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	resp, err := http.Get(ts.URL + "/fwd/web/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "preview page" {
		t.Fatalf("该跟到上游的 /preview/，实际 %d %q（上游依次收到 %v）", resp.StatusCode, body, got)
	}
}

// 指到别的网站的 302 不归 wr 管，原样透传。
func TestFwdAbsoluteForeignRedirectPassesThrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://example.com/fwd/web/")
		w.WriteHeader(http.StatusFound)
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(ts.URL + "/fwd/web/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "https://example.com/fwd/web/" {
		t.Fatalf("别人的域名 wr 不该动: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// 正常的 302（跳到上游自己的别的路径）必须原样透传——拦过头比不拦更糟。
func TestFwdNormalRedirectPassesThrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			w.Header().Set("Location", "/new")
			w.WriteHeader(http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "new page")
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(ts.URL + "/fwd/web/old")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/new" {
		t.Fatalf("上游自己的 302 不该动: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// 路径里带第二个 /fwd/ 段是普通路径：上游要的就是它，wr 只剥第一层前缀。
func TestFwdSecondFwdSegmentIsJustAPath(t *testing.T) {
	var paths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	resp, err := http.Get(ts.URL + "/fwd/web/fwd/web/?token=0123456789abcdef0123")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("带第二个 /fwd/ 段的路径该照常转发，实际 %d", resp.StatusCode)
	}
	// 上游还会收到探活的 /，所以看整体：只能剥掉一层前缀。
	if !slices.Contains(paths, "/fwd/web/") {
		t.Errorf("前缀剥错了: %v（只该剥掉一层 /fwd/web）", paths)
	}
}

// 上游对不存在的路径回 200 时，wr 探一次上游的根：根不是 200 就报 404。
func TestFwdUpstreamMissingBecomes404(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 探活是在后台发的，可能晚到；用 /__probe 这种不存在的路径也算根之外的请求。
		if r.URL.Path == "/" {
			w.Header().Set("Location", "/login")
			w.WriteHeader(http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "soft-200")
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(ts.URL + "/fwd/web/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("上游兜底的 200 该报 404，实际 %d %s", resp.StatusCode, body)
	}
}

// SPA 对任何路径都回 200 是合理行为，不能一律改 404。
func TestFwdSpaKeeps200(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<html>spa</html>")
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	resp, err := http.Get(ts.URL + "/fwd/web/deep/link?token=0123456789abcdef0123")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "spa") {
		t.Fatalf("真正对任何路径都回 200 的上游要照旧: %d %q", resp.StatusCode, body)
	}
}

// /fwd/call 不跟随重定向：agent 要看到的是「302 → 哪儿」。
func TestFwdCallDoesNotFollowRedirect(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/fwd/web/")
		w.WriteHeader(http.StatusFound)
	}))
	defer up.Close()

	s, _ := newFwdServer(t, up.URL)
	res, err := s.fwdCall(nil, proto.FwdReq{Target: "web", Path: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != http.StatusFound {
		t.Fatalf("该把 302 原样交出来，实际 %d", res.Status)
	}
	if !strings.Contains(strings.Join(res.Headers, "\n"), "Location: /fwd/web/") {
		t.Errorf("Location 得给 agent 看到: %v", res.Headers)
	}
}

// 转发根路径时告诉上游「外面看到的是什么」，它才能拼对重定向和资源地址。
func TestFwdSetsForwardedOnlyOnLanding(t *testing.T) {
	got := map[string]string{}
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 探活是在别的 goroutine 里发的，写 map 得加锁。
		// 转发根路径的那次和探活落在同一个路径上，只认带 X-Forwarded-* 的那个。
		mu.Lock()
		v := r.Header.Get("X-Forwarded-Host") + "|" +
			r.Header.Get("X-Forwarded-Proto") + "|" + r.Header.Get("X-Forwarded-Prefix")
		if v != "||" || got[r.URL.Path] == "" {
			got[r.URL.Path] = v
		}
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	for _, p := range []string{"/fwd/web/", "/fwd/web/sub/x"} {
		resp, err := http.Get(ts.URL + p + "?token=0123456789abcdef0123")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	mu.Lock()
	root, sub := got["/"], got["/sub/x"]
	mu.Unlock()
	if !strings.Contains(root, "|http|/fwd/web/") {
		t.Errorf("转发根路径该带上 X-Forwarded-*: %q", root)
	}
	if sub != "||" {
		t.Errorf("子路径的请求不该断言外部地址（断言错了会把重定向带偏）: %q", sub)
	}
}

// 大 body 必须原样到底：攒不住的时候要一路直写，不能截在缓冲区边界上。
func TestFwdLargeBodyIntact(t *testing.T) {
	body := make([]byte, 300<<10)
	for i := range body {
		body[i] = byte(i % 251)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	resp, err := http.Get(ts.URL + "/fwd/web/big?token=0123456789abcdef0123")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应体出错（长度 %d）: %v", len(got), err)
	}
	if len(got) != len(body) {
		t.Fatalf("body 被截断了: %d / %d", len(got), len(body))
	}
	for i := range body {
		if got[i] != body[i] {
			t.Fatalf("第 %d 字节不一致", i)
		}
	}
}

// 流式响应必须当场往下走，不能被攒住。
func TestFwdStreamPassesThrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: %d\n\n", i)
			fl.Flush()
		}
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	resp, err := http.Get(ts.URL + "/fwd/web/hmr?token=0123456789abcdef0123")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("流式第一块读不到: %v", err)
	}
	if !strings.HasPrefix(string(buf[:n]), "data: 0") {
		t.Errorf("流式内容不对: %q", buf[:n])
	}
}

// /fwds 与 /fwd 的唯一差别：入口段也一起给上游。
// 配了 base:'/fwds/web/' 的 dev server，页面里的绝对路径才会落回同一个前缀上。
func TestFwdsKeepsEntryPath(t *testing.T) {
	var paths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = io.WriteString(w, r.URL.Path)
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	for _, tc := range []struct{ req, want string }{
		{"/fwds/web/", "/fwds/web/"},
		{"/fwds/web/src/main.js", "/fwds/web/src/main.js"},
		{"/fwd/web/src/main.js", "/src/main.js"}, // 另一条路照旧剥前缀
	} {
		resp, err := http.Get(ts.URL + tc.req + "?token=0123456789abcdef0123")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != tc.want {
			t.Errorf("%s → 上游收到 %q，期望 %q（%d）", tc.req, body, tc.want, resp.StatusCode)
		}
	}
}

// 全透传那条路上，上游按自己的前缀就能拼出正确的静态资源地址。
func TestFwdsAbsolutePathResolvesBack(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fwds/web/src/main.js" {
			_, _ = io.WriteString(w, "console.log(1)")
			return
		}
		// 典型 SPA：首页里写死的是带 base 的绝对路径。
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<script src="/fwds/web/src/main.js"></script>`)
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	resp, err := http.Get(ts.URL + "/fwds/web/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), `src="/fwds/web/src/main.js"`) {
		t.Fatalf("页面里的绝对路径不对: %s", page)
	}
	// 浏览器照着这个 src 再敲一次 wr——必须打到上游同一段路径上。
	js, err := http.Get(ts.URL + "/fwds/web/src/main.js")
	if err != nil {
		t.Fatal(err)
	}
	defer js.Body.Close()
	body, _ := io.ReadAll(js.Body)
	if js.StatusCode != 200 || string(body) != "console.log(1)" {
		t.Fatalf("绝对路径没回到同一个上游路径: %d %q", js.StatusCode, body)
	}
}

// 全透传路上游要跳到自己的子路径：跟着走，别把 302 扔给浏览器。
func TestFwdsFollowsUpstreamRedirect(t *testing.T) {
	var got []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path)
		if r.URL.Path == "/fwds/web/login" {
			_, _ = io.WriteString(w, "login page")
			return
		}
		w.Header().Set("Location", "/fwds/web/login")
		w.WriteHeader(http.StatusFound)
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	resp, err := http.Get(ts.URL + "/fwds/web/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "login page" {
		t.Fatalf("该跟到上游的 /fwds/web/login，实际 %d %q（上游依次收到 %v）", resp.StatusCode, body, got)
	}
}

// 全透传那条路上，上游说「去我的根」就是它自己的根，原样透传；
// 只有它自己把自己指回同一段（/fwds/web/ → /fwds/web/）才是环。
func TestFwdsLoopReported(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/fwds/web/")
		w.WriteHeader(http.StatusFound)
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(ts.URL + "/fwds/web/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("自己指回自己的入口该拦成 502，实际 %d %s", resp.StatusCode, body)
	}
}

// 清单要把两条路都写出来：agent 只有看得见 /fwds，才会用它。
func TestFwdListShowsBothEntries(t *testing.T) {
	s, _ := newFwdServer(t, "http://127.0.0.1:5173")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/fwd", nil)
	req.Header.Set("Authorization", "Bearer 0123456789abcdef0123")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /fwd 该回 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	var got proto.FwdResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Forward != "/fwd/<name>/" || got.ForwardAll != "/fwds/<name>/" {
		t.Errorf("两个入口都要给出来: %+v", got)
	}
	if len(got.Targets) != 1 || !strings.Contains(got.Targets[0].Path, "/fwds/web/") {
		t.Errorf("目标的路径规则里要提到 /fwds: %+v", got.Targets)
	}
}

// /fwds 也是转发入口，同样不校验 token（浏览器里点开页面时没有 header 可带）。
func TestFwdsNoToken(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "no token needed")
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	resp, err := http.Get(ts.URL + "/fwds/web/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "no token needed" {
		t.Fatalf("/fwds 也不该要 token: %d %q", resp.StatusCode, body)
	}
}

// 两条入口的探活结论不能互相串：上游只认 /fwds/web/ 这个根、对自己真正的 / 回 404 时，
// /fwds/web/ 的 200 是它自己认下的页面，不是兜底——不能被剥前缀那条路的「根 404」判成 404。
//
// 反过来也一样：先看 /fwds（根 404）、再走 /fwd（根 200 的 SPA）时，
// 剥前缀那条路的 200 该照旧放行。
func TestFwdProbeIsPerEntry(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/fwds/web/":
			_, _ = io.WriteString(w, "mounted page")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer up.Close()

	_, ts := newFwdServer(t, up.URL)
	// 先让剥前缀那条路把「上游的根是 404」记进它自己的缓存。
	if resp, err := http.Get(ts.URL + "/fwd/web/"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	resp, err := http.Get(ts.URL + "/fwds/web/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "mounted page" {
		mu.Lock()
		seen := append([]string(nil), paths...)
		mu.Unlock()
		t.Fatalf("/fwds 的 200 不该被 /fwd 的探活结论改判: %d %q（上游依次收到 %v）", resp.StatusCode, body, seen)
	}

	// 反方向：/fwds 那条路的根已经 404 了，/fwd 侧「任何路径都回 200」的结论照旧有效。
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fwds/web/" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, "<html>spa</html>")
	}))
	defer up2.Close()

	_, ts2 := newFwdServer(t, up2.URL)
	if resp, err := http.Get(ts2.URL + "/fwds/web/"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	spa, err := http.Get(ts2.URL + "/fwd/web/deep/link")
	if err != nil {
		t.Fatal(err)
	}
	defer spa.Body.Close()
	sbody, _ := io.ReadAll(spa.Body)
	if spa.StatusCode != 200 || !strings.Contains(string(sbody), "spa") {
		t.Fatalf("对任何路径都回 200 的上游要照旧放行: %d %q", spa.StatusCode, sbody)
	}
}

// 清单里的路径示例要照着念就能用：全透传那条给的是上游真的会收到的 /fwds/<name>/x。
func TestFwdListPathExampleIsUpstreamView(t *testing.T) {
	s, _ := newFwdServer(t, "http://127.0.0.1:5173")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/fwd", nil)
	req.Header.Set("Authorization", "Bearer 0123456789abcdef0123")
	s.Handler().ServeHTTP(rec, req)
	var got proto.FwdResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Targets) != 1 || !strings.Contains(got.Targets[0].Path, "→ 上游 /fwds/web/x") {
		t.Fatalf("全透传那条路的示例要是上游视角的 /fwds/web/x: %+v", got.Targets)
	}
}
