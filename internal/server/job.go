package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cnb.cool/yyx946/tteist/internal/audit"
	"cnb.cool/yyx946/tteist/internal/proto"
	"cnb.cool/yyx946/tteist/internal/shellx"
)

// job 是「后台跑着的进程」的最小实现：起、看日志、停。
// 不做伪终端、不做交互输入——那两件事在 Windows 上脆得没法长期维护，
// 而 agent 真正缺的只是「npm run dev 起来后能 curl 一下」。
//
// 输出直接追加到 root 里的文件，好处有两个：
//   1. 进程崩了、服务重启了，日志还在磁盘上；
//   2. 客户端可以 tail 一段就走，不必持有长连接。

const (
	maxJobs       = 16
	maxLogBytes   = 64 << 20 // 单 job 日志上限，超了就停采集，不杀进程
	jobDirName    = ".wr-jobs"
	logPollPeriod = 100 * time.Millisecond
	// untilTimeout 是「等某一行出现」的兜底：服务起不来时进程会退出、长轮询立刻返回，
	// 但一个既不起也不退的进程（卡在下载依赖上）只能靠它收场。
	untilTimeout = 10 * time.Second
)

var (
	ErrJobNotFound = errors.New("job 不存在")
	ErrTooManyJobs = errors.New("后台任务数已达上限")
)

type job struct {
	info   proto.JobInfo
	cmd    string
	absLog string
	cancel context.CancelFunc
	done   chan struct{}

	// 审计用：起止时间要能算出一致的一对行；remote 是发起方，事后追责只认它。
	started time.Time
	remote  string
	dir     string // 相对 root 的工作目录，退出埋点原样复用
	shell   string // 起任务时用的 shell profile，退出那行跟着带上

	mu   sync.Mutex
	err  error // cmd.Wait 的原始结果
	exit int   // 已归一的 exit code
	cut  bool  // 日志超上限
}

func (j *job) running() bool {
	select {
	case <-j.done:
		return false
	default:
		return true
	}
}

func (j *job) snapshot() proto.JobInfo {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := j.info
	out.Running = j.running()
	out.ExitCode = j.exit
	if out.Running {
		out.DurationMS = time.Since(time.Unix(out.StartedAt, 0)).Milliseconds()
	}
	if fi, err := os.Stat(j.absLog); err == nil {
		out.LogBytes = fi.Size()
	}
	return out
}

// StartJob 起一个后台任务。命令的 stdout+stderr 合并写同一个日志文件，
// 因为 agent 关心的是「按时间顺序发生了什么」，而不是哪个流。
func (s *Server) StartJob(req proto.JobStartReq) (proto.JobInfo, error) {
	return s.startJob(req, "")
}

