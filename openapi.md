# wr API

> 这份文档是手写的「人读版」。机器可读版由服务端实时生成：
> `GET /api/openapi.json`，等价路径 `GET /api/doc`，给人看的目录页 `GET /api`（**都要 token**，
> `?token=<token>` 最省事——「机器读」说的是格式，不是免鉴权，curl 一样得带）。全都挂在 `/api` 下，根路径 `/` 回空 204——
> 服务默认监听网卡，不给扫描器递地图；接口清单也是元信息，跟 `/info` 同级，同样不白给。
> 加了端点忘了改这里，`TestOpenAPICoversRoutes` 会挂——文档不会腐。

- **鉴权**：`Authorization: Bearer <token>`、`X-WR-Token: <token>`，或 URL 上的 `?token=<token>`（header 优先）
- **错误**：统一 `{"error": "..."}`，HTTP 400；token 不对 401；job 不存在 404
- **严格解码**：请求体里出现未知字段直接 400，并给出最像的字段名；
  必填字段缺失（没打 `omitempty` 的那些）同样 400 并点名。
  拼错参数名不会静默变成「没搜到」，`{"error":"未知字段 \"ignorecase\"，你是不是想用 \"ignore_case\""}`
- **路径**：除 `/raw` 外，所有 `path` / `dir` 都是**相对沙箱根**，绝对路径和 `..` 一律拒绝
- **写入边界**：`.git` 下只读不写（改索引会把仓库搞坏），其他都能写

## 端点一览

| 方法 | 路径 | 作用 | 鉴权 |
|---|---|---|---|
| GET | `/` | 空 body 的 204，什么都不给 | — |
| GET | `/healthz` | 存活探测，body 就是两个字符 `ok`（不含版本号，版本看 `/info`） | — |
| GET | `/info` | 沙箱目录、OS、可用 shell、git 仓库、端点清单、审计日志路径、已配置的转发目标 | ✓ |
| GET | `/api/openapi.json` `/api/doc` | 本文件的机器可读版 | ✓ |
| GET | `/api` | 接口目录页（HTML，`X-Robots-Tag: noindex`） | ✓ |
| POST | `/exec` | 跑一条命令，返回 stdout/stderr/exit_code | ✓ |
| POST | `/read` | 按行读，带行号 | ✓ |
| POST | `/write` | 整文件写（默认允许新建） | ✓ |
| POST | `/edit` | 局部替换，old 必须唯一命中 | ✓ |
| POST | `/delete` | 删文件；删目录要 recursive | ✓ |
| POST | `/move` | 改名 / 搬移 | ✓ |
| POST | `/mkdir` | 建目录（mkdir -p） | ✓ |
| POST | `/glob` | `**/*.go` 这类通配，按 .gitignore 过滤 | ✓ |
| POST | `/grep` | RE2 正则搜内容，按 .gitignore 剪枝 + 并发 | ✓ |
| POST | `/list` | 列目录，带 size/mtime | ✓ |
| POST | `/sync/plan` | 增量同步：比对两端清单，返回要不要传 | ✓ |
| POST | `/sync/apply` | 增量同步：建目录 / 删除 | ✓ |
| GET | `/raw?path=` | 大文件下载（裸流）；`.git` 下一律拒绝 | ✓ |
| PUT | `/raw?path=` | 大文件上传，≤512MiB；`.git` 下一律拒绝 | ✓ |
| POST | `/job/start` | 起后台任务 | ✓ |
| POST | `/job/logs` | 取日志（tail / since / 长轮询） | ✓ |
| POST | `/job/list` | 列后台任务 | ✓ |
| POST | `/job/kill` | 停后台任务（杀整棵进程树） | ✓ |
| POST | `/log` | 读审计日志（tail / after 增量） | ✓ |
| POST | `/log/clear` | 清空审计日志（动作本身也留痕） | ✓ |
| GET | `/log` | 内置实时日志页面（token 走 `?token=`，页面自己记住） | — |
| GET | `/log/stream` | 页面背后的实时流：`text/plain` 裸流，先 tail 再跟新增 | ✓ |
| GET | `/fwd` | 看服务端配了哪些转发目标（同 `/info` 的 `fwd` 字段） | ✓ |
| POST | `/fwd/call` | 把一次调用打到上游，结果按 JSON 回 | — |
| ANY | `/fwd/{name}/{path}` | 转发到 `<url>/{path}`，剥掉入口段（含 websocket、流式） | — |
| ANY | `/fwds/{name}/{path}` | 转发到 `<url>/fwds/{name}/{path}`，路径原样带上 | — |

