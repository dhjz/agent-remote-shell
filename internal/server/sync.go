package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"cnb.cool/yyx946/tteist/internal/proto"
	"cnb.cool/yyx946/tteist/internal/xfer"
)

// sync 是 rsync 语义：一次请求同步整个目录，只搬差异。
//
// 判断「要不要传」的顺序按代价从低到高：
//  1. 远端没有 → 传
//  2. 大小不同 → 传
//  3. sha256 已知 → 服务端算一遍按内容判等，不同就传
//  4. mtime 落在远端 ± 容差内 → 跳过；否则传
//
// 第 4 步的方向很重要：默认是 **rsync 的 quick check**，即「mtime + size 一致就当没变」。
// 不能理解成「本地比远端旧就跳过」——PUT /raw 落盘时 mtime 是远端当下时间，
// 一个刚同步完的文件天然「远端更新」，按新旧判会导致每次全量重传。
// 本机复制保不住 mtime（cp 不带 -p、u 盘、FAT）时就轮到 -checksum：
// 全量算一遍 sha256 换准确性，代价是读一遍文件。
func (s *Server) SyncPlan(req proto.SyncPlanReq) (proto.SyncPlanResp, error) {
	var resp proto.SyncPlanResp
	base, err := s.syncBase(req.Dir)
	if err != nil {
		return resp, err
	}
	resp.Dir = cleanRel(req.Dir)

	tol := req.MtimeTolerance
	if tol <= 0 {
		tol = 2 // FAT 的粒度；按秒严格比会把刚传的又传一遍
	}

	want := make(map[string]proto.SyncItem, len(req.Want))
	for _, it := range req.Want {
		p := cleanRel(it.Path)
		if p == "" || p == "." {
			return resp, fmt.Errorf("路径为空")
		}
		if escapes(p) || filepath.IsAbs(it.Path) {
			return resp, fmt.Errorf("路径越出目录: %q", it.Path)
		}
		if xfer.InGit(p) {
			return resp, fmt.Errorf(".git 下不参与同步：%s", p)
		}
		if it.Dir {
			continue // 目录由 mkdirs 处理，不占清单
		}
		it.Path = p
		want[p] = it
	}

	// 远端多出来的：只有在 prune 时才动手。
	pruneSet := map[string]bool{}
	if req.Prune {
		err := filepath.WalkDir(base, func(abs string, d os.DirEntry, werr error) error {
			if werr != nil {
				return nil
			}
			if abs == base || d.IsDir() {
				return nil
			}
			rel, rerr := filepath.Rel(base, abs)
			if rerr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if _, ok := want[rel]; !ok {
				pruneSet[rel] = true
			}
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return resp, err
		}
	}

	for _, it := range want {
		p := it.Path
		fi, serr := os.Stat(filepath.Join(base, filepath.FromSlash(p)))
		switch {
		case serr != nil:
			resp.Upload = append(resp.Upload, it)
			resp.UploadBytes += it.Size
			continue
		case fi.IsDir():
			return resp, fmt.Errorf("远端 %s 是目录，本地是文件", p)
		}
		if fi.Size() != it.Size {
			resp.Upload = append(resp.Upload, it)
			resp.UploadBytes += it.Size
			continue
		}
		if req.Checksum || it.SHA256 != "" {
			sum, herr := fileSHA(filepath.Join(base, filepath.FromSlash(p)))
			if herr != nil || sum != it.SHA256 {
				resp.Upload = append(resp.Upload, it)
				resp.UploadBytes += it.Size
				continue
			}
			resp.Same = append(resp.Same, it)
			resp.SameBytes += it.Size
			continue
		}
		if diff := it.ModTime - fi.ModTime().Unix(); diff > int64(tol) || diff < -int64(tol) {
			resp.Upload = append(resp.Upload, it)
			resp.UploadBytes += it.Size
			continue
		}
		resp.Same = append(resp.Same, it)
		resp.SameBytes += it.Size
	}

	sort.Slice(resp.Upload, func(i, j int) bool { return resp.Upload[i].Path < resp.Upload[j].Path })
	sort.Slice(resp.Same, func(i, j int) bool { return resp.Same[i].Path < resp.Same[j].Path })
	for p := range pruneSet {
		resp.Delete = append(resp.Delete, p)
	}
	sort.Strings(resp.Delete)
	return resp, nil
}

// SyncApply 建目录、删多余。内容不在这里传：调用方随后逐个 PUT /raw。
// 顺序是「先建目录再删」：删错的情况比建不了的少，先保证写入能落地。
func (s *Server) SyncApply(req proto.SyncApplyReq) (proto.SyncApplyResp, error) {
	var resp proto.SyncApplyResp
	base, err := s.syncBase(req.Dir)
	if err != nil {
		return resp, err
	}
	resp.Dir = cleanRel(req.Dir)

	for _, d := range req.Mkdirs {
		p := cleanRel(d)
		if p == "" || p == "." {
			continue
		}
		if escapes(p) {
			return resp, fmt.Errorf("路径越出目录: %q", d)
		}
		if xfer.InGit(p) {
			return resp, s.inGitBlocked("sync_apply", p)
		}
		abs, rerr := s.root.Resolve(filepath.ToSlash(filepath.Join(cleanRel(req.Dir), filepath.FromSlash(p))))
		if rerr != nil {
			return resp, rerr
		}
		if mkerr := os.MkdirAll(abs, 0o755); mkerr != nil {
			return resp, mkerr
		}
		resp.Created++
	}

	for _, d := range req.Delete {
		p := cleanRel(d)
		if p == "" || p == "." {
			return resp, fmt.Errorf("拒绝删除同步根目录")
		}
		if escapes(p) {
			return resp, fmt.Errorf("路径越出目录: %q", d)
		}
		if xfer.InGit(p) {
			return resp, s.inGitBlocked("sync_apply", p)
		}
		abs, rerr := s.root.Resolve(filepath.ToSlash(filepath.Join(cleanRel(req.Dir), filepath.FromSlash(p))))
		if rerr != nil {
			return resp, rerr
		}
		// 只删文件：prune 的清单里只有文件。顺手支持空目录收敛（rsync --delete 的副作用）。
		if rmErr := os.Remove(abs); rmErr != nil && !os.IsNotExist(rmErr) {
			return resp, rmErr
		}
		resp.Deleted++
		s.pruneEmptyDirs(filepath.Dir(abs), base)
	}
	return resp, nil
}

// pruneEmptyDirs 从 leaf 往上删空目录，到 stop 为止。
// rsync --delete 之后远端会留下空壳目录，人肉删很烦。
func (s *Server) pruneEmptyDirs(leaf, stop string) {
	for {
		if leaf == stop || !strings.HasPrefix(leaf, stop) || leaf == s.root.Path() {
			return
		}
		ents, err := os.ReadDir(leaf)
		if err != nil || len(ents) > 0 {
			return
		}
		if os.Remove(leaf) != nil {
			return
		}
		leaf = filepath.Dir(leaf)
	}
}

// syncBase 解析同步目标目录并建好它：目标不存在是正常情况（首次同步）。
func (s *Server) syncBase(dir string) (string, error) {
	rel := cleanRel(dir)
	if xfer.InGit(rel) {
		return "", fmt.Errorf(".git 下不参与同步")
	}
	abs, err := s.root.Resolve(relOrDot(rel))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("同步目标 %q 不是目录", dir)
	}
	return abs, nil
}

// cleanRel 统一成正斜杠、去掉 ./ 与尾随 /。
// 结果仍可能含 ".."：清理只是归一化，越界与否由调用方决定——
// 同步清单里带着 ".." 一律拒绝，宁可报错也不要「看起来清了其实逃了」。
func cleanRel(p string) string {
	p = filepath.ToSlash(strings.TrimSpace(p))
	p = strings.TrimPrefix(p, "./")
	if p == "" {
		return "."
	}
	return path.Clean(p)
}

// fileSHA 流式算 sha256：同步目录里可能有几十 MB 的产物，不该整个读进内存。
func fileSHA(abs string) (string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// escapes 判断归一化后的相对路径是否还想往外走。
// 注意不能用 path.Clean 之后的 HasPrefix("../")：Clean("..") 就是 ".." 本身。
func escapes(p string) bool {
	return p == ".." || strings.HasPrefix(p, "../")
}
