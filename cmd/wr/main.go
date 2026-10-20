// wr —— 远程高效协同工具。
//
//	wr serve   在目标机上起一个服务（单 exe，零依赖）
//	wr exec    在目标机的仓库目录里跑命令（git bash / pwsh / bash / cmd…）
//	wr read/write/edit/glob/grep/ls  文件操作，改几行就传几行
//
// 设计目标只有一个：让 agent 在远端机器上开发，手感接近本地。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cnb.cool/yyx946/tteist/internal/audit"
	"cnb.cool/yyx946/tteist/internal/client"
	"cnb.cool/yyx946/tteist/internal/proto"
	"cnb.cool/yyx946/tteist/internal/server"
	"cnb.cool/yyx946/tteist/internal/shellx"
)

// version 从 proto 取，服务端和客户端不会再说两个版本号。
// var 而非 const：发布时靠 -ldflags -X 覆盖 proto.Version，const 会把它定死在编译期。
var version = proto.Version

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "serve", "start":
		err = cmdServe(args)
	case "exec", "run":
		err = cmdExec(args)
	case "read", "cat":
		err = cmdRead(args)
	case "write":
		err = cmdWrite(args)
	case "edit":
		err = cmdEdit(args)
	case "glob":
		err = cmdGlob(args)
	case "grep":
		err = cmdGrep(args)
	case "ls":
		err = cmdLs(args)
	case "rm", "delete":
		err = cmdRm(args)
	case "mv", "move":
		err = cmdMv(args)
	case "mkdir":
		err = cmdMkdir(args)
	case "sync":
		err = cmdSync(args)
	case "job":
		err = cmdJob(args)
	case "log":
		err = cmdLog(args)
	case "openapi", "api":
		err = cmdOpenAPI(args)
	case "put":
		err = cmdPut(args)
	case "get":
		err = cmdGet(args)
	case "fwd":
		err = cmdFwd(args)
	case "info", "shells":
		err = cmdInfo(args)
	case "version", "-v", "--version":
		fmt.Println("wr", version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wr: "+err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`wr —— 远程高效协同工具 (单 exe，无第三方依赖)

服务端（目标机上跑，沙箱限定一个仓库目录）:
  wr serve [-root D:\repos\myrepo] [-p 7573] [-token <随机串>] [-git-dir path] [-deny ''] [-log 文件] [-no-log]
           [-job-retain 72h] [-fwd name=端口 | name=http(s)://host:port]...
                                默认监听所有网卡的 7573；不传 -token 会自动生成并打印
                                审计日志默认写 <启动目录>/.wr.log，-log 可指定路径，-no-log 关掉
  wr start                       serve 的别名，直接起服务
  wr serve -gen-token            生成随机 token 后退出

  转发（-fwd 可重复，名字随目标一起由你定，客户端只能选名字）:
  wr serve -root . -fwd web=5173                    简写：本机 127.0.0.1:5173，http
  wr serve -root . -fwd web=5173 -fwd pw=8931       只写端口，最常用
  wr serve -root . -fwd pw=http://10.0.0.9:8931     别的机器 / 要 https / 带子路径时才写全
  → 用户和 agent 都只认一个地址，两个入口选一个：
    $WR_URL/fwd/web/x    剥掉入口段，打到上游 /x          （上游 base:'/'）
    $WR_URL/fwds/web/x   原样过去，打到上游 /fwds/web/x   （上游 base:'/fwds/web/'）

客户端（agent 侧，任何装了 wr 的机器）:
  wr exec  [-shell bash] [-dir src] [-timeout 300] 'go test ./...'
  wr exec  -stdin 'go run /dev/stdin' < main.go
  wr read  main.go [-offset 1 -limit 200]
  wr write new.go -create < file.go         从 stdin 读内容
  wr edit  main.go -old 'a' -new 'b'        改几行就传几行
  wr glob  '**/*.go'
  wr grep  'func main' -glob '*.go' [-i] [-l] [-c 2] [-A 2 -B 2] [-no-ignore]
  wr ls    src [-long]                      列目录（-long 带大小和时间）
  wr rm    dist [-r]                        删文件；删目录必须 -r
  wr mv    a.go b.go [-force]               改名 / 搬移
  wr mkdir -p internal/job                  建目录
  wr put   local.bin -to dist/local.bin     上传（一律裸流，二进制安全）
  wr get   dist/local.bin -o ./local.bin    下载
  wr sync  ./dist -to build [-delete] [-dry-run]   目录增量同步（只传差异）
  wr job start 'npm run dev'                起后台任务
  wr job logs <id> -until 'Listening on'    等到这行出现就返回（起常驻服务的正解）
  wr job logs <id> -f                       跟日志（长轮询，不刷屏）
  wr job list / wr job kill <id> [-rm-log]  看 / 停后台任务；-rm-log 连日志一起删
  wr log  [-tail 200] [-f]                 看服务端审计日志（默认最后 200 行）
  wr log  -after <offset>                  只要偏移之后的新增
  wr log  -clear                           清空审计日志（动作本身也留痕）
  wr fwd                                   看服务端配了哪些转发目标（/fwd）
  wr fwd web -path /api/health             把一条请求打到上游，结果按 JSON 回
  wr fwd web -X POST -path /api/x -d '{}'  换个方法、带请求体
  wr openapi                               打印接口清单（/api/openapi.json）
  wr info                                  看服务端目录、可用 shell、git 仓库

环境变量: WR_URL / WR_TOKEN 可省去 -url / -token
不指定 -shell 时用服务端默认 shell。

短参数（只给 wr start / serve 这条常驻命令，等价于长名，混着写也行）:
  -r / -d root    -p port    -a addr    -t token
  -l log          -f fwd     -e deny
  客户端子命令（exec / read / grep / sync …）一律用长名：它们一条里
  常挤三四个标志，短名省的是按键，赔的是「-d 是 dir 还是 dry-run」
  这种每次都要停一下的成本。拿不准就写长名——长名永远对。
`)
}

// newFlagSet 比 flag.NewFlagSet 多一张短名表，其余用起来完全一样。
//
// 为什么不用 flag 包自带的「同名不同 flag」：两个名字各是一个变量，
// 用户写 -t 还是 -token 得事后比一遍才知道哪个生效。这里在 Parse 之前
// 就把 -t 改写成 -token，只留一份状态，也就不存在「谁覆盖谁」。
//
// 短名目前只挂在 wr start：客户端子命令一条里常挤三四个标志，
// 省几个按键换来「-d 到底是 dir 还是 dry-run」的记忆负担，不划算。
type flagSet struct {
	*flag.FlagSet
	alias map[string]string // 短名 → 全名
}

func newFlagSet(name string, errHandling flag.ErrorHandling) *flagSet {
	return &flagSet{FlagSet: flag.NewFlagSet(name, errHandling), alias: map[string]string{}}
}

// short 给已有 flag 挂一个短名。全名本身就叫这名时跳过，免得自己指向自己。
func (fs *flagSet) short(short, name string) { fs.alias[short] = name }

// shorts 一次挂一批：长名只能是字母，短名优先取首字母。
// 一批里同一个短名挂两次是写错了名单，直接 panic 比等用户踩到再说强。
func (fs *flagSet) shorts(names ...string) {
	seen := map[string]string{}
	for _, n := range names {
		sh := n[:1]
		if prev, ok := seen[sh]; ok {
			panic("短名冲突: -" + sh + " 同时指向 -" + prev + " 和 -" + n)
		}
		seen[sh] = n
		fs.short(sh, n)
	}
}

func (fs *flagSet) Parse(args []string) error {
	return fs.FlagSet.Parse(fs.rewrite(args))
}

// rewrite 把短名整体换成长名：-t x → -token x，-t=x → -token=x，--t 也算。
// 只换「两个连字符以内、名字完全等于短名」的那种，
// -timeout 这种同前缀的长名不会被误伤。
func (fs *flagSet) rewrite(args []string) []string {
	if len(fs.alias) == 0 {
		return args
	}
	out := make([]string, 0, len(args))
	for _, a := range args {
		name, val, hasVal := cutFlagValue(a)
		if name == "" {
			out = append(out, a)
			continue
		}
		full, ok := fs.alias[name]
		if !ok {
			out = append(out, a)
			continue
		}
		if !hasVal {
			out = append(out, "-"+full)
			continue
		}
		out = append(out, "-"+full+"="+val)
	}
	return out
}

// cutFlagValue 拆出 -name / -name=value 里的名字和值；不是标志就返回空名。
// 三个连字符以上（---x）原样放行，让 flag 包自己报错。
func cutFlagValue(a string) (name, val string, hasVal bool) {
	if len(a) < 2 || a[0] != '-' {
		return "", "", false
	}
	body := a[1:]
	if strings.HasPrefix(body, "-") {
		body = body[1:]
		if strings.HasPrefix(body, "-") {
			return "", "", false
		}
	}
	if i := strings.IndexByte(body, '='); i >= 0 {
		return body[:i], body[i+1:], true
	}
	return body, "", false
}

// isShortArg 判断一个参数是不是「短名本身」，给带值的短名判断要不要吃下一个参数用。
func (fs *flagSet) isShortArg(a string) bool {
	name, _, hasVal := cutFlagValue(a)
	if name == "" || hasVal {
		return false
	}
	_, ok := fs.alias[name]
	return ok
}

// splitFlags 把「位置参数 + 标志」的混排拆开：标志先走，位置参数后留。
// Go 的 flag 包遇到第一个非标志就停止解析，而人写命令习惯是
// `wr grep 'pattern' -glob '*.go'`，不修一下会很反直觉。
//
// 布尔型标志（-stdin / -json / -create …）后面不吃值，否则
// `wr exec -stdin cat` 会把 cat 当成 -stdin 的参数，命令就没了。
func splitFlags(fs *flagSet, args []string) (flags, pos []string) {
	isBool := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			isBool[f.Name] = true
		}
	})
	// 短名先还原成长名再判断，否则 `-t xxx` 里的 xxx 会被当成位置参数，
	// 命令就歪到「xxx 是路径」那去了。
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !isFlag(a) {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		name, hasVal := fs.longName(a)
		if hasVal || isBool[name] {
			continue
		}
		// 值即使是空串也要带上，否则 `-deny ""` 这种显式清空会被吞
		if i+1 < len(args) && !isFlag(args[i+1]) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return flags, pos
}

// longName 给出标志的长名，以及它是不是自带值（-name=value 形态）。
func (fs *flagSet) longName(a string) (string, bool) {
	name, _, hasVal := cutFlagValue(a)
	if name == "" {
		return strings.TrimLeft(a, "-"), strings.Contains(a, "=")
	}
	if full, ok := fs.alias[name]; ok {
		return full, hasVal
	}
	return name, hasVal
}

func isFlag(s string) bool {
	return len(s) > 1 && s[0] == '-' && !(s[1] >= '0' && s[1] <= '9')
}

func parse(fs *flagSet, args []string) []string {
	f, p := splitFlags(fs, args)
	if err := fs.Parse(f); err != nil {
		// 参数写错就停在这一句上，别带着半套配置去连服务端。
		fmt.Fprintln(os.Stderr, "wr: "+err.Error())
		os.Exit(2)
	}
	return p
}

// ---------- serve ----------

// defaultPort 是 wr serve 不指定端口时的监听端口。
const defaultPort = 7573

func cmdServe(args []string) error {
	fs := newFlagSet("serve", flag.ExitOnError)
	root := fs.String("root", ".", "沙箱根目录（所有文件操作都限制在这里）")
	port := fs.Int("p", defaultPort, "监听端口；只绑端口，所有网卡都能连")
	addr := fs.String("addr", "", "监听地址，覆盖 -p，形如 127.0.0.1:7573；不传则用 -p 只绑端口")
	token := fs.String("token", "", "访问 token，至少 6 字符；不传则自动生成并打印")
	deny := fs.String("deny", "", "追加命令黑名单正则，逗号分隔；传空串只保留内置规则")
	noDefaults := fs.Bool("no-defaults", false, "关闭内置黑名单（不建议）")
	logFlag := fs.String("log", "", "审计日志落点；不传就是启动目录下的 .wr.log，相对路径按启动目录算")
	noLog := fs.Bool("no-log", false, "不记审计日志（默认就是记的，这个开关用来关掉）")
	gitDir := fs.String("git-dir", "", "显式指定仓库 .git 路径（默认在 root 内自动探测）")
	fwds := multiFlag{}
	fs.Var(&fwds, "fwd", "转发目标 name=端口（简写，本机 http）或 name=http(s)://host:port，可重复；\n挂到 /fwd/<name>/（剥前缀）和 /fwds/<name>/（原样）两个入口下")
	gen := fs.Bool("gen-token", false, "生成随机 token 后退出")
	quiet := fs.Bool("quiet", false, "不打印启动信息")
	retain := fs.String("job-retain", "", "后台任务日志保留时长，如 72h、30m；不传=不自动清")
	// serve 是「机器上只跑一次」的那条命令，短名先给这里。
	// -d 指 root（目录），和 wr 其它地方「-d 是目录」的说法一致。
	fs.short("t", "token")
	fs.short("d", "root")
	fs.short("r", "root")
	fs.shorts("addr", "deny")
	fs.short("l", "log")
	fs.short("f", "fwd")
	fs.short("e", "deny")
	parse(fs, args)

	if *gen {
		t, err := randomToken()
		if err != nil {
			return err
		}
		fmt.Println(t)
		return nil
	}

	listen, err := listenAddr(*port, *addr)
	if err != nil {
		return err
	}

	// 不传 -token 就现生成一个：能 `wr serve` 直接跑起来，又不给固定默认口令。
	generated := false
	if *token == "" {
		t, err := randomToken()
		if err != nil {
			return err
		}
		*token, generated = t, true
	}
	if len(*token) < minTokenLen {
		return fmt.Errorf("token 太短（%d），至少 %d 字符", len(*token), minTokenLen)
	}

	var extra []string
	if *deny != "" {
		extra = strings.Split(*deny, ",")
	}

	logPath, err := resolveLogPath(*logFlag, *noLog)
	if err != nil {
		return err
	}

	var retainFor time.Duration
	if *retain != "" {
		d, err := parseRetain(*retain)
		if err != nil {
			return err
		}
		retainFor = d
	}

	srv, err := server.New(server.Config{
		Addr: listen, Token: *token, Root: *root, Deny: extra,
		NoDefaults: *noDefaults, LogPath: logPath, Quiet: *quiet, Git: *gitDir,
		Fwd: fwds, JobRetain: retainFor,
	})
	if err != nil {
		return err
	}
	defer srv.Close()
	if !*quiet {
		if generated {
			fmt.Fprintf(os.Stderr, "wr: 未指定 -token，本次生成的 token：%s\n", *token)
		}
		if ip := strings.Split(listen, ":")[0]; ip == "" || ip == "0.0.0.0" || ip == "::" {
			fmt.Fprintf(os.Stderr, "wr: 监听 %s，同网段任何机器都能连（跨机器访问务必配防火墙）\n", listen)
		}
	}
	return srv.ListenAndServe()
}

// parseRetain 解析 -job-retain。只认「数字+单位」，不认 "72" 这种裸数字：
// 秒和小时差 3600 倍，猜错一次就是清早了一天，宁可让它报错。
func parseRetain(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d, nil
	}
	return 0, fmt.Errorf("-job-retain 需要带单位的正时长（如 72h、30m），收到 %q", s)
}

// resolveLogPath 决定审计日志写哪儿。
//
// 默认就记，而且按 **执行 wr start 的目录** 算，不是 -root、也不是 home：
// 说"记到 .wr.log"，人回头找它时一定站在刚才那个目录里。
// 显式给的相对路径（含裸文件名）同样按当前目录解析；
// 只有 -no-log 才彻底不记。
func resolveLogPath(flagVal string, disabled bool) (string, error) {
	if disabled {
		return "", nil
	}
	if flagVal == "" {
		// 没写 -log：落到启动目录下的 .wr.log
		return audit.Resolve(".", audit.DefaultFile)
	}
	return audit.Resolve(".", flagVal)
}

// minTokenLen 是 token 下限。只是挡「空口令 / 随手一个字符」，
// 真正的强度靠随机生成，不靠长度门槛。
const minTokenLen = 6

// listenAddr 把「只给端口」和「给完整地址」统一成 net.Listen 的 host:port。
// 默认只给端口就意味着绑 0.0.0.0（双栈时是 ::），这是有意的：
// 这工具本来就是拿去连远端机器的，只听 127.0.0.1 等于自带一个必填坑。
func listenAddr(port int, addr string) (string, error) {
	if addr != "" {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return "", fmt.Errorf("-addr 需要 host:port 形式：%w", err)
		}
		return addr, nil
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("端口 %d 不合法，取值范围 1-65535", port)
	}
	return ":" + strconv.Itoa(port), nil
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// 24 字符够长，肉眼抄得下来，也不至于一行糊满屏。
	return hex.EncodeToString(b[:12]), nil
}

// ---------- 客户端公共 ----------

type common struct {
	url, token string
	asJSON     bool
}

func (c *common) bind(fs *flagSet) {
	fs.StringVar(&c.url, "url", envOr("WR_URL", "http://127.0.0.1:7573"), "服务端地址")
	fs.StringVar(&c.token, "token", envOr("WR_TOKEN", ""), "访问 token")
	fs.BoolVar(&c.asJSON, "json", false, "输出原始 JSON")
	// 客户端子命令一律长名：一条命令里挤三四个短名，省下的是按键，
	// 搭进去的是「-d 是 dir 还是 dry-run」这种每次都要想一下的成本。
	// 短名只给 wr start 这条常驻命令。
}

func (c *common) client() (*client.Client, error) {
	if c.token == "" {
		return nil, fmt.Errorf("缺少 token：用 -token 或环境变量 WR_TOKEN")
	}
	return client.New(c.url, c.token, 0), nil
}

// respectIgnoreFlag 把「-no-ignore」翻成三态字段：不加 flag 就不传，
// 让服务端用默认值（读 .gitignore），老服务端也不会因为多了个字段报错。
func respectIgnoreFlag(noIgnore bool) *bool {
	if !noIgnore {
		return nil
	}
	v := false
	return &v
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// ---------- exec ----------

func cmdExec(args []string) error {
	fs := newFlagSet("exec", flag.ExitOnError)
	var c common
	c.bind(fs)
	shell := fs.String("shell", "", "shell: bash|gitbash|sh|pwsh|powershell|cmd|wsl|<路径>")
	dir := fs.String("dir", "", "工作目录（相对仓库根）")
	timeout := fs.Int64("timeout", 120, "超时秒数")
	envFlag := fs.String("env", "", "追加环境变量，逗号分隔 KEY=VAL")
	stdinFlag := fs.Bool("stdin", false, "从本地 stdin 读内容作为远端 stdin")
	quiet := fs.Bool("q", false, "只输出 stdout，不打印元信息")

	pos := parse(fs, args)
	if len(pos) == 0 {
		return fmt.Errorf("缺少命令")
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	req := proto.ExecRequest{
		Cmd: strings.Join(pos, " "), Shell: *shell, Dir: *dir,
		TimeoutMS: *timeout * 1000,
	}
	if *envFlag != "" {
		req.Env = strings.Split(*envFlag, ",")
	}
	if *stdinFlag {
		b, err := readAll(os.Stdin)
		if err != nil {
			return err
		}
		req.Stdin = b
	}
	resp, err := cl.Exec(req)
	if err != nil {
		return err
	}
	if c.asJSON {
		printJSON(resp)
	} else {
		if resp.Stdout != "" {
			fmt.Fprint(os.Stdout, resp.Stdout)
			if !strings.HasSuffix(resp.Stdout, "\n") {
				fmt.Println()
			}
		}
		if resp.Stderr != "" {
			fmt.Fprint(os.Stderr, resp.Stderr)
			if !strings.HasSuffix(resp.Stderr, "\n") {
				fmt.Fprintln(os.Stderr)
			}
		}
		if !*quiet {
			fmt.Fprintf(os.Stderr, "[wr] exit=%d %dms\n", resp.ExitCode, resp.DurationMS)
		}
	}
	if resp.ExitCode != 0 {
		os.Exit(resp.ExitCode)
	}
	return nil
}

// ---------- 文件 ----------

func cmdRead(args []string) error {
	fs := newFlagSet("read", flag.ExitOnError)
	var c common
	c.bind(fs)
	offset := fs.Int("offset", 1, "起始行（1 基）")
	limit := fs.Int("limit", 2000, "最多行数")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("用法: wr read <path>")
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	resp, err := cl.Read(proto.ReadReq{Path: pos[0], Offset: *offset, Limit: *limit})
	if err != nil {
		return err
	}
	if c.asJSON {
		printJSON(resp)
		return nil
	}
	fmt.Print(resp.Content)
	if resp.Truncated {
		fmt.Fprintf(os.Stderr, "[wr] 共 %d 行，已显示 %d-%d；继续用 -offset %d\n",
			resp.Total, resp.From, resp.To, resp.To+1)
	}
	return nil
}

func cmdWrite(args []string) error {
	fs := newFlagSet("write", flag.ExitOnError)
	var c common
	c.bind(fs)
	content := fs.String("content", "", "内容；不传则从 stdin 读")
	overwrite := fs.Bool("overwrite", true, "覆盖已存在的文件；-overwrite=false 时只允许新建")
	create := fs.Bool("create", true, "兼容旧参数，等价于 -overwrite=true")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("用法: wr write <path> [-content 文本 | < file]")
	}
	body := *content
	if body == "" {
		b, err := readAll(os.Stdin)
		if err != nil {
			return err
		}
		body = b
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	ow := *overwrite || *create
	resp, err := cl.Write(proto.WriteReq{Path: pos[0], Content: body, Overwrite: &ow})
	if err != nil {
		return err
	}
	if c.asJSON {
		printJSON(resp)
	} else {
		fmt.Printf("%s %d bytes sha256=%s\n", resp.Path, resp.Bytes, resp.SHA256[:12])
	}
	return nil
}

func cmdEdit(args []string) error {
	fs := newFlagSet("edit", flag.ExitOnError)
	var c common
	c.bind(fs)
	old := fs.String("old", "", "被替换文本（必须在文件里唯一出现）")
	new_ := fs.String("new", "", "替换为")
	editsFile := fs.String("edits", "", "JSON 文件：[{\"old\":..,\"new\":..}]，批量替换")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("用法: wr edit <path> -old '...' -new '...'")
	}
	var edits []proto.Edit
	if *editsFile != "" {
		b, err := os.ReadFile(*editsFile)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &edits); err != nil {
			return fmt.Errorf("-edits 解析失败: %w", err)
		}
	} else {
		if *old == "" {
			return fmt.Errorf("缺少 -old")
		}
		edits = []proto.Edit{{Old: *old, New: *new_}}
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	resp, err := cl.Edit(proto.EditReq{Path: pos[0], Edits: edits})
	if err != nil {
		return err
	}
	if c.asJSON {
		printJSON(resp)
	} else {
		fmt.Printf("%s 应用 %d 处替换：%d → %d bytes\n", resp.Path, resp.Applied, resp.BytesIn, resp.BytesOut)
	}
	return nil
}

func cmdGlob(args []string) error {
	fs := newFlagSet("glob", flag.ExitOnError)
	var c common
	c.bind(fs)
	limit := fs.Int("limit", 200, "最多结果数")
	noIgnore := fs.Bool("no-ignore", false, "不读 .gitignore，被忽略的文件也一起找")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("用法: wr glob '**/*.go' [-limit 200] [-no-ignore]")
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	resp, err := cl.Glob(proto.GlobReq{Pattern: pos[0], Limit: *limit, RespectIgnore: respectIgnoreFlag(*noIgnore)})
	if err != nil {
		return err
	}
	if c.asJSON {
		printJSON(resp)
		return nil
	}
	for _, f := range resp.Files {
		fmt.Println(f)
	}
	if resp.Truncated {
		// 说清「一共多少、给了哪一段」：只报「截断到 200 条」会让人以为文件就这么多。
		fmt.Fprintf(os.Stderr, "[wr] 共 %d 条，只给了字典序前 %d 条；调 -limit 或换更窄的模式\n",
			resp.TotalFiles, len(resp.Files))
	}
	return nil
}

func cmdGrep(args []string) error {
	fs := newFlagSet("grep", flag.ExitOnError)
	var c common
	c.bind(fs)
	glob := fs.String("glob", "", "限定文件，如 *.go")
	exclude := fs.String("exclude", "", "排除文件，如 *.min.js")
	ic := fs.Bool("i", false, "忽略大小写")
	limit := fs.Int("limit", 50, "最多匹配数（0 和负数都不表示不限）")
	filesOnly := fs.Bool("l", false, "只列文件名")
	ctx := fs.Int("c", 0, "命中行前后各显示几行（上限 10）")
	before := fs.Int("B", 0, "只要前几行")
	after := fs.Int("A", 0, "只要后几行")
	bin := fs.Bool("binary", false, "连二进制文件一起搜")
	timeout := fs.Int64("timeout", 60, "超时秒数")
	noIgnore := fs.Bool("no-ignore", false, "不读 .gitignore，被忽略的文件也一起搜")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("用法: wr grep 'pattern' [-glob '*.go'] [-exclude '*.min.js'] [-i] [-l] [-c 2] [-A 2] [-B 2] [-binary] [-no-ignore]")
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	resp, err := cl.Grep(proto.GrepReq{
		Pattern: pos[0], Glob: *glob, Exclude: *exclude, IgnoreCase: *ic,
		Limit: *limit, FilesOnly: *filesOnly, Context: *ctx,
		Before: *before, After: *after, SearchBinary: *bin,
		TimeoutMS:     *timeout * 1000,
		RespectIgnore: respectIgnoreFlag(*noIgnore),
	})
	if err != nil {
		return err
	}
	if c.asJSON {
		printJSON(resp)
		return nil
	}
	for _, f := range resp.Files {
		fmt.Println(f)
	}
	for _, m := range resp.Matches {
		col := ""
		if m.Col > 0 && m.Match {
			col = fmt.Sprintf(":%d", m.Col)
		}
		// 上下文行用 '-' 分隔，和 grep -A/-B 的输出习惯一致
		sep := "-"
		if m.Match {
			sep = ":"
		}
		fmt.Printf("%s%s%d%s %s\n", m.File, sep, m.Line, col, m.Text)
	}
	if resp.TimedOut {
		fmt.Fprintf(os.Stderr, "[wr] 超时（-timeout %d 秒），只搜了一部分仓库；放宽 -glob 或加 -timeout\n", *timeout)
	}
	// 截断只说「还有更多」没用：总数才是「我能不能就此下结论」的依据。
	if resp.Truncated && !resp.TimedOut {
		fmt.Fprintf(os.Stderr, "[wr] 已截断：显示 %d/%d 条，涉及 %d 个文件（files_only 或 -limit 调整）\n",
			displayed(resp), resp.TotalMatches, resp.TotalFiles)
	}
	if s := resp.Skipped; s.Binary+s.TooLarge+s.Ignored > 0 {
		fmt.Fprintf(os.Stderr, "[wr] 跳过：二进制 %d、超大 %d、硬剪枝 %d\n", s.Binary, s.TooLarge, s.Ignored)
	}
	return nil
}

func displayed(resp proto.GrepResp) int {
	if resp.Matches != nil {
		return len(resp.Matches)
	}
	return len(resp.Files)
}

func cmdLs(args []string) error {
	fs := newFlagSet("ls", flag.ExitOnError)
	var c common
	c.bind(fs)
	limit := fs.Int("limit", 500, "最多条目数")
	long := fs.Bool("long", false, "带大小与修改时间")
	pos := parse(fs, args)
	dir := "."
	if len(pos) > 0 {
		dir = pos[0]
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	resp, err := cl.List(dir, *limit)
	if err != nil {
		return err
	}
	if c.asJSON {
		printJSON(resp)
		return nil
	}
	for _, e := range resp.Entries {
		if !*long {
			if e.Dir {
				fmt.Println(e.Name + "/")
			} else {
				fmt.Println(e.Name)
			}
			continue
		}
		kind := "-"
		if e.Dir {
			kind = "d"
		}
		fmt.Printf("%s %10d  %s  %s\n", kind, e.Size, humanTime(e.ModTime), e.Name)
	}
	if resp.Truncated {
		fmt.Fprintf(os.Stderr, "[wr] 已截断到 %d 条，用 -limit 放大\n", *limit)
	}
	return nil
}

func humanTime(unix int64) string {
	if unix <= 0 {
		return "          "
	}
	return time.Unix(unix, 0).Format("01-02 15:04")
}

// ---------- 删除 / 搬移 / 建目录 ----------

func cmdRm(args []string) error {
	fs := newFlagSet("rm", flag.ExitOnError)
	var c common
	c.bind(fs)
	recursive := fs.Bool("r", false, "递归删除目录")
	pos := parse(fs, args)
	if len(pos) == 0 {
		return fmt.Errorf("用法: wr rm <path> [-r]")
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	for _, p := range pos {
		resp, err := cl.Delete(proto.DeleteReq{Path: p, Recursive: *recursive})
		if err != nil {
			return err
		}
		if c.asJSON {
			printJSON(resp)
		} else {
			fmt.Printf("%s 已删除（%d 项）\n", resp.Path, resp.Removed)
		}
	}
	return nil
}

func cmdMv(args []string) error {
	fs := newFlagSet("mv", flag.ExitOnError)
	var c common
	c.bind(fs)
	force := fs.Bool("force", false, "目标已存在时覆盖")
	pos := parse(fs, args)
	if len(pos) != 2 {
		return fmt.Errorf("用法: wr mv <from> <to>")
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	resp, err := cl.Move(proto.MoveReq{From: pos[0], To: pos[1], Overwrite: *force})
	if err != nil {
		return err
	}
	if c.asJSON {
		printJSON(resp)
	} else {
		fmt.Printf("%s → %s\n", resp.From, resp.To)
	}
	return nil
}

func cmdMkdir(args []string) error {
	fs := newFlagSet("mkdir", flag.ExitOnError)
	var c common
	c.bind(fs)
	perm := fs.String("perm", "0755", "权限（八进制，Windows 忽略）")
	parent := fs.Bool("p", true, "父目录不存在时一起建；保持默认即可")
	pos := parse(fs, args)
	if len(pos) == 0 {
		return fmt.Errorf("用法: wr mkdir <dir> [-perm 0755]")
	}
	_ = parent
	cl, err := c.client()
	if err != nil {
		return err
	}
	for _, p := range pos {
		resp, err := cl.Mkdir(proto.MkdirReq{Path: p, Perm: *perm})
		if err != nil {
			return err
		}
		if c.asJSON {
			printJSON(resp)
		} else {
			fmt.Printf("%s 已创建\n", resp.Path)
		}
	}
	return nil
}

// ---------- job ----------

func cmdJob(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: wr job start|logs|list|kill ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "start", "run":
		return jobStart(rest)
	case "logs", "log", "tail":
		return jobLogs(rest)
	case "list", "ls", "ps":
		return jobList(rest)
	case "kill", "stop":
		return jobKill(rest)
	}
	return fmt.Errorf("未知子命令 %q（start|logs|list|kill）", sub)
}

func jobStart(args []string) error {
	fs := newFlagSet("job start", flag.ExitOnError)
	var c common
	c.bind(fs)
	shell := fs.String("shell", "", "shell")
	dir := fs.String("dir", "", "工作目录（相对仓库根）")
	timeout := fs.Int64("timeout", 0, "超时秒数；0 表示长驻")
	envFlag := fs.String("env", "", "追加环境变量 KEY=VAL,逗号分隔")
	pos := parse(fs, args)
	if len(pos) == 0 {
		return fmt.Errorf("用法: wr job start 'npm run dev'")
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	req := proto.JobStartReq{
		Cmd: strings.Join(pos, " "), Shell: *shell, Dir: *dir,
		TimeoutMS: *timeout * 1000,
	}
	if *envFlag != "" {
		req.Env = strings.Split(*envFlag, ",")
	}
	info, err := cl.JobStart(req)
	if err != nil {
		return err
	}
	if c.asJSON {
		printJSON(info)
		return nil
	}
	fmt.Printf("%s 已启动  pid=%d  logs=%s\n", info.ID, info.PID, info.LogPath)
	fmt.Printf("跟日志: wr job logs %s -f\n", info.ID)
	return nil
}

func jobLogs(args []string) error {
	fs := newFlagSet("job logs", flag.ExitOnError)
	var c common
	c.bind(fs)
	tail := fs.Int("tail", 0, "只看最后 N 行")
	follow := fs.Bool("f", false, "持续跟（进程退出即停）")
	wait := fs.Int64("wait", 0, "长轮询等待毫秒数；-f 时默认 20000")
	since := fs.Int64("since", 0, "只看 offset 之后的增量")
	until := fs.String("until", "", "等到匹配这行正则就返回，如 -until 'Listening on'")
	timeout := fs.Int64("timeout", 0, "等到 -until 命中的兜底秒数；缺省 10")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("用法: wr job logs <id> [-f] [-tail 100] [-until 'Listening on']")
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	if *follow && *wait == 0 && *until == "" {
		*wait = 20000
	}
	for {
		resp, err := cl.JobLogs(proto.JobLogsReq{
			ID: pos[0], Tail: *tail, Since: *since, Wait: *wait,
			Until: *until, Timeout: *timeout * 1000,
		})
		if err != nil {
			return err
		}
		if c.asJSON {
			printJSON(resp)
			return nil
		}
		fmt.Print(resp.Output)
		*since = resp.Offset
		*tail = 0 // 跟随时只要增量
		// -until 的两种收场要分开说：命中了就是「等到想等的了」，
		// 没命中而进程还活着，只说明等够了——别让人误以为服务起来了。
		if *until != "" && !resp.Running {
			fmt.Fprintf(os.Stderr, "[wr] 已退出 exit=%d\n", resp.ExitCode)
			return nil
		}
		if *until != "" && resp.Matched {
			return nil
		}
		if *until != "" && !resp.Matched && !*follow {
			fmt.Fprintf(os.Stderr, "[wr] 等够 %ds 也没等到 /%s/，进程还在跑（用 -timeout 放宽或用 -f 继续跟）\n",
				*timeout, *until)
			os.Exit(1)
		}
		if !*follow || !resp.Running {
			if !resp.Running {
				fmt.Fprintf(os.Stderr, "[wr] 已退出 exit=%d\n", resp.ExitCode)
			}
			return nil
		}
	}
}

func jobList(args []string) error {
	fs := newFlagSet("job list", flag.ExitOnError)
	var c common
	c.bind(fs)
	parse(fs, args)
	cl, err := c.client()
	if err != nil {
		return err
	}
	resp, err := cl.JobList()
	if err != nil {
		return err
	}
	if c.asJSON {
		printJSON(resp)
		return nil
	}
	if len(resp.Jobs) == 0 {
		fmt.Println("没有后台任务")
		return nil
	}
	for _, j := range resp.Jobs {
		state := "running"
		if !j.Running {
			state = fmt.Sprintf("exit=%d", j.ExitCode)
		}
		fmt.Printf("%s  %-12s pid=%-7d %s  %s\n", j.ID, state, j.PID, j.Cmd, j.LogPath)
	}
	return nil
}

func jobKill(args []string) error {
	fs := newFlagSet("job kill", flag.ExitOnError)
	var c common
	c.bind(fs)
	drop := fs.Bool("drop", false, "同时从列表移除")
	rmLog := fs.Bool("rm-log", false, "连 .wr-jobs/<id>.log 一起删掉（收尾用）")
	pos := parse(fs, args)
	if len(pos) == 0 {
		return fmt.Errorf("用法: wr job kill <id> [-drop] [-rm-log]")
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	for _, id := range pos {
		info, err := cl.JobKill(id, *drop, *rmLog)
		if err != nil {
			return err
		}
		if c.asJSON {
			printJSON(info)
		} else {
			fmt.Printf("%s 已停止 exit=%d\n", info.ID, info.ExitCode)
		}
	}
	return nil
}

// ---------- openapi ----------

func cmdOpenAPI(args []string) error {
	fs := newFlagSet("openapi", flag.ExitOnError)
	var c common
	c.bind(fs)
	parse(fs, args)
	cl, err := c.client()
	if err != nil {
		return err
	}
	doc, err := cl.OpenAPI()
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

func cmdPut(args []string) error {
	fs := newFlagSet("put", flag.ExitOnError)
	var c common
	c.bind(fs)
	to := fs.String("to", "", "远端路径（相对仓库根），默认与本地同名")
	create := fs.Bool("create", true, "允许覆盖已存在的远端文件；-create=false 时拒绝")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("用法: wr put <localhost> [-to remotepath]")
	}
	local := pos[0]
	remote := *to
	if remote == "" {
		remote = filepath.Base(local)
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	resp, err := cl.Put(remote, local, *create)
	if err != nil {
		return err
	}
	fmt.Printf("%s <- %s  %d bytes\n", resp.Path, local, resp.Bytes)
	return nil
}

func cmdGet(args []string) error {
	fs := newFlagSet("get", flag.ExitOnError)
	var c common
	c.bind(fs)
	out := fs.String("out", "", "本地保存路径；不传则打印到 stdout")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("用法: wr get <remotepath> [-o local]")
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	resp, err := cl.Get(pos[0], *out)
	if err != nil {
		return err
	}
	if *out == "" {
		fmt.Print(resp.Content)
	} else {
		fmt.Printf("%s → %s\n", resp.Path, *out)
	}
	return nil
}

func cmdInfo(args []string) error {
	fs := newFlagSet("info", flag.ExitOnError)
	var c common
	c.bind(fs)
	parse(fs, args)
	cl, err := c.client()
	if err != nil {
		return err
	}
	info, err := cl.Info()
	if err != nil {
		return err
	}
	if c.asJSON {
		printJSON(info)
		return nil
	}
	fmt.Printf("wr %s  服务端 %s  沙箱 %s\n", info.Version, info.OS, info.Root)
	if info.Git != "" {
		fmt.Printf("git 仓库 %s\n", info.Git)
	} else {
		fmt.Printf("git 仓库 （未找到，exec 里的 git 会报 not a repository）\n")
	}
	// 转发目标是「这台 wr 背后还有什么」，和 shell 一样属于起手就该看见的信息。
	// 名字只有 /info（或带 token 的 /fwd）会告诉你：/fwd/<name>/ 走错名字时是空的。
	for _, t := range info.Fwd {
		fmt.Printf("转发 %-11s → %s  (/fwd/%s/ 剥前缀，/fwds/%s/ 原样)\n", t.Name, t.URL, t.Name, t.Name)
	}
	def := shellx.DefaultID()
	for _, s := range info.Shells {
		mark := " "
		if s.Found {
			mark = "✓"
		}
		tag := ""
		if s.ID == def {
			tag = "  (本地默认名)"
		}
		fmt.Printf(" %s %-11s %s%s\n", mark, s.ID, s.Path, tag)
	}
	return nil
}

// ---------- 审计日志 ----------

func cmdLog(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "clear", "reset", "truncate":
			return logClear(args[1:])
		}
	}
	fs := newFlagSet("log", flag.ExitOnError)
	var c common
	c.bind(fs)
	tail := fs.Int("tail", 200, "只看最后 N 行；0 表示从头")
	after := fs.Int64("after", 0, "只要这个字节偏移之后的新增")
	follow := fs.Bool("f", false, "持续跟")
	raw := fs.Bool("raw", false, "原样输出，不加行号")
	clear := fs.Bool("clear", false, "清空日志")
	pos := parse(fs, args)
	if *clear {
		return logClear(nil)
	}
	_ = pos
	cl, err := c.client()
	if err != nil {
		return err
	}
	for {
		req := proto.LogReq{Tail: *tail, After: *after}
		resp, err := cl.Log(req)
		if err != nil {
			return err
		}
		if c.asJSON {
			printJSON(resp)
			return nil
		}
		printLogLines(resp.Output, *raw)
		*after, *tail = resp.Offset, 0 // 跟随时只要增量
		if !*follow {
			return nil
		}
		// 审计日志没有「进程退出」这个终止信号，只能定点再看一眼。
		// 用固定间隔而不是长轮询：这端点的价值在「人盯着看」，
		// 真接 SIEM 的直接读文件，不走这条 HTTP。
		time.Sleep(500 * time.Millisecond)
	}
}

func logClear(args []string) error {
	fs := newFlagSet("log clear", flag.ExitOnError)
	var c common
	c.bind(fs)
	parse(fs, args)
	cl, err := c.client()
	if err != nil {
		return err
	}
	resp, err := cl.LogClear()
	if err != nil {
		return err
	}
	if c.asJSON {
		printJSON(resp)
		return nil
	}
	fmt.Printf("已清空 %s（%d 字节）\n", resp.Path, resp.Cleared)
	return nil
}

// printLogLines 是给人看的形态：带行号，跟 wr log 的其它输出保持一致。
// 裸 JSONL 一行可能很长，加行号是为了能说"第 42 行那条"。
func printLogLines(out string, raw bool) {
	if out == "" {
		return
	}
	if raw {
		fmt.Print(out)
		return
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	for i, l := range lines {
		fmt.Printf("%6d\t%s\n", i+1, l)
	}
}

func readAll(f *os.File) (string, error) {
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeCharDevice != 0 {
		return "", nil
	}
	b, err := io.ReadAll(f)
	return string(b), err
}

// ---------- fwd ----------

// cmdFwd 两个用法：
//
//	wr fwd                       看这台 wr 背后挂了哪些上游
//	wr fwd <name> [-path /x]     把一条请求打过去，结果按 JSON 回
//
// 不做「本机端口监听」那种隧道：agent 手上只有 WR_URL，它要的是一个地址，
// 不是再学一套端口分配。
func cmdFwd(args []string) error {
	fs := newFlagSet("fwd", flag.ExitOnError)
	var c common
	c.bind(fs)
	method := fs.String("X", "GET", "HTTP 方法：GET/POST/HEAD…")
	path := fs.String("path", "/", "上游路径，可带 query")
	data := fs.String("d", "", "请求体（原样发送，默认 Content-Type: application/json）")
	header := fs.String("H", "", "追加请求头，形如 'K: V'；需要多条就分多次 -H")
	timeout := fs.Int64("timeout", 60, "超时秒数")
	pos := parse(fs, args)

	cl, err := c.client()
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		list, err := cl.Fwd()
		if err != nil {
			return err
		}
		if c.asJSON {
			printJSON(list)
			return nil
		}
		if len(list.Targets) == 0 {
			fmt.Println("没有配置转发目标：起服务时加 -fwd name=端口，例如 -fwd web=5173")
			return nil
		}
		for _, t := range list.Targets {
			// Spec 是启动参数原样写法：简写成 web=5173 时，URL 那列是补全后的地址。
			fmt.Printf("%-12s %-28s →  %s/fwd/%s/\n", t.Name, t.Spec, c.url, t.Name)
		}
		return nil
	}

	// -H 只支持一条：flag 包不支持重复标志，需要多条就走 /fwd/<name>/ 的裸转发。
	var headers []string
	if *header != "" {
		headers = append(headers, *header)
	}
	res, err := cl.FwdCall(proto.FwdReq{
		Target: pos[0], Method: *method, Path: *path, Body: *data,
		Headers: headers, TimeoutMS: *timeout * 1000,
	})
	if err != nil {
		return err
	}
	if c.asJSON {
		printJSON(res)
		return nil
	}
	// 人看的时候把状态码写在第一行，body 原样吐出去——不管它是不是 JSON。
	fmt.Printf("HTTP %d  %d bytes  %dms\n", res.Status, res.Bytes, res.DurationMS)
	if res.BodyB64 != "" {
		fmt.Printf("（二进制响应 %d 字节，用 -json 取 body_b64）\n", res.Bytes)
		return nil
	}
	fmt.Print(res.Body)
	if !strings.HasSuffix(res.Body, "\n") {
		fmt.Println()
	}
	return nil
}

// multiFlag 让 -fwd 可以写多次。flag 包本身只留最后一个值，
// 而「转发目标」天然是复数。
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}
