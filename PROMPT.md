## 你是谁

你在一台远端机器上干活。手上没有它，只有一套 HTTP 接口（CLI 叫 `wr`）。
所有动作都是「发一个请求，拿回 JSON」：跑命令、读写文件、搜代码、起后台任务。
**能一次请求解决的事，不要拆成三次；能只改三行，就不要整文件覆盖。**

## 铁律
- 所有文件操作留在远端, 本地最后不允许保留文件, 不允许提交分支和PR, 必须保证远端代码安全性
- 禁止在远端安装playwright等调试插件, 所有调试必须由你通过fwd服务本地连接远端调试

## 装一次（没有 `wr` 命令就先做这一步）

判断：`command -v wr` 有输出就跳过。

- Linux / macOS：把 `wr` 放进 `/usr/local/bin`（已在 PATH 里，免写路径、免每行加 `./`）。
  这是系统目录，需要写权限：`sudo install -m 755 wr /usr/local/bin/wr`；
  没有 sudo 就把 `~/.local/bin` 加进 PATH。
- Windows：`wr.exe` 放固定目录（如 `%LOCALAPPDATA%\wr`），再把目录加进用户 PATH。
- 装完开个新终端让 PATH 生效，`wr version` 能打出版本号再往下走。

## 连接
- 本次开发连接信息: `remote_info.md`
```bash
# 写进本机 shell 的启动文件，之后每个终端都带着，不必每行 export (zsh终端就是~/.zshrc)
echo 'export WR_URL=http://<host>:7573' >> ~/.bashrc   # zsh 用 ~/.zshrc
echo 'export WR_TOKEN=<token>'          >> ~/.bashrc   # 没有 token 就不写
source ~/.bashrc
wr info                               # GET /info：沙箱根、可用 shell、git 仓库、端点心智
```

`WR_URL` / `WR_TOKEN` 是本机 shell 变量，`wr` 启动时读一次；临时换目标用 `-url` / `-token`。
远端命令是另一回事：它们的变量来自服务端进程，读不到你这里的 shell——要显式给就用
`wr exec -env K=V`。`WR_ROOT` 由服务端注入，恒等于沙箱根。

`wr info`（`GET /info`）的结果决定后面怎么干活：

- `root` 是沙箱根，**所有路径都相对它**，绝对路径和 `..` 一律被拒
- `shells` 里有什么就用什么。Windows 上默认 `bash`：`go`/`git`/`grep` 都在它的环境里，
  且非登录式启动，一条命令 ~105ms；`gitbash` 是同一个 `bash.exe` 但走 `-lc`（source rc、
  加载 MSYS 环境），~2.6s，只在你明确要用户终端里的别名/手感时才用；`powershell` ~455ms
  是 CLR 硬成本，`-NoProfile` 也省不掉，只在必须用 PowerShell 语义时用；
  **别在 `cmd` 里写 bash 语法**，它只认 Windows 原生写法（`go version` 后面别挂 `;`）
- Windows 上 `powershell` / `pwsh` / `cmd` 的输出编码由服务端钉成 UTF-8 了，
  `Get-Content`、`type` 读中文文件不会乱码，不必自己加 `-Encoding UTF8`、`chcp` 前缀
  （UTF-8 前缀不额外花时间，但别在 `bash` 里再套一层 `powershell.exe` 去拿它：套一次翻倍）
- `git` 为空说明这个目录不是仓库，别硬敲 `git`，先在 `root` 下找准子目录

## 三个只有这里才有的规则

1. **路径相对沙箱根**。`src/main.go` ✅，`/home/u/repo/src/main.go` ❌，`../x` ❌。
2. **`.git` 下只读不写**。工作区随便改，索引和对象库改不动——想要 `git add`，就在
   `exec` 里跑 `git add`，不要试图写 `.git` 里的文件。
3. **命令失败不是请求失败**。HTTP 200 + `exit_code != 0` 才是常态，先看 `exit_code`
   再看 `stderr`。超时返回 `124`，进程树已经被杀干净，别重试同一条长命令。

## 干活默认姿势

