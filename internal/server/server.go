// Package server 是 wr 的服务端：一组扁平 HTTP 端点 + 共享 token 鉴权。
// 刻意不走 JSON-RPC / MCP：agent 直接 POST 一个 JSON，比协议握手快一个数量级。
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"cnb.cool/yyx946/tteist/internal/audit"
	"cnb.cool/yyx946/tteist/internal/policy"
	"cnb.cool/yyx946/tteist/internal/proto"
	"cnb.cool/yyx946/tteist/internal/shellx"
	"cnb.cool/yyx946/tteist/internal/xfer"
)

// Version 只在 proto 里定义一次，这里留个别名免得老代码全改。
// 用 var 而非 const：const 绑定会在编译期定死，-ldflags -X 注入的版本号进不来。
var Version = proto.Version

// maxRawBytes 是 /raw PUT 的硬上限。
const maxRawBytes = 512 << 20

type Config struct {
	Addr       string
	Token      string
	Root       string
	Deny       []string
	NoDefaults bool
	// LogPath 是审计日志落点（绝对路径）。空表示不记。
	// 落点由 cmd 侧算好：默认是 wr start 的执行目录下 .wr.log，
	// server 不掺和「相对谁」这种只有调用者知道的事。
	LogPath string
	Quiet   bool

	Git   string // 仓库 .git 路径；空表示没找到，exec 里 git 会报 not a repository
	GitAt string // 相对 root 的仓库位置（"." 或 "sub/dir"），只用于展示

	AllowGitSearch bool // glob/grep 是否进 .git（默认不进：索引和对象库对搜代码没意义）

	// Fwd 是「名字=URL」的上游清单，见 /fwd/<name>/...。
	// 只从启动参数来，不接受请求里的 URL——wr 带 token 能跑命令，
	// 再让它自由选地址就等于开放代理。
	Fwd []string

	// JobRetain 是「进程已退出多久之后，日志跟着清掉」，0 表示不自动清。
	// .wr-jobs 只涨不落：起一次 dev server 留一份日志，攒上两个月谁也不敢删。
	// 判定用日志文件的 mtime 而不是 job 记录——服务重启过、记录没了，日志还在。
	JobRetain time.Duration
}

type Server struct {
	cfg  Config
	root *xfer.Root
	pol  *policy.Policy
	aud  *audit.Logger

	jobMu sync.Mutex
	jobs  map[string]*job

	fwd map[string]*fwdTarget
}

func New(cfg Config) (*Server, error) {
	root, err := xfer.NewRoot(cfg.Root)
	if err != nil {
		return nil, err
	}
	pol, err := policy.New(cfg.Deny, cfg.NoDefaults)
	if err != nil {
		return nil, err
	}
	fwd, err := newFwdTargets(cfg.Fwd, cfg.Token)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, root: root, pol: pol, jobs: map[string]*job{}, fwd: fwd}
	if cfg.LogPath != "" {
		al, err := audit.Open(cfg.LogPath)
		if err != nil {
			return nil, fmt.Errorf("打开日志文件失败: %w", err)
		}
		s.aud = al
	}
	s.detectGit()
	s.reapJobLogs()
	return s, nil
}

