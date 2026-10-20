package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"cnb.cool/yyx946/tteist/internal/audit"
	"cnb.cool/yyx946/tteist/internal/proto"
	"cnb.cool/yyx946/tteist/internal/xfer"
)

// skipDir 是「浏览/搜索时跳过」的目录，不是沙箱边界。
// .git 在这里面：agent 找代码不该翻索引和对象库，
// 但需要 git 本身能跑（见 exec.go 的 gitEnv）。
const (
	defaultReadLimit = 2000
	maxReadBytes     = 2 << 20
	skipDir          = ".git,node_modules,vendor,dist,build,target,.idea,.vscode,__pycache__,.venv"
)

var skipSet = func() map[string]bool {
	m := map[string]bool{}
	for _, s := range strings.Split(skipDir, ",") {
		m[s] = true
	}
	return m
}()

// Read 按行读，带行号范围。offset/limit 是为了让 agent 大文件里只取需要的那段。
func (s *Server) Read(req proto.ReadReq) (proto.ReadResp, error) {
	return s.read(req, nil)
}

// read 的埋点同 write：谁读了哪个文件、读了几行，一样要留痕。
// 但只记路径和行数，绝不记内容——审计要的是「手伸到过哪里」，不是把文件再抄一份。
func (s *Server) read(req proto.ReadReq, remote *string) (proto.ReadResp, error) {
	resp, err := s.doRead(req)
	// 行数只在真读成的时候算：响应零值时 From=To=0，减出来是 1，
	// 会让一条失败记录看起来像「读了 1 行」。
	lines := 0
	if err == nil && resp.To >= resp.From {
		lines = resp.To - resp.From + 1
	}
	s.auditOp("read", remote, audit.Entry{
		Path: req.Path, Count: audit.Count(lines), Bytes: respBytes(err, resp),
	}, err)
	return resp, err
}

func (s *Server) doRead(req proto.ReadReq) (proto.ReadResp, error) {
	var resp proto.ReadResp
	abs, err := s.root.Resolve(req.Path)
	if err != nil {
		return resp, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return resp, err
	}
	// CRLF 原样带回去，不在这儿吞 \r：/write 是照传入的字节写盘的，读取一侧
	// 悄悄剥掉一半，就成了「写进去的和读回来的不是一个东西」——agent 用 /read
	// 校对自己刚写的脚本时会被骗（实测：CRLF 的 x.sh 看着正常，跑起来 shell 报
	// `x: command not found`，因为 \r 还在）。要不要按行处理由调用方决定，
	// 这里只保证「读到的就是文件里的」。
	crlf := bytes.Contains(data, []byte("\r\n"))
	if len(data) > maxReadBytes {
		return resp, fmt.Errorf("文件 %d 字节，超过单次读取上限 %d；用 offset/limit 或 exec 里的 tail/sed 取片段",
			len(data), maxReadBytes)
	}
	lines := splitLines(data)
	total := len(lines)
	from := req.Offset
	if from < 1 {
		from = 1
	}
	if from > total {
		from = total + 1
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultReadLimit
	}
	to := from + limit - 1
	truncated := false
	if to > total {
		to = total
	} else if to < total {
		truncated = true
	}

	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i, lines[i-1])
	}
	resp = proto.ReadResp{
		Path: req.Path, Content: b.String(),
		Total: total, From: from, To: to, Truncated: truncated,
		CRLF: crlf,
	}
	return resp, nil
}

// Write 整文件写入。默认允许新建，覆盖已有文件要求 Overwrite 明确为 false 之外的值。
// 日常改几行请用 Edit，别传整文件——那会让上下文翻倍。
func (s *Server) Write(req proto.WriteReq) (proto.WriteResp, error) {
	return s.write(req, nil)
}

// write 的埋点放在这里而不是 handler：直接调 Write 的路径（测试、将来的嵌入用法）
// 同样要留痕，审计不能只挂在 HTTP 那一层。
func (s *Server) write(req proto.WriteReq, remote *string) (proto.WriteResp, error) {
	resp, err := s.doWrite(req)
	s.auditOp("write", remote, audit.Entry{Path: req.Path, Bytes: audit.Bytes(int64(resp.Bytes))}, err)
	return resp, err
}