## 命令

### POST /exec

```json
{"cmd": "go test ./...", "shell": "bash", "dir": "src", "env": ["GOFLAGS=-v"], "timeout_ms": 600000}
```

返回 `{"exit_code":0,"stdout":"...","stderr":"...","duration_ms":12}`。
命令失败**不是** HTTP 错误，看 `exit_code`。超时返回 124 并已杀掉整棵进程树。

命令会过一遍内置黑名单：注册表操作（`reg query/add/…`、`regedit`、PowerShell 的
`HKLM:` 与 `*-ItemProperty`，读也拦）、首段就是 `sudo`/`nc`/`shutdown`/`Restart-Computer`/
`chmod` 这类动作、以及格式化磁盘 / `curl | sh` / 编码命令 / fork 炸弹。
增删改查另有一条沙箱边界：路径落在 `-root` 里就放行（`rm build/tmp.o`、
`mkdir -p src/a`），跨出去才拦（`rm -rf /`、`del C:\Windows\*`、`cat /etc/shadow`）。

判路径时按**词**切，不按空格切：`rm -rf "dir with space"`、`rm -rf 'dist 2'` 都是一个参数，
引号里的空格不算分隔符。通配看的是「最多能碰到哪儿」，不是「有没有星号」——
`rm -f ./NU*`、`rm -f src/*.go`、`rm -rf build/*` 收在工作区里，放行；
`rm -rf ../*`、`rm -f C:\Windows\*` 撑破边界，拦。
一个字母都不带的通配（`rm -rf *`、`rm -rf *.*`）还是拦：那是「这一层全算」，
仓库根上敲下去就是整棵树，跟 `rm -rf .` 是一句话。写清挑哪些就够了。
两处例外，都按「碰了会不会弄坏别人的东西」判，不按「字符串里有没有斜杠」判：

- 空设备：`> /dev/null`、`> NUL`、`-o /dev/null` 一个字节都不落地，放行。
- 根 `/tmp`：临时文件的公用地，写测试产物、`go build -o /tmp/x`、读 `cat /tmp/x`、
  删 `rm -rf /tmp/x` 都放行（写了不收拾只会越攒越多）。整个 `/tmp` 本身不放手
  （`rm -rf /tmp`、`/tmp/`）——那是端走公用地。只认 `/tmp`、`\temp` 这几个根名，
  `~/tmp`、`/root/tmp`、`/var/tmp`、`/tmpx`、`/tmp/../etc` 一律不算。
被拒时返回 400，正文说明命中了哪条，`-no-defaults` 可整体关掉。

需要长驻的（dev server、watch）用 `/job/start`，别用 `timeout_ms` 硬撑。

**Windows 下的编码**：`shell` 是 `powershell` / `pwsh` / `cmd` 时，服务端会在脚本前面
补一句把控制台编码钉成 UTF-8（`[Console]::OutputEncoding` + `$OutputEncoding`，
cmd 走 `chcp 65001`），并让 `Get-Content` / `Set-Content` 默认按 UTF-8 读写。
不补的话中文 Windows 的默认代码页是 936（GBK），`Get-Content` 会把 UTF-8 文件解成
`鏃ュ織绯荤粺`——文件本身没坏，坏的是输出链路。`git` / `go` 这类直接吐字节的原生命令
不受影响，从来没乱过。

想拿回系统默认编码（比如就要 GBK 字节）就在命令里自己改回去，服务端不覆盖你的显式设置。

### 文件读写

`/read` → `{"path":"main.go","offset":1,"limit":200}`
`/write` → `{"path":"new.go","content":"...","overwrite":true}`（默认就允许新建；`overwrite:false` 时已存在会报错）
`/edit` → `{"path":"main.go","edits":[{"old":"a","new":"b"}]}`（`old` 命中 0 次或多次都报错）