// reapJobLogs 按 JobRetain 清掉过期的后台任务日志，启动时跑一次。
//
// 只删「已经不在跑」的那种：mtime 比保留期还老，说明这份日志早就没人写了。
// 还在跑的 job 文件 mtime 是新的，天然不在清理范围里。
func (s *Server) reapJobLogs() {
	if s.cfg.JobRetain <= 0 {
		return
	}
	dir, err := s.root.Resolve(jobDirName)
	if err != nil {
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-s.cfg.JobRetain)
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// Close 关掉审计文件。服务进程退不退出无所谓，测试和嵌入式用法需要收干净。
func (s *Server) Close() error {
	return s.aud.Close()
}

// audit 写一行审计。所有埋点都走这里，字段名、时间格式只有一份。
func (s *Server) audit(e audit.Entry) {
	s.aud.Log(e)
}

// auditRemote 从请求里取出调用方地址。token 是共享的，出事之后
// 来源地址就是唯一还能追的线索，所以每行都带上（同网段时它并不精确，但比没有强）。
func auditRemote(r *http.Request) string {
	// 前面挂反代时 RemoteAddr 是反代那台机器，真实来源只在 X-Forwarded-For 里。
	if ip := clientIP(r.Header.Get("X-Forwarded-For")); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// clientIP 取 X-Forwarded-For 最左一项。代理会往右追加，最左是客户端自己报的，也就这一项是原始来源。
func clientIP(xff string) string {
	first, _, _ := strings.Cut(xff, ",")
	return strings.TrimSpace(first)
}

// auditBlocked 记一次被拒：策略拒绝和 .git 保护都走这里，语义都是「想做但不让」。
func (s *Server) auditBlocked(tool, reason string, r *http.Request) {
	s.audit(audit.Entry{Decision: audit.Blocked, Tool: tool, Reason: reason, Remote: auditRemote(r)})
}

// detectGit 找沙箱里属于「这个仓库」的 .git。
// root 本身就是仓库根是最常见的情况；root 是 D:\repos 这种上层目录时，
// 从 root 往下最多找两层，只认第一个——多仓库场景显式用 -git-dir 指定。
func (s *Server) detectGit() {
	if s.cfg.Git != "" {
		if abs, err := filepath.Abs(s.cfg.Git); err == nil {
			s.cfg.Git = abs
		}
		return
	}
	root := s.root.Path()
	cands := []string{root}
	for _, e := range subDirs(root, 2) {
		cands = append(cands, e)
	}
	for _, dir := range cands {
		if fi, err := os.Stat(filepath.Join(dir, ".git")); err != nil || (!fi.IsDir() && fi.Size() == 0) {
			continue
		}
		// 有 .git 不等于能跑 git：得让 git 自己确认一遍。
		// 空手搭个 .git 目录、或者 .git 文件（worktree/submodule）写错，
		// 都会让 GIT_DIR 变成「not a git repository」。
		if s.resolveGit(dir) == "" {
			continue
		}
		if rel, err := filepath.Rel(root, dir); err == nil {
			s.cfg.GitAt = filepath.ToSlash(rel)
		}
		return
	}
}

// resolveGit 在 dir 上问一次 git，成功则返回真正可用的 GIT_DIR。
func (s *Server) resolveGit(dir string) string {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--absolute-git-dir")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(string(out))
	if p == "" {
		return ""
	}
	s.cfg.Git = p
	return p
}

// subDirs 只往下走 depth 层，且跳过重度目录，避免在大 root 上把启动拖慢。
func subDirs(root string, depth int) []string {
	if depth <= 0 {
		return nil
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(root, e.Name())
		out = append(out, p)
		out = append(out, subDirs(p, depth-1)...)
	}
	return out
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// 存活探测只回两个字符 "ok"：它是给 keepalive / 探活脚本看的，
	// 不是接口目录，不该顺手把版本号递出去。
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("Content-Length", "2")
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "ok")
	})
	// 根路径什么都不给：空 body、204。监听在网卡上的服务，"/" 是扫描器
	// 第一个敲的门，回一页目录等于把地图递出去。
	// 方法也照收（POST / 之类）：反正什么都不做，省得它回一句「method not allowed」
	// 让人以为这里有个接口只是方法用错了。
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		noIndex(w)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("GET /info", s.auth(http.HandlerFunc(s.handleInfo)))
	mux.Handle("POST /exec", s.auth(http.HandlerFunc(s.handleExec)))
	mux.Handle("POST /read", s.auth(http.HandlerFunc(s.handleRead)))
	mux.Handle("POST /write", s.auth(http.HandlerFunc(s.handleWrite)))
	mux.Handle("POST /edit", s.auth(http.HandlerFunc(s.handleEdit)))
	mux.Handle("POST /glob", s.auth(http.HandlerFunc(s.handleGlob)))
	mux.Handle("POST /grep", s.auth(http.HandlerFunc(s.handleGrep)))
	mux.Handle("POST /list", s.auth(http.HandlerFunc(s.handleList)))
	mux.Handle("POST /delete", s.auth(http.HandlerFunc(s.handleDelete)))
	mux.Handle("POST /move", s.auth(http.HandlerFunc(s.handleMove)))
	mux.Handle("POST /mkdir", s.auth(http.HandlerFunc(s.handleMkdir)))
	// 增量同步：plan 出清单，apply 建目录/删除，内容走 /raw
	mux.Handle("POST /sync/plan", s.auth(http.HandlerFunc(s.handleSyncPlan)))
	mux.Handle("POST /sync/apply", s.auth(http.HandlerFunc(s.handleSyncApply)))

	// 转发：/fwd/<name>/... 原样打给启动参数里配的上游（浏览器机器、别的 wr…）。
	// 这条子树不鉴权：里面是上游自己的服务（vite 页面、HMR websocket、mcp 端点），
	// 它认的是自己的登录态，带上 wr 的 token 反而多一层——浏览器里点开页面、
	// 页面自己发的 XHR / websocket 都拿不到 wr 的 header，鉴权在这里只添堵。
	// 能到达的仍然是启动参数里点名过的那几台上游，不是任意 URL。
	// 两个入口是同一条路的两种语义：/fwd 剥掉入口段，/fwds 把入口段一起给上游。
	// 不合并成一个开关，因为它们的用途本来就不一样（见 fwd.go 顶部）。
	if len(s.fwd) > 0 {
		// 每个目标单独注册 "/fwd/<name>/" 和 "/fwds/<name>/"：交给 mux 做最长前缀
		// 匹配，而不是让一个 /fwd/ 的 handler 自己解析名字（理由见 handleFwd）。
		//
		// 两条路都用前缀式：mux 只挑最长前缀、不动 r.URL.Path，所以 fwdGuard 拿到
		// 的就是客户端那条原始路径，路线之间的差别只剩「入口段剥不剥」（见 upstreamPath）。
		for name := range s.fwd {
			mux.Handle(fwdPrefix+name+"/", s.fwdGuard(s.fwd[name]))
			mux.Handle(fwdAllPrefix+name+"/", s.fwdGuard(s.fwd[name]))
		}
		mux.Handle(fwdPrefix, http.HandlerFunc(s.handleFwd))
		mux.Handle(fwdAllPrefix, http.HandlerFunc(s.handleFwd))
		// 纯文本通道的 agent 用这个：回 JSON，不用自己拼 URL、也不用处理裸流。
		// 同一条路，同理不鉴权。
		mux.Handle("POST /fwd/call", http.HandlerFunc(s.handleFwdCall))
		// 清单只读、且会回显上游地址，留着鉴权：它是 wr 自己的元信息，不是上游的接口。
		mux.Handle("GET /fwd", s.auth(http.HandlerFunc(s.handleFwdList)))
	}

	mux.Handle("GET /raw", s.auth(http.HandlerFunc(s.handleRawGet)))
	mux.Handle("PUT /raw", s.auth(http.HandlerFunc(s.handleRawPut)))

	// 后台任务
	mux.Handle("POST /job/start", s.auth(http.HandlerFunc(s.handleJobStart)))
	mux.Handle("POST /job/logs", s.auth(http.HandlerFunc(s.handleJobLogs)))
	mux.Handle("POST /job/list", s.auth(http.HandlerFunc(s.handleJobList)))
	mux.Handle("POST /job/kill", s.auth(http.HandlerFunc(s.handleJobKill)))

	// 审计日志：读和清空。日志常在沙箱外，也可能早就超过 /read 的上限，
	// 所以单开一个端点，agent 不必拿自己的文件工具去够它。
	mux.Handle("POST /log", s.auth(http.HandlerFunc(s.handleLog)))
	mux.Handle("POST /log/clear", s.auth(http.HandlerFunc(s.handleLogClear)))

	// 人看的两个入口：/log 是内置的实时页面（页面本身不含数据，所以不鉴权），
	// /log/stream 是它拉数据的管道（必须带 token，同一个 auth）。
	// 页面不鉴权是有意的：token 由用户自己从 ?token= 带进来，页面把它记在
	// localStorage 里再喂给接口——静态页面没法弹一个密码框。
	mux.HandleFunc("GET /log", s.handleLogPage)
	mux.Handle("GET /log/stream", s.auth(http.HandlerFunc(s.handleLogStream)))

	// 自描述端点藏在 /api 下，和真接口同一层前缀。
	// 机器可读的 openapi.json（及等价的 /api/doc）也要 token：里面是这台机器
	// 暴露了哪些能力，和 /info 同级的元信息，只该给有 token 的人看。
	// 目录页 /api 跟着一起收——它自己会去 fetch openapi.json，免鉴权也没用。
	mux.Handle("GET /api", s.auth(http.HandlerFunc(s.handleDocIndex)))
	mux.Handle("GET /api/openapi.json", s.auth(http.HandlerFunc(s.handleOpenAPI)))
	mux.Handle("GET /api/doc", s.auth(http.HandlerFunc(s.handleOpenAPI)))
	return mux
}

