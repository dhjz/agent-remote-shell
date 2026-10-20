package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"cnb.cool/yyx946/tteist/internal/proto"
)

func mustWrite(t *testing.T, s *Server, rel, content string) {
	t.Helper()
	if _, err := s.Write(proto.WriteReq{Path: rel, Content: content}); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// 新建文件不该再需要额外 flag——这是 agent 最常踩的一次失败。
func TestWriteCreatesByDefault(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, "a/b/new.go", "package a\n")
	if _, err := os.Stat(filepath.Join(s.root.Path(), "a/b/new.go")); err != nil {
		t.Fatal(err)
	}
}

func TestWriteOverwriteGuard(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, "x.txt", "1")
	no := false
	if _, err := s.Write(proto.WriteReq{Path: "x.txt", Content: "2", Overwrite: &no}); err == nil {
		t.Fatal("overwrite=false 时覆盖已有文件应报错")
	}
	yes := true
	if _, err := s.Write(proto.WriteReq{Path: "x.txt", Content: "2", Overwrite: &yes}); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteFileAndDir(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, "keep.txt", "1")
	mustWrite(t, s, "d/1.txt", "1")
	mustWrite(t, s, "d/2.txt", "2")

	if _, err := s.Delete(proto.DeleteReq{Path: "keep.txt"}); err != nil {
		t.Fatal(err)
	}
	// 非空目录不带 recursive 必须拒绝
	if _, err := s.Delete(proto.DeleteReq{Path: "d"}); err == nil {
		t.Fatal("非空目录不加 recursive 应报错")
	}
	resp, err := s.Delete(proto.DeleteReq{Path: "d", Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Removed < 2 {
		t.Errorf("removed=%d，应统计目录内条目", resp.Removed)
	}
	if _, err := os.Stat(filepath.Join(s.root.Path(), "d")); !os.IsNotExist(err) {
		t.Error("目录应该没了")
	}
}

func TestDeleteRefusesRoot(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.Delete(proto.DeleteReq{Path: ".", Recursive: true}); err == nil {
		t.Fatal("删根目录必须拒绝")
	}
}

func TestMove(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, "a.txt", "hello")
	if _, err := s.Move(proto.MoveReq{From: "a.txt", To: "sub/b.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.root.Path(), "sub/b.txt")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, s, "c.txt", "x")
	if _, err := s.Move(proto.MoveReq{From: "c.txt", To: "sub/b.txt"}); err == nil {
		t.Fatal("目标已存在且未 overwrite 时应报错")
	}
	if _, err := s.Move(proto.MoveReq{From: "c.txt", To: "sub/b.txt", Overwrite: true}); err != nil {
		t.Fatal(err)
	}
}

func TestMkdir(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.Mkdir(proto.MkdirReq{Path: "a/b/c"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(s.root.Path(), "a/b/c")); err != nil || !fi.IsDir() {
		t.Fatal("目录没建出来")
	}
}

// .git 是唯一被放行的隐藏目录，但只放行读：写进去会把索引搞坏。
func TestGitReadOnly(t *testing.T) {
	s := newTestServer(t)
	gitDir := filepath.Join(s.root.Path(), ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Read(proto.ReadReq{Path: ".git/HEAD"}); err != nil {
		t.Fatalf("应该能读 .git/HEAD: %v", err)
	}
	if _, err := s.Write(proto.WriteReq{Path: ".git/HEAD", Content: "x"}); err == nil {
		t.Fatal("写 .git 必须拒绝")
	}
	if _, err := s.Delete(proto.DeleteReq{Path: ".git/HEAD"}); err == nil {
		t.Fatal("删 .git 必须拒绝")
	}
	if _, err := s.Move(proto.MoveReq{From: ".git/HEAD", To: "HEAD"}); err == nil {
		t.Fatal("搬 .git 必须拒绝")
	}

	// 列目录默认不把 .git 铺开，但 .git 自己作为一项可以看到
	resp, err := s.List(proto.ListReq{Dir: "."})
	if err != nil {
		t.Fatal(err)
	}
	var sawGit bool
	for _, e := range resp.Entries {
		if e.Name == ".git" {
			sawGit = true
		}
	}
	if !sawGit {
		t.Error("根目录应该能看到 .git")
	}
	// 进到 .git 里面才列内容
	inner, err := s.List(proto.ListReq{Dir: ".git"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inner.Entries) == 0 {
		t.Error(".git 下应该能列到 HEAD")
	}
}

// glob/grep 默认跳过 .git：搜代码搜到对象库纯属噪音。
func TestGlobSkipsGit(t *testing.T) {
	s := newTestServer(t)
	if err := os.MkdirAll(filepath.Join(s.root.Path(), ".git", "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, s, "main.go", "package main\n")
	if err := os.WriteFile(filepath.Join(s.root.Path(), ".git", "config"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err := s.Glob(proto.GlobReq{Pattern: "**/*"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range resp.Files {
		if strings.HasPrefix(f, ".git/") {
			t.Errorf("glob 不该命中 %s", f)
		}
	}
}

func TestListHasMeta(t *testing.T) {
	s := newTestServer(t)
	mustWrite(t, s, "f.txt", "12345")
	resp, err := s.List(proto.ListReq{Dir: "."})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("entries=%d", len(resp.Entries))
	}
	e := resp.Entries[0]
	if e.Size != 5 || e.ModTime == 0 || e.Dir {
		t.Errorf("条目信息不全: %+v", e)
	}
}

// 仓库根就是 root 时，exec 里的 git 必须能跑起来。
// 这里用一个真的 git init 出来的仓库——手搭一个空 .git 目录
// 只能证明「有目录」，证明不了 git 能认。
func TestExecGitAvailable(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("环境没有 git")
	}
	root := t.TempDir()
	initGit(t, root)
	s, err := New(Config{Token: "0123456789abcdef0123", Root: root, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.cfg.Git == "" {
		t.Fatal("detectGit 没认出 .git")
	}
	resp, err := s.Exec(context.Background(), proto.ExecRequest{Cmd: "git rev-parse --git-dir", Shell: "sh"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("git 跑不起来 exit=%d stderr=%s", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, ".git") {
		t.Errorf("输出不像 git 仓库: %q", resp.Stdout)
	}
}

// root 是上层目录（D:\repos 这种）时，也要能定位到子目录里的仓库。
func TestDetectGitInSubdir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("环境没有 git")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "proj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	initGit(t, repo)
	s, err := New(Config{Token: "0123456789abcdef0123", Root: root, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.cfg.GitAt != "proj" {
		t.Errorf("GitAt=%q want proj", s.cfg.GitAt)
	}
}

// 假 .git（空目录）不能认，否则 GIT_DIR 会让所有 git 命令报 not a repository。
func TestDetectGitRejectsFake(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Token: "0123456789abcdef0123", Root: root, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.cfg.Git != "" {
		t.Errorf("空 .git 目录不该被认成仓库: %s", s.cfg.Git)
	}
}

func initGit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "wr@example.com"},
		{"config", "user.name", "wr"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

// openapiPath 把端点清单里的路径参数占位（`<name>`）换成 OpenAPI 的写法（`{name}`）。
// 两边必须对得上，但路由本身不该为了文档好看而写成假路径。
func openapiPath(ep string) string {
	method, p, _ := strings.Cut(ep, " ")
	// ANY 是「任意方法」：注册时一个目标一条路由，文档里也只写方法无关的那条路径。
	if method == "ANY" {
		method = "GET"
	}
	var b strings.Builder
	for {
		i := strings.IndexByte(p, '<')
		if i < 0 {
			b.WriteString(p)
			break
		}
		j := strings.IndexByte(p[i:], '>')
		if j < 0 {
			b.WriteString(p)
			break
		}
		b.WriteString(p[:i] + "{" + p[i+1:i+j] + "}")
		p = p[i+j+1:]
		if strings.HasPrefix(p, "/...") {
			// 尾巴上的 ... 表示「后面任意路径」，对应 OpenAPI 的 {path}。
			b.WriteString("/{path}")
			p = ""
			break
		}
	}
	return method + " " + b.String()
}

// sameMethod 把两边的方法对齐到同一个坐标系。
// 路由写成 ANY（任意方法都走这里），spec 里只能挑一个方法写，
// 比的时候把 ANY 当作「这条路径上的任意方法都算数」。
func sameMethod(a, b string) bool {
	if a == b {
		return true
	}
	return a == "ANY" || b == "ANY"
}

// 每条注册的路由都必须在 openapi 里有对应描述，反之亦然。
// 这条测试是「文档不会腐」的唯一保证。
func TestOpenAPICoversRoutes(t *testing.T) {
	s := newTestServer(t)
	spec := s.spec()
	paths := spec["paths"].(map[string]any)

	seen := map[string]bool{}
	for p, raw := range paths {
		for m := range raw.(map[string]any) {
			seen[strings.ToUpper(m)+" "+p] = true
		}
	}
	for _, ep := range endpointList() {
		method, p, _ := strings.Cut(openapiPath(ep), " ")
		raw, _ := paths[p].(map[string]any)
		found := false
		for m := range raw {
			// spec 里的方法 key 是小写的，比之前统一大写。
			if sameMethod(strings.ToUpper(method), strings.ToUpper(m)) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("路由 %s 没写进 openapi", ep)
		}
	}
	for ep := range seen {
		found := false
		for _, e := range endpointList() {
			em, epath, _ := strings.Cut(openapiPath(e), " ")
			sm, spath, _ := strings.Cut(ep, " ")
			if epath == spath && sameMethod(strings.ToUpper(em), strings.ToUpper(sm)) {
				found = true
				break
			}
		}
		// GET / 是根路径的兜底（空 204，不列进 /info 的端点清单——它不是能力），
		// 所以只在 openapi 里出现、不在 endpointList 里是故意的。
		if !found && ep != "GET /" {
			t.Errorf("openapi 里的 %s 不在端点清单里", ep)
		}
	}
}

// openapi.md 是给人看的版本，容易写忘了。至少保证每个端点都在文档里露过面。
func TestOpenAPIMarkdownInSync(t *testing.T) {
	data, err := os.ReadFile("../../openapi.md")
	if err != nil {
		t.Skipf("读不到 openapi.md: %v", err)
	}
	md := string(data)
	for _, ep := range endpointList() {
		// 路由写成 /fwd/<name>/...，文档里写成 /fwd/{name}/{path}，按同一形式比。
		if _, path, _ := strings.Cut(openapiPath(ep), " "); !strings.Contains(md, path) {
			t.Errorf("openapi.md 里没有 %s", ep)
		}
	}
}

func TestOpenAPISchemas(t *testing.T) {
	s := newTestServer(t)
	spec := s.spec()
	comps := spec["components"].(map[string]any)["schemas"].(map[string]any)
	for _, want := range []string{"ExecRequest", "WriteReq", "DeleteReq", "MoveReq", "MkdirReq", "JobStartReq", "JobLogsReq", "FwdReq", "FwdResult"} {
		if comps[want] == nil {
			t.Errorf("schemas 缺 %s", want)
		}
	}
	// 每个 $ref 都要能解析到
	raw := toJSON(t, spec)
	for _, ref := range findRefs(raw) {
		name := strings.TrimPrefix(ref, "#/components/schemas/")
		if comps[name] == nil {
			t.Errorf("悬空 $ref: %s", ref)
		}
	}
}

// 根路径什么都不给：不返回 HTML 目录页，不返回接口清单，连 404 的 body 里
// 都不提文档在哪。暴露在网卡上的服务，"/" 回一页漂亮的 API 目录
// 等于把地图递给扫描器——它们正是从 GET / 开始敲的。
func TestRootLeaksNothing(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	// "/" 只回空 body 的 204：既不列目录，也不提文档在哪。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("GET / 应该是 204，实际 %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("GET / 的 body 该是空的，实际 %q", rec.Body.String())
	}

	// /log 是有意留下的唯一页面；除此之外根下什么都不认。
	for _, path := range []string{"/openapi.json", "/api/doc/", "/swagger", "/index.html", "/log.html"} {
		if path == "/api/doc/" {
			continue // 结尾多一个斜杠会被 ServeMux 归一化，行为不值得押
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s 应该是 404，实际 %d", path, rec.Code)
		}
		body := rec.Body.String()
		for _, leak := range []string{"/exec", "/write", "Bearer", "openapi"} {
			if strings.Contains(body, leak) {
				t.Errorf("GET %s 的 404 body 泄露了 %q：%s", path, leak, body)
			}
		}
	}
}

// 文档本身还得能拿到，只是挂在 /api 下；并且要告诉爬虫别记。
func TestDocEndpointsUnderAPI(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	// 文档要 token：没带就 401，带了才给——接口清单和 /info 同级，不该白送。
	for _, path := range []string{"/api/openapi.json", "/api/doc", "/api"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s 不带 token 该 401，实际 %d", path, rec.Code)
		}
	}

	for _, path := range []string{"/api/openapi.json", "/api/doc", "/api"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path+"?token=0123456789abcdef0123", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s 带 token 应该是 200，实际 %d", path, rec.Code)
		}
		if got := rec.Header().Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
			t.Errorf("GET %s 缺 X-Robots-Tag: noindex，实际 %q", path, got)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/openapi.json?token=0123456789abcdef0123", nil))
	if !strings.Contains(rec.Body.String(), "\"/exec\"") {
		t.Error("/api/openapi.json 没返回接口清单")
	}
}

// 「恰好 limit 条」不是截断：旧实现把 ==limit 当成截断，让「就这些」和
// 「还有更多」长得一模一样，调用方只能瞎猜。
func TestGlobNotTruncatedWhenExactlyLimit(t *testing.T) {
	s := newTestServer(t)
	for i := 0; i < 5; i++ {
		mustWrite(t, s, fmt.Sprintf("d%d/a.go", i), "x")
	}
	resp, err := s.Glob(proto.GlobReq{Pattern: "**/*.go", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Truncated {
		t.Errorf("恰好 %d 条不该标 truncated：total=%d", len(resp.Files), resp.TotalFiles)
	}
	if resp.TotalFiles != 5 {
		t.Errorf("total_files=%d want 5", resp.TotalFiles)
	}

	resp, err = s.Glob(proto.GlobReq{Pattern: "**/*.go", Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Truncated || resp.TotalFiles != 5 {
		t.Errorf("limit=4 时 truncated=%v total=%d，期望 true/5", resp.Truncated, resp.TotalFiles)
	}
}

// 截断给的是字典序前 N 条，不是「碰巧先遍历到的 N 条」。
// 并发 worker 的完成顺序不稳定，所以这条断言同时防「结果不可复现」。
func TestGlobTruncatedIsLexicographic(t *testing.T) {
	s := newTestServer(t)
	const n = 40
	for i := 0; i < n; i++ {
		mustWrite(t, s, fmt.Sprintf("pkg/z%02d/f.go", i), "x")
	}
	mustWrite(t, s, "pkg/a00/f.go", "x")

	var want []string
	for i := 0; i < n; i++ {
		want = append(want, fmt.Sprintf("pkg/z%02d/f.go", i))
	}
	want = append(want, "pkg/a00/f.go")
	sort.Strings(want)

	for round := 0; round < 3; round++ {
		resp, err := s.Glob(proto.GlobReq{Pattern: "pkg/**/*.go", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if !resp.Truncated || resp.TotalFiles != n+1 {
			t.Fatalf("truncated=%v total=%d，期望 true/%d", resp.Truncated, resp.TotalFiles, n+1)
		}
		if len(resp.Files) != 10 {
			t.Fatalf("给了 %d 条，期望 10", len(resp.Files))
		}
		if !reflect.DeepEqual(resp.Files, want[:10]) {
			t.Fatalf("第 %d 轮结果不是字典序前 10：%v", round, resp.Files)
		}
	}
}