两端对换行是**同一套字节**：`/write` 照传入的内容原样落盘，`/read` 也原样带回来，
CRLF 的行尾 `\r` 不抹（响应里 `crlf:true` 提示这份文件是 Windows 换行）。
写进去的和读回来的必须是一个东西——早先读取一侧悄悄把 `\r` 吃掉，agent 拿 `/read`
校对自己刚写的脚本时看着正常，跑起来 shell 报 `x: command not found`。
`/grep` 同理，命中的行文本里那个 `\r` 还在。

改几行就用 `/edit`。整文件传会让上下文翻倍，还容易把别处的改动冲掉。

### 删除 / 搬移 / 建目录

```
POST /delete  {"path":"dist","recursive":true}
POST /move    {"from":"a.go","to":"internal/a.go","overwrite":false}
POST /mkdir   {"path":"internal/job","perm":"0755"}
```

`/delete` 对非空目录必须 `recursive:true`，`/mkdir` 自带 `-p` 语义。
跨盘 rename 失败时 `/move` 自动退化成 copy + delete。

### 搜索

```
POST /glob {"pattern":"**/*.go","limit":200}
POST /grep {"pattern":"func main","glob":"*.go","ignore_case":true,"context":2}
POST /list {"dir":"src","limit":500}
```

`glob` 支持 `**`、`*`、`?`、`{a,b}`。
`grep` 的 `glob` 按「任意深度文件名」匹配（`*.go` 能吃 `src/a.go`）。
`list` 返回 `{"name","dir","size","mtime"}`，别靠文件名猜是不是构建产物。

### /glob 的契约

```json
{"files":["a.go","b/c.go"],"total_files":2,"truncated":false}
```

- **`files` 按字典序排序后再截断**：`limit` 内的前 N 条是字典序前 N 条，
  不是「遍历时碰巧先扫到的 N 条」。同一个查询两次结果一致
- **`truncated` 只在「真的还有更多」时为 true**。结果恰好等于 `limit` 条**不算**截断——
  这条是踩过的坑：把 `== limit` 当截断，调用方就分不清「就这些」和「还有更多」
- **`total_files` 是命中总数**。撞到 `limit` 后仍会把树走完再计数，所以能知道漏了多少；
  命中数超过 `limit` 一万倍时提前收工，此时 `total_files` 退化成「至少这么多」，
  但 `truncated` 仍为 true
- 缺省 `limit` = 200。**看到 `truncated:true` 别下结论**：调大 `limit`，
  或者把模式写窄（`**/` 换成具体目录）

### /grep 的契约

返回体是固定形状，不随模式变形——空结果是 `[]`，不是 `null`：

```json
{"matches":[{"file":"a.go","line":12,"text":"func main() {","col":1,"match":true}],
 "files":["a.go"],"total_matches":1,"total_files":1,"truncated":false,
 "skipped":{"binary":0,"too_large":0,"ignored":0}}
```

几条约定，都是为了让「没命中」和「没搜过」不再长得一样：

- **limit 只有一种缺省**：不传 = 50。`0` 和负数都是 400，不猜「不限」
- **truncated=true 时看 `total_matches` / `total_files`**：它们是 limit 之外的
  全量计数。`files` 在非 `files_only` 模式下也给全量（不截断）
- **顺序稳定**：按 `(file, line)` 排序后再截断，同一查询两次结果一致
- **skipped 是必看的**：二进制（含 `\0`）、超过 2MiB 的文件默认不搜，但会计数；
  `search_binary:true` 连二进制一起搜（匹配即可，不保证行是干净的）
- **`encoding:"lossy"`**：文件不是干净 UTF-8（GBK/GB18030 老项目）时按字节匹配，
  但 `text` 里的中文会是 U+FFFD。能搜到，别拿 `text` 当原文去比对
- **`col` / `bytes` / `text_truncated`**：单行超 300 字节会被截断，
  这几个字段告诉你命中在行内哪个位置、原行多长
- **`match`**：上下文行也出现在 `matches` 里，没有这个字段的就是上下文
  （`before`/`after` 可分别给，`context` 两边都给，上限各 10 行）
- **`exclude`**：排除用的 glob，语义同 `glob`（`*.min.js` 排除任意深度的）
- **逐行匹配**：pattern 不能跨行，`(?s)` 无效。要找多行结构走 `/exec` + `rg -U`

