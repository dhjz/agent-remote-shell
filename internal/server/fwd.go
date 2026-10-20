package server

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"cnb.cool/yyx946/tteist/internal/proto"
)

// 转发的两个固定入口。做成路径前缀而不是「一个端口一种服务」，是为了让 agent
// 始终只认 WR_URL 一个地址：页面、HMR 的 websocket、Playwright 的 HTTP 服务，
// 全部挂在同一个 origin 下面。
//
// 差别只有一处：入口段要不要跟着请求一起去上游。
//
//	/fwd/web/x   → 上游 /x        （剥掉入口段：上游当自己的根跑，base:'/'）
//	/fwds/web/x  → 上游 /fwds/web/x（全透传：上游就挂在 /fwds/web/ 上跑，
//	                               base:'/fwds/web/'，页面的绝对路径自己就对得上）
//
// 两种都要，因为上游有两种活法，而 wr 不该替它猜：SPA 用相对路径时剥前缀最省事，
// 用绝对路径（vite 的 base、Next 的 assetPrefix）时只有把前缀一起给它才不会 404。
const (
	fwdPrefix    = "/fwd/"
	fwdAllPrefix = "/fwds/"
)

// fwdUpHeader / fwdNameHeader 带着「这次请求在上游那边该走哪条路径」和「走的是哪个转发名」。
// Up 是上游视角的完整路径（已经按入口段剥过或没剥），Rewrite 只管照着写。
//
// 这两个只有 wr 自己算得准：跟着上游的 Location 换路时，r.In.URL.Path 已经不是
// 客户端那条路径了（剥过没有分不清）。所以在 fwdTrip 里定死，Rewrite 只照着写。
// 都是内部头，Rewrite 里第一件事就是删掉，不漏给上游。
const (
	fwdUpHeader   = "X-Wr-Fwd-Path"
	fwdNameHeader = "X-Wr-Fwd-Name"
)

// fwdTarget 一台被转发的上游。
//
// 只支持 http/https：更多协议（比如 CDP 的裸 TCP）要另一套东西，
// 而 90% 的场景——vite dev server、playwright-mcp 的 HTTP 模式——都只是 HTTP。
type fwdTarget struct {
	name string
	base *url.URL
	rp   *httputil.ReverseProxy
	tr   *http.Transport
	// land 是「转发根路径」时上游收到的那个 URL，提示里要原样报出来。
	land *url.URL
	// probing 是给「上游对不存在的路径回 200」兜底用的探活 client + 缓存。
	// 用指针：横幅和 /fwd 会按值把 fwdTarget 传下去，带着锁的结构体不该被复制。
	//
	// 两条入口的「根」在上游那边是不同的路径（/、/fwds/<name>/），结论文档里
	// 各算各的：一条路上游只对自己的根回 200 时，不能拿它的结论去判另一条路的 200。
	probing *fwdProbe
	// fromSpec 是启动参数里的原始写法（name=…），横幅和 /fwd 都回显它。
	// 简写成 name=5173 时 base 里存的是 127.0.0.1:5173，光看 base 不知道原本怎么写的。
	fromSpec string
	// bare 是「剥掉入口段」的那条路上游视角的根：上游的挂载路径，规范化成 "/"。
	// 全透传那条路的根是入口本身（/fwds/<name>/），不需要缓存。
	bare string
	// all 是全透传那条路的探活缓存，根是 allRoot（上游视角的 /fwds/<name>/）。
	// 和 probing 分开：两条路的根不是同一段路径，结论不能互相借用。
	all     *fwdProbe
	allRoot string
}

