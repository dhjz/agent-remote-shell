// Package proto 定义 wr 服务端与客户端之间的线格式。
package proto

import (
	"reflect"
	"strings"
)

// IsRequired 是「必填字段」的唯一定义，服务端校验和 openapi.json 都从这里取。
//
// 规则只有一条：没打 omitempty 的字段就是必填。指针字段（overwrite、
// respect_ignore）天然可选——它们的零值 nil 和 false 语义不同，不能混。
// 曾经文档写着 required、实际却放行空值，客户端拿着空结果当「已经同步完了」，
// 所以这一条必须两边同源，不能各写一份。
func IsRequired(f reflect.StructField) bool {
	if !f.IsExported() || f.Type.Kind() == reflect.Ptr {
		return false
	}
	tag, ok := f.Tag.Lookup("json")
	if !ok || tag == "-" {
		return false
	}
	_, opts, _ := strings.Cut(tag, ",")
	return !strings.Contains(opts, "omitempty")
}

// Version 版本号只有这一处，main 与 server 都从这里取。
// 必须是 var：const 无法被 -ldflags -X 覆盖，发布时会悄悄留着开发版本号。
var Version = "0.2.0"

// ExecRequest 一次命令执行。
type ExecRequest struct {
	Cmd       string   `json:"cmd"`
	Args      []string `json:"args,omitempty"`
	Shell     string   `json:"shell,omitempty"`
	Dir       string   `json:"dir,omitempty"`
	Env       []string `json:"env,omitempty"` // KEY=VALUE
	Stdin     string   `json:"stdin,omitempty"`
	TimeoutMS int64    `json:"timeout_ms,omitempty"`
}

// ExecResponse 执行结果。DurationMS 便于直接定位慢命令。
type ExecResponse struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMS int64  `json:"duration_ms"`
	TimedOut   bool   `json:"timed_out,omitempty"`
}

// 文件读写。Content 走 JSON 字符串，避免 base64 让体量膨胀 33%。
// 大文件走 raw 端点（见 server/routes.go），这里只服务「改几行」的日常场景。

type ReadReq struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"` // 从第几行开始，1 基；0 视作 1
	Limit  int    `json:"limit,omitempty"`  // 最多返回多少行；0 表示到文件尾
}

type ReadResp struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Total     int    `json:"total_lines"`
	From      int    `json:"from_line"`
	To        int    `json:"to_line"`
	Truncated bool   `json:"truncated"`
	// CRLF 说明这份文件是 Windows 换行。行内容里的 \r 原样保留——读到的就是
	// 磁盘上的，不然 /read 回来的东西跟 /write 写下去的对不上。
	// 只给一个提示位，让调用方知道「这行末尾那个看不见的字符是什么」。
	CRLF bool `json:"crlf,omitempty"`
}

// WriteReq 整文件写入。默认就允许新建（agent 十次里九次是在建新文件），
// 覆盖已有文件需要显式 Overwrite，免得手滑把别人的代码刷掉。
type WriteReq struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Overwrite *bool  `json:"overwrite,omitempty"` // 覆盖已存在文件；缺省 true
}

type WriteResp struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Edit 是一个替换单元。Old 必须在文件里恰好出现一次，防止改错位置。
type Edit struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// EditReq 一次应用多个替换；apply_patch 风格，但不需要记行号。
type EditReq struct {
	Path  string `json:"path"`
	Edits []Edit `json:"edits"`
}

type EditResp struct {
	Path     string `json:"path"`
	Applied  int    `json:"applied"`
	BytesIn  int    `json:"bytes_in"`
	BytesOut int    `json:"bytes_out"`
}

type Entry struct {
	Name    string `json:"name"`
	Dir     bool   `json:"dir"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"` // Unix 秒
}

type ListReq struct {
	Dir   string `json:"dir"`
	Limit int    `json:"limit,omitempty"`
}

type ListResp struct {
	Dir       string  `json:"dir"`
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated"`
}

type DeleteReq struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"` // 目录必须显式带，防止误删整棵树
}

