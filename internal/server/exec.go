package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cnb.cool/yyx946/tteist/internal/audit"
	"cnb.cool/yyx946/tteist/internal/proto"
	"cnb.cool/yyx946/tteist/internal/shellx"
)

const (
	defaultTimeout = 120 * time.Second
	maxTimeout     = 30 * time.Minute
	maxOutput      = 1 << 20 // 单个流 1MiB，够看，又不炸上下文
)

// Exec 执行一条命令。返回的 err 只代表「服务器自己出问题」，
// 命令失败体现在 ExitCode 上。
func (s *Server) Exec(ctx context.Context, req proto.ExecRequest) (proto.ExecResponse, error) {
	return s.exec(ctx, req, "")
}

// exec 是 Exec 的实现，remote 只用于审计（来源地址）。
// 走接口的路径带真实 RemoteAddr，CLI 和测试里没有请求上下文就留空。
func (s *Server) exec(ctx context.Context, req proto.ExecRequest, remote string) (proto.ExecResponse, error) {
	var resp proto.ExecResponse
	start := time.Now()

	if strings.TrimSpace(req.Cmd) == "" {
		return resp, errors.New("cmd 为空")
	}
	// 命令拼接放在策略之后会漏：args 也是要上执行面的，策略只看 cmd 会被绕。
	if err := s.pol.Check(req.Cmd, req.Args); err != nil {
		s.audit(audit.Entry{
			Decision: audit.Blocked, Tool: "exec", Reason: err.Error(),
			Command: req.Cmd, Shell: req.Shell, Cwd: relOrDot(req.Dir), Remote: remote,
		})
		return resp, err
	}

	prof, err := shellx.Resolve(req.Shell)
	if err != nil {
		return resp, err
	}

	cwd := s.root.Path()
	if req.Dir != "" {
		p, err := s.root.Resolve(req.Dir)
		if err != nil {
			return resp, err
		}
		cwd = p
	}

	timeout := defaultTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := s.newCmd(cctx, prof, assemble(req), cwd, req.Env)
	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
	}

	var out, errb limitWriter
	out.n, errb.n = maxOutput, maxOutput
	cmd.Stdout = &out
	cmd.Stderr = &errb

	setProcAttr(cmd) // 平台差异：Windows 进程树 / POSIX 进程组

	runErr := cmd.Run()
	// 命令本身结束后，可能还有它 fork 出去的孙进程拴着管道。
	// bash 的 `a & b & wait` 里 $! 拿到的是孙进程，只 kill 组主进程会漏。
	// 这里在超时/退出后补一刀，避免残留进程把机器拖死。
	reapOrphans(cmd)
	resp.Stdout = out.String()
	resp.Stderr = errb.String()
	resp.DurationMS = time.Since(start).Milliseconds()

	entry := audit.Entry{
		Tool: "exec", Command: req.Cmd, Shell: prof.ID,
		Cwd:      s.relCwd(cwd),
		Duration: audit.Duration(resp.DurationMS),
		Bytes:    audit.Bytes(int64(len(resp.Stdout) + len(resp.Stderr))),
		Remote:   remote,
		ExitCode: audit.Int(resp.ExitCode),
		Decision: audit.Allowed,
	}

	// 下面三条失败分支都必须把「为什么」写进去。早先一条都不写，日志里只剩
	// exitCode:1 / exitCode:127 两种哑巴结论——工具本身是通的，出错的永远是
	// 用户那条命令，而它到底说了什么才是唯一有用的线索。
	if cctx.Err() == context.DeadlineExceeded {
		resp.TimedOut = true
		resp.ExitCode = 124
		resp.Stderr += fmt.Sprintf("\n[wr] 超时 %s，已终止进程树", timeout)
		entry.Decision, entry.ExitCode = audit.Error, audit.Int(124)
		entry.Reason = "超时 " + timeout.String()
		// 超时是从进程树里抢出来的半截输出：只有 stdout/stderr 里真有的那部分，
		// 摘一段开头就够了，不必再去翻磁盘。
		entry.Error = audit.Excerpt(resp.Stderr, resp.Stdout)
		s.audit(entry)
		return resp, nil
	}
	_ = runErr
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			resp.ExitCode = ee.ExitCode()
			// 非零退出记 error：命令跑了但没成，grep error 该一把捞完，
			// 不必逼人再按 exitCode != 0 过一遍。
			entry.Decision, entry.ExitCode = audit.Error, audit.Int(resp.ExitCode)
			entry.Error = audit.Excerpt(resp.Stderr, resp.Stdout)
			s.audit(entry)
			return resp, nil
		}
		entry.Decision = audit.Error
		if resp.ExitCode != 0 {
			entry.ExitCode = audit.Int(resp.ExitCode)
		}
		entry.Reason = "启动 " + prof.Path + " 失败: " + runErr.Error()
		entry.Error = runErr.Error()
		s.audit(entry)
		return resp, fmt.Errorf("启动 %s 失败: %w", prof.Path, runErr)
	}
	s.audit(entry)
	return resp, nil
}