// newFwdTargets 在启动时把名字解析一次。
//
// 目标清单只能来自服务端启动参数（-fwd name=端口 / name=url），不能来自请求体：
// wr 带 token 就能跑任意命令，再让它能自由指定 URL 的话，
// 拿到 token 的人等于拿到一台内网开放代理。
func newFwdTargets(specs []string, token string) (map[string]*fwdTarget, error) {
	// 透传和 /fwd/call 共用一份 transport：同一台上游的连接池不必开两份。
	tr := &http.Transport{
		Proxy:               nil,
		MaxIdleConns:        32,
		IdleConnTimeout:     30 * time.Second,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	out := map[string]*fwdTarget{}
	for _, spec := range specs {
		name, u, from, err := parseFwdSpec(spec)
		if err != nil {
			return nil, err
		}
		if _, dup := out[name]; dup {
			return nil, errors.New("转发名重复：" + name)
		}
		probe := &http.Client{
			Transport:     tr,
			Timeout:       3 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		out[name] = &fwdTarget{
			name: name, base: u, tr: tr, fromSpec: from, bare: mountOf(u),
			land: &url.URL{Scheme: u.Scheme, Host: u.Host, Path: mountOf(u)},
			rp:   newReverseProxy(u, name, token, tr, u.Path),
			// 两条路各一份结论：同一个上游，剥前缀看的是 /，全透传看的是 /fwds/<name>/。
			probing: &fwdProbe{client: probe},
			all:     &fwdProbe{client: probe},
			allRoot: u.Scheme + "://" + u.Host + fwdAllPrefix + name + "/",
		}
	}
	return out, nil
}

// parseFwdSpec 解析一条 -fwd。
//
// 两种写法：
//
//	name=5173                   简写，本机 127.0.0.1:5173，http
//	name=http://10.0.0.9:8931   完整 URL（带子路径、https 都得用这个）
//
// 分支按「整个 target 是不是纯数字端口」切，而不是先 url.Parse：
// url.Parse("5173") 会把 5173 当成 scheme 收下，两件事容易混。简写只认端口，
// name=host:port 这种半截地址一律报错——它太容易被读成「支持任意上游」，
// 而这层转发恰恰不该让人以为它能连外网。
func parseFwdSpec(spec string) (name string, u *url.URL, from string, err error) {
	raw := strings.TrimSpace(spec)
	name, target, ok := strings.Cut(raw, "=")
	name, target, from = strings.TrimSpace(name), strings.TrimSpace(target), raw
	if !ok || name == "" || target == "" {
		return "", nil, raw, errors.New("-fwd 需要 name=url 或 name=端口 形式，收到: " + spec)
	}
	if strings.ContainsAny(name, "/?#") {
		return "", nil, raw, errors.New("转发名不能带 / ? #：" + name)
	}
	if n, err := strconv.Atoi(target); err == nil {
		if n < 1 || n > 65535 {
			return "", nil, raw, errors.New("-fwd " + name + " 的端口超出 1-65535：" + target)
		}
		return name, &url.URL{Scheme: "http", Host: "127.0.0.1:" + target}, from, nil
	}
	if strings.Contains(target, "://") {
		u, err = url.Parse(target)
		if err != nil {
			return "", nil, raw, errors.New("-fwd " + name + " 的 url 解析失败: " + err.Error())
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return "", nil, raw, errors.New("-fwd " + name + " 只支持 http/https，收到: " + u.Scheme)
		}
		if u.Host == "" {
			return "", nil, raw, errors.New("-fwd " + name + " 缺少主机名: " + target)
		}
		return name, u, from, nil
	}
	return "", nil, raw, errors.New("-fwd " + name + " 只认端口或 http(s) URL，收到: " + target +
		"（简写只支持端口，别写主机名）")
}

// newReverseProxy 建一条转发。mount 是上游自己的挂载路径（URL 的 path 部分），
// 「转发根路径」的判定要用它：请求落到上游 / 才算 landing。
func newReverseProxy(base *url.URL, name, token string, tr *http.Transport, mount string) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{
		// 上游是 dev server 时，keep-alive 很容易被对端先关掉留下脏连接，
		// 一个请求偶发 502 会让人误以为是目标机挂了。禁用复用省这个心。
		Transport:     tr,
		FlushInterval: -1, // 立刻刷：SSE / 流式响应不能被缓冲住
		Rewrite: func(r *httputil.ProxyRequest) {
			up := r.In.Header.Get(fwdUpHeader)
			fwdName := r.In.Header.Get(fwdNameHeader)
			r.Out.Header.Del(fwdUpHeader)
			r.Out.Header.Del(fwdNameHeader)
			// 「转发根路径」= 上游收到的正是它自己的根（剥前缀那条路的 /，
			// 或者全透传那条路的 /fwds/<name>/）。只有这种请求上没有别的线索，
			// 才需要 X-Forwarded-* 告诉上游外面挂在哪儿。
			rewriteFwd(r, base, token, up, entryOf(fwdName, up), landing(up, mount))
			// 只摘「值与 wr 自己的 token 相同」的那条凭据：上游自己的 Authorization
			// （basic、别的 Bearer）原样过去，不再是「谁都得放弃 Authorization」的一刀切。
			// 上游自己有 X-WR-Token 时也算它的，别再顶一份覆盖掉。
			if !stripOwnToken(r.Out.Header, token) && token != "" {
				// 上游也认 token 时给它一份（例如另一个 wr 实例、带鉴权的 mcp 服务）。
				// 只有在显式配了同一口令时才有意义，所以直接沿用 wr 的。
				r.Out.Header.Set("X-WR-Token", token)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// 502 的正文要能一眼看出是哪台上游、连不上还是超时。
			writeErr(w, http.StatusBadGateway, "转发 "+name+" ("+base.Host+") 失败: "+err.Error())
		},
	}
	return rp
}

// rewriteFwd 把上游路径 rest 拼到 base 上。
//
// rest 是上游视角的完整路径（/fwd、/fwds 这两层由 fwdTrip 按入口算好），
// 这里只管拼：SetURL 而不是 SetXForwarded 一起处理，因为 JoinPath 保留上游
// 已有的路径前缀（上游配的是 http://host:3000/app 这种带子路径的情况）。
func rewriteFwd(r *httputil.ProxyRequest, base *url.URL, token, rest, entry string, land bool) {
	r.Out.URL.Scheme = base.Scheme
	r.Out.URL.Host = base.Host
	r.Out.URL.Path, r.Out.URL.RawPath = joinURLPath(base.Path, rest)
	r.Out.Host = base.Host
	// 上游通常按自己的域名/路径拼静态资源和 HMR 地址，
	// 这里把 Referer 抹掉，避免它拿 wr 的地址去推断站点根。
	r.Out.Header.Del("Referer")
	r.Out.URL.RawQuery = stripToken(r.In.URL.RawQuery, token)
	if land {
		setForwarded(r, entry)
	}
}

// stripOwnToken 把「wr 自己的口令」从发往上游的 header 里摘掉。
//
// 判据是**值**，不是名字：只有当这条凭据携带的正是 wr 的 token 时才删。
// 上游自己的 Authorization（basic、别的 Bearer）不是 wr 的口令，摘它等于替上游
// 取消鉴权，所以留着。三个 header 各自按自己的规矩取值：
//
//	Authorization: Bearer <wr token>  → 删；Bearer <上游自己的> → 留
//	Authorization: <wr token>         → 删（有的客户端不写 scheme）；basic 的 base64 → 留
//	X-WR-Token: <wr token>            → 删；上游自己的值 → 留
//
// 名字不区分大小写（http.Header 本来就是），值也不区分：口令是 base64url 生成的，
// 「同一个口令、大小写不同」不会出现，放宽比较只会多删该删的。
//
// 返回上游自己有没有带 X-WR-Token：带了就别再往上顶一份，覆盖人家的凭据。
func stripOwnToken(h http.Header, token string) (hadOwnWrToken bool) {
	if token == "" {
		return h.Get("X-Wr-Token") != "" // 没配口令，就没有「wr 自己的凭据」这回事
	}
	if rest, ok := cutBearer(h.Get("Authorization")); ok && sameToken(rest, token) {
		h.Del("Authorization")
	} else if sameToken(h.Get("Authorization"), token) {
		// 有的客户端把口令直接塞进 Authorization，不写 scheme（auth 也是这么认的）。
		h.Del("Authorization")
	}
	if wr := h.Get("X-Wr-Token"); sameToken(wr, token) {
		h.Del("X-Wr-Token")
		return true // 删掉的是 wr 自己的，后面按需重新顶一份
	} else if wr != "" {
		return true // 上游自己的，留着，也别覆盖
	}
	return false
}

// cutBearer 拆出 "Bearer <token>" 里的口令部分。
func cutBearer(v string) (string, bool) {
	rest, ok := strings.CutPrefix(v, "Bearer ")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

func sameToken(got, token string) bool {
	return got != "" && strings.EqualFold(strings.TrimSpace(got), token)
}

// stripToken 去掉 query 里与 wr 口令相同的那条 token。
//
// auth 认 ?token=，于是浏览器里那条最省事的写法（/fwd/web/?token=…）
// 会把 wr 的口令一路带到上游的访问日志里。它是个还能用的活口令，
// 不该出现在别人的日志里。其它参数顺序、编码原样保留。
//
// 判据同样是值：上游自己的 ?token= 是它的鉴权参数（mcp、回调服务常这么用），
// 按名字一刀切会把上游的鉴权关掉，所以只删对得上的那条。
func stripToken(raw, token string) string {
	if raw == "" || token == "" || !strings.Contains(raw, "token=") {
		return raw
	}
	parts := strings.Split(raw, "&")
	kept := parts[:0]
	for _, p := range parts {
		if k, v, ok := strings.Cut(p, "="); ok && k == "token" && sameToken(unescapeQuery(v), token) {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(kept, "&")
}

// unescapeQuery 按 query 的规矩解一层百分号编码，只为了和口令比值得上。
func unescapeQuery(s string) string {
	if !strings.Contains(s, "%") && !strings.Contains(s, "+") {
		return s
	}
	if u, err := url.QueryUnescape(s); err == nil {
		return u
	}
	return s
}

// setForwarded 告诉上游「外部看到的是哪个地址」。
//
// 只有「转发根路径」时才发：那时上游收到的路径是它自己的 /，
// 除了这几个 header，它没有任何线索知道外面挂在哪。
// 子路径的请求不断言——断言错了会把上游的重定向带错地方。
func setForwarded(r *httputil.ProxyRequest, entry string) {
	proto := "http"
	if r.In.TLS != nil {
		proto = "https"
	}
	r.Out.Header.Set("X-Forwarded-Host", r.In.Host)
	r.Out.Header.Set("X-Forwarded-Proto", proto)
	// 前缀要带结尾斜杠，和 nginx 的 $http_x_forwarded_prefix 一个规矩。
	r.Out.Header.Set("X-Forwarded-Prefix", entry+"/")
}

// isFwdAll 判断这条路径走的是不是 /fwds/（全透传）那条路。
// 调用点在 fwdGuard 里，那时 r.URL.Path 已经是客户端那条原始路径了。
func isFwdAll(path string) bool {
	return strings.HasPrefix(path, fwdAllPrefix)
}

// upstreamPath 是客户端这条路径在上游那边对应的路径。
//
// 两条路的唯一差别就在这里：
//
//	/fwd/web/src/main.ts   → /src/main.ts     （入口段是 wr 的，剥掉）
//	/fwds/web/src/main.ts  → /fwds/web/src/main.ts（入口段也是上游的，留着）
//
// 进来的请求一定已经匹配过某个目标的 "/fwd/<name>/" 或 "/fwds/<name>/"，
// 所以这里只处理「前缀之后」的那段；前缀本身由 mux 保证。
func upstreamPath(path string) string {
	if strings.HasPrefix(path, fwdAllPrefix) {
		return "/" + strings.TrimPrefix(path, "/") // 入口段也是上游的，原样过去
	}
	// 剥掉入口段 "/fwd" 之后，第一段是转发名，名字之后才是上游要的路径。
	_, rest, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(path, fwdPrefix), "/"), "/")
	if rest == "" {
		return "/"
	}
	return "/" + rest
}

// entryOf 是上游视角的「入口」：全透传那条路上游自己也看得见这段前缀。
func entryOf(name, up string) string {
	if strings.HasPrefix(up, fwdAllPrefix) {
		return fwdAllPrefix + name
	}
	return fwdPrefix + name
}

// mountOf 是上游的挂载路径，规范化成「有且只有一个结尾斜杠」。
func mountOf(u *url.URL) string {
	return strings.TrimSuffix(u.Path, "/") + "/"
}

// landing 判断这次请求是不是「按路径落到了上游的根」。
// 剥前缀那条路的根是上游自己的挂载路径（base 的 path），全透传那条路是入口本身。
func landing(rest, mount string) bool {
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" {
		rest = "/"
	}
	if mount == "" {
		return rest == "/"
	}
	return strings.TrimSuffix(mount, "/") == rest
}

// joinURLPath 拼上游前缀与剩余路径，顺手避免双斜杠。
func joinURLPath(a, b string) (path, rawPath string) {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:], ""
	case !aslash && !bslash && a != "":
		return a + "/" + b, ""
	}
	return a + b, ""
}