// handleDocIndex 给个人看的接口页：一个 HTML 直接渲染 openapi.json，
// 免得为了 swagger-ui 引一堆静态资源。挂在 /api 而不是 /，
// 因为根路径曾经是一个完整的 API 目录，等于给扫描器递上手的地图。
func (s *Server) handleDocIndex(w http.ResponseWriter, r *http.Request) {
	noIndex(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, docIndexHTML)
}

// noIndex 让搜索引擎和顺手爬一遍的扫描器都别记住这个入口。
func noIndex(w http.ResponseWriter) {
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
}

// auth 用常量时间比较。token 走 Authorization: Bearer、X-WR-Token，
// 或 URL 上的 ?token= —— 后者是给人用的：浏览器直接点开某个接口看返回、
// curl 里少写一个 -H，都不用再抄一遍 header。
// header 优先于 query：脚本里带 header 就不该被 URL 上的旧 token 覆盖。
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = r.Header.Get("X-WR-Token")
		}
		if got == "" {
			got = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
			writeErr(w, 401, "token 无效")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	var shells []proto.ShellInfo
	for _, p := range shellx.List() {
		shells = append(shells, proto.ShellInfo{ID: p.ID, Path: p.Path, Found: shellx.Found(p.Path), Note: p.Note})
	}
	writeJSON(w, 200, proto.InfoResp{
		Version: Version, Root: s.root.Path(), OS: runtime.GOOS,
		Git: s.cfg.GitAt, Shells: shells, Endpoints: endpointList(),
		AuditLog: s.aud.Path(), // 没开就是空，客户端据此决定要不要调 /log
		Fwd:      s.fwdHops(), // 没配就是空数组：客户端据此知道走不了 /fwd/<name>/
	})
}