定位阶段先 `{"files_only":true}`：同样一次全库扫描，返回体只有文件列表，
比拿 200 条行文本省一个数量级，然后 `/read -offset` 看细节。

### .gitignore 与并发

`glob` / `grep` 默认**读 `.gitignore`**，嵌套文件按 git 的栈式语义作用域生效
（子目录规则不外溢，父规则在子目录里仍适用，`!` 取反可覆盖）。

- 被忽略的目录**直接不进去**，不是遍历完再筛
- `.git`、`node_modules`、`.wr-jobs` 是硬名单，写 `!` 也救不回来，
  `respect_ignore:false` 也打不开（`.wr-jobs` 是 wr 自己的后台任务日志，
  剪不剪不该取决于项目有没有把它写进 `.gitignore`）；关掉过滤时被硬剪的数量
  会在 `skipped.ignored` 里报出来
- `dist`/`build`/`target` 这类是软名单：只在仓库里一条 `.gitignore` 都没有时兜底；
  写了规则就完全以规则为准（`!dist` 能救回来）
- 传 `respect_ignore: false` 关掉过滤，连被忽略的一起找

目录遍历串行（要传 .gitignore 作用域），文件读取与正则匹配走 worker pool 并发
（`min(NumCPU, 16)` 个 worker）。并发只影响速度：出结果之后统一按 `(file, line)`
排序再截断，所以顺序是确定的——`limit` 截掉的是「排在后头的」，不是「最后扫完的」。

## 增量同步

`wr sync` 的协议面。rsync 语义拆成三步，判断全在服务端，客户端只管搬字节：

```json
// 1. 比对。want 是本地清单，只列普通文件
POST /sync/plan
{"dir":"deploy","want":[{"path":"src/main.go","size":812,"mtime":1792296405}],
 "prune":false,"checksum":false,"mtime_tolerance":2}
→ {"dir":"deploy",
   "upload":[{"path":"src/main.go","size":812,"mtime":1792296405}],  // 要传
   "same":[],                                                       // 跳过
   "delete":[],                                                     // prune 时要删的
   "upload_bytes":812,"same_bytes":0}

// 2. 建目录 + 删除（传文件之前做，传一半断了不会留半套结构）
POST /sync/apply
{"dir":"deploy","mkdirs":["src"],"delete":["src/old.go"]}
→ {"dir":"deploy","created":1,"deleted":1}

// 3. 内容：每个要传的文件走 PUT /raw?path=deploy/src/main.go
```

**判定顺序**（按代价从低到高）：

1. 远端没有 → 传
2. 大小不同 → 传
3. 给了 `sha256`（或 `checksum:true`）→ 服务端算一遍按内容判等
4. mtime 落在远端 `± mtime_tolerance`（默认 2 秒，FAT 的粒度）内 → 跳过，否则传

第 4 条是 rsync 的 quick check，**不是「本地更新才传」**。`PUT /raw` 落盘时
mtime 是远端当下时间，刚同步完的文件天然「远端更新」，按新旧判会每次全量重传。
本机复制保不住 mtime（`cp` 不带 `-p`、U 盘、FAT）时用 `checksum:true`。

- `prune:true` 时远端多出来的文件进 `delete`，删完顺手收敛空目录
- `.git` 下不参与同步；`..` 和绝对路径一律拒绝
- 目录自动创建，首次同步到不存在的目标目录是正常情况

## 后台任务

一次性 `/exec` 撑不住「起服务 → 看日志 → 打两枪接口」这个循环，所以有 job：

```
POST /job/start {"cmd":"npm run dev","dir":"web","timeout_ms":0}
→ {"id":"1018-112347-dfae390e","pid":8960,"log_path":".wr-jobs/<id>.log", ...}

POST /job/logs  {"id":"...","tail":100}              # 最后 100 行
POST /job/logs  {"id":"...","since":4096,"wait_ms":20000}  # 只要新增，等到有为止
POST /job/logs  {"id":"...","until":"Listening on","timeout_ms":10000}  # 等到这行出现就返回
POST /job/list  {}
POST /job/kill  {"id":"...","drop":true,"drop_log":true}
```