// handleFwd 处理「路径里根本没有 <name> 这一层」的 /fwd 请求。
//
// 为什么不再做一个吃下整个 /fwd/ 子树的 handler：那样 /fwd/nope/x 也会落进
// 这里，剥完前缀正好剩 /nope/x，和「/fwd/web/fwd/web/」剥完剩 /fwd/web/
// 长得一模一样——分不清「名字写错了」还是「上游就是要这个路径」。
// 现在每个目标单独注册 "/fwd/<name>/"，最长的匹配赢：
//
//	/fwd/web/            上游 /            （剥一次前缀，交给上游）
//	/fwd/web/fwd/web/    上游 /fwd/web/    （第二个 /fwd/ 是普通路径）
//
// 名字匹配不上时只回空 200，什么都不说。这条路径免 token，谁都能敲一次；
// 回「已配置: web, api」等于把名字清单白送出去（之前就是这么干的）。
// 客户端想知道配了什么，自己拿 token 走 GET /info 或 GET /fwd。
func (s *Server) handleFwd(w http.ResponseWriter, r *http.Request) {
	noIndex(w)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
}

// fwdGuard 是裸转发的最后一层：wr 自己拍的板，得让人看得出是 wr 拍的。
//
// 三种上游的不良行为在这里被翻译成人话：
//
//	上游连不上         → 502，正文点名目标（不是裸的 "502 Bad Gateway"）
//	上游把不存在当 200 → 探一次上游的根，两边都 200 才认，否则 404
//	上游 302 指回入口  → 先跟着它走，走不通才 502
//
// 第三条是这层的由来，也是它最容易写错的地方。剥前缀那条路上游回
// Location: /fwd/web/ 时，「上游把自己挂在 /fwd/web/ 上」和「上游在把我送回
// 我自己的入口」是同一串字符：Location 是个相对路径，按 RFC 9110 它属于上游，
// 指向上游自己的端口——那台 dev server 听在 127.0.0.1:4173 上，外面根本连不到，
// 它不可能把浏览器送回 wr。
//
// 所以不猜，跟着走：把入口那一层剥掉，替上游再要一次。
//
//	/            上游 302 → /fwd/web/    剥一层 → 上游 /fwd/web/   200
//	/fwd/web/    上游 200
//
// 上游真挂着这个子路径，一遍就通了；每一跳都换到新路径上，也就走开了。
// 只有绕回已经走过的路径（/ → /fwd/web/ → /）才是死循环，那时才 502。
//
// 全透传那条路（/fwds/<name>/）不需要这套跟踪：入口段本来就归上游，
// 上游说的路径就是它自己的路径，原样去取即可。
func (s *Server) fwdGuard(t *fwdTarget) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 两条路都用前缀式注册，mux 只挑最长前缀、不动 r.URL.Path，
		// 所以这里就是客户端那条原始路径，两条路一视同仁。
		path := r.URL.Path
		all := isFwdAll(path)
		// 探活要知道「上游的根」在上游那边叫什么：剥前缀那条路是上游的挂载路径，
		// 全透传那条路是入口本身。两条路各用各的缓存——根不是同一段路径，
		// 借来的结论会把「另一条路的根 404」读成「这条路的 200 是兜底」。
		probe, probeURL := t.probing, t.land.String()
		if all {
			probe, probeURL = t.all, t.allRoot
		}
		go probe.alive(probeURL) // 顺手把探活结论刷新，别让它在判断那一刻才去连
		up := upstreamPath(path)
		g := s.fwdTrip(t, r, w, up)
		if g.hijacked || g.streaming {
			return // websocket 升上去了、或者流已经开始推，wr 不能再插话
		}

		// 当前路径自己算一次：第一跳正好回到它就是原地打转。
		// 反过来，第一跳落到 / 是正常的一步（base:'/fwd/web/' 的 dev server 就这样）。
		seen := map[string]bool{strings.TrimSuffix(path, "/"): true}
		followed := false
		for hop := 0; hop < fwdMaxHops; hop++ {
			g = settle(g)
			loc := g.Header().Get("Location")
			if g.committed || g.status < 300 || g.status >= 400 || loc == "" {
				break
			}
			if u, err := url.Parse(loc); err == nil && u.Host != "" && !strings.EqualFold(u.Host, r.Host) {
				break // 指向别处，不归 wr 管，原样交给浏览器
			}
			// 第一跳里上游说的路径要先剥掉入口段才是它自己的（剥前缀那条）；
			// 全透传那条的入口段本来就归上游，fwdHop 会原样留下。
			to, ok := fwdHop(hop, strings.TrimSuffix(fwdPrefix+t.name, "/"), loc, all)
			if !ok || seen[to] {
				break
			}
			seen[to], followed, up = true, true, to
			g.dropPending()
			g = s.fwdTrip(t, r, w, up)
			if g.hijacked || g.streaming {
				return
			}
		}

		g = settle(g)
		if followed && g.status >= 300 && g.status < 400 && g.Header().Get("Location") != "" {
			// 替上游走过一遍，还在往同一段跳：这条链子上游自己解不开。
			// 一次都没跟过的 302 是上游的正常跳转，原样透传（见下）。
			loc := g.Header().Get("Location")
			g.dropPending()
			writeErr(w, http.StatusBadGateway, loopMessage(t, path, loc, all))
			return
		}
		if !followed && g.status == http.StatusOK && probe.dead(probeURL) {
			// 一次都没跟过、上游却说 200：它可能对任何路径都回 200。根已经不是 200 了，
			// 说明它在兜底，报 404 更接近真相。跟过的请求不在这条判断里——那个 200
			// 是上游自己认下的路径。
			g.dropPending()
			writeErr(w, http.StatusNotFound, "上游没有这个路径: "+path+
				"（wr 探过上游的根，它现在对自己的根都不回 200，所以这个 200 是它自己在兜底）")
			return
		}
		g.commit()
	})
}