func (s *Server) startJob(req proto.JobStartReq, remote string) (proto.JobInfo, error) {
	var info proto.JobInfo
	if strings.TrimSpace(req.Cmd) == "" {
		return info, errors.New("cmd 为空")
	}
	if err := s.pol.Check(req.Cmd, req.Args); err != nil {
		s.audit(audit.Entry{
			Decision: audit.Blocked, Tool: "job_start", Reason: err.Error(),
			Command: req.Cmd, Shell: req.Shell, Cwd: relOrDot(req.Dir), Remote: remote,
		})
		return info, err
	}
	prof, err := shellx.Resolve(req.Shell)
	if err != nil {
		return info, err
	}
	cwd := s.root.Path()
	if req.Dir != "" {
		p, rerr := s.root.Resolve(req.Dir)
		if rerr != nil {
			return info, rerr
		}
		cwd = p
	}

	id, err := newJobID()
	if err != nil {
		return info, err
	}
	relLog := path.Join(jobDirName, id+".log")
	absLog, err := s.root.Resolve(relLog)
	if err != nil {
		return info, err
	}
	if err := os.MkdirAll(filepath.Dir(absLog), 0o755); err != nil {
		return info, err
	}
	f, err := os.Create(absLog)
	if err != nil {
		return info, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	var timeoutMS int64
	if req.TimeoutMS > 0 {
		timeoutMS = req.TimeoutMS
		if time.Duration(timeoutMS)*time.Millisecond > maxTimeout {
			timeoutMS = maxTimeout.Milliseconds()
		}
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(timeoutMS)*time.Millisecond)
	}

	cmd := s.newCmd(ctx, prof, assemble(proto.ExecRequest{Cmd: req.Cmd, Args: req.Args}), cwd, req.Env)
	w := &jobWriter{f: f, limit: maxLogBytes}
	cmd.Stdout, cmd.Stderr = w, w
	setProcAttr(cmd)

	j := &job{
		cmd:     req.Cmd,
		absLog:  absLog,
		cancel:  cancel,
		done:    make(chan struct{}),
		started: time.Now(),
		remote:  remote,
		dir:     relOrDot(req.Dir),
		shell:   prof.ID,
		info: proto.JobInfo{
			ID: id, Cmd: req.Cmd, Shell: prof.ID,
			Dir: relOrDot(req.Dir), StartedAt: time.Now().Unix(), LogPath: relLog,
		},
	}

	s.jobMu.Lock()
	if len(s.jobs) >= maxJobs {
		s.jobMu.Unlock()
		f.Close()
		cancel()
		return info, fmt.Errorf("%w（%d 个），先用 wr job kill 收掉不用的", ErrTooManyJobs, maxJobs)
	}
	s.jobs[id] = j
	s.jobMu.Unlock()

	if err := cmd.Start(); err != nil {
		s.jobMu.Lock()
		delete(s.jobs, id)
		s.jobMu.Unlock()
		f.Close()
		cancel()
		return info, fmt.Errorf("启动 %s 失败: %w", prof.Path, err)
	}

	j.mu.Lock()
	j.info.PID = cmd.Process.Pid
	j.mu.Unlock()
	s.audit(audit.Entry{
		Decision: audit.Allowed, Tool: "job_start", Command: req.Cmd, Shell: prof.ID,
		Cwd: s.relCwd(cwd), Session: id, Remote: remote,
	})

	go func() {
		err := cmd.Wait()
		// Wait 之前就已经被取消 = 明确 kill 掉的；之后再取消只是清理。
		// 顺序反了会把「正常 exit 3」也记成 130。
		wasKilled := ctx.Err() == context.Canceled
		reapOrphans(cmd)
		_ = f.Close()
		cancel()

		// 124 = 超时，130 = 被 kill（shell 对 SIGINT 的惯例），
		// 剩下的用进程真实退出码。负数（被信号打断）统一归到 130。
		exit := 0
		var ee *exec.ExitError
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			exit = 124
		case wasKilled:
			exit = 130
		case errors.As(err, &ee):
			exit = ee.ExitCode()
			if exit < 0 {
				exit = 130
			}
		case err != nil:
			exit = 1
		}
		j.mu.Lock()
		j.err = err
		j.exit = exit
		j.cut = w.Cut()
		size := w.n
		j.mu.Unlock()
		// 退出这一行和启动那行用同一个 sessionId 成对：后台任务最容易「起了就没影」，
		// 只有启动那行的话，事后分不清是还在跑、跑挂了、还是被谁杀了。
		decision := audit.Allowed
		switch {
		case exit != 0:
			decision = audit.Error
		}
		e := audit.Entry{
			Decision: decision, Tool: "job_exit", Command: j.cmd, Shell: j.shell,
			Cwd: j.dir, Session: id, Remote: j.remote,
			ExitCode: audit.Int(exit),
			Duration: audit.Duration(time.Since(j.started).Milliseconds()),
			Bytes:    audit.Bytes(size),
		}
		// 后台任务「起了就没影」时，光有 exitCode 还是猜；日志文件就在磁盘上，
		// 成功的任务不去读它，失败的那条摘一眼开头——通常第一行就是根因。
		if exit != 0 {
			e.Error = jobExcerpt(absLog)
		}
		s.audit(e)
		close(j.done)
	}()

	return j.snapshot(), nil
}

// jobExcerpt 从任务的日志文件里摘一眼原因，交给 audit.Excerpt 收口。
// 只在已经失败的 job 上读一次：日志上限 64MiB，摊到一次退出事件上可以接受，
// 换来的是 job_exit 那行不再只有一个裸的 exitCode。
func jobExcerpt(absLog string) string {
	data, err := os.ReadFile(absLog)
	if err != nil {
		return ""
	}
	// stdout 和 stderr 被合并进同一个文件，分不出哪句是谁说的；
	// 这种情况下「不是空行的那第一行」就是最像原因的那一行。
	return audit.Excerpt(string(data), "")
}