func (s *Server) doWrite(req proto.WriteReq) (proto.WriteResp, error) {
	var resp proto.WriteResp
	if xfer.InGit(req.Path) {
		return resp, s.inGitBlocked("write", req.Path)
	}
	abs, err := s.root.Resolve(req.Path)
	if err != nil {
		return resp, err
	}
	if _, err := os.Stat(abs); err == nil && req.Overwrite != nil && !*req.Overwrite {
		return resp, fmt.Errorf("%s 已存在，且 overwrite=false", req.Path)
	}
	data := []byte(req.Content)
	if err := xfer.WriteAtomic(abs, data, 0o644); err != nil {
		if errors.Is(err, xfer.ErrIsDir) {
			return resp, fmt.Errorf("%s 是目录", req.Path)
		}
		return resp, err
	}
	return proto.WriteResp{Path: req.Path, Bytes: len(data), SHA256: xfer.Sha256(data)}, nil
}

// Edit 是这套工具的效率关键：只在本地传 old/new，不传整个文件。
// old 必须唯一命中，命中 0 次或多处都报错，避免改错地方。
func (s *Server) Edit(req proto.EditReq) (proto.EditResp, error) {
	return s.edit(req, nil)
}

func (s *Server) edit(req proto.EditReq, remote *string) (proto.EditResp, error) {
	resp, err := s.doEdit(req)
	s.auditOp("edit", remote, audit.Entry{Path: req.Path, Bytes: audit.Bytes(int64(resp.BytesOut))}, err)
	return resp, err
}

func (s *Server) doEdit(req proto.EditReq) (proto.EditResp, error) {
	var resp proto.EditResp
	if xfer.InGit(req.Path) {
		return resp, s.inGitBlocked("edit", req.Path)
	}
	abs, err := s.root.Resolve(req.Path)
	if err != nil {
		return resp, err
	}
	if len(req.Edits) == 0 {
		return resp, errors.New("edits 为空")
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return resp, err
	}
	orig := data
	content := string(data)

	for i, e := range req.Edits {
		if e.Old == "" {
			return resp, fmt.Errorf("第 %d 个 edit 的 old 为空", i+1)
		}
		n := strings.Count(content, e.Old)
		switch {
		case n == 0:
			return resp, fmt.Errorf("第 %d 个 edit 的 old 未匹配到（注意缩进/换行/CRLF）", i+1)
		case n > 1:
			return resp, fmt.Errorf("第 %d 个 edit 的 old 匹配到 %d 处，请补充上下文使其唯一", i+1, n)
		}
		content = strings.Replace(content, e.Old, e.New, 1)
	}

	if err := xfer.WriteAtomic(abs, []byte(content), 0o644); err != nil {
		return resp, err
	}
	return proto.EditResp{
		Path: req.Path, Applied: len(req.Edits),
		BytesIn: len(orig), BytesOut: len(content),
	}, nil
}

// Glob 按 ** 与 * 匹配，相对 root。刻意跳过 .git/node_modules 之类，
// 并按 .gitignore 过滤——搜文件时最烦的就是被 dist/、*.min.js 淹掉。
func (s *Server) Glob(req proto.GlobReq) (proto.GlobResp, error) {
	return s.glob(req, nil)
}

// glob 记的是「拿什么模式找了，找到多少个」：pattern 本身就是这次动作的内容，
// 命中清单不进日志（那是搜索结果，不是操作痕迹）。
func (s *Server) glob(req proto.GlobReq, remote *string) (proto.GlobResp, error) {
	resp, err := s.doGlob(req)
	s.auditOp("glob", remote, audit.Entry{
		Pattern: req.Pattern, Count: audit.Count(len(resp.Files)), Bytes: respBytes(err, resp),
	}, err)
	return resp, err
}

func (s *Server) doGlob(req proto.GlobReq) (proto.GlobResp, error) {
	var resp proto.GlobResp
	if strings.TrimSpace(req.Pattern) == "" {
		return resp, errors.New("pattern 为空")
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 200
	}
	pat := filepath.ToSlash(req.Pattern)
	got := s.matchGlob(pat, respectIgnore(req.RespectIgnore), limit, &resp.TotalFiles)
	resp.Truncated = resp.TotalFiles > len(got)
	resp.Files = got
	return resp, nil
}