// fwdMaxHops 是一条转发最多跟几跳上游的 Location。
//
// 一跳是常态（base 指向自己的子路径）。给到 3 是留给「/ → /app/ → /app/index」这种
// 上游自己多跳一次的实现；再多就不是配置能解释的了，报 502 比替它走下去更诚实。
const fwdMaxHops = 3

// fwdTrip 替上游要一次，上游看到的是 up 这条路径。
//
// 路径必须在 wr 这边定死再交给 ReverseProxy：它的 Rewrite 是拿 r.In.URL 现算的，
// 跟着 Location 换路时就分不清「剥过没有」了（/fwd/web/ 和 / 剥完都是 /）。
func (s *Server) fwdTrip(t *fwdTarget, r *http.Request, w http.ResponseWriter, up string) *fwdGuardWriter {
	out := r.Clone(r.Context())
	out.URL.Path, out.URL.RawPath = up, ""
	out.RequestURI = ""
	out.Header.Set(fwdUpHeader, up)
	out.Header.Set(fwdNameHeader, t.name)
	g := &fwdGuardWriter{rw: w, hdr: w.Header()}
	t.rp.ServeHTTP(g.superset(), out)
	return g
}

// settle 补上「上游一个字节都没回、ReverseProxy 也没报错」这种沉默：按空 body 的 200 算，
// 否则后面拿 status 判断的地方全都读到一个 0。
//
// 连不上是另一种沉默，那一路 ReverseProxy 已经写好了 502，committed 为真，这里不碰。
func settle(g *fwdGuardWriter) *fwdGuardWriter {
	if !g.headerDone && !g.committed {
		g.status = http.StatusOK
	}
	return g
}