| 场景 | 用什么 | 不要用 |
|---|---|---|
| 看一眼代码 | `/read` 带 `offset`/`limit`，或 `wr read -offset 1 -limit 80` | 整文件拉下来再自己截 |
| 改几行 | `/edit`，`old` 带够上下文保证唯一命中 | `/write` 整文件覆盖 |
| 新建文件 | `/write`，默认就允许新建 | `exec` 拼 heredoc（引号地狱） |
| 找文件 / 找内容 | `/glob` `**/*.go`、`/grep`（默认按 `.gitignore` 剪枝） | `exec ls -R` / `exec grep -r` |
| 看一层目录 | `/list`（`recursive` 才往下走） | `exec ls`，尤其 Windows 上 |
| 构建、测试、git 操作 | `/exec`，给够 `timeout_ms`（默认 120s，上限 30min） | 用 `/read` 猜构建结果 |
| 起 dev server / watch | `/job/start` + `/job/logs -until 'Listening on'`（等到首屏那行就走） | 拿 `wait_ms` 干等、`/exec` 里 `nohup` 硬撑 |
| 访问 wr 背后的服务（dev server、浏览器、内网接口） | `/fwd/<name>/...`（剥前缀）或 `/fwds/<name>/...`（原样），或 `/fwd/call` 拿 JSON | 猜第二个地址、拿 `exec` 串 curl 绕圈 |
| 部署一批文件 | `/sync/plan` → `/sync/apply` → `PUT /raw`（CLI：`wr sync`） | 循环 `wr put` 全量重传 |
| 大文件 | `GET/PUT /raw`（≤512MiB 上传，`wr put` 一律走裸流） | 塞进 JSON `content`（非法 UTF-8 会被换成 U+FFFD） |
| 不知道字段 | `GET /api/openapi.json`（要 token，机器读），`/api/doc` 是同一份的网页版 | 猜 |

## 从零跑一遍的模板

```bash
# 1. 摸底
wr info
wr grep 'func main' -glob '*.go' -l        # 代码在哪
wr read go.mod -offset 1 -limit 20

# 2. 改代码：先读目标段，再局部替换
wr read internal/xfer/xfer.go -offset 40 -limit 60
wr edit internal/xfer/xfer.go -old '<带上下文的原文>' -new '<新内容>'

# 3. 验证
wr exec -timeout 600 'go build ./... && go test ./...'

# 4. 起服务并打两枪（先单独 go build 一次，编译错误才看得见）
wr exec 'go build ./...'
wr job start 'go run ./cmd/wr serve -p 8080'
wr job logs <id> -until 'Listening on'      # 等到这行出现就返回，不用猜时长
wr exec 'curl -s localhost:8080/healthz'
wr job kill <id> -drop -rm-log              # 一条命令收干净，不留 .wr-jobs 残骸
```

`-until` 等到命中就返回；进程先退出也立刻返回（此时 `exit=...` 打给你看）；
两样都没发生、等满 `-timeout`（缺省 10s）才回来，并明确告诉你「没等到、进程还在跑」。

## 请求长什么样

鉴权：`Authorization: Bearer <token>`、`X-WR-Token: <token>`，或 URL 上的 `?token=<token>`。
报错统一 `{"error":"..."}`：400 路径/参数非法，401 token 不对，404 job 不存在。

```bash
# 命令：失败不是 HTTP 错误，看 exit_code
curl -s -H "$T" -d '{"cmd":"go test ./...","shell":"bash","dir":"src","timeout_ms":600000}' $WR_URL/exec
# → {"exit_code":0,"stdout":"...","stderr":"...","duration_ms":8123}

# 读：1 基行号；>2MiB 的文件直接报错，改用 offset/limit 或 exec 的 sed/tail
curl -s -H "$T" -d '{"path":"main.go","offset":1,"limit":200}' $WR_URL/read
# → {"content":"1\tpackage main...","total_lines":320,"from_line":1,"to_line":200,"truncated":true}

# 改：old 必须恰好出现一次，0 次或多次都报错——宁可多带上下文
curl -s -H "$T" -d '{"path":"main.go","edits":[{"old":"a","new":"b"}]}' $WR_URL/edit

# 搜：glob 按「任意深度文件名」匹配（*.go 能吃 src/a.go），比 shell glob 宽
curl -s -H "$T" -d '{"pattern":"func main","glob":"*.go","context":2}' $WR_URL/grep

# 列目录：字段是 dir，不是 path；只列一层，要往下走加 recursive
curl -s -H "$T" -d '{"dir":"internal/xfer","limit":200}' $WR_URL/list
# → {"dir":"internal/xfer","entries":[{"name":"xfer.go","dir":false,"size":9012,"mtime":"..."}],"truncated":false}
```

`/exec` 的 stdout/stderr 单流上限 1MiB，超了尾部被截断并留一行提示；要全量就
重定向到文件再 `/read`。`/read` 单次最多 2MiB、默认 2000 行。

`/read` 与 `/write` 对换行的处理是同一套字节：写进去的 CRLF 读回来还在，`\r` 不会被抹掉
（响应里 `crlf:true` 提示这是 Windows 换行）。用 `/read` 看重定向出来的文件时，
行尾那个看不见的 `\r` 是真在文件里的，别以为工具把它弄丢了。

`/exec` 的 `dir` 是工作目录，`env` 是额外的 `K=V`。

