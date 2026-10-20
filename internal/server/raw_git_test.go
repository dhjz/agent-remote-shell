package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"cnb.cool/yyx946/tteist/internal/xfer"
)

// /raw 的读侧也要挡 .git：worktree 的 .git 是个文件，内容就是沙箱外那条
// 绝对路径；仓库自己的 .git/config 又可能带凭据。两者都不该顺着下载口出去。
func TestRawGetBlocksGit(t *testing.T) {
	s := newTestServer(t)
	root := s.root.Path()

	// git init：真仓库的样子最省事，也顺带覆盖 .git/config 这种「文件在但别处也有口子」的情况
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[remote \"origin\"]\n\turl = https://u:p@example.com/r.git\n"), 0o644)
	// worktree 的样子：.git 是文件，内容指到仓外
	os.MkdirAll(filepath.Join(root, "wt"), 0o755)
	os.WriteFile(filepath.Join(root, "wt", ".git"), []byte("gitdir: /outside/repo/.git/worktrees/wt\n"), 0o644)

	for _, p := range []string{"wt/.git", ".git/config"} {
		w := httptest.NewRecorder()
		s.handleRawGet(w, httptest.NewRequest(http.MethodGet, "/raw?path="+p, nil))
		if w.Code != 400 {
			t.Errorf("%s 应当被拒，实际 %d: %s", p, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "gitdir:") || strings.Contains(w.Body.String(), "example.com") {
			t.Errorf("%s 的内容漏出去了: %s", p, w.Body.String())
		}
	}

	// 普通文件照旧能下
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("ok"), 0o644)
	w := httptest.NewRecorder()
	s.handleRawGet(w, httptest.NewRequest(http.MethodGet, "/raw?path=a.txt", nil))
	if w.Code != 200 || w.Body.String() != "ok" {
		t.Fatalf("普通文件应当能下，实际 %d: %s", w.Code, w.Body.String())
	}
}

// 判断按「任意一层 .git」算：嵌套仓库的 .git 不能被当成普通目录。
func TestInGitAnyLevel(t *testing.T) {
	yes := []string{".git", ".git/config", "sub/.git", "sub/wt/.git", "./.git/index"}
	no := []string{".gitignore", "a/.gitmodules", "git", "sub/a.git/x", ""}
	for _, p := range yes {
		if !xfer.InGit(p) {
			t.Errorf("%q 应当判为 .git 内", p)
		}
	}
	for _, p := range no {
		if xfer.InGit(p) {
			t.Errorf("%q 不该判为 .git 内", p)
		}
	}
}

// InGitAbs 只看前缀，不碰文件系统：软链指向哪由 Resolve 管（出沙箱就拒），
// 这里保证「解析出来的路径落在沙箱 .git 里」这一条判断不会把外面的算进来。
func TestRootInGitAbs(t *testing.T) {
	s := newTestServer(t)
	git := filepath.Join(s.root.Path(), ".git")
	if !s.root.InGitAbs(git) || !s.root.InGitAbs(filepath.Join(git, "config")) {
		t.Fatal("sandbox/.git 及其子路径应当判为 .git 内")
	}
	for _, p := range []string{s.root.Path(), filepath.Join(s.root.Path(), "a.txt"), t.TempDir()} {
		if s.root.InGitAbs(p) {
			t.Errorf("%s 不该判为 .git 内", p)
		}
	}
}