// fwdHop 判断这条 Location 该不该替上游再走一次，是的话给出上游视角的路径。
//
// 只跟落在 wr 入口段上的那种：/fwd/<name>/… 是 wr 剥掉的这一层，上游说的路径
// 要先剥掉这一层才是它自己的。第一跳之后（hop>0）客户端的路径已经不在入口下，
// 上游说的就是它自己的路径，原样去取。
//
// keep 为真（/fwds 那条路）时什么都不剥：入口段本来就归上游，它说的路径
// 就是它自己的路径。只有往回指到入口段根的才算「可能成环」，交给调用方的 seen 判。
//
// 带 host 的 Location 只有指到 wr 自己的入口才跟（host 由调用方比）：上游不知道
// 外面挂在哪个域名上，它写出的绝对 URL 指向别处，那不归 wr 管。
func fwdHop(hop int, mine, loc string, keep bool) (string, bool) {
	u, err := url.Parse(loc)
	if err != nil {
		return "", false
	}
	p := u.Path
	if p == "" {
		p = "/"
	}
	if hop > 0 || keep {
		return p, p != "/"
	}
	tail := strings.TrimPrefix(p, mine)
	if tail == p || tail == "" || !strings.HasPrefix(tail, "/") {
		return "", false // 不在入口段下，或者剥完什么都不剩
	}
	return tail, true
}