// relCwd 把绝对工作目录折回相对 root 的写法：日志里的路径要和人敲命令时一致，
// 否则同一件事在两边长得不一样，grep 的时候就对不上。
func (s *Server) relCwd(abs string) string {
	if rel, err := filepath.Rel(s.root.Path(), abs); err == nil {
		return filepath.ToSlash(rel)
	}
	return abs
}

// newCmd 是所有命令（exec 与 job）共用的构造点：
// shell 参数、工作目录、环境变量都只在这里拼一次，免得两条路径行为不一致。
func (s *Server) newCmd(ctx context.Context, prof shellx.Profile, script, cwd string, extraEnv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, prof.Path, prof.Args(shellx.Wrap(prof, script))...)
	cmd.Dir = cwd
	cmd.Env = append(s.baseEnv(), extraEnv...)
	return cmd
}

// assemble 把 cmd + args 拼成一段脚本。
// args 走 shell 转义而不是直接塞 argv，是因为不同 shell 的 argv 语义差异太大
// （cmd /c 会把后面整串当一个字符串，bash -c 只吃一个参数）。
func assemble(req proto.ExecRequest) string {
	if len(req.Args) == 0 {
		return req.Cmd
	}
	q := make([]string, len(req.Args))
	for i, a := range req.Args {
		q[i] = strconv.Quote(a)
	}
	return req.Cmd + " " + strings.Join(q, " ")
}

func (s *Server) baseEnv() []string {
	env := os.Environ()
	if !shellx.IsWin() {
		env = append(env, "TERM=dumb", "GIT_PAGER=cat")
	}
	if s.cfg.Git != "" {
		// GIT_DIR 是这块的关键：不设它，只有当 root 恰好是 git 仓库根时才找得到 .git。
		// 设了它，root 是整个 D:\repos 也能对指定仓库跑 git，还省掉 -C。
		env = append(env, "GIT_DIR="+s.cfg.Git)
	}
	return append(env,
		"WR_ROOT="+s.root.Path(),
		"NO_COLOR=1", // 去掉 ANSI，输出才好读
		"GIT_PAGER=cat",
		"GIT_TERMINAL_PROMPT=0", // 没有交互终端，别卡在凭据提示上
	)
}

// limitWriter 只留前 n 字节，其余丢弃但计数量。
// 不用 io.Pipe：命令写满管道会阻塞，杀掉进程前先卡住。
type limitWriter struct {
	buf bytes.Buffer
	n   int
	cut bool
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if w.cut {
		return len(p), nil
	}
	if w.buf.Len()+len(p) > w.n {
		w.buf.Write(p[:w.n-w.buf.Len()])
		w.cut = true
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

func (w *limitWriter) String() string {
	if w.cut {
		return w.buf.String() + fmt.Sprintf("\n[wr] 输出超过 %d 字节，已截断", w.n)
	}
	return w.buf.String()
}
