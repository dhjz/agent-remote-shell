package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"cnb.cool/yyx946/tteist/internal/client"
	"cnb.cool/yyx946/tteist/internal/proto"
	"cnb.cool/yyx946/tteist/internal/server"
)

// cmdSync 把一个本地目录增量推到远端。
//
// 为什么不是「循环调 put」：put 不知道该传什么，每次都全量。
// 「部署到 B」的场景里，一个 dist 目录整体上传是常态，全量的代价肉眼可见。
// 所以先让服务端比对清单（/sync/plan），再只传差异（/raw），最后收敛多余文件。
func cmdSync(args []string) error {
	fs := newFlagSet("sync", flag.ExitOnError)
	var c common
	c.bind(fs)
	to := fs.String("to", "", "远端目标目录（相对沙箱根）；不传则用本地目录名")
	del := fs.Bool("delete", false, "远端多出来的文件也删掉（rsync --delete）")
	dry := fs.Bool("dry-run", false, "只比对，不传不删")
	checksum := fs.Bool("checksum", false, "按 sha256 判断，不看 mtime（mtime 不可信时用）")
	noIgnore := fs.Bool("no-ignore", false, "不读 .gitignore，被忽略的文件也同步")
	workers := fs.Int("j", 4, "并发上传数")
	excludes := fs.String("exclude", "", "额外排除，逗号分隔的路径片段，如 .git,node_modules")
	// sync 是客户端子命令，短名同理不给：-d 在这里只会让人猜是 dir 还是 dry-run。
	pos := parse(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("用法: wr sync <本地目录> [-to remotepath] [-delete] [-dry-run] [-checksum]")
	}
	src := pos[0]
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s 不是目录；单个文件用 wr put", src)
	}
	remote := *to
	if remote == "" {
		remote = filepath.Base(filepath.Clean(src))
	}
	cl, err := c.client()
	if err != nil {
		return err
	}

	// 1. 本地清单。sha256 只在 checksum 模式算——否则等于把每个文件多读一遍。
	items, err := localManifest(src, *noIgnore, *checksum, *excludes)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Fprintf(os.Stderr, "[wr] %s 里没有可同步的文件（被 .gitignore 全挡了？试 -no-ignore）\n", src)
	}

	// 2. 服务端比对
	plan, err := cl.SyncPlan(proto.SyncPlanReq{Dir: remote, Want: items, Prune: *del, Checksum: *checksum})
	if err != nil {
		return err
	}

	// 3. 建目录 / 删除，再传文件
	mkdirs := neededDirs(plan.Upload)
	needApply := len(mkdirs) > 0 || len(plan.Delete) > 0
	if *dry {
		reportSync(src, remote, items, plan, true)
		fmt.Fprintln(os.Stderr, "[wr] dry-run：什么都没改")
		return nil
	}
	if needApply {
		if _, err := cl.SyncApply(proto.SyncApplyReq{Dir: remote, Mkdirs: mkdirs, Delete: plan.Delete}); err != nil {
			return err
		}
	}
	if err := uploadAll(cl, src, remote, plan.Upload, *workers); err != nil {
		return err
	}
	reportSync(src, remote, items, plan, false)
	return nil
}