// loopMessage 解释这条回环是谁造成的：跟着上游走了一遍，还在往同一个入口跳。
func loopMessage(t *fwdTarget, path, loc string, all bool) string {
	if all {
		return "转发回环：请求 " + path + " → 上游 " + t.base.String() + " → 上游 302 回 " + loc +
			"（这一跳是上游自己指回自己的根，跟着走还是跳回这段）。" +
			"上游的 base 要么写成 " + fwdAllPrefix + t.name + "/，要么用相对路径，别和它对「根在哪」各说一套。"
	}
	root := ""
	if t.bare != "/" {
		root = "（上游自己的根其实是 " + t.bare + "）"
	}
	head := "转发回环：请求 " + path + " → 上游 " + t.land.String() + " → 上游 302 回 " + loc +
		"（剥掉 " + fwdPrefix + t.name + " 这一层再要一次，还是跳回这段）。"
	if landing(upstreamPath(path), t.base.Path) {
		return head + "上游把自己的根挂在了 " + fwdPrefix + t.name + "/ 上，而这一段是 wr 的入口，" +
			"两边对「根在哪」的说法冲突，浏览器就会原地打转。" + root +
			"要么把上游的 base 改成相对路径，要么改用 " + fwdAllPrefix + t.name + "/：那条路会把前缀原样带给上游，" +
			"上游按 base:'" + fwdAllPrefix + t.name + "/' 跑就行。"
	}
	return head + "上游把 " + fwdPrefix + t.name + "/ 当成了自己的挂载点，而这个前缀是 wr 的入口。" + root
}

// fwdProbe 回答的是「这台上游还认自己的根吗」。
//
// 用途是给「上游对不存在的路径回 200」下判断：根还是 200，说明它对任何路径都回 200
// （SPA 的常见行为），这次的 200 就是真的；根已经不是 200 了，说明它自己在兜底，
// 报 404 更接近真相。连不上则两头都不算——上游挂了的答案是 502，不是 404。
//
// 探活不能变成每个请求一次额外往返：结论缓存一段时间，请求进门时顺手续上。
//
// 一份 fwdProbe 只回答一个 URL（上游的某个「根」）：结论缓存不带键，
// 两条入口的根不是同一段路径，混用会把一条路的 404 读成另一条路的兜底。
type fwdProbe struct {
	mu      sync.Mutex
	client  *http.Client
	ok      bool      // 上一次探活的结果：上游的根是不是 200
	known   bool      // 探过没有；没探到（连不上）就不算数
	expires time.Time // 过期之后下一次请求会再探一次
}

// probeTTL 是探活结论的保质期。它回答的是「这台上游的根还活着吗」，
// 一个静态配置，不该跟着流量抖。
const probeTTL = 10 * time.Second

// dead 比 alive 多一层意思：只有「探到了、而且根不是 200」才算死了。
// 连不上上游是另一回事，那条路上 wr 该说的是 502。
func (p *fwdProbe) dead(u string) bool {
	alive := p.alive(u)
	p.mu.Lock()
	defer p.mu.Unlock()
	return !alive && p.known
}

func (p *fwdProbe) alive(u string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Now().Before(p.expires) {
		return p.ok
	}
	resp, err := p.client.Get(u)
	if err != nil {
		// 连不上：既不能说它活着，也不能说它死了。缓存成「不知道」，
		// 让 fwdUpstreamMissing 不拿这个结论去报 404——上游挂了的答案是 502。
		p.ok, p.known, p.expires = false, false, time.Now().Add(probeTTL)
		return false
	}
	p.known = true
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	resp.Body.Close()
	p.ok, p.expires = resp.StatusCode == http.StatusOK, time.Now().Add(probeTTL)
	return p.ok
}