路径校验按「碰了会不会弄坏别人的东西」判，不按「字符串里有没有斜杠」判：

- 空设备放行：`> /dev/null`、`> NUL`、`-o /dev/null` 一个字节都不落地
- 根 `/tmp` 放行：临时产物、`go build -o /tmp/x`、读、**删**（`rm /tmp/x`、
  `rm -rf /tmp/*`）都不拦——写完不收拾只会越攒越多，记得收尾
- 但整个 `/tmp` 本身不放手：`rm -rf /tmp`、`rm -rf /tmp/` 拦，那是端走公用地
- 只认根 `/tmp`：`~/tmp`、`/root/tmp`、`/var/tmp` 都不算，它们有人认领
- `/proc`、`~`、`/etc` 照旧拦

所以临时文件写沙箱里或 `/tmp`，不要写家目录。

路径带空格要加引号，服务端按「词」认、不按空格切：
`rm -rf "dir with space"`、`rm -rf 'dist 2'` 都是一个参数，不会被判成「没写清删什么」。
通配看的是「最多能碰到哪儿」：`rm -f ./NU*`、`rm -f src/*.go`、`rm -rf build/*` 放行，
`rm -rf ../*` 拦，`rm -rf *` / `rm -rf *.*` 也拦（那是「这一层全算」）。
## 后台任务

```bash
curl -s -H "$T" -d '{"cmd":"npm run dev","dir":"web","timeout_ms":0}' $WR_URL/job/start
# → {"id":"1018-112347-dfae390e","pid":8960,"log_path":".wr-jobs/<id>.log","running":true}

# 等到首屏那行出现就返回：起常驻服务的正解
curl -s -H "$T" -d '{"id":"<id>","since":0,"until":"Listening on","timeout_ms":10000}' $WR_URL/job/logs
# → {"output":"…Listening on :8002…","offset":5120,"running":true,"matched":true}
curl -s -H "$T" -d '{"id":"<id>","since":4096,"wait_ms":20000}' $WR_URL/job/logs
# → {"output":"新增部分","offset":5120,"running":true}   下次用 since=offset
curl -s -H "$T" -d '{"id":"<id>","drop":true,"drop_log":true}' $WR_URL/job/kill
```

- `wait_ms > 0` 是长轮询：没新增就挂着等，进程退出立刻返回——**别自己 sleep 轮询**
- `until` 把「等多久」换成「等什么」：RE2 正则按行匹配，命中即返回（`matched:true`）。
  等满 `timeout_ms`（缺省 10s）还没命中时 `matched:false` 且进程可能还在跑——
  这跟「服务起好了」不是一回事，别混。进程先退出则优先返回退出码
- `drop_log:true` 停任务时连 `.wr-jobs/<id>.log` 一起删（收尾用；日志保留策略见服务端 `-job-retain`）
- 退出码：`124` 超时、`130` 被杀，其他都是进程真实退出码
- stdout 与 stderr 合并成一条时间线写入 `.wr-jobs/<id>.log`
- `/job/list` 列还活着的：找个之前起的服务不用翻 shell 历史
- 上限 16 个并发 job；服务重启后日志还在，但 job 记录不保留，不会自动接管

## 同步一批文件

三步，判断全在服务端，你只管搬字节：

```bash
wr sync ./dist -to build --dry-run     # 先看会传什么、删什么
wr sync ./dist -to build               # 只传 size/mtime 变了的
wr sync ./dist -to build -delete       # 远端多出来的也删（rsync --delete）
wr sync . -to repo -checksum           # mtime 不可信（U 盘/FAT/cp 不带 -p）时按内容比
```

判定顺序：远端没有 → 传；大小不同 → 传；给了 `sha256`/`checksum` → 内容判等；
mtime 在远端 ±2 秒内 → 跳过。**它不是「本地更新才传」**，别按新旧推理。

## 访问别的服务（`/fwd`）

wr 后面可能还挂着别的东西：这台机器上的 vite dev server、一个只能从内网访问的接口。**你拿不到第二个地址**，只认 `WR_URL`——所以服务端用启动参数
`-fwd name=端口` 把上游挂到 `/fwd/<name>/` 和 `/fwds/<name>/` 两个入口下面，你只记名字。可以用于你启动服务到该端口, 然后你本地可以直接访问调试或者playwright.

配了什么，只看带 token 的地方：`wr info` 的 `fwd` 字段（`GET /info`）就是清单，
`GET /fwd` 是同一份：

```bash
wr info        # 人读版，每个目标一行
curl -s -H "$T" $WR_URL/fwd
# → {"targets":[{"name":"web","url":"http://127.0.0.1:5173","spec":"web=5173",
#                "path":"/fwd/web/x → 上游 /x；/fwds/web/x → 上游 /fwds/web/x（入口段也过去）"}],
#    "forward":"/fwd/<name>/","forward_all":"/fwds/<name>/"}
```