// JobLogs 支持三种取法：从头、只取新增（since）、只要最后 N 行（tail）。
// wait_ms 是长轮询：进程还在跑且没新增就挂着，最多等这么久——
// 客户端不用自己 sleep 轮询，也不会漏掉「刚起就退出」这种最常见的情况。
//
// until 把「等多久」换成「等什么」：起常驻服务时人真正关心的是首屏那行
// 「Listening on :8002」，出现就该撤。拿 wait_ms 猜时长只有两个结局——
// 猜短了没等到、猜长了白挂着，而后者按分钟计。
func (s *Server) JobLogs(req proto.JobLogsReq) (proto.JobLogsResp, error) {
	var resp proto.JobLogsResp
	j, err := s.getJob(req.ID)
	if err != nil {
		return resp, err
	}
	var until *regexp.Regexp
	if req.Until != "" {
		until, err = regexp.Compile(req.Until)
		if err != nil {
			return resp, fmt.Errorf("until 不是合法的正则: %w", err)
		}
	}

	// until 什么时候收场由两个上限管：wait_ms（一次没新增挂多久）和 timeout_ms（总共等多久）。
	// 两个都没给时，wait_ms 顶成兜底时长，免得调用方以为「没给就是不用等」。
	wait := req.Wait
	if wait <= 0 {
		wait = untilWait(req).Milliseconds()
	}
	deadline := time.Now().Add(untilWait(req))
	for {
		resp = s.readLog(j, req)
		// 命中判在「这次要回的那段」上：扫到文件尾，反正读日志本来就要读一遍。
		// 先扫后读会让「读到的新增」和「扫过的范围」错开一轮，命中那行反而落进下一段。
		resp.Matched = matchLine(resp.Output, until)
		if resp.Matched || resp.Offset > req.Since || !resp.Running || wait <= 0 || time.Now().After(deadline) {
			return resp, nil
		}
		req.Wait = wait
		time.Sleep(logPollPeriod)
	}
}

// untilWait 是等 pattern 的总时长：until 有自己的缺省与上限，
// wait_ms 保持原样——它一直是「没新增就挂着」的那个时长，两种语义别混。
func untilWait(req proto.JobLogsReq) time.Duration {
	ms := req.Timeout
	if ms <= 0 {
		ms = untilTimeout.Milliseconds()
	}
	return time.Duration(ms) * time.Millisecond
}

// matchLine 判一段日志里有没有命中 until 的行。按行匹配：until 说的是
// 「等哪一行」，跨行的正则在这条路上没有意义（也没有哪行去锚它）。
func matchLine(output string, until *regexp.Regexp) bool {
	if until == nil || output == "" {
		return false
	}
	for _, line := range strings.Split(output, "\n") {
		if line != "" && until.MatchString(line) {
			return true
		}
	}
	return false
}

func (s *Server) readLog(j *job, req proto.JobLogsReq) proto.JobLogsResp {
	snap := j.snapshot()
	resp := proto.JobLogsResp{
		ID: snap.ID, ExitCode: snap.ExitCode, Running: snap.Running,
	}

	f, err := os.Open(j.absLog)
	if err != nil {

		return resp
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return resp
	}
	size := fi.Size()
	if req.Since > 0 {
		if req.Since >= size {
			resp.Offset = size
			if req.Since > size {
				// 偏移比文件还大：日志被 drop 掉了、或者 job 被重起过。
				// 静默回空会让人以为「进程没输出」，其实整段都没了。
				resp.Output = "[wr] 日志已重置（旧偏移 " + itoa64(req.Since) + "，当前 " + itoa64(size) + " 字节）\n"
			}
			return resp
		}
		if _, err := f.Seek(req.Since, io.SeekStart); err != nil {
			return resp
		}
		data, _ := io.ReadAll(f)
		resp.Output, resp.Offset = string(data), size
		return resp
	}

	const tailCap = 1 << 20 // 不做 tail 时最多回 1MiB，剩下的用 since 分段取
	start := int64(0)
	if req.Tail > 0 {
		start = tailOffset(f, size, req.Tail)
	} else if size > tailCap {
		start = size - tailCap
		resp.Truncated = true
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return resp
	}
	data, _ := io.ReadAll(f)
	resp.Output = string(data)
	if start > 0 && req.Tail == 0 {
		resp.Output = "[wr] 日志 " + itoa64(size) + " 字节，只返回最后 1MiB；要完整内容用 since=0 分段读\n" + resp.Output
	}
	resp.Offset = size
	return resp
}