// fwdGuardWriter 攒着上游的回包，等 wr 判断完再决定要不要发出去。
//
// 为什么要攒：ResponseWriter 一旦写出状态码就没有回头路了，而「该不该拦」
// 要等上游的回包到了才知道。所以这里只把**头**和不超过 64KiB 的**小 body**
// 留在内存里——大 body（页面、图片）超过阈值就地放行，宁可漏拦一次也不攒。
//
// 还有个坑：ReverseProxy 只有在 ResponseWriter 不实现它认识的接口时才走
// flushInterval 那条路、在写第一个字节前回调 FirstWrite。所以传给它的必须
// 是一个「裸类型」的包装（我们这套 interface 一个都不实现），不能是它见过的
// *http.response——否则它会自己算出一个 io.ReaderFrom，直接写穿。
type fwdGuardWriter struct {
	hdr        http.Header // ReverseProxy 写进这里；commit 时才并到底层，drop 时整份丢掉
	rw         http.ResponseWriter
	status     int
	headerDone bool
	hijacked   bool
	streaming  bool
	committed  bool
	pending    []byte // 还没发出去的小 body
}

// guardSpill 是小 body 的攒放上限：超过就当场放行，之后 wr 不再有话语权。
const guardSpill = 64 << 10

// FirstWrite 是 ReverseProxy 在写第一个字节前的回调。
func (g *fwdGuardWriter) FirstWrite() {}

// Header 给 ReverseProxy 写响应用：写在 hdr 上，commit 时才并到底层。
func (g *fwdGuardWriter) Header() http.Header { return g.hdr }

func (g *fwdGuardWriter) WriteHeader(code int) {
	g.headerDone, g.status = true, code
	if g.Header().Get("Content-Type") == "text/event-stream" {
		g.streaming = true
	}
}

func (g *fwdGuardWriter) Write(b []byte) (int, error) {
	if !g.headerDone {
		g.headerDone, g.status = true, http.StatusOK
	}
	// committed 之后没得选了：头已经发出去，wr 只剩注释权，攒着只会断流。
	if g.committed || g.streaming || len(g.pending)+len(b) > guardSpill {
		// 冲过上限：把这批连同已经攒着的一起发出去。
		// 返回值只能报「上游给的那些字节都收下了」，两边长度对不上 ReverseProxy
		// 会按 io.ErrShortWrite 直接断流——攒着那截是替它留的，不算没收。
		n, err := g.flushPending(b)
		return n, err
	}
	g.pending = append(g.pending, b...)
	return len(b), nil
}

// Flush 必须当没听见。
//
// ReverseProxy 是 FlushInterval:-1，每写一块 body 就 Flush 一次；真转给底层的话，
// 攒着的那一版立刻就发出去了，fwdGuard 也就不用判断了。
// 攒到上限（大文件、SSE）时 Write 会先 commit 再直写，那条路上真正的 Flush 自己会走。
func (g *fwdGuardWriter) Flush() {}

func (g *fwdGuardWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := g.rw.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("这个连接不支持 hijack")
	}
	c, rw, err := hj.Hijack()
	if err == nil {
		g.hijacked, g.headerDone = true, true
	}
	return c, rw, err
}

// flushPending 把攒着的那截和这一批一起发出去，之后 body 直接往下写。
//
// 返回值对 ReverseProxy 只有一个含义：上游给我的这些字节我都收下了。
// 攒着的那截是替它留的，不算没收——按实际写出的长度报，它会以为短写而断开。
func (g *fwdGuardWriter) flushPending(b []byte) (int, error) {
	head := g.pending
	g.pending = nil
	g.commit()
	if len(head) > 0 {
		if _, err := g.rw.Write(head); err != nil {
			return 0, err
		}
	}
	if _, err := g.rw.Write(b); err != nil {
		return 0, err
	}
	return len(b), nil
}

// commit 把攒着的回包原样发出去。发过之后 wr 只剩注释权。
func (g *fwdGuardWriter) commit() {
	if g.committed {
		return
	}
	g.committed = true
	if !g.headerDone {
		g.rw.WriteHeader(http.StatusOK)
		return
	}
	for k, vs := range g.hdr {
		g.rw.Header()[k] = vs
	}
	g.rw.WriteHeader(g.status)
	if len(g.pending) > 0 {
		_, _ = g.rw.Write(g.pending)
		g.pending = nil
	}
}

// dropPending 丢掉攒着的回包：上游那一版不发了。
func (g *fwdGuardWriter) dropPending() {
	g.committed = true
	g.pending = nil
	for k := range g.hdr {
		delete(g.hdr, k)
	}
}

// superset 返回一个「什么接口都不实现」的 ResponseWriter 给 ReverseProxy。
//
// 目的正是让它认不出来：认不出来才会调 FirstWrite，也才会把 body 交给我们中转。
func (g *fwdGuardWriter) superset() http.ResponseWriter { return guardOnly{g} }

type guardOnly struct{ g *fwdGuardWriter }

func (o guardOnly) Header() http.Header         { return o.g.Header() }
func (o guardOnly) Write(b []byte) (int, error) { return o.g.Write(b) }
func (o guardOnly) WriteHeader(code int)        { o.g.WriteHeader(code) }