type DeleteResp struct {
	Path    string `json:"path"`
	Removed int    `json:"removed"` // 删掉的条目数（目录递归时 > 1）
}

type MoveReq struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

type MoveResp struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type MkdirReq struct {
	Path string `json:"path"`
	Perm string `json:"perm,omitempty"` // 八进制字符串，如 "0755"；Windows 忽略
}

type MkdirResp struct {
	Path string `json:"path"`
}

// RespectIgnore 让 glob/grep 按 .gitignore 过滤（默认开）。
// 关掉它的场景：想看被忽略的东西到底在哪，比如「dist 里那个文件是不是构建出来的」。
type GlobReq struct {
	Pattern       string `json:"pattern"` // 支持 ** ，相对仓库根
	Limit         int    `json:"limit,omitempty"`
	RespectIgnore *bool  `json:"respect_ignore,omitempty"` // 缺省 true
}

// GlobResp 的 files 是按字典序截断的前 limit 条，所以同一个查询两次结果一致。
//
// Truncated 只在「撞到 limit 且确实还有更多」时为 true——结果恰好等于 limit 条
// 不算截断。旧实现把「== limit」当截断，让调用方无从判断是「就这些」还是
// 「还有更多」，这种静默的含糊比慢更贵。
type GlobResp struct {
	Files []string `json:"files"`
	// TotalFiles 是遍历完整跑完后的命中总数；截断时按 TotalFiles 走完了整棵树，
	// 所以它准，可以用来判断「还有多少没看见」。爬到上限就停时它表示「至少这么多」，
	// 此时计数为真、扫描为假。
	TotalFiles int  `json:"total_files"`
	Truncated  bool `json:"truncated"`
}

// GrepReq 一次内容检索。逐行匹配：pattern 不能跨行，多行结构用 exec。
type GrepReq struct {
	Pattern       string `json:"pattern"` // RE2 正则
	Glob          string `json:"glob,omitempty"`
	Exclude       string `json:"exclude,omitempty"` // 排除用，语义同 Glob（"*.min.js" 排除任意深度的）
	IgnoreCase    bool   `json:"ignore_case,omitempty"`
	Limit         int    `json:"limit,omitempty"` // 0 = 缺省 50；负数 400。超了置 truncated
	FilesOnly     bool   `json:"files_only,omitempty"`
	Context       int    `json:"context,omitempty"`        // 命中行前后各带几行，上限 10
	Before        int    `json:"before,omitempty"`         // 只要上文，同样上限 10
	After         int    `json:"after,omitempty"`          // 只要下文，同样上限 10
	RespectIgnore *bool  `json:"respect_ignore,omitempty"` // 缺省 true
	SearchBinary  bool   `json:"search_binary,omitempty"`  // 缺省跳过二进制文件（只在 skipped 里计数）
	TimeoutMS     int64  `json:"timeout_ms,omitempty"`     // 定时长；0 表示服务端缺省
}

// GrepMatch 一行命中。上下文行同样出现在 Matches 里，靠 Match 区分。
type GrepMatch struct {
	File  string `json:"file"`
	Line  int    `json:"line"`
	Text  string `json:"text"`
	Col   int    `json:"col,omitempty"`   // 命中位置，1 基（按字节）；上下文行没有
	Match bool   `json:"match,omitempty"` // 上下文行不带这个字段
	// Encoding 只在文件不是干净 UTF-8 时出现（"lossy"）：Text 里的 U+FFFD 是替换出来的，
	// 别拿它跟原文比。
	Encoding string `json:"encoding,omitempty"`
	// Bytes/TextTruncated 在整行超过 300 字节被截断时给出原始长度。
	Bytes         int  `json:"bytes,omitempty"`
	TextTruncated bool `json:"text_truncated,omitempty"`
}

// GrepSkipped 是「看见了但没搜」的显式回执。
// 静默跳过的代价是调用方得出「世界上没有这个文件」这种结论。
type GrepSkipped struct {
	Binary   int `json:"binary"`
	TooLarge int `json:"too_large"`
	Ignored  int `json:"ignored"` // 只统计 respect_ignore=false 时被引擎硬剪掉的（.git、.wr-jobs）
}

