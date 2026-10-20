package server

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"

	"cnb.cool/yyx946/tteist/internal/xfer"
)

// walkConfig 是遍历的几个开关。grep/glob 共用一套走法，
// 免得两个端点的「什么文件算可见」慢慢漂成两套语义。
type walkConfig struct {
	respectIgnore bool // 读 .gitignore
	allowGit      bool // 进 .git（默认不进）
	skip          map[string]bool
}

// walkFiles 遍历 root 下的文件，边遍历边并发跑 handle。
//
// 目录遍历必须串行（否则 .gitignore 的父子作用域没法传），但读文件、正则匹配
// 这些真正的开销是 IO+CPU 混合型，扔给 worker pool 就能把多核实打实用起来。
// 目录本身在遍历线程里就地过滤，命中目录直接不进去——
// 这是 `.gitignore` 相对「遍历完再筛」最大的收益。
func walkFiles(root string, cfg walkConfig, handle func(rel, abs string) (keepGoing bool)) {
	work := make(chan fileTask, 256)
	var wg sync.WaitGroup
	workers := runtime.NumCPU()
	if workers < 2 {
		workers = 2
	}
	if workers > 16 {
		workers = 16
	}

	var mu sync.Mutex
	stopped := false
	stop := func() {
		mu.Lock()
		stopped = true
		mu.Unlock()
	}
	isStopped := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return stopped
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range work {
				if isStopped() {
					continue // 排空队列，避免生产侧阻塞
				}
				if !handle(t.rel, t.abs) {
					stop()
				}
			}
		}()
	}

	ctx := IgnoreCtx{}
	if cfg.respectIgnore {
		// skip 里除了硬名单，还有 build/dist/target 这类「默认不看、但 .gitignore
		// 说了算」的目录：用户写了 `!dist` 就该能搜到 dist。
		ctx = ctx.WithBuiltin(softSkips(), hardSkips())
	}

	walk(root, root, ctx, cfg, func(t fileTask) bool {
		work <- t
		return !isStopped()
	})
	close(work)
	wg.Wait()
}

// softSkips 是「没写 .gitignore 也默认不看」的目录，但用户可以用 ! 翻案。
func softSkips() []string {
	var out []string
	for name := range skipSet {
		if !contains(hardSkips(), name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// hardSkips 进了就是灾难的目录：几万个对象 / 几十万依赖文件 / 一堆日志。
// 这些不参与取反——搜索里出现 node_modules 从来不是用户想要的。
//
// .wr-jobs 是 wr 自己的后台任务日志目录：它是不是「用户的代码」不取决于
// 项目 .gitignore 写没写它（很多项目根本没写），所以在这里硬剪，
// 否则一次 respect_ignore=false 的搜索会被自己的日志淹掉。
func hardSkips() []string {
	return []string{xfer.GitDir, "node_modules", jobDirName}
}

// respectIgnore 把「缺省开」翻译成三态指针：没传就是 true，
// 让「默认行为正确」和「显式关掉」共存，不用给旧客户端加必填字段。
func respectIgnore(v *bool) bool {
	return v == nil || *v
}

type fileTask struct {
	rel, abs string
}

// walk 递归。nested 只在 respectIgnore 时有意义。
// ctx 需要是每个目录独立的一份：子目录会补规则，兄弟目录不该互相污染。
func walk(root, dir string, ctx IgnoreCtx, cfg walkConfig, emit func(fileTask) bool) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return true
	}
	if cfg.respectIgnore {
		rel, _ := filepath.Rel(root, dir)
		relSlash := filepath.ToSlash(rel)
		if relSlash == "." {
			relSlash = ""
		}
		ctx = loadIgnoreRules(dir, relSlash, ctx)
	}

	for _, e := range ents {
		name := e.Name()
		abs := filepath.Join(dir, name)
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			continue
		}
		relSlash := filepath.ToSlash(rel)
		// .gitignore 本身是配置文件，不参与搜索：agent 找代码时它只是噪音。
		// （但也不属于「被忽略」，Exclude 是另一回事。）
		if name == ".gitignore" {
			continue
		}

		if e.IsDir() {
			// 软链目录不跟：环 + 外逃，两个都不值得为「找文件」付出
			if isSymlinkEntry(e, abs) {
				continue
			}
			// allow_git 收的是 .git：它不在 ignored 里（用户没写规则），
			// 但显示搜索索引和对象库纯属噪音，得单独一刀。
			// 硬剪枝不看 respect_ignore：.git / .wr-jobs / node_modules 就算
			// 关掉 .gitignore 过滤也不进来。这些不是「被忽略的代码」，
			// 是「搜了也只会得到噪音」的东西。
			if isHardSkipName(name) && !(cfg.allowGit && name == xfer.GitDir) {
				continue
			}
			gitAllowed := cfg.allowGit && name == xfer.GitDir
			ignored := false
			if cfg.respectIgnore && !gitAllowed {
				ignored = ctx.Match(relSlash, true)
			}
			// 内置忽略集（files）只在「没有 .gitignore 管这个仓库」时才生效：
			// 用户写了规则就以规则为准，免得「我明明没忽略 dist」变成玄学。
			// 硬名单（strict）不在此列——它已经由 Match 直接判死了。
			if ctx.noGitignore && ctx.ignores(relSlash) {
				ignored = true
			}
			if ignored {
				continue
			}
			if !walk(root, abs, ctx, cfg, emit) {
				return false
			}
			continue
		}
		if !e.Type().IsRegular() {
			if isSymlinkEntry(e, abs) {
				continue
			}
			continue
		}
		if cfg.respectIgnore && ctx.Match(relSlash, false) {
			continue
		}
		if !emit(fileTask{rel: relSlash, abs: abs}) {
			return false
		}
	}
	return true
}

// isHardSkipName 判断目录名是否属于「进了就是噪音」的硬名单。
func isHardSkipName(name string) bool {
	for _, n := range hardSkips() {
		if n == name {
			return true
		}
	}
	return false
}

func isSymlinkEntry(e fs.DirEntry, abs string) bool {
	if e.Type()&os.ModeSymlink == 0 {
		return false
	}
	// 只看指向：指文件的话当普通文件读没风险，指目录才需要拦下
	fi, err := os.Stat(abs)
	return err != nil || fi.IsDir()
}