// matchGlob 找 pattern 命中的前 limit 条（按字典序），并回填命中总数。
//
// 两件事必须做对，否则调用方拿到的是「看起来对」的错答案：
//
//   - **排序在前、截断在后**。遍历顺序（尤其是并发 worker 的完成顺序）跟字典序
//     没关系，边遍历边截得到的是「碰巧先扫到的 limit 条」，同一个查询两次结果不同。
//   - **撞到 limit 也要把树走完**。提前收工就永远不知道还有多少，只能猜；多走一遍
//     目录比让 agent 基于偏序样本下结论便宜得多。防跑飞的阀门放在这里：
//     命中数超过 limit 一万倍就停，此时 total 退化成「至少这么多」。
func (s *Server) matchGlob(pat string, respectIgnore bool, limit int, total *int) []string {
	const hardCapFactor = 10000
	hardCap := limit * hardCapFactor

	var mu sync.Mutex
	hits := make([]string, 0, limit)
	n := 0
	walkFiles(s.root.Path(), s.walkConfig(respectIgnore), func(rel, _ string) bool {
		if ok, _ := matchGlob(pat, rel); !ok {
			return true
		}
		mu.Lock()
		defer mu.Unlock()
		n++
		hits = append(hits, rel)
		return n < hardCap
	})
	*total = n
	sort.Strings(hits)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// Grep 用 Go regexp（RE2），返回 file:line 便于直接跳转。
// 文件读取与匹配丢给 worker pool 并发跑；目录侧按 .gitignore 剪枝。
//
// 并发只在「算」这一段，出结果之后一律排序再截断：同一个查询两次结果不同，
// 调用方会以为自己错了。被跳过的东西（二进制、超大、硬剪枝）也要报出来，
// 否则「没有命中」和「没搜过」长得一模一样。
func (s *Server) Grep(req proto.GrepReq) (proto.GrepResp, error) {
	return s.grep(req, nil)
}

// grep 记正则和命中规模，不记命中行本身。
// 「搜过 secret|password 这些词」跟「搜到了什么」是两件事，
// 前者是审计要看的手伸向哪里，后者只是又一份敏感内容副本。
func (s *Server) grep(req proto.GrepReq, remote *string) (proto.GrepResp, error) {
	resp, err := s.doGrep(req)
	e := audit.Entry{Pattern: req.Pattern, Count: audit.Count(resp.TotalMatches), Bytes: respBytes(err, resp)}
	if req.Glob != "" {
		// 复用 targets 表达「在哪些文件里搜」，不再为它单开字段。
		e.Targets = []string{req.Glob}
	}
	s.auditOp("grep", remote, e, err)
	return resp, err
}

func (s *Server) doGrep(req proto.GrepReq) (proto.GrepResp, error) {
	resp := proto.GrepResp{
		Matches: []proto.GrepMatch{}, Files: []string{}, Skipped: proto.GrepSkipped{},
	}
	if req.Pattern == "" {
		return resp, errors.New("pattern 为空")
	}
	// limit 只有一种缺省语义：没传（0）= 50。0 不当「不限」，负数不猜，直接拒。
	limit := req.Limit
	if limit == 0 {
		limit = defaultGrepLimit
	}
	if limit < 0 {
		return resp, fmt.Errorf("limit 必须 >= 1（缺省 %d），不能用 0 或负数表示不限", defaultGrepLimit)
	}
	ctxLines, err := grepContext(req)
	if err != nil {
		return resp, err
	}

	pat := req.Pattern
	if req.IgnoreCase {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return resp, fmt.Errorf("正则非法: %w", err)
	}
	var globPat, exclPat string
	if req.Glob != "" {
		globPat = filepath.ToSlash(req.Glob)
	}
	if req.Exclude != "" {
		exclPat = filepath.ToSlash(req.Exclude)
	}

	// 超时是给「路径给得很宽」的那种查询兜底的：仓库大到几十万文件时，
	// 用户要的是「先给我一部分」，不是一个跑到天荒地老的进程。
	// 0 走缺省，负数是写错了参数，直接拒。
	deadline := time.Time{}
	if req.TimeoutMS != 0 {
		if req.TimeoutMS < 0 {
			return resp, errors.New("timeout_ms 必须 >= 0")
		}
		deadline = time.Now().Add(time.Duration(req.TimeoutMS) * time.Millisecond)
	}

	var mu sync.Mutex
	var hits []proto.GrepMatch
	files := map[string]int{} // 命中文件 → 命中行数
	var skipped proto.GrepSkipped
	timedOut := false

	walkFiles(s.root.Path(), s.walkConfig(respectIgnore(req.RespectIgnore)), func(rel, abs string) bool {
		if !deadline.IsZero() && time.Now().After(deadline) {
			timedOut = true
			return false // 停止遍历；已搜到的照样返回，不确定的那部分要标出来
		}
		if globPat != "" && !globMatchAny(globPat, rel) {
			return true
		}
		if exclPat != "" && globMatchAny(exclPat, rel) {
			return true
		}
		fi, err := os.Stat(abs)
		if err != nil {
			return true
		}
		// 搜索上限按 2MiB：再大就是数据文件了，整读进来的代价不如让调用方 read。
		if fi.Size() > maxGrepFileSize {
			mu.Lock()
			skipped.TooLarge++
			mu.Unlock()
			return true
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return true
		}
		binary := bytes.IndexByte(data[:min(len(data), 512)], 0) >= 0
		if binary && !req.SearchBinary {
			mu.Lock()
			skipped.Binary++
			mu.Unlock()
			return true
		}
		lines, lossy := grepLines(data)
		var fileHits []proto.GrepMatch
		n := 0
		for i, l := range lines {
			loc := re.FindStringIndex(l)
			if loc == nil {
				continue
			}
			n++
			if req.FilesOnly {
				continue // 只列文件时不用攒行，计数照走
			}
			for j := max(0, i-ctxLines.before); j <= min(len(lines)-1, i+ctxLines.after); j++ {
				m := proto.GrepMatch{File: rel, Line: j + 1}
				text := lines[j]
				// 300 字节的单行上限：命中十万字的长行，把整行搬回去只是烧 token。
				if len(text) > maxGrepLineBytes {
					m.Bytes, m.TextTruncated = len(text), true
					text = text[:maxGrepLineBytes] + "…"
				}
				m.Text = text
				if j == i {
					m.Match, m.Col = true, loc[0]+1
				}
				if lossy {
					m.Encoding = "lossy"
				}
				fileHits = append(fileHits, m)
			}
		}
		if n == 0 {
			return true
		}
		mu.Lock()
		files[rel] = n
		hits = append(hits, fileHits...)
		mu.Unlock()
		return true
	})

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].File != hits[j].File {
			return hits[i].File < hits[j].File
		}
		return hits[i].Line < hits[j].Line
	})
	for f, n := range files {
		resp.Files = append(resp.Files, f)
		resp.TotalMatches += n
	}
	sort.Strings(resp.Files)
	resp.TotalFiles = len(resp.Files)
	resp.Skipped = skipped
	// 超时只是「没搜完」，不是「没有」。两者混为一谈，调用方会据此下错结论。
	resp.TimedOut = timedOut
	resp.Truncated = resp.Truncated || timedOut
	// respect_ignore=false 时，引擎级硬剪枝会把 .git / .wr-jobs 挡在外面；
	// 不报出来，调用方会以为「整棵树都搜过了」。
	if req.RespectIgnore != nil && !*req.RespectIgnore {
		resp.Skipped.Ignored = s.countHardSkipped()
	}

	if req.FilesOnly {
		if len(resp.Files) > limit {
			resp.Files = resp.Files[:limit]
			resp.Truncated = true
		}
		return resp, nil
	}
	if len(hits) > limit {
		hits = hits[:limit]
		resp.Truncated = true
	}
	if hits != nil {
		resp.Matches = hits
	}
	return resp, nil
}