// GrepResp 的形状是固定的：matches / files / skipped / 三个计数永远都在，
// 空结果是 []，不是 null——下游不用每处判空。
type GrepResp struct {
	Matches []GrepMatch `json:"matches"`
	Files   []string    `json:"files"` // 去重后按路径排序；files_only 时受 limit 截断，否则是全量
	// Total* 是 limit 之外的真相：truncated=true 时它告诉你一共多少。
	TotalMatches int         `json:"total_matches"`
	TotalFiles   int         `json:"total_files"`
	Truncated    bool        `json:"truncated"`
	Skipped      GrepSkipped `json:"skipped"`
	// TimedOut 表示「搜到一半时间到了」：结果只覆盖了一部分仓库，
	// 不能当成「没有更多命中」。此时 Truncated 也一定是 true。
	TimedOut bool `json:"timed_out,omitempty"`
}

type ShellInfo struct {
	ID    string `json:"id"`
	Path  string `json:"path"`
	Found bool   `json:"found"`
	Note  string `json:"note,omitempty"`
}

type InfoResp struct {
	Version   string      `json:"version"`
	Root      string      `json:"root"`
	OS        string      `json:"os"`
	Git       string      `json:"git,omitempty"` // 沙箱内识别到的 git 仓库位置，相对 root
	Shells    []ShellInfo `json:"shells"`
	Endpoints []string    `json:"endpoints,omitempty"` // 端点清单，等价 /openapi.json 的 paths
	// AuditLog 是审计日志的绝对路径；空表示没开。
	// 日志可能落在沙箱外，路径只有服务端自己知道，不告诉客户端就只能靠猜。
	AuditLog string `json:"audit_log,omitempty"`
	// Fwd 是 -fwd 配好的上游清单，和 GET /fwd 同一份内容。
	// 起手 /info 就该知道「这台 wr 背后还有什么」：不然只会看到端点清单里有
	// /fwd/<name>/，却不知道名字填什么。没配时是空数组，不是 null。
	Fwd []FwdHop `json:"fwd"`
}

// 日志读取。日志文件常常在沙箱外，也常常早就超过 /read 的 2MiB，
// 让 agent 拿自己的 /read 去读日志是绕远路——给它一个能分页的口子。

type LogReq struct {
	Tail  int   `json:"tail,omitempty"`  // 只看最后 N 行；0 表示不限
	After int64 `json:"after,omitempty"` // 只要这个字节偏移之后的新增（跟日志用）
	Limit int   `json:"limit,omitempty"` // 单次最多回多少字节，默认 1MiB
}

type LogResp struct {
	Path      string `json:"path"`   // 日志文件绝对路径
	Output    string `json:"output"` // 原始 JSONL 文本，一行一条
	Offset    int64  `json:"offset"` // 本次读到的位置，下次当 after 传
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated,omitempty"`
}

type LogClearResp struct {
	Path    string `json:"path"`
	Cleared int64  `json:"cleared"` // 清掉了多少字节
}

// Error 统一错误体。
type Error struct {
	Error string `json:"error"`
}

// Job 是「后台跑的进程」。agent 最常干的事是 `npm run dev` 起服务再去 curl，
// 一次性 exec 撑不住这个场景：要么超时被杀，要么 nohup 之后失联。
// 所以给一个最小 job 概念：起、看日志、停。不做伪终端、不做交互输入。
type JobStartReq struct {
	Cmd       string   `json:"cmd"`
	Args      []string `json:"args,omitempty"`
	Shell     string   `json:"shell,omitempty"`
	Dir       string   `json:"dir,omitempty"`
	Env       []string `json:"env,omitempty"`
	TimeoutMS int64    `json:"timeout_ms,omitempty"` // 定时长；0 表示长驻
}