- stdout 与 stderr 合并写同一个日志文件，顺序即真实发生顺序
- `wait_ms > 0` 是长轮询：没新增就挂着等，进程一退出立刻返回，不用客户端 sleep 轮询
- `until` 是 RE2 正则、按行匹配：起常驻服务时等首屏那行（`Listening on :8002`），
  命中即返回（`matched:true`），不必拿 `wait_ms` 猜时长——猜短了没等到、猜长了白等。
  进程先退出则优先返回退出码；等满 `timeout_ms`（缺省 10s、上限 10min）仍未命中时
  `matched:false`，而进程可能还在跑，两者别混
- `drop_log:true` 停任务时连日志文件一起删。服务端 `-job-retain 72h` 会在启动时
  清掉 mtime 超过保留期的 `.wr-jobs/*.log`；不传这个参数就永不自动清
- 退出码：`124` 超时、`130` 被 kill、其他是进程真实退出码
- 并发上限 16 个；单 job 日志上限 64MiB，超了停采集但不杀进程
- 日志落在沙箱 `.wr-jobs/` 下，服务重启后文件还在（但 job 记录不保留，不会自动接管）

不做伪终端、不做交互输入——Windows 上那套太脆，收益也不对等。

## 审计日志

默认就记，落点是**执行 `wr start` 的那个目录**下的 `.wr.log`（不是 `-root`、也不是 home）：

```bash
wr start                              # → ./​.wr.log
wr start -log app.jsonl               # → ./app.jsonl
wr start -log ./logs/wr.log            # → ./logs/wr.log，目录自动建
wr start -log 'D:\logs\wr.jsonl'      # → 全路径，落沙箱外也行
wr start -no-log                      # 不记
```

落点的绝对路径会打在启动横幅和 `/info` 的 `audit_log` 上，不用猜。

一行一个 JSON 对象（JSONL），字段对齐 winreach-mcp 的审计格式，便于同一套 grep / SIEM 管道处理：

```json
{"time":"2026-10-18T22:32:55.651+08:00","decision":"allowed","tool":"exec","command":"echo hi","cwd":".","exitCode":0,"durationMs":1,"bytes":3,"remote":"10.1.2.3"}
{"time":"2026-10-18T22:32:55.654+08:00","decision":"blocked","tool":"write","path":".git/config","reason":".git 下不允许写入：.git/config"}
{"time":"2026-10-18T22:32:55.658+08:00","decision":"error","tool":"exec","command":"go build ./...","cwd":".","exitCode":1,"durationMs":812,"bytes":96,"error":"./main.go:12:2: undefined: Foo","remote":"10.1.2.3"}
{"time":"2026-10-18T22:32:55.751+08:00","decision":"error","tool":"job_exit","command":"npm run dev","cwd":".","sessionId":"1018-223255-e177dbdf","exitCode":1,"durationMs":658,"bytes":184,"error":"npm ERR! missing script: dev"}
{"time":"2026-10-18T22:32:56.101+08:00","decision":"allowed","tool":"read","path":"internal/server/exec.go","count":80,"bytes":2974,"remote":"10.1.2.3"}
{"time":"2026-10-18T22:32:56.204+08:00","decision":"allowed","tool":"grep","pattern":"secret","targets":["*.go"],"count":2,"bytes":413,"remote":"10.1.2.3"}
```

| 字段 | 含义 |
|---|---|
| `time` | ISO-8601（带纳秒） |
| `decision` | `allowed` / `blocked`（策略或 `.git` 拒绝）/ `error`（命令非零退出、超时、执行失败） |
| `tool` | 写：`exec` / `job_start` / `job_exit` / `job_kill` / `write` / `edit` / `delete` / `move` / `mkdir` / `put` / `sync_apply` / `log_clear`；读：`read` / `glob` / `grep` / `list` |
| `command` | 执行的命令（exec / job） |
| `cwd` | 相对沙箱根的工作目录 |
| `path` | 操作的文件或目录（相对沙箱根） |
| `pattern` | `glob` 的模式 / `grep` 的正则（只读动作的「做了什么」，不记结果内容） |
| `targets` | 多目标操作的附加项（move 的目标、sync 的删除清单、grep 的 `glob` 限定） |
| `count` | 结果规模：`read` 的行数、`glob`/`grep` 的命中数、`list` 的条目数；读 0 条也会写 |
| `sessionId` | job id，`job_start` 与 `job_exit` 成对 |
| `reason` | **服务器**判定这次动作为什么被拒 / 失败（策略拦截、超时、拉不起 shell） |
| `error` | **命令自己**说它为什么失败：`exec` 取 stderr（为空时退回 stdout）的第一个非空行，最多 120 字符；`job_exit` 取日志文件同样的位置。没有输出时该字段不出现 |
| `exitCode` | 进程退出码（0 也会写出来） |
| `durationMs` | 耗时 |
| `bytes` | 字节规模：`exec` 是 stdout+stderr，`read`/`glob`/`grep`/`list` 是整个响应体序列化后的大小，写类动作是写入/删除的字节数；失败时不写 |
| `remote` | 调用方 IP（token 是共享的，出事之后这是唯一还能追的线索） |