// defaultGrepLimit 比 200 小：定位阶段真正需要的是「有哪些文件」，
// 需要更多行就该显式要，或者先 files_only 再 read。
const (
	defaultGrepLimit = 50
	maxGrepFileSize  = 2 << 20
	maxGrepLineBytes = 300
	maxGrepContext   = 10
)

// grepContext 合并 context / before / after 三种写法。
// 显式给了 before/after 就以它为准，否则两边都用 context——
// 混着传时按「取了较大的那一边」算，免得让人以为自己只给了 2。
type grepCtx struct{ before, after int }

func grepContext(req proto.GrepReq) (grepCtx, error) {
	for _, v := range []struct {
		name string
		val  int
	}{{"context", req.Context}, {"before", req.Before}, {"after", req.After}} {
		if v.val < 0 {
			return grepCtx{}, fmt.Errorf("%s 必须 >= 0，负数不表示「全都要」", v.name)
		}
		if v.val > maxGrepContext {
			return grepCtx{}, fmt.Errorf("%s 最大 %d 行：上下文体量是命中行的数倍，确认位置后用 /read 看细节", v.name, maxGrepContext)
		}
	}
	c := grepCtx{before: req.Context, after: req.Context}
	if req.Before > c.before {
		c.before = req.Before
	}
	if req.After > c.after {
		c.after = req.After
	}
	return c, nil
}