// joinForDoc 只给清单里的路径示例用：按上游拼路径的规则连起来，别让人猜 /x 落在哪。
func joinForDoc(prefix, tail string) string {
	p, _ := joinURLPath(prefix, tail)
	return p
}

func (s *Server) fwdNames() []string {
	names := make([]string, 0, len(s.fwd))
	for n := range s.fwd {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// fwdHops 把 -fwd 的配置整理成一份清单。/info 和 /fwd 共用它：
// 「这台 wr 背后有什么」只有一个来源，两处各拼一份迟早说法不一致。
func (s *Server) fwdHops() []proto.FwdHop {
	hops := make([]proto.FwdHop, 0, len(s.fwd))
	for _, n := range s.fwdNames() {
		t := s.fwd[n]
		hops = append(hops, proto.FwdHop{
			Name: n, URL: t.base.String(), Spec: t.fromSpec,
			Path: fwdPrefix + n + "/x → 上游 " + joinForDoc(t.bare, "x") + "；" +
				fwdAllPrefix + n + "/x → 上游 " + joinForDoc(fwdAllPrefix+n+"/", "x") + "（入口段也过去）",
		})
	}
	return hops
}

func (s *Server) handleFwdList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, proto.FwdResp{
		Targets:    s.fwdHops(),
		Forward:    fwdPrefix + "<name>/",
		ForwardAll: fwdAllPrefix + "<name>/",
	})
}

// maxFwdBody 是 /fwd/call 单次回包的体量上限。
// 转发是给 agent「看一眼」用的，几十兆的 JS bundle 拉回来只会烧上下文；
// 真要整份文件，走 /fwd/<name>/ 的裸流。
const maxFwdBody = 8 << 20

// fwdCall 是只拿一个 JSON 通道的 agent 的入口：
// 一条请求告诉 wr「哪个上游、什么方法、什么路径」，回来的是状态码 + header + body。
//
// 有浏览器的 agent 不需要它——那一层用 /fwd/<name>/... 更省事。
func (s *Server) fwdCall(ctx context.Context, req proto.FwdReq) (proto.FwdResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	t, ok := s.fwd[req.Target]
	if !ok {
		return proto.FwdResult{}, fmt.Errorf("没有叫 %q 的转发目标（清单见 GET /fwd）", req.Target)
	}
	timeout := time.Duration(req.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	p := req.Path
	if p == "" {
		p = "/"
	} else if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	full := *t.base
	full.Path, full.RawPath = joinURLPath(t.base.Path, p)
	if i := strings.IndexByte(p, '?'); i >= 0 {
		full.RawQuery = p[i+1:]
		full.Path, full.RawPath = joinURLPath(t.base.Path, p[:i])
	}

	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}
	hreq, err := http.NewRequestWithContext(ctx, strings.ToUpper(orDefault(req.Method, "GET")), full.String(), body)
	if err != nil {
		return proto.FwdResult{}, err
	}
	for _, kv := range req.Headers {
		k, v, ok := strings.Cut(kv, ":")
		if !ok {
			continue
		}
		hreq.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	if req.Body != "" && hreq.Header.Get("Content-Type") == "" {
		hreq.Header.Set("Content-Type", "application/json")
	}

	start := time.Now()
	// 不跟随重定向：这条通道的价值是「把真相说全」。跟一跳回来一个 200，
	// agent 会以为自己问的那个 URL 就是 200，而实际是上游 302 到的别处——
	// 该说的那句是「302，Location: …」，不是「200」。
	client := &http.Client{
		Transport:     t.tr,
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return proto.FwdResult{}, fmt.Errorf("转发 %s (%s) 失败: %w", t.name, t.base.Host, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFwdBody+1))
	if err != nil {
		return proto.FwdResult{}, err
	}
	res := proto.FwdResult{
		Target:     t.name,
		Status:     resp.StatusCode,
		Bytes:      len(data),
		DurationMS: time.Since(start).Milliseconds(),
	}
	for k, vs := range resp.Header {
		switch k {
		case "Authorization", "Set-Cookie":
			continue // 上游的口令和 cookie 不往 agent 手里塞
		}
		for _, v := range vs {
			res.Headers = append(res.Headers, k+": "+v)
		}
	}
	sort.Strings(res.Headers)
	truncated := len(data) > maxFwdBody
	if truncated {
		data = data[:maxFwdBody]
		res.Bytes = len(data)
	}
	if utf8.Valid(data) {
		res.Body = string(data)
	} else {
		res.BodyB64 = base64.StdEncoding.EncodeToString(data)
	}
	if truncated {
		res.Body += "\n… 已截断（上限 8MiB），完整内容走 GET /fwd/" + t.name + p + " 裸流"
	}
	return res, nil
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func (s *Server) handleFwdCall(w http.ResponseWriter, r *http.Request) {
	var req proto.FwdReq
	if !decode(w, r, &req) {
		return
	}
	res, err := s.fwdCall(r.Context(), req)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, res)
}