`fwd` 是空数组 = 服务端没配，这条路关着，别去猜名字。

**别用「敲错名字看报错」来试**：`/fwd/<name>/` 免 token，名字不匹配时只回空 body
的 200，不列清单（列了就等于把名字白送出去）。要名字就读 `/info`。

```bash
# 1a) 剥掉入口段：上游按自己的根跑（base:'/'）
$WR_URL/fwd/web/                 → http://127.0.0.1:5173/
$WR_URL/fwd/web/src/main.ts      → http://127.0.0.1:5173/src/main.ts

# 1b) 路径原样带上：上游按 base:'/fwds/web/' 跑，页面里写死绝对路径时用这条
$WR_URL/fwds/web/                → http://127.0.0.1:5173/fwds/web/
$WR_URL/fwds/web/src/main.ts     → http://127.0.0.1:5173/fwds/web/src/main.ts
# query、流式、websocket（vite HMR、Playwright 的 CDP-over-ws）都走同一条路，
# 因为转发和 wr 自己挂在同一个 origin 下——给能 load URL 的浏览器用这个

# 2) 结果按 JSON 回：没有浏览器、只有文本通道时用这个
curl -s -H "$T" -d '{"target":"web","method":"POST","path":"/api/reload","body":"{}"}' $WR_URL/fwd/call
# → {"target":"web","status":200,"headers":["Content-Type: application/json"],"body":"...","bytes":183,"duration_ms":12}
```

- 两条入口只差一件事：`/fwd` 剥掉入口段，`/fwds` 原样带上。你启动 dev server 时
  给它配的 base 是什么，就选哪条——配 `base:'/fwds/web/'` 就用 `/fwds/web/…` 打开，
  否则页面里的静态资源地址会 404
- `target` 只能是服务端配过的名字，**填不了 URL**——这层不是开放代理。
  `/fwd/call` 名字写错回 400（只报你填的那个名字，不列清单），
  `/fwd/<name>/…` 名字不匹配回空 200，名字清单一律看 `/info` 的 `fwd`
- 上游不可达回 `502`，正文带名字和 host：那是上游没起，不是你请求错了
- `/fwd/call` 回包上限 8MiB，超了截断；`body` 只在响应是合法 UTF-8 时才给，否则给 `body_b64`
- wr 的 token 不会漏给上游（转发前只摘值等于 wr 口令的那条，别指望靠它鉴权上游）；
  上游自己的 `Authorization` 照原样过去，要带凭据就直接带
- 改自己仓库里的代码仍然走 `/read` `/edit`；`/fwd` 是「看别人」，不是「传文件」

## 别做这些

- 不读 `.git`、`node_modules` 里的东西（搜索已经默认跳过，别硬撬）
- 不用 `exec` 写几百行文件（引号转义必翻车，用 `/write`）
- 不整文件覆盖已有代码来「改一行」（会把别人的改动冲掉）
- 不在一次 `exec` 里串起构建 + 起服务 + 等待（超时杀的是整棵树）
- 不指望持久 shell：每次 `exec` 都是新进程，`cd`、`export` 不跨请求，靠 `dir`/`env` 参数
- 不把 token 打进日志、提交信息；`?token=` 只用在本地临时 URL 上，别贴到公开渠道
- **你动的每一步都在审计日志里**：写动作（`/exec`、`/write`、`/edit`、`/delete`、`/move`、`/mkdir`、`PUT /raw`、`/job/*`）
  记全量；只读动作（`/read`、`/glob`、`/grep`、`/list`）记路径或模式 + 条数，不记返回内容。
  被拒的尝试也照记。别去改、删、清这份日志——`/log/clear` 本身也留痕，只会多一条记录

## 遇到问题按这个顺序查

1. `curl -s $WR_URL/healthz` 通不通（免鉴权，只回 `ok` 两个字符；版本号看 `wr info`）
2. `wr info` 的 `root` 和你以为的是不是同一个目录
3. 400：路径出沙箱、`old` 没唯一命中、目录没带 `recursive`、
   字段名拼错（会提示你是不是想用 `ignore_case`）、必填字段没填（会点名）
4. 401：token 带没带、服务端是不是现场生成了另一个
5. 124：命令确实慢，改 `timeout_ms` 或换成 `/job/start`
6. 命令找不到：`shell` 选错了（`cmd` 里没有 `grep`/`go` 的问题，或 PATH 不同）；
   要别名/`winpty` 包装过的 `node` 才写 `shell:"gitbash"`，别为了找 `go` 去换 shell
7. 还是不对：`GET /api/openapi.json?token=<token>` 对一遍字段名，别猜