// grepLines 切行，并顺手判断这份字节流是不是干净 UTF-8。
// 不是的话照样搜（按字节逐行扫），但 Text 里的中文会变成 U+FFFD，
// 得让调用方知道「这一行不是原文」，而不是拿着替换字符去推理。
//
// CRLF 的 \r 留在行尾：删表达式的 $ 在 Go 里匹配文本末尾而不是行尾，
// 所以 `foo$` 本来就命中不了 CRLF 的行，不用替它抹；而返回的 Text 是原文，
// 拿它去改文件时才不会少一个字节。
func grepLines(data []byte) ([]string, bool) {
	s := strings.TrimSuffix(string(data), "\n")
	if s == "" {
		return nil, false
	}
	return strings.Split(s, "\n"), strings.ContainsRune(s, utf8.RuneError)
}

// countHardSkipped 数一遍被引擎硬剪掉的顶层目录（.git、.wr-jobs、node_modules）。
// respect_ignore=false 时它们仍然不进来，但得让调用方知道这是「我替你挡的」。
func (s *Server) countHardSkipped() int {
	ents, err := os.ReadDir(s.root.Path())
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == xfer.GitDir || name == jobDirName || name == "node_modules" {
			n++
		}
	}
	return n
}

// walkConfig 把「搜索时什么算可见」收在一处，glob 和 grep 不会各写一套慢慢漂开。
func (s *Server) walkConfig(respectIgnore bool) walkConfig {
	return walkConfig{respectIgnore: respectIgnore, allowGit: s.cfg.AllowGitSearch, skip: skipSet}
}

// List 列目录，带上大小和 mtime。
// 只有名字的话，agent 判断「这是不是构建产物、什么时候改的」全靠猜。
func (s *Server) List(req proto.ListReq) (proto.ListResp, error) {
	return s.list(req, nil)
}

// list 连文件名都不记，只记「看了哪个目录、看到几项」。
// 想知道里面有什么，去查那一刻的仓库状态；日志只回答「谁翻过这个目录」。
func (s *Server) list(req proto.ListReq, remote *string) (proto.ListResp, error) {
	resp, err := s.doList(req)
	s.auditOp("list", remote, audit.Entry{
		Path: relIfEmpty(req.Dir), Count: audit.Count(len(resp.Entries)), Bytes: respBytes(err, resp),
	}, err)
	return resp, err
}

// relIfEmpty 把空目录名写成 "."：日志里留个空字符串，看不出是在看仓库根。
func relIfEmpty(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return "."
	}
	return dir
}

func (s *Server) doList(req proto.ListReq) (proto.ListResp, error) {
	resp := proto.ListResp{Dir: req.Dir}
	abs, err := s.root.Resolve(relOrDot(req.Dir))
	if err != nil {
		return resp, err
	}
	ents, err := os.ReadDir(abs)
	if err != nil {
		return resp, err
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 500
	}
	inGitDir := xfer.InGit(relOrDot(req.Dir))
	resp.Entries = make([]proto.Entry, 0, len(ents))
	for _, e := range ents {
		name := e.Name()
		// .git 只在顶层露出来一项（想看就 wr ls .git），.git 内部和 node_modules
		// 之类一律不铺开——它们对「找代码」只有噪音。
		if skipSet[name] && !(name == xfer.GitDir && !inGitDir) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		resp.Entries = append(resp.Entries, proto.Entry{
			Name: name, Dir: e.IsDir(), Size: info.Size(), ModTime: info.ModTime().Unix(),
		})
		if len(resp.Entries) >= limit {
			resp.Truncated = len(ents) > len(resp.Entries)
			break
		}
	}
	return resp, nil
}