规矩：

- **失败必须有原因**。`exec` 非零退出、超时、拉不起 shell，`job_exit` 非零退出，
  都会在 `error` 里带上命令自己吐的第一行（≤120 字符）。早先这些路径只写 `exitCode`，
  日志里只剩「退出码 1」，等于把唯一的线索扔了。它和 `reason` 分工明确：
  `reason` 说服务器为什么判它失败，`error` 说它自己为什么失败。
  仍然只留第一行——审计记的是「失败长什么样」，不是把 stderr 再存一份
- **读写都记，只是详略不同**。`read` / `glob` / `grep` / `list` 各留一行，带路径或模式 + `count` + `bytes`，
  但**永远不记返回内容**——审计要回答「手伸到过哪里、伸了多长」，不是把文件再抄一份存下来。
  `count` 说「几条」，`bytes` 说「多长」：命中 1 条和命中 200 条在 `count` 上一样是一行日志，
  只有 `bytes` 能看出这次到底把几 KB 塞进了调用方的上下文
- 一行几十字节，噪音靠页面上的工具筛选和动作分色消化，不靠「记录层少记」
- **append-only**：重启是追加，不覆盖；不自动轮转（悄悄改名或截断，按位置去找的人会以为丢了一段）
- **token 永不落盘**：只记 path，不记 query，也不记 header
- 401 不记：没通过鉴权的请求连「动作」都算不上

读取用 `/log` 或 `wr log`（三种取法同 `/job/logs`）：

```bash
wr log                       # 最后 200 行
wr log -tail 50              # 最后 50 行
wr log -after 8192           # 偏移之后的新增
wr log -f                    # 持续跟
wr log -clear                # 清空（动作本身留一行）
```

日志常常在沙箱外，也常常早就超过 `/read` 的 2MiB，所以单开一个端点，
而不是让 agent 拿自己的 `/read` 去够它。没开日志（`-no-log`）时 `/log` 返回 400。

要捞所有被拒的尝试：

```bash
grep '"decision":"blocked"' .wr.log
```

### 浏览器里看日志

`wr start` 的横幅会打出一行 `日志页面 http://127.0.0.1:<端口>/log?token=<token>`，点开就是实时页。

页面有两个版本，字段完全一样（就是上面那张表）：

- `log.html`（在线）：页面从 `GET /log/stream` 拿数据。这是一个 `text/plain` 的裸流，
  先吐最后 `tail` 行（默认 500，最多 5000），之后每 500ms 看一眼审计文件，
  有新行就 push。浏览器侧 `fetch` + `resp.body.getReader()` 自己切行。
  页面上最多留 500 条，够看「刚刚发生了什么」。
- `index.html`（离线）：把 `.wr.log` 拖进去就行，靠浏览器文件句柄边读边跟，
  不需要服务端、不上传任何字节——这张可以单独拷到任何静态服务上用。

```bash
# 裸流长这样：一行一条 JSON，和文件里逐字相同
curl -N -H 'Authorization: Bearer <token>' 'localhost:7573/log/stream?tail=3'

# 断线重连可以直接报偏移，接着上次的位置推
curl -N -H 'Authorization: Bearer <token>' 'localhost:7573/log/stream?after=8192'
```

为什么不是 SSE：`EventSource` 只认 `text/event-stream`，也不能自定义 header，
token 只能裸在 URL 上。这里要的东西比 SSE 少得多——一条不断行的管道就够了。