// localManifest 走一遍源目录，产出清单。规则和 grep/glob 一致：
// 默认读 .gitignore、跳过 .git/node_modules，符号链接不跟（环 + 外逃都不值得）。
//
// 这里不调服务端 /grep 之类的端点去「借」文件列表，因为清单必须在本地算——
// 同步的方向是本地 → 远端，远端不知道本地有什么。
func localManifest(src string, noIgnore, checksum bool, excludes string) ([]proto.SyncItem, error) {
	var extra []string
	for _, e := range strings.Split(excludes, ",") {
		if e = strings.TrimSpace(e); e != "" {
			extra = append(extra, e)
		}
	}
	skip := func(rel string) bool {
		for _, e := range extra {
			if rel == e || strings.HasPrefix(rel, e+"/") || strings.Contains("/"+rel+"/", "/"+e+"/") {
				return true
			}
		}
		return false
	}

	// 复用服务端的 .gitignore 解析与匹配（server.IgnoreCtx）：两边对「什么被忽略」
	// 必须完全一致，否则本地算出来的清单和远端的视野就对不上。
	// 根目录自己也得读一次：.gitignore 通常就在这一层。
	var ign *ignoreStack
	if !noIgnore {
		ign = &ignoreStack{}
		ign.push(src, "")
	}
	var out []proto.SyncItem

	// walk 在「已进到 dir」的状态下遍历 dir 的内容。
	// 目录进的时候先 Push、出的时候先 Pop（等价 defer），否则子目录的
	// 规则会漏到还没遍历的兄弟目录上——node_modules 就在这栽过一次。
	var walk func(dir, rel string) error
	walk = func(dir, rel string) error {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range ents {
			name := e.Name()
			if name == ".git" || name == ".wr-jobs" {
				continue
			}
			if name == ".gitignore" && ign != nil {
				continue // 配置，不是产物
			}
			childRel := name
			if rel != "" {
				childRel = rel + "/" + name
			}
			if skip(childRel) {
				continue
			}
			abs := filepath.Join(dir, name)
			if e.IsDir() {
				if e.Type()&os.ModeSymlink != 0 {
					continue
				}
				// 内置软名单（dist/build/target…）不参与同步：
				// grep 是「不进这些目录」，同步是「搬产物」——语义不同，
				// 所以这里只认 .gitignore。
				if ign != nil && ign.match(childRel, true) {
					continue
				}
				if ign != nil {
					ign.push(abs, childRel)
				}
				err := walk(abs, childRel)
				if ign != nil {
					ign.pop()
				}
				if err != nil {
					return err
				}
				continue
			}
			if !e.Type().IsRegular() {
				continue
			}
			if ign != nil && ign.match(childRel, false) {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			it := proto.SyncItem{
				Path: childRel, Size: info.Size(), ModTime: info.ModTime().Unix(),
			}
			if checksum {
				sum, herr := localSHA(abs)
				if herr != nil {
					return herr
				}
				it.SHA256 = sum
			}
			out = append(out, it)
		}
		return nil
	}
	if err := walk(src, ""); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// ignoreStack 把「进目录压规则、出目录弹规则」包成一个小类型。
// 关键：Push/Pop 改的是结构体自己，不能写成 `*c = load(..., *c)` 再靠
// 指针调用——那样 Pop 永远只能弹到第一层的拷贝上（踩过）。
// 用嵌套指针是为了让这份状态在闭包里稳定存在。
type ignoreStack struct {
	c *server.IgnoreCtx
}

func (s *ignoreStack) push(dir, rel string) {
	if s.c == nil {
		s.c = &server.IgnoreCtx{}
	}
	s.c.Push(dir, rel)
}

func (s *ignoreStack) pop() {
	if s.c != nil {
		s.c.Pop()
	}
}

func (s *ignoreStack) match(rel string, isDir bool) bool {
	return s.c != nil && s.c.Match(rel, isDir)
}

func localSHA(abs string) (string, error) {
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

// neededDirs 只算「要传文件的那些目录」，不铺整棵树。
// 传 1 个文件不该在远端建出 20 个空目录。
func neededDirs(items []proto.SyncItem) []string {
	set := map[string]bool{}
	for _, it := range items {
		d := path.Dir(it.Path)
		for d != "." && d != "/" && d != "" {
			if set[d] {
				break
			}
			set[d] = true
			d = path.Dir(d)
		}
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

func uploadAll(cl *client.Client, src, remote string, items []proto.SyncItem, workers int) error {
	if len(items) == 0 {
		return nil
	}
	if workers <= 0 {
		workers = 4
	}
	if workers > 16 {
		workers = 16
	}
	queue := make(chan proto.SyncItem)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	done, total := 0, len(items)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range queue {
				mu.Lock()
				failed := firstErr != nil
				mu.Unlock()
				if failed {
					continue
				}
				local := filepath.Join(src, filepath.FromSlash(it.Path))
				target := path.Join(remote, it.Path)
				if _, err := cl.PutRaw(target, local); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("上传 %s: %w", it.Path, err)
					}
					mu.Unlock()
					continue
				}
				mu.Lock()
				done++
				fmt.Fprintf(os.Stderr, "[wr] %d/%d  %s\n", done, total, it.Path)
				mu.Unlock()
			}
		}()
	}
	for _, it := range items {
		queue <- it
	}
	close(queue)
	wg.Wait()
	return firstErr
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func reportSync(src, remote string, items []proto.SyncItem, plan proto.SyncPlanResp, dry bool) {
	var localBytes int64
	for _, it := range items {
		localBytes += it.Size
	}
	verb := "上传"
	if dry {
		verb = "将上传"
	}
	fmt.Fprintf(os.Stderr, "[wr] %s → %s  文件 %d（%s）\n", src, remote, len(items), humanBytes(localBytes))
	fmt.Fprintf(os.Stderr, "[wr] %s %d 个（%s）  跳过 %d 个（%s）",
		verb, len(plan.Upload), humanBytes(plan.UploadBytes), len(plan.Same), humanBytes(plan.SameBytes))
	if len(plan.Delete) > 0 {
		fmt.Fprintf(os.Stderr, "  删除 %d 个", len(plan.Delete))
	}
	fmt.Fprintln(os.Stderr)
	if dry {
		for _, u := range plan.Upload {
			fmt.Printf("+ %s\n", u.Path)
		}
		for _, d := range plan.Delete {
			fmt.Printf("- %s\n", d)
		}
	}
}