// Delete 删文件或目录。目录要 recursive，避免「想删个文件顺手清空 src」。
func (s *Server) Delete(req proto.DeleteReq) (proto.DeleteResp, error) {
	return s.del(req, nil)
}

func (s *Server) del(req proto.DeleteReq, remote *string) (proto.DeleteResp, error) {
	resp, err := s.doDelete(req)
	s.auditOp("delete", remote, audit.Entry{Path: req.Path, Bytes: audit.Bytes(int64(resp.Removed))}, err)
	return resp, err
}

func (s *Server) doDelete(req proto.DeleteReq) (proto.DeleteResp, error) {
	var resp proto.DeleteResp
	if xfer.InGit(req.Path) {
		return resp, s.inGitBlocked("delete", req.Path)
	}
	abs, err := s.root.Resolve(req.Path)
	if err != nil {
		return resp, err
	}
	if abs == s.root.Path() {
		return resp, xfer.ErrIsRoot
	}
	n, err := xfer.Remove(abs, req.Recursive)
	if err != nil {
		return resp, err
	}
	return proto.DeleteResp{Path: req.Path, Removed: n}, nil
}

// Move 改名/搬移，等价于 git mv 的底层动作。
func (s *Server) Move(req proto.MoveReq) (proto.MoveResp, error) {
	return s.move(req, nil)
}

func (s *Server) move(req proto.MoveReq, remote *string) (proto.MoveResp, error) {
	resp, err := s.doMove(req)
	s.auditOp("move", remote, audit.Entry{Path: req.From, Targets: []string{req.To}}, err)
	return resp, err
}

func (s *Server) doMove(req proto.MoveReq) (proto.MoveResp, error) {
	var resp proto.MoveResp
	if xfer.InGit(req.From) || xfer.InGit(req.To) {
		return resp, s.inGitBlocked("move", req.From+" -> "+req.To)
	}
	from, err := s.root.Resolve(req.From)
	if err != nil {
		return resp, err
	}
	to, err := s.root.Resolve(req.To)
	if err != nil {
		return resp, err
	}
	if from == s.root.Path() || to == s.root.Path() {
		return resp, xfer.ErrIsRoot
	}
	if err := xfer.Move(from, to, req.Overwrite); err != nil {
		return resp, err
	}
	return proto.MoveResp{From: req.From, To: req.To}, nil
}

// Mkdir 建目录，等价 mkdir -p。
func (s *Server) Mkdir(req proto.MkdirReq) (proto.MkdirResp, error) {
	return s.mkdir(req, nil)
}

func (s *Server) mkdir(req proto.MkdirReq, remote *string) (proto.MkdirResp, error) {
	resp, err := s.doMkdir(req)
	s.auditOp("mkdir", remote, audit.Entry{Path: req.Path}, err)
	return resp, err
}

func (s *Server) doMkdir(req proto.MkdirReq) (proto.MkdirResp, error) {
	var resp proto.MkdirResp
	if xfer.InGit(req.Path) {
		return resp, s.inGitBlocked("mkdir", req.Path)
	}
	abs, err := s.root.Resolve(req.Path)
	if err != nil {
		return resp, err
	}
	perm := os.FileMode(0o755)
	if req.Perm != "" {
		var v uint32
		if _, err := fmt.Sscanf(req.Perm, "%o", &v); err != nil {
			return resp, fmt.Errorf("perm %q 不是八进制", req.Perm)
		}
		perm = os.FileMode(v)
	}
	if err := os.MkdirAll(abs, perm); err != nil {
		return resp, err
	}
	return proto.MkdirResp{Path: req.Path}, nil
}