## curl 速查

服务默认监听所有网卡的 `7573`：`wr serve`（或 `wr start`）不带 `-token` 会打印一个随机 token，
带上它就能直接调：

```bash
T=Authorization:Bearer\ <token>
curl -s -H "$T" -d '{"cmd":"git status --short"}' localhost:7573/exec
curl -s -H "$T" -d '{"path":"main.go","limit":40}' localhost:7573/read
curl -s -H "$T" -d '{"dir":"."}' localhost:7573/list
curl -s -H "$T" -d '{"pattern":"func main","respect_ignore":false}' localhost:7573/grep
curl -s -o app.exe "localhost:7573/raw?path=build/app.exe" -H "$T"
curl -s -X PUT --data-binary @app.exe "localhost:7573/raw?path=build/app.exe" -H "$T"
curl -s "localhost:7573/api/openapi.json" | jq '.paths | keys'
```

## 转发（`/fwd`）

### 为什么有这层

真实拓扑常常是三台机器：**agent → wr → 浏览器机器**。浏览器（Playwright + Chromium）
不在 wr 这台机上，而 agent 手上只有一个 `WR_URL`——它拿不到第二个地址。

所以让 wr 自己当那层垫子：目标由**服务端启动参数**给定，agent 只认名字。

`-fwd` 有两种写法，写端口就够的别写全：

```bash
# 目标机上。简写 = 本机 127.0.0.1 + http，只给端口
wr serve -root . -fwd web=5173 -fwd pw=8931

# 要连别的机器 / https / 带子路径时写全
wr serve -root . -fwd pw=http://10.0.0.9:8931 -fwd app=https://box:8443/prefix
```

简写只认端口：`name=host:port`、`name=localhost:5173` 这类半截地址会直接报错，
免得有人以为这层能连任意上游。`name=0`、`name=70000` 同样拒。

`/fwd/<name>/...` 和 `/fwd/call` **不校验 wr 的 token**：里面跑的是上游自己的服务
（vite 页面、HMR websocket、mcp 端点），它认自己的登录态；浏览器点开页面时也带不上
wr 的 header，鉴权加在这层只添堵。能到达的仍然只有启动参数里点名过的那几台上游，
不是任意 URL。

名字清单只从**带 token** 的地方拿：`GET /info` 的 `fwd` 字段（和 `GET /fwd` 同一份）、
启动横幅、`wr info`。名字匹配不上时 `/fwd/<name>/` 回**空 body 的 200**，
不告诉你配了什么——这条路免 token，谁都能敲一次，回一份「已配置: web, api」
等于把名字白送出去。所以别用「敲错名字看报错」当发现手段，直接读 `/info`。

启动横幅会把这几个名字打出来。之后：

```bash
# 1) 剥掉入口段：上游按自己的根跑（base:'/'）时用这条
$WR_URL/fwd/web/            → http://127.0.0.1:5173/
$WR_URL/fwd/web/src/main.ts → http://127.0.0.1:5173/src/main.ts

# 2) 路径原样带上：上游按 base:'/fwds/web/' 跑时用这条
$WR_URL/fwds/web/            → http://127.0.0.1:5173/fwds/web/
$WR_URL/fwds/web/src/main.ts → http://127.0.0.1:5173/fwds/web/src/main.ts
# vite 的 HMR websocket 也走同一条路，因为它挂在同一个 origin 下

# 3) 结果按 JSON 回来：给只有文本通道的 agent 用
curl -s -H "$T" -d '{"target":"pw","method":"POST","path":"/mcp","body":"{\"tool\":\"navigate\"}"}' $WR_URL/fwd/call
# → {"target":"pw","status":200,"headers":["Content-Type: application/json"],"body":"...","bytes":183,"duration_ms":12}
```

`wr fwd` 看清单（第一列是原始写法，简写后面跟着补全后的地址），
`wr fwd web -path /api/health` 打一发。

### 两条入口，只差一处

| 入口 | 上游收到 | 上游该怎么配 |
|---|---|---|
| `/fwd/<name>/x` | `/x` | `base:'/'`，页面里用相对路径 |
| `/fwds/<name>/x` | `/fwds/<name>/x` | `base:'/fwds/<name>/'`，页面里可以写绝对路径 |