type JobLogsReq struct {
	ID    string `json:"id"`
	Tail  int    `json:"tail,omitempty"`    // 只回最后 N 行
	Since int64  `json:"since,omitempty"`   // 只看 offset > since 的新增字节
	Wait  int64  `json:"wait_ms,omitempty"` // 长轮询：等到有新增或结束，最多等这么久
	// Until 是「等到某一行出现就返回」：RE2 正则，逐段（按行）匹配。
	// 起常驻服务时等「Listening on :8002」这类首屏日志，用它就不必拿 wait_ms 干等——
	// 那个时长只能靠猜，猜短了没等到，猜长了白等一整个超时。
	// 服务起不来时进程会退出，长轮询照样立刻返回；两条件取先到的那个。
	Until   string `json:"until,omitempty"`
	Timeout int64  `json:"timeout_ms,omitempty"` // Until 的兜底时长，缺省 10000，上限 10min
}

type JobLogsResp struct {
	ID        string `json:"id"`
	Output    string `json:"output"`
	Offset    int64  `json:"offset"` // 下次拿 since=offset 就只取新增部分
	ExitCode  int    `json:"exit_code,omitempty"`
	Running   bool   `json:"running"`
	Truncated bool   `json:"truncated,omitempty"`
	// Matched 只在带了 until 时出现：这一段里有没有命中那一行。
	// until 到点返回、但一直没匹配上，它就是 false——不能当成「服务起好了」。
	Matched bool `json:"matched,omitempty"`
}

// JobInfo 单个 job 的状态。LogPath 便于直接拿 tail/浏览器看。
type JobInfo struct {
	ID         string `json:"id"`
	Cmd        string `json:"cmd"`
	Shell      string `json:"shell"`
	Dir        string `json:"dir"`
	PID        int    `json:"pid"`
	Running    bool   `json:"running"`
	ExitCode   int    `json:"exit_code"`
	StartedAt  int64  `json:"started_at"`            // Unix 秒
	DurationMS int64  `json:"duration_ms,omitempty"` // 已运行时长
	LogPath    string `json:"log_path"`              // 相对 root
	LogBytes   int64  `json:"log_bytes"`
	TimedOut   bool   `json:"timed_out,omitempty"`
}

type JobListResp struct {
	Jobs []JobInfo `json:"jobs"`
}