// auditOp 是写类操作的统一收口：一次调用一行，成功 allowed、被拒 blocked、
// 其它失败 error。被拒也要走到这里（而不是在判定处就写日志），
// 因为只有这一层拿得到 remote——被拒的尝试恰恰是最该记来源的那种。
func (s *Server) auditOp(tool string, remote *string, e audit.Entry, err error) {
	if s.aud == nil {
		return
	}
	e.Tool = tool
	if remote != nil {
		e.Remote = *remote
	}
	var be *blockedErr
	switch {
	case err == nil:
		if e.Decision == "" {
			e.Decision = audit.Allowed
		}
	case errors.As(err, &be):
		e.Decision, e.Reason = audit.Blocked, be.Error()
		if e.Path == "" {
			e.Path = be.path
		}
	default:
		e.Decision, e.Reason = audit.Error, err.Error()
	}
	s.audit(e)
}

// respBytes 是「这次返回给调用方的东西有多大」——按整个响应序列化后的字节算。
// 和 exec 的 bytes 对齐：那边是 stdout+stderr 的字节数，这里就是回给 agent 的
// 那个 JSON body 的字节数。只记大小不记内容，一个数就能分辨「翻了一页目录」
// 和「把整个 glob 结果抄回去了」；失败时不写（响应是零值，0 字节等于「没返回」）。
func respBytes[T any](err error, resp T) *int64 {
	if err != nil {
		return nil
	}
	b, mErr := json.Marshal(resp)
	if mErr != nil {
		return nil
	}
	return audit.Bytes(int64(len(b)))
}

// inGitBlocked 造出「.git 只读」的拒绝。这里不写日志：埋点统一收在 auditOp，
// 那边才拿得到来源地址。带标记的错误让 auditOp 认出「这是拒绝」而非「执行出错」。
func (s *Server) inGitBlocked(tool, p string) error {
	return &blockedErr{err: fmt.Errorf(".git 下不允许%s：%s", gitVerb(tool), p), tool: tool, path: p}
}

// blockedErr 是「想做但不让」。和「执行失败」分开：前者记 blocked、后者记 error，
// 事后看这两件事的意义完全不同。
type blockedErr struct {
	err  error
	tool string
	path string
}

func (e *blockedErr) Error() string { return e.err.Error() }

// gitVerb 让报错读起来还是人话：write → 写入，move → 搬移。
func gitVerb(tool string) string {
	switch tool {
	case "write", "edit", "put":
		return "写入"
	case "delete":
		return "删除"
	case "mkdir":
		return "创建目录"
	case "move":
		return "搬移"
	}
	return "修改"
}

func relOrDot(p string) string {
	if strings.TrimSpace(p) == "" {
		return "."
	}
	return p
}

// splitLines 只管切行，不管行尾是什么：CRLF 的 \r 留在行内容里原样往上带。
// 抹掉它等于替调用方改文件，/read 回来的东西就跟磁盘上的不一致了。
func splitLines(data []byte) []string {
	s := strings.TrimSuffix(string(data), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// matchGlob 支持 ** / * / ? ，用的是自己写的转换而非常规 match：
// filepath.Match 不支持 **，而 ** 是 agent 用得最多的写法（**/*.go）。
func matchGlob(pat, name string) (bool, error) {
	re, err := globToRegexp(pat)
	if err != nil {
		return false, err
	}
	return re.MatchString(name), nil
}

// globMatchAny 按「文件名任意深度」语义匹配：`*.go` 命中 `src/a.go`。
// 这是 grep --glob / ripgrep -g 的约定，和 glob 工具本身不同
// （glob 工具里 `*.go` 只找根目录，跟 shell 一致）。
func globMatchAny(pat, rel string) bool {
	if ok, _ := matchGlob(pat, rel); ok {
		return true
	}
	base := rel
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		base = rel[i+1:]
	}
	ok, _ := matchGlob(pat, base)
	return ok
}

var (
	globMu    sync.Mutex
	globCache = map[string]*regexp.Regexp{}
)

func globToRegexp(pat string) (*regexp.Regexp, error) {
	globMu.Lock()
	defer globMu.Unlock()
	if re, ok := globCache[pat]; ok {
		return re, nil
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		switch c {
		case '*':
			if i+1 < len(pat) && pat[i+1] == '*' {
				i++
				if i+1 < len(pat) && pat[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '{':
			b.WriteString("(?:")
		case '}':
			b.WriteString(")")
		case ',':
			b.WriteString("|")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, err
	}
	globCache[pat] = re
	return re, nil
}