// endpointList 让 agent 一次 /info 就知道能调什么，
// 不必先拉整份 openapi.json 再解析。
func endpointList() []string {
	return []string{
		"GET /healthz", "GET /info", "GET /api", "GET /api/openapi.json", "GET /api/doc",
		"POST /exec",
		"POST /read", "POST /write", "POST /edit",
		"POST /delete", "POST /move", "POST /mkdir",
		"POST /glob", "POST /grep", "POST /list",
		"POST /sync/plan", "POST /sync/apply",
		"GET /raw", "PUT /raw",
		"POST /job/start", "POST /job/logs", "POST /job/list", "POST /job/kill",
		"POST /log", "POST /log/clear",
		// 转发只在 -fwd 配了时才挂上，但清单是给 agent 认路的：列出来它才知道
		// 可以先 GET /fwd 探一下，而不是干脆不知道有这条路。
		"GET /fwd", "POST /fwd/call",
		"ANY /fwd/<name>/<path>", "ANY /fwds/<name>/<path>",
	}
}

// remoteOf 给文件类端点取值用：埋点在 server 包里做，HTTP 这层只补来源地址。
//
// 只读动作（read/glob/grep/list）也记，和写动作同一张表：审计要回答的是
// 「这个 token 被谁、从哪、动过什么」，读写都是「动过」。区别只在字段——
// 只读记路径/模式/结果条数，不记返回内容，所以一行还是几十字节。
// 页面按动作分色、按工具筛，噪音在展示层解决，不在记录层丢信息。
func remoteOf(r *http.Request) *string {
	v := auditRemote(r)
	return &v
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req proto.ExecRequest
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.exec(r.Context(), req, auditRemote(r))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	var req proto.ReadReq
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.read(req, remoteOf(r))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	var req proto.WriteReq
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.write(req, remoteOf(r))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	var req proto.EditReq
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.edit(req, remoteOf(r))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleGlob(w http.ResponseWriter, r *http.Request) {
	var req proto.GlobReq
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.glob(req, remoteOf(r))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleGrep(w http.ResponseWriter, r *http.Request) {
	var req proto.GrepReq
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.grep(req, remoteOf(r))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	var req proto.ListReq
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.list(req, remoteOf(r))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req proto.DeleteReq
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.del(req, remoteOf(r))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	var req proto.MoveReq
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.move(req, remoteOf(r))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var req proto.MkdirReq
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.mkdir(req, remoteOf(r))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleSyncPlan(w http.ResponseWriter, r *http.Request) {
	var req proto.SyncPlanReq
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.SyncPlan(req)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleSyncApply(w http.ResponseWriter, r *http.Request) {
	var req proto.SyncApplyReq
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.SyncApply(req)
	// 同步的「动了什么」主要在删除清单上；建了目录由 created 计数兜住。
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	var req proto.LogReq
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.Log(req)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleLogClear(w http.ResponseWriter, r *http.Request) {
	resp, err := s.ClearLog()
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleJobStart(w http.ResponseWriter, r *http.Request) {
	var req proto.JobStartReq
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.startJob(req, auditRemote(r))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleJobLogs(w http.ResponseWriter, r *http.Request) {
	var req proto.JobLogsReq
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.JobLogs(req)
	if err != nil {
		writeErr(w, jobErrCode(err), err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handleJobList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.ListJobs())
}

func (s *Server) handleJobKill(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Drop bool   `json:"drop,omitempty"`
		// DropLog 把 .wr-jobs/<id>.log 一起删掉：后台任务的收尾动作，
		// 免得每次起服务都留一份日志、攒到没人敢清。
		DropLog bool `json:"drop_log,omitempty"`
	}
	if !decode(w, r, &req) {
		return
	}
	var (
		info proto.JobInfo
		err  error
	)
	remote := auditRemote(r)
	if req.Drop {
		err = s.dropJob(req.ID, req.DropLog, remote)
		if err == nil {
			info, _ = s.getJobInfo(req.ID)
		}
	} else {
		info, err = s.killJob(req.ID, req.DropLog, remote)
	}
	if err != nil {
		writeErr(w, jobErrCode(err), err.Error())
		return
	}
	writeJSON(w, 200, info)
}

func (s *Server) getJobInfo(id string) (proto.JobInfo, error) {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return proto.JobInfo{ID: id}, nil
	}
	return j.snapshot(), nil
}

func jobErrCode(err error) int {
	if errors.Is(err, ErrJobNotFound) {
		return 404
	}
	return 400
}

// raw 端点服务大文件：body 就是文件本身，不要 JSON 包一层。
// handleRawGet 是大文件下载：body 就是文件本身。
//
// 读侧也要挡 .git，和写侧同一个理由：.git/config 里有 url.<remote>.insteadOf，
// 可能带凭据，而远端地址本身就该由人告诉 wr，不该由 agent 从 index 里翻。
// worktree 的 .git 是个文件（"gitdir: <沙箱外路径>"），下载它等于把沙箱外的
// 绝对路径送到对方手里；真正的仓库 .git 又在沙箱外，也读不到——所以整层拦掉。
// 只拦 /raw：/read 与 exec 里的 git 命令照旧能用，agent 不缺干活的手。
func (s *Server) handleRawGet(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	abs, err := s.root.Resolve(rel)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if xfer.InGit(rel) || s.root.InGitAbs(abs) {
		s.auditBlocked("get", ".git 下不允许下载: "+rel, r)
		writeErr(w, 400, ".git 下不允许下载")
		return
	}
	http.ServeFile(w, r, abs)
}

func (s *Server) handleRawPut(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	remote := remoteOf(r)
	abs, err := s.root.Resolve(rel)
	if err != nil {
		s.auditOp("put", remote, audit.Entry{Path: rel}, err)
		writeErr(w, 400, err.Error())
		return
	}
	if xfer.InGit(rel) {
		s.auditBlocked("put", ".git 下不允许写入: "+rel, r)
		writeErr(w, 400, ".git 下不允许写入")
		return
	}
	if r.ContentLength > maxRawBytes {
		err := fmt.Errorf("单次上传上限 512MiB")
		s.auditOp("put", remote, audit.Entry{Path: rel}, err)
		writeErr(w, 413, "单次上传上限 512MiB")
		return
	}
	// LimitReader 而不是手写 Read 循环：既尊重 Content-Length，
	// 也挡住「声称 1KB 实际传 1GB」的 chunked 请求。
	data, err := io.ReadAll(io.LimitReader(r.Body, maxRawBytes+1))
	if err != nil {
		s.auditOp("put", remote, audit.Entry{Path: rel}, err)
		writeErr(w, 400, err.Error())
		return
	}
	if int64(len(data)) > maxRawBytes {
		err := fmt.Errorf("单次上传上限 512MiB")
		s.auditOp("put", remote, audit.Entry{Path: rel}, err)
		writeErr(w, 413, "单次上传上限 512MiB")
		return
	}
	err = xfer.WriteAtomic(abs, data, 0o644)
	s.auditOp("put", remote, audit.Entry{Path: rel, Bytes: audit.Bytes(int64(len(data)))}, err)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, proto.WriteResp{Path: rel, Bytes: len(data), SHA256: xfer.Sha256(data)})
}

func (s *Server) ListenAndServe() error {
	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// 长命令靠 client 的 timeout 控制，这里不设 WriteTimeout
	}
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	if !s.cfg.Quiet {
		// 走 stderr：stdout 留给「把地址管道给别的工具」的场景，日志照旧混在里面。
		targets := make([]fwdTarget, 0, len(s.fwd))
		for _, n := range s.fwdNames() {
			targets = append(targets, *s.fwd[n])
		}
		fmt.Fprint(os.Stderr, bannerWith(ln.Addr().String(), s.cfg.Token, s.root.Path(), s.cfg.Git, s.aud.Path(), targets...))
	}
	return srv.Serve(ln)
}

// decode 严格解码请求体：未知字段、必填字段缺失都当场 400。
//
// 这两条不是洁癖，是「静默降级」的解药：拼错参数名（"ignorecase" 而不是
// "ignore_case"）过去只会得到一份看起来正常的空结果，调用方会得出
// 「这仓库里没这个词」这种错误结论。宁可报错，也不要给一个像样的假答案。
//
// 必填 = proto 里没打 omitempty 的字段，和 openapi.json 的 required 同源。
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	// 限制 body：命令和文本改动都该是小的，大文件走 /raw
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		// encoding/json 只会说「结构体 proto.GrepReq 没有 ignorecase 字段」，
		// 这句话对 agent 没用；把字段名抠出来，配上候选字段名再回。
		if field, ok := unknownField(err); ok {
			writeErr(w, 400, unknownFieldMsg(field, jsonFields(reflect.TypeOf(v))))
			return false
		}
		writeErr(w, 400, "JSON 解析失败: "+err.Error())
		return false
	}
	// 字段名对、但写成了默认值的那些（"limit":0、"context":-1）过去一律静默
	// 当没传：调用方看到的是「结果正常」，实际拿到的是另一个查询。也要拦。
	if bad := invalidFieldValue(v); bad != "" {
		writeErr(w, 400, bad)
		return false
	}
	if missing := missingRequired(v); len(missing) > 0 {
		writeErr(w, 400, "缺少必填字段: "+strings.Join(missing, ", "))
		return false
	}
	return true
}

// invalidFieldValue 校验「字段名对、值没意义」的组合。
// 现在只有 GrepReq：limit 用 0 或负数表示「不限」是最容易想当然的写法，
// 而 wr 的 0 是缺省（50）、负数直接拒。在这里拦一次，比事后解释便宜。
func invalidFieldValue(v any) string {
	g, ok := v.(*proto.GrepReq)
	if !ok {
		return ""
	}
	switch {
	case g.Limit < 0:
		return "limit 必须 >= 1（缺省 " + strconv.Itoa(defaultGrepLimit) + "）；负数不表示不限"
	case g.Context < 0:
		return "context 必须 >= 0；不传就是 0 行"
	case g.Before < 0 || g.After < 0:
		return "before / after 必须 >= 0"
	}
	return ""
}

// unknownField 从 encoding/json 的报错里抠出未知字段名。
// 靠前缀匹配而不是解析整句：Go 版本换了措辞也不会静默退化成「解析失败」。
func unknownField(err error) (string, bool) {
	const p = "json: unknown field "
	msg := err.Error()
	if !strings.HasPrefix(msg, p) {
		return "", false
	}
	f, uerr := strconv.Unquote(strings.TrimSpace(strings.TrimPrefix(msg, p)))
	if uerr != nil {
		return "", false
	}
	return f, true
}

// unknownFieldMsg 把「打错的字段」翻译成「你是不是想写 X」。
// 拼错和忘了加下划线才是常态（"ignorecase" vs "ignore_case"），
// 所以匹配前先把字段名归一化掉大小写和下划线；
// 归一化也救不了的空字段名（例如往 /exec 传 "path"）就只报未知。
func unknownFieldMsg(field string, cands []string) string {
	msg := "未知字段 " + strconv.Quote(field)
	if sug := nearestField(field, cands); sug != "" {
		msg += "，你是不是想用 " + strconv.Quote(sug)
	}
	return msg + "？（字段名见 /api/openapi.json）"
}

// nearestField 找和 field 最像的字段名，差距超过 2 个编辑就放弃。
// 归一化之后 ignorecase 和 ignore_case 完全相同，等于精确命中。
func nearestField(field string, cands []string) string {
	best, bestD := "", 3
	nf := normalizeField(field)
	for _, c := range cands {
		if d := editDistance(nf, normalizeField(c)); d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

// normalizeField 抹掉大小写和下划线：json 字段名里这两个是最容易写错的部分。
func normalizeField(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "_", ""))
}

// editDistance 是经典 Levenshtein，字段名都很短，O(n²) 无所谓。
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// jsonFields 列出一个请求结构体接受的全部字段名，供拼写纠错用。
func jsonFields(t reflect.Type) []string {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out = append(out, name)
	}
	return out
}

// missingRequired 只认 proto 的 IsRequired：手写一份必填表迟早和协议脱节，
// 而「文档说必填、服务端放行」正是这套接口最坑人的失败模式。
func missingRequired(v any) []string {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Ptr && !rv.IsNil() {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}
	var missing []string
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Type().Field(i)
		if !proto.IsRequired(f) {
			continue
		}
		if rv.Field(i).IsZero() {
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			missing = append(missing, strconv.Quote(name))
		}
	}
	return missing
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // 不然 <、>、& 被转义，看 diff 很难受
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, proto.Error{Error: msg})
}