// tailOffset 从文件尾往前数 n 个换行，给出起点。
//
// 数到第 n 个换行就停，起点是它后面那个字节——也就是倒数第 n 行的行首。
// 往回多走一个换行会多带一行，/log?tail=2 会给你 3 行。
func tailOffset(f *os.File, size int64, n int) int64 {
	if size == 0 {
		return 0
	}
	const chunk = 8 << 10
	buf := make([]byte, chunk)
	pos := size
	count := 0
	for pos > 0 {
		readSize := int64(chunk)
		if pos < readSize {
			readSize = pos
		}
		pos -= readSize
		if _, err := f.ReadAt(buf[:readSize], pos); err != nil && !errors.Is(err, io.EOF) {
			return 0
		}
		// 文件末尾本身没有换行，所以第 n 个换行就是倒数第 n 行的行尾。
		for i := int(readSize) - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				count++
				if count == n {
					return pos + int64(i) + 1
				}
			}
		}
	}
	return 0
}

// KillJob 杀整棵进程树。POSIX 按进程组、Windows 走 taskkill /T，
// 否则 `npm run dev` 这种会留一地 node 子进程。
func (s *Server) KillJob(id string) (proto.JobInfo, error) {
	return s.killJob(id, false, "")
}

func (s *Server) killJob(id string, dropLog bool, remote string) (proto.JobInfo, error) {
	j, err := s.getJob(id)
	if err != nil {
		return proto.JobInfo{}, err
	}
	j.cancel()
	select {
	case <-j.done:
	case <-time.After(5 * time.Second):
	}
	s.audit(audit.Entry{
		Decision: audit.Allowed, Tool: "job_kill", Command: j.cmd, Shell: j.shell,
		Cwd: j.dir, Session: id, Remote: remote,
	})
	info := j.snapshot()
	if dropLog {
		// 收尾一条命令就够：kill 顺手把日志带走，不然 .wr-jobs 只涨不落。
		// 进程先杀再删——还在写的文件在 Windows 上删不掉。
		if err := os.Remove(j.absLog); err == nil {
			info.LogBytes = 0
		}
	}
	return info, nil
}

func (s *Server) ListJobs() proto.JobListResp {
	s.jobMu.Lock()
	list := make([]*job, 0, len(s.jobs))
	for _, j := range s.jobs {
		list = append(list, j)
	}
	s.jobMu.Unlock()
	out := proto.JobListResp{Jobs: make([]proto.JobInfo, 0, len(list))}
	for _, j := range list {
		out.Jobs = append(out.Jobs, j.snapshot())
	}
	sort.Slice(out.Jobs, func(a, b int) bool { return out.Jobs[a].StartedAt > out.Jobs[b].StartedAt })
	return out
}

// DropJob 停掉并从列表移除（日志文件保留，方便事后翻）。
func (s *Server) DropJob(id string) error {
	return s.dropJob(id, false, "")
}

func (s *Server) dropJob(id string, dropLog bool, remote string) error {
	if _, err := s.killJob(id, dropLog, remote); err != nil {
		return err
	}
	s.jobMu.Lock()
	delete(s.jobs, id)
	s.jobMu.Unlock()
	return nil
}

func (s *Server) getJob(id string) (*job, error) {
	if id == "" {
		return nil, errors.New("id 为空")
	}
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s（wr job list 看看有哪些）", ErrJobNotFound, id)
	}
	return j, nil
}

// jobWriter 边写边计数，超上限后继续「吃」但不落盘。
// 不返回错误：写日志失败不该把用户的后台进程干掉。
type jobWriter struct {
	f     *os.File
	limit int64
	n     int64
	cut   bool
}

func (w *jobWriter) Write(p []byte) (int, error) {
	if w.cut {
		return len(p), nil
	}
	if w.n+int64(len(p)) > w.limit {
		room := w.limit - w.n
		if room > 0 {
			_, _ = w.f.Write(p[:room])
			w.n += room
		}
		w.cut = true
		_, _ = w.f.WriteString("\n[wr] 日志超过 " + itoa64(w.limit) + " 字节，后续输出已丢弃\n")
		return len(p), nil
	}
	n, _ := w.f.Write(p)
	w.n += int64(n)
	return len(p), nil
}

func (w *jobWriter) Cut() bool { return w.cut }

func newJobID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return time.Now().Format("0102-150405") + "-" + hex.EncodeToString(b), nil
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
