// Package xfer 是所有文件写入的唯一入口：权限沙箱 + 原子写 + 一致性校验。
package xfer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var (
	ErrEscaped = errors.New("路径越出 root 沙箱")
	ErrIsDir   = errors.New("目标是目录")
	ErrIsRoot  = errors.New("不能操作沙箱根目录本身")
)

// GitDir 是沙箱内唯一允许「读」的隐藏目录。
// agent 在远端最需要的就是 git diff / log，把 .git 一刀切掉等于让它瞎着眼改代码。
// 只读的边界靠 Root.ResolveRead 守：写路径仍然拒绝 .git 下的任何东西。
const GitDir = ".git"

// Root 仓库根沙箱。所有文件路径必须先过 Resolve。
type Root struct {
	abs string
}

func NewRoot(p string) (*Root, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("root 不存在: %s", abs)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("root 不是目录: %s", abs)
	}
	return &Root{abs: abs}, nil
}

func (r *Root) Path() string { return r.abs }

// Resolve 把用户给的路径规整成绝对路径，并保证在 root 内。
// 用 EvalSymlinks 处理「root 内软链指向 root 外」这种绕过。
func (r *Root) Resolve(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("路径为空")
	}
	if filepath.IsAbs(p) || (len(p) > 1 && p[1] == ':') {
		return "", fmt.Errorf("%w: 不接受绝对路径 %q", ErrEscaped, p)
	}
	joined := filepath.Join(r.abs, filepath.FromSlash(p))
	if !within(r.abs, joined) {
		return "", fmt.Errorf("%w: %q", ErrEscaped, p)
	}
	// 已存在的部分做软链解析
	if real, err := filepath.EvalSymlinks(joined); err == nil {
		if !within(r.abs, real) {
			return "", fmt.Errorf("%w: %q 指向沙箱外", ErrEscaped, p)
		}
		return real, nil
	}
	// 还不存在（write -create）：解析最近的已存在父目录
	dir := filepath.Dir(joined)
	for {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			if !within(r.abs, real) {
				return "", fmt.Errorf("%w: 父目录指向沙箱外", ErrEscaped)
			}
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir || !within(r.abs, parent) {
			break
		}
		dir = parent
	}
	return joined, nil
}

// InGit 判断相对路径是否落在某个 .git 里——**任意一层**都算，不只是根下那个。
// sub/wt/.git 是 worktree 的入口文件，sub/.git 是沙箱里嵌另一个仓库时的索引与对象库；
// 只认根目录那一层等于给"嵌套仓库"留了个后门。
//
// 沙箱本身放行 .git（否则 git status 会直接报 "not a git repository"），
// 写入侧靠它拦：agent 该改工作区，不该改索引和对象库。
func InGit(p string) bool {
	p = filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	if p == "." {
		return false // Clean("") 回来的是 "."
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == GitDir {
			return true
		}
	}
	return false
}

// Root.InGitAbs 回答的是同一个问题，输入换成**已解析**的绝对路径。
//
// 为什么不能只看相对路径：Resolve 会对存在的路径做 EvalSymlinks，回来的是
// 真实路径。软链 .git -> ../ext 时相对路径写作 ".git"，真实路径早就出了沙箱；
// 反过来 index 不在 .git 目录里、只是一个文件（worktree 的 ".git" 文件）时，
// 相对路径那层判断也够用，但绝对路径这层能一并拦住「软链指着别处的 .git」。
//
// 只做前缀比较，不碰文件系统：调用方拿到的已经是 EvalSymlinks 之后的结果。
func (r *Root) InGitAbs(abs string) bool {
	return within(filepath.Join(r.abs, GitDir), abs)
}

// Remove 删除文件或目录。目录必须显式 recursive——
// 少一个 flag 比多一次误删便宜得多。
func Remove(abs string, recursive bool) (int, error) {
	fi, err := os.Lstat(abs)
	if err != nil {
		return 0, err
	}
	if fi.IsDir() {
		if !recursive {
			if empty, _ := isEmptyDir(abs); !empty {
				return 0, fmt.Errorf("%s 是目录且非空；确实要删请加 recursive", filepath.Base(abs))
			}
		}
		return countEntries(abs), os.RemoveAll(abs)
	}
	return 1, os.Remove(abs)
}

// Move 用 rename；跨盘（Windows 上很常见）rename 会失败，退化成 copy+remove。
func Move(from, to string, overwrite bool) error {
	if !overwrite {
		if _, err := os.Lstat(to); err == nil {
			return fmt.Errorf("%s 已存在；要覆盖请加 overwrite", filepath.Base(to))
		}
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	err := os.Rename(from, to)
	if err == nil {
		return nil
	}
	if _, statErr := os.Lstat(from); statErr != nil {
		return err
	}
	if copyErr := copyTree(from, to); copyErr != nil {
		return copyErr
	}
	_, rmErr := Remove(from, true)
	return rmErr
}

func isEmptyDir(p string) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return len(names) == 0, nil
}

func countEntries(root string) int {
	n := 0
	_ = filepath.WalkDir(root, func(_ string, _ os.DirEntry, err error) error {
		if err == nil {
			n++
		}
		return nil
	})
	return n
}

func copyTree(from, to string) error {
	fi, err := os.Lstat(from)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		if err := os.MkdirAll(to, fi.Mode().Perm()); err != nil {
			return err
		}
		ents, err := os.ReadDir(from)
		if err != nil {
			return err
		}
		for _, e := range ents {
			if err := copyTree(filepath.Join(from, e.Name()), filepath.Join(to, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(from)
		if err != nil {
			return err
		}
		_ = os.Remove(to)
		return os.Symlink(target, to)
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	data, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	return WriteAtomic(to, data, fi.Mode().Perm())
}

// SortedNames 只给测试和调试用，稳定输出。
func SortedNames(p string) []string {
	ents, err := os.ReadDir(p)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func within(root, p string) bool {
	root = strings.TrimRight(root, string(os.PathSeparator))
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+string(os.PathSeparator))
}

// WriteAtomic 先写临时文件再 rename。半截文件比写失败更难查。
func WriteAtomic(abs string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	fi, err := os.Stat(abs)
	if err == nil {
		if fi.IsDir() {
			return ErrIsDir
		}
		perm = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, ".wr-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		return err
	}
	return os.Rename(name, abs)
}

func Sha256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