// Sync 是「一次请求搬整个目录」的增量通道。
//
// 传统 put 是单文件；`wr sync` 要做的是 rsync 语义：列出两端、按
// size+mtime+sha256 判断谁新、只搬差异。所以协议拆成两步，而不是一个
// 带文件列表的大请求：
//
//	POST /sync/plan   两端清单 → 要传 / 要删 / 跳过
//	POST /sync/apply  按计划落盘（内容走 /raw，不在这条 JSON 里）
//
// 拆开的好处是 dry-run 和真跑共用一份计划逻辑，而且判断「要不要传」
// 全在服务端做，客户端只管搬字节。
type SyncItem struct {
	Path    string `json:"path"` // 相对路径，正斜杠
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`            // Millis？不是：Unix 秒。Windows FAT 只到 2s，比较时留容差
	SHA256  string `json:"sha256,omitempty"` // 可选：给了就按内容判等
	Dir     bool   `json:"dir,omitempty"`
}

// SyncPlanReq 一次增量比对。
//
// Want 是本地文件的清单（只列普通文件，目录由服务端按需建）。
// 服务端返回它看不见的、或者比本地旧的、或者内容不同的——那些才需要传。
type SyncPlanReq struct {
	Dir   string     `json:"dir"`             // 目标目录（相对沙箱根）
	Want  []SyncItem `json:"want"`            // 本地清单
	Prune bool       `json:"prune,omitempty"` // 远端多出来的文件删掉（rsync --delete）
	// Checksum 强制对所有文件算 sha256 比对：mtime 不可信时（FAT/U盘、
	// 跨盘复制保不住 mtime）用这个，代价是要读一遍文件。
	Checksum bool `json:"checksum,omitempty"`
	// MtimeTolerance 秒。默认 2：FAT 的时间戳粒度就是 2 秒，
	// 按「秒级相等」判会把刚传的文件又传一遍。
	MtimeTolerance int `json:"mtime_tolerance,omitempty"`
}

type SyncPlanResp struct {
	Dir         string     `json:"dir"`
	Upload      []SyncItem `json:"upload"` // 需要上传（远端没有 / 旧了 / 内容不同）
	Same        []SyncItem `json:"same"`   // 内容一致，跳过
	Delete      []string   `json:"delete"` // prune 时远端会被删掉的
	UploadBytes int64      `json:"upload_bytes"`
	SameBytes   int64      `json:"same_bytes"`
}

// SyncApplyReq 按计划落盘。文件内容通过 /raw PUT 之前先调这个建目录；
// 删除也在这里发生——分两步是为了让「传了一半断了」不会留下半套目录结构。
type SyncApplyReq struct {
	Dir    string   `json:"dir"`
	Mkdirs []string `json:"mkdirs,omitempty"` // 需要创建的目录（相对目标目录）
	Delete []string `json:"delete,omitempty"` // 要删的文件/目录
}

type SyncApplyResp struct {
	Dir     string `json:"dir"`
	Created int    `json:"created"`
	Deleted int    `json:"deleted"`
}

// 转发：wr 站在 agent 和「wr 够不着的那台机器」之间。
//
// 存在的理由很窄但很硬：真实拓扑里浏览器（Playwright + Chromium）常常跑在
// 第三台机器上，agent 手上只有一个 WR_URL，拿不到第二个地址。
// 所以让 wr 把 /fwd/<name>/... 原样转出去，agent 还是只认一个地址。
//
// 刻意不做「把 wr 变成隧道/代理服务器」：目标一律由服务端启动参数给定，
// 客户端只能选名字，不能填 URL——否则一个带 token 的 wr 就是一台开放代理。

type FwdHop struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Spec 是启动参数里的原样写法（name=5173 或 name=http://host:port）。
	// 简写时 URL 是补全后的 127.0.0.1 地址，Spec 才能让人认出自己写了什么。
	Spec string `json:"spec"`
	// Path 是上游那边实际走的路径规则，照着念就能用：
	//   /fwd/web/...   → 剥掉 /fwd/web 再打给上游（入口段是 wr 的，上游看不到）
	//   /fwds/web/...  → 原样打给上游，/fwds/web 也一起过去（上游按带前缀的路径跑）
	Path string `json:"path"`
}

type FwdResp struct {
	Targets []FwdHop `json:"targets"`
	// 两种入口的路径模板，都带结尾斜杠。
	Forward string `json:"forward"` // /fwd/<name>/  剥掉入口段再打给上游
	ForwardAll string `json:"forward_all"` // /fwds/<name>/ 原样打给上游，入口段一起过去
}

// FwdReq 是「把一次调用打到某个上游，并把结果以 JSON 带回来」。
// 和 /fwd/<name>/... 的分工是：那层给带浏览器的 agent 用（原样透传），
// 这层给只有纯文本通道的 agent 用（看得见状态码和 body，不用自己拼 URL）。
//
// 刻意不收完整 URL：Target 只能是服务端 -fwd 里配过的名字。
// 一个带 token 的 wr 本来就能跑任意命令，再放开「客户端指定地址」，
// 它立刻变成一台内网开放代理。
type FwdReq struct {
	Target    string   `json:"target"`
	Method    string   `json:"method,omitempty"` // 缺省 GET
	Path      string   `json:"path,omitempty"`   // 上游路径，缺省 /；自动补前导 /
	Body      string   `json:"body,omitempty"`
	Headers   []string `json:"headers,omitempty"` // "K: V" 形式，可重复
	TimeoutMS int64    `json:"timeout_ms,omitempty"`
}

// FwdResult 是「转发回来的东西」的 JSON 形态。
//
// Body 用字符串：既容得下 HTML/JSON，也容得下图片（服务端只在是合法 UTF-8 时返回 body，
// 否则给 body_b64）。agent 要图时更该走 /raw 拉文件，这里先让它「看得见」。
type FwdResult struct {
	Target     string   `json:"target"`
	Status     int      `json:"status"`
	Headers    []string `json:"headers,omitempty"`
	Body       string   `json:"body,omitempty"`
	BodyB64    string   `json:"body_b64,omitempty"`
	Bytes      int      `json:"bytes"`
	DurationMS int64    `json:"duration_ms"`
}