差别只有「入口段要不要跟过去」。SPA 用相对路径时前者最省事；页面里写死了
带前缀的绝对路径（vite 的 `base`、Next 的 `assetPrefix`）时只有后者不会 404，
因为浏览器照着 `<script src="/fwds/web/src/main.js">` 再敲一次 wr，路径正好对得上。

其余（方法、body、header、query、websocket、SSE、token 处理、502/404/回环判定）两条路完全一样。

### 302 与 404

上游是个 dev server 时，最容易撞的两件事都在这一层被翻译成人话：

```
上游 base='/fwd/web/'，/ 上什么都没有
  $WR_URL/fwd/web/           → wr 转成上游 /  → 上游 302 回 /fwd/web/
                             wr 剥掉那一层再要一次（上游 /fwd/web/）→ 200，页面正常打开

上游对任何路径都跳回 /fwd/web/（剥一层还是它自己）
  $WR_URL/fwd/web/           → 502，正文说明「上游把自己的根挂在了 /fwd/ 的入口上」

$WR_URL/fwd/web/fwd/web/     第二个 /fwd/ 段是普通路径，照常转发到上游的 /fwd/web/
                             （只剥一层前缀；上游不认这个路径且它的根也不是 200 时回 404）
```

`Location` 是个相对路径，按 RFC 9110 它属于上游——指向的是上游自己的端口，
外部根本连不到，所以它不可能是「把浏览器送回 wr」。wr 因此不猜，跟着走：
最多 3 跳，绕回走过的路径才判回环。带 host 且指向别处的 Location 原样透传。

上游对任何路径都回 200（SPA 的常见行为）是**合理的**，wr 不拦；
只有当它对自己的根都不回 200、对不存在的路径却回 200 时才改报 404。

两条入口各探各的根：剥前缀那条探上游的 `/`，全透传那条探上游的 `/fwds/<name>/`，
结论分开存。上游只挂了 `/fwds/web/`、对自己真正的 `/` 回 404 时，`/fwds/web/` 的
200 就是它自己认下的页面——不会因为 `/fwd` 那条路的探活说过「根不是 200」而被改判成 404。

`/fwd/call` 不跟随重定向：要的就是「302 → Location: 哪儿」这个事实本身。

### 几条纪律

- **名字只能服务端定**。请求里填不了 URL——一个带 token 的 wr 本来就能跑任意命令，
  再放开「客户端指定地址」它立刻变成内网开放代理。
- **只支持 http/https**。CDP 那种裸 TCP 请用 `wr exec` 另想办法。
- **wr 的 token 不往上漏，但只摘「值等于 wr 口令」的那条**。转发前逐条比**值**而不是名字：
  `Authorization: Bearer <wr token>`、裸的 `Authorization: <wr token>`、`X-WR-Token: <wr token>` 摘掉，
  上游自己的 `Authorization`（basic、别的 Bearer）和它自己的 `X-WR-Token` 原样过去。
  摘完若上游没带 `X-WR-Token`，就把 wr 的 token 顶上（上游也是 wr、或者带同样鉴权的 mcp 服务时正好对上）。
- **转发不校验 wr 的 token**。浏览器点开页面、页面自己发的 XHR、HMR 的 websocket
  都拿不到 header，而能打到的只有 `-fwd` 点名过的那几台上游，不是开放代理。
  代价是这条路得自己守口：名字写错只回空 200，不回清单；配置本身要 token 才看得到
  （`/info` 的 `fwd`、`GET /fwd`）。
- **query 里等于 wr 口令的那个 `token` 不往上漏**。最省事的写法 `?token=…` 会把口令带进上游的访问日志；
  上游自己的同名参数（`?token=<上游的>`，mcp 这类常见）按值比对不相等，原样过去。
- **上游不可达回 502**，正文里带名字和 host，别让 agent 猜是超时还是断网。
- 转发根路径时会带上 `X-Forwarded-Host` / `X-Forwarded-Proto` / `X-Forwarded-Prefix`，
  上游才有机会拼对外部地址；子路径的请求不断言（断言错了会把重定向带偏）。
- `/fwd/call` 回包上限 8MiB，超了截断并提示改走 `/fwd/<name>/` 裸流；
  非 UTF-8 的响应体放 `body_b64`。

