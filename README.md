# wr —— 远程高效协同工具

一个 exe，把「跑命令 / 读写文件 / 搜代码」暴露成几个扁平的 HTTP 接口，
让 agent 在远端机器上开发的手感接近本地。

- 单二进制，零第三方依赖，Windows / Linux / macOS 同一份代码
- 文件操作**增量传输**：改 3 行只传 3 行；目录用 `wr sync` 按 mtime+size+sha 只搬差异
- 多 shell 一等公民：`pwsh` / `powershell` / `cmd` / `gitbash` / `bash` / `sh` / `wsl`；
  Windows 上输出编码由服务端钉成 UTF-8，`Get-Content` 出来的中文不会是 `鏃ュ織绯荤粺`
- 一个 `-root` 沙箱，只碰一个仓库目录；沙箱里的 `git status/diff/log` 直接可用
- **搜代码认 `.gitignore`**：被忽略的目录直接不进去，文件读取并发跑
- **后台任务**：`npm run dev` 起来 → 跟日志 → curl 打两枪 → kill，一条链路闭环
- **目录同步**：`wr sync ./dist -to build`，rsync 语义，只传变了的，`-delete` 收敛远端
- **审计日志**：读写动作都落一行 JSONL（字段对齐 winreach-mcp；只读不记返回内容；exec/job 连用哪个 shell 一起记），默认写启动目录的 `.wr.log`，`wr log` 直接看
- **自描述**：`GET /api/openapi.json` 实时生成（要 token），还有一份人读的 [openapi.md](openapi.md)；根路径 `/` 回空 204，`/healthz` 只回 `ok`，都不认领任何页面
- 不做隧道、不做桌面控制、不做 MCP 协议层

## 快速上手

客户端就是 `wr` 自己，没有单独的安装包。

```bash
# 目标机：起服务端（没有 -token 会现场生成一个打在 stderr 上）
wr start -root /home/me/repo

# agent 侧：两个变量 + 开工第一枪
export WR_URL=http://<目标机IP>:7573
export WR_TOKEN=<同一个 token>
wr info                                     # 沙箱根、可用 shell、git 仓库
wr exec -shell gitbash 'go test ./...'
```

二进制从 Release 拿（Tag 推上去时流水线编好 linux amd64/arm64 和 windows amd64），
解压即用；要自己编就 `go build -o wr ./cmd/wr`。
参数全表见[服务端](#服务端目标机上跑)，子命令见[客户端](#客户端agent-侧)。

agent 专用的那几条规矩（路径相对沙箱根、`.git` 只读、`exit_code != 0` 不算请求失败、
起服务用 `job`、访问上游走 `/fwd`）写在 [`PROMPT.md`](PROMPT.md)：`wr start` 之后
把 `WR_URL` / `WR_TOKEN` 和这份文件一起交给它。

## 为什么不是 MCP

MCP 是给「工具发现」用的，代价是每次调用都要过一层 JSON-RPC 封装 + 会话协商。
agent 真正需要的只是「给我在这个目录里跑条命令，把输出还我」——
一个 POST 就够，延迟从几十毫秒降到毫秒级，客户端也不需要任何 SDK。

## 编译

```bash
go build -o wr ./cmd/wr
# Windows
GOOS=windows GOARCH=amd64 go build -o wr.exe ./cmd/wr
```

产物 9MB 左右，`CGO_ENABLED=0` 也一样能编。

## 服务端（目标机上跑）

```bash
./wr serve -root D:\repos\myrepo
# 没有 -token 就现场生成一个，打在 stderr 上；默认只绑端口 7573，所有网卡都能连

./wr start -root D:\repos\myrepo -p 8080 -token mysecret
# start 是 serve 的别名，效果一样；审计日志默认落到当前目录的 .wr.log
```

启动时会把两条**带 token 的完整地址**和本机**哪些终端能用**一起打出来，照抄即可：

```
wr 0.2.0  沙箱目录 D:\repos\myrepo
  日志页面  http://127.0.0.1:8080/log?token=mysecret
  接口清单  http://127.0.0.1:8080/api/openapi.json?token=mysecret
  接口目录  http://127.0.0.1:8080/api?token=mysecret
审计日志 D:\repos\myrepo\.wr.log
git 仓库 D:\repos\myrepo\.git
可用终端（-shell 取值）：
  ✓ pwsh        C:\Program Files\PowerShell\7\pwsh.exe  PowerShell 7+；默认
  ✓ gitbash     C:\Program Files\Git\bin\bash.exe      Git for Windows 的 MSYS2 bash
  ✗ wsl         WSL 默认发行版，文件系统与宿主隔离；未安装
token 也可走 header：Authorization: Bearer <token> 或 X-WR-Token
```

地址只打印 `127.0.0.1`：服务端猜不准自己对外是哪个 IP，列一排网卡只是噪音。
要从别的机器连，把 `127.0.0.1` 换成那台机器的地址，端口照抄。`?token=` 与 header 等价，
方便浏览器直接点、curl 少写一个 `-H`。

常用参数（`wr start` 这些都用得上，短名一栏空着就是没有短名）：

| 参数 | 短名 | 说明 |
|---|---|---|
| `-root` | `-r` / `-d` | 沙箱根目录，所有文件操作都锁死在这里，绝对路径 / `..` / 外逃软链一律拒绝。`-d` 跟 wr 别处「d = 目录」一致 |
| `-p` | `-p` | 监听端口，默认 `7573`；只绑端口不绑地址，等于 `0.0.0.0:7573` |
| `-addr` | `-a` | 想只绑某块网卡时用 `host:port` 覆盖 `-p`，例如 `127.0.0.1:7573` |
| `-token` | `-t` | 至少 6 字符；不传就自动生成 24 字符随机串并打印出来 |
| `-deny` | `-e` | 追加命令黑名单正则，逗号分隔 |
| `-no-defaults` | | 关掉内置黑名单 |
| `-log` | `-l` | 审计日志落点：绝对路径、相对路径或裸文件名。不传默认 `<执行 wr start 的目录>/.wr.log` |
| `-no-log` | | 关掉审计日志（默认是记的） |
| `-job-retain` | | 后台任务日志保留时长，如 `72h`、`30m`；启动时清掉 `.wr-jobs/` 里 mtime 超过它的日志。不传=永不自动清 |
| `-git-dir` | | 显式指定仓库 `.git` 路径（worktree / submodule / root 下有多个仓库时用） |
| `-fwd` | `-f` | 转发目标，可重复。简写只给端口 = 本机 `127.0.0.1` + http，如 `-fwd web=5173`；要连别的机器 / https / 带子路径就写 `name=http(s)://host:port[/prefix]`。配好后有两条入口：`/fwd/<name>/x` 剥掉入口段打给上游 `/x`，`/fwds/<name>/x` 原样打给上游 `/fwds/<name>/x`。让 agent 始终只认一个地址。这层不校验 token（上游认自己的登录态），`GET /fwd` 清单除外 |

```bash
wr start -r D:\repos\myrepo -p 8080 -t mysecret
wr start -d D:\repos\myrepo -p 8080 -t mysecret     # -d 和 -r 都是 root
```

**短名只给 `wr start` / `wr serve`。** 常驻命令是手敲一遍、之后再不改的那条，
`-r` `-p` `-t` 省下的是真时间；客户端子命令一条里常挤三四个标志，短名省几个按键，
赔的是「`-d` 到底是 `-dir` 还是 `-dry-run`」这种每次都得停一下的成本，不值。
客户端写短名会直接报 `flag provided but not defined`——不给就是不给。

短名是在 `Parse` 之前整词替换成长名的：`-t` 不会误伤 `-timeout`，
`-t=30` / `-t 30` 都认，`-` 开头的位置参数（如 `seq -2 -1`）不受影响。
同一批短名撞车会在启动那一刻 panic，而不是等用户踩到才发现。

## 客户端（agent 侧）

```bash
export WR_URL=http://10.66.66.2:7573
export WR_TOKEN=<同一个 token>

wr exec -shell gitbash 'go test ./...'      # 用 git bash 跑
wr exec -dir src -timeout 600 'npm run build'
wr exec -stdin 'go run /dev/stdin' < main.go
wr exec -shell powershell 'Get-Content main.go -TotalCount 5'   # 中文不乱码，不用自己 chcp
wr read main.go -offset 1 -limit 80         # 带行号，可分页
wr edit main.go -old 'a' -new 'b'           # 改几行就传几行
wr glob '**/*.go'
wr grep 'func main' -glob '*.go' -l       # 先只列文件（-l），再 wr read 看细节
wr grep 'func main' -glob '*.go' -c 2      # 默认按 .gitignore 过滤，-no-ignore 关掉
wr put dist/app.exe -to build/app.exe       # 一律裸流，二进制安全
wr get build/app.exe -o ./app.exe
wr sync ./dist -to build                    # 目录增量同步：只传变的
wr sync . -to repo -delete --dry-run        # 先看会传什么、删什么
wr sync . -to repo -checksum                # mtime 不可信时按内容比对
wr rm  build -r                             # 删目录要 -r
wr mv  a.go internal/a.go
wr mkdir -p internal/job
wr ls  internal -long                       # 带大小与修改时间
wr job start 'npm run dev'                  # 后台起服务
wr job logs <id> -until 'Listening on'      # 等到首屏那行就返回，不用猜时长
wr job logs <id> -f                         # 跟日志，进程退出即停
wr job kill <id> [-drop] [-rm-log]          # -rm-log 连日志一起删（收尾用）
wr log -tail 200                            # 看审计日志（-f 跟、-after 增量、-clear 清空）
wr fwd                                      # 看这台 wr 背后挂了哪些上游
wr fwd web -path /api/health                # 把一条请求打到上游，结果按 JSON 回
wr openapi                                  # 拉接口清单（= /api/openapi.json）
wr info                                     # 沙箱目录、shell、git 仓库
```

**短参数只给 `wr start` / `wr serve`。** 常驻命令是手敲一遍、之后再不改的那条，
`-r` `-p` `-t` 省下的是真时间；客户端子命令一条里常挤三四个标志，短名省几个按键，
赔的是「`-d` 到底是 `-dir` 还是 `-dry-run`」这种每次都得停一下的成本，不值。

```bash
wr start -r D:\repos\myrepo -p 8080 -t mysecret
wr start -d D:\repos\myrepo -p 8080 -t mysecret     # -d 和 -r 都是 root
```

| 短名 | 长名 | 说明 |
|---|---|---|
| `-r` / `-d` | `-root` | `-d` 跟 wr 别处「d = 目录」一致 |
| `-p` | `-p` | 端口本来就是短名 |
| `-a` | `-addr` | 监听地址 |
| `-t` | `-token` | 访问 token |
| `-l` | `-log` | 审计日志落点 |
| `-f` | `-fwd` | 转发目标 |
| `-e` | `-deny` | 追加命令黑名单 |

短名是在 `Parse` 之前整词替换成长名的：`-t` 不会误伤 `-timeout`，
`-t=30` / `-t 30` 都认，`-` 开头的位置参数（如 `seq -2 -1`）不受影响。
同一批短名撞车会在启动那一刻 panic，而不是等用户踩到才发现。
客户端子命令写短名会直接报 `flag provided but not defined`——
不给就是不给，别指望「写一半也能跑」。

| 端点 | 作用 |
|---|---|
| `POST /exec` | 执行命令，返回 stdout / stderr / exit_code / 耗时 |
| `POST /read` `/write` `/edit` | 按行读、整写、局部替换 |
| `POST /delete` `/move` `/mkdir` | 删、搬、建目录 |
| `POST /glob` `/grep` `/list` | 找文件、搜内容、列目录 |
| `POST /sync/plan` `/sync/apply` | 目录增量同步（比对 → 建目录/删除 → 走 `/raw` 传内容） |
| `GET|PUT /raw` | 大文件裸流上传下载（`.git` 下一律拒绝，两个方向都是） |
| `POST /job/start` `/job/logs` `/job/list` `/job/kill` | 后台任务与日志 |
| `POST /log` `/log/clear` | 读 / 清空审计日志 |
| `GET /log` | 内置实时日志页面（免鉴权，token 由 URL 带） |
| `GET /log/stream` | 页面背后的实时流：`text/plain` 裸流按行推，先 tail 再跟新增 |
| `GET /api/openapi.json` `/api/doc` `/api` | 接口自描述（OpenAPI 3.0 / HTML 目录页，需 token） |
| `GET /fwd` / `POST /fwd/call` | 看转发目标；把一次调用打到上游，结果按 JSON 回 |
| `ANY /fwd/<name>/...` | 转发到上游，剥掉入口段：`/fwd/web/x` → 上游 `/x`。含 query、websocket、流式，免 token；上游不可达 502；上游 302 落在入口段上时替它剥一层再要一次，绕回走过的路径才报回环 |
| `ANY /fwds/<name>/...` | 转发到上游，路径原样带上：`/fwds/web/x` → 上游 `/fwds/web/x`。上游按 `base:'/fwds/web/'` 跑时用这条 |
| `GET /info` | 元信息：沙箱、shell、git、端点清单、已配置的转发目标 |
| `GET /healthz` | 存活探测，body 就是 `ok` 两个字符（免鉴权） |
| `GET /` | 空 body 的 204（免鉴权，什么都不给） |

完整字段、示例、curl 片段见 [openapi.md](openapi.md)。
`/api/openapi.json` 是运行时生成的，所以它永远和这一版的二进制一致——
加了端点忘了写文档，测试会挂。

`/grep` 的返回体是固定形状（`matches`/`files` 恒为数组，带 `total_matches`/
`total_files`/`skipped`），结果按 `(file, line)` 排序后截断，`limit` 缺省 50 且不接受
0 或负数——「没命中」和「没搜过」在这套返回体里不是一回事。字段细节见
[openapi.md](openapi.md#grep-的契约)。

## 安全模型

三层，缺一层都不算收：

1. **网络**：默认监听所有网卡的 `7573`（这工具就是拿来连远端的）。想只听本机就
   `-addr 127.0.0.1:7573`，暴露到公网前必须有防火墙或反代
2. **鉴权**：token 走常量时间比较，没给就随机生成 24 字符；下限 6 字符只挡空口令
3. **文件**：`-root` 沙箱，写入走原子 rename，软链外逃也会被拒
4. **命令**：内置黑名单分四层——注册表（`reg` / `regedit` / PowerShell 的
   `HKLM:` 与 `*-ItemProperty`，读写都拦）、首段动作（`sudo` / `nc` / `sc` /
   `systemctl` / `Restart-Computer` 这类，只看命令第一个词；**删**这一档看路径，
   见下）、改权限（`chmod` / `chown` / `icacls` 这类只看参数）、模式（格式化磁盘、
   `curl | sh`、编码命令、fork 炸弹）
5. **留痕**：读写动作全部进审计日志（只读只记路径/模式/条数），被拒的尝试（黑名单、`.git`）也照记

`.git` 是**读写两个方向都挡**的：`write`/`edit`/`put`/`delete`/`move`/`mkdir` 不许碰，
`GET /raw` 下载同样不许——worktree 的 `.git` 是个文件，内容就是仓外那条绝对路径，
`.git/config` 里的 `insteadOf` 又常带着凭据。判断按**任意一层**的 `.git` 算，
`sub/wt/.git` 这种嵌套仓库不会被当成普通目录放过去。

黑名单的判据说清楚，免得当成边界用：

- **注册表一律拦，查询也拦**。`reg query HKLM\SAM` 和 `reg add HKCU\… \d x` 一样过不去。
  理由不是「写危险」，是「读一眼就能知道这台机器装了什么、跑在谁的账户下」，
  而 agent 该待在工作区里。判据是注册表动词 + 注册表地址同时在，所以
  `go test -run Query`、`grep -rn 'reg query'` 这类不会误伤。
- **首段动作只看第一个词**。`sudo` / `nc` / `sc` / `systemctl` / `Restart-Computer`
  一出现就是越界，`go test -modfile ./x` 不是。前缀赋值、`cd x &&`、`cmd /c` 会被
  跳过去看真正那条命令。
- **增删改查按沙箱边界判，不按动词判**。这是 `-root` 那条边界挪到命令行上：
  `rm build/tmp.o`、`del build\tmp.o`、`rd /s /q build`、`mkdir -p src/a`、
  `cp a.go b.go` 都在沙箱里，放行；`rm -rf /`、`del C:\Windows\*`、
  `cat /etc/shadow`、`go build -o /root/x` 跨出去了，拦。判据落在路径上：
  `..` 往外走、`/x` 与 `D:\x` 这类绝对路径、`~`/`$HOME`、以及 `/etc`、`/usr`、
  `C:\Windows` 这些系统目录。
  切词按「词」不按空格：`rm -rf "dir with space"` 是一个参数，引号里的空格不算分隔符。
  通配看「最多能碰到哪儿」：`rm -f ./NU*`、`rm -f src/*.go`、`rm -rf build/*` 收在工作区里，
  放行；`rm -rf ../*`、`rm -f C:\Windows\*` 撑破边界，拦。
  一个字母都不带的通配（`rm -rf *`、`rm -rf *.*`、`rm -rf .`）还是拦——那是「这一层全算」，
  仓库根上敲下去就是整棵树。写清挑哪些就够了。
  删这一档因此不在「首段动作」里：见 `rm` 就拦，agent 连清理产物都做不到，
  只能绕道 `git clean` 或者求人。
- **改权限只看参数，不看首段**。`chmod +x ./wr`、`chmod 755 build/run.sh`、
  `chmod u+x bin/*`、`git update-index --chmod=+x run.sh` 都放行——打包、跑测试
  第一步就得用。拦的是 `-R` 递归、`777`/`666`/`a+w`/`o+w`/`+s`、以及
  `/`、`~`、`$HOME`、`..` 这类出工作区的路径；`chown` / `icacls` / `setfacl` /
  `takeown` 改属主或接管一律拦（工作区里没这个需求）。边界和 `-root` 是同一条：
  仓库里的文件权限随便定，仓库外的别碰。
- **引号里那段只在「执行面在引号里」时才判**。`sh -c "rm -rf /"`、`cmd /c "reg query HKLM\SAM"`
  得拦；`echo "rm -rf /" > notes.md`、`git commit -m "rm -rf /"`、`grep -rn 'reg query'`
  里那段是数据，不拦。判据是引号前面那个词：`sh`/`bash`/`pwsh`/`cmd`/`iex` 这类，
  或 `env`/`sudo`/`xargs` 后面第一个不带开关的词，才当命令看。
- **分段看，不只看第一段**。`echo x && rm -rf /` 里 `&&` 后面那条真会跑，
  按 `;`、`|`、`&` 切开挨段看首段动作。
- `-no-defaults` 把内置词表与模式一起关掉，只留 `-deny` 的规则。
  想恢复默认就在同一处删掉这个参数。

注意黑名单**不是安全边界**，token 也不是。真正的边界是：

- **`exec` 能跑任何东西。** 有 token 就能 `cat /etc/passwd`、写 `.git/evil`、
  改沙箱外的东西——`-root` 只约束文件类端点，shell 里它形同虚设。
  所以别把 token 给不该有 shell 的人；对 agent 就是「你愿意让它在这台机器上敲命令」。
- **跑服务的账户权限 + 目录 ACL。** 给 agent 一个只能读这个目录的低权限账户，
  比写 100 条正则都管用。
- **网络位置。** 默认绑 `0.0.0.0`，暴露到公网前必须有防火墙或反代；
  只信本机就 `-addr 127.0.0.1:7573`。

`wr exec` 里拿到的 shell 权限就是你起服务那个账户的权限，这一点必须有意识。

## 设计取舍

**不做持久会话，但做后台任务。**
常驻 shell 在 Windows 上要靠 base64 + marker 注入，脆且跨 shell 语义不一致——这条不做。
但「起个 dev server 再打两枪」是 agent 的日常，一次性 exec 撑不住（要么超时被杀，要么
`nohup` 之后失联）。所以有 `job`：不交互、不占连接，输出直接落 `.wr-jobs/<id>.log`，
客户端用 `since=offset` 只取增量，用 `wait_ms` 长轮询不用自己 sleep。

**`wait_ms` 猜不准，所以补了 `until`。**
起常驻服务时人真正关心的是首屏那一行（`Listening on :8002`）。`wait_ms` 只有两个结局：
猜短了没等到、猜长了白挂一整段——实测里最贵的一次是 620 秒的 `job_exit durationMs`，
全是「等一个不该等的东西」。`until` 把「等多久」换成「等什么」：正则按行匹配，
命中即返回；进程先退出也立刻返回（服务起不来时不会陪它等满兜底）；两样都没发生才等到
`timeout_ms`，并明确报 `matched:false` —— 「等够了」和「等到了」必须分得开。
`.wr-jobs/` 只涨不落的问题同理：`-job-retain` 在启动时清过期日志、`job kill` 带 `drop_log`，
一次收尾不用记两遍命令。

**`.git` 放行，但只读。**
沙箱把所有路径锁在 `-root` 里，`.git` 不例外——不然 `git status` 直接报 `not a git repository`，
等于让 agent 瞎着眼改代码。启动时用 `git -C <dir> rev-parse --absolute-git-dir` 真问一次，
拿到可用的 `GIT_DIR` 注入进 exec 的环境；不是「有 .git 目录就当仓库」（空目录、坏掉的
worktree `.git` 文件都会让后续所有 git 命令全废）。
写入侧仍然拒绝 `.git` 下的任何改动：改工作区可以，改索引和对象库不行。
浏览和搜索默认跳过 `.git`（列目录时它作为一项露出来，想看就 `wr ls .git`）。

**同步走「清单比对」而不是「全量重传」。**
`wr put` 一次一个文件，不知道该传什么，所以每次都全量。整个 `dist/` 上传是部署的日常，
全量的代价肉眼可见。`wr sync` 先把本地清单交给服务端比对（`/sync/plan`），
只传差异，再收敛多余文件。判定用 rsync 的 quick check：size + mtime（容差 2 秒，
FAT 的粒度），`-checksum` 时改用 sha256。
注意 mtime 这条**不是「本地更新才传」**——远端落盘的 mtime 是当下时间，
刚传完的文件天然「远端更新」，按新旧判会每次全量重传。

**审计默认开，读写都记，只读不记内容。**
默认开是因为「出事之后才想开日志」永远来不及；落点按**执行 `wr start` 的目录**算，
不是 `-root` 也不是 home——说"记到 `.wr.log`"，回头找它时一定站在那个目录里。
`read` / `glob` / `grep` / `list` 也各留一行，只带路径或模式 + `count`（读了几行、命中几处）
+ `bytes`（返回体多大），**返回内容永不落盘**：审计要回答「这个 token 被谁、从哪、动过什么」，
读了哪些文件同样是「动过」。`count` 说几条、`bytes` 说多长——同一次 glob 命中 1 个和 200 个，
在日志里都得能分出轻重。

失败的行还要有原因。`exec` 非零退出 / 超时 / 拉不起 shell、`job_exit` 非零退出，
都在 `error` 字段里带上命令自己吐的**第一个非空行**（最多 120 字符，stderr 空时退回 stdout）：
`{"tool":"exec","exitCode":1,"durationMs":812,"bytes":96,"error":"./main.go:12:2: undefined: Foo"}`。
只留第一行是刻意的——审计要回答「这次失败长什么样」，不是把 stderr 再存一份；
`error` 和 `reason` 也分工明确：`reason` 是服务器判它的理由（策略拦截、超时），`error` 是它自己的说法。
`exec` / `job_start` / `job_exit` / `job_kill` 还多一个 `shell`：记的是**实际跑命令的那个 profile**
（`bash` / `gitbash` / `pwsh` / `cmd` / `wsl` / 自定义路径），不是请求里那个可能为空的入参。
只记 `command` 不够——同一条 `Get-ChildItem x | % Name` 在 pwsh 和 cmd 里根本不是一回事，
「这台机器跑得通、那台跑不通」十有八九是 shell 不同，不是命令不同。
噪音交给展示层——页面上按工具筛、按结论分色，比在记录层把信息丢掉强。
`exitCode` 0 也写出来，因为 0 恰恰是最有意义的那种。

**浏览器里也能看日志，两种看法。**
`index.html` 是离线版：把 `.wr.log` 拖进去，靠浏览器的文件句柄边读边跟，
不需要服务端、不上传任何字节，这一张可以直接拷到任何静态服务上。
`log.html` 是在线版：`GET /log/stream` 一条 `text/plain` 的裸流，先给最后 500 行，
之后每 500ms 看一眼文件、有新行就 push——页面 fetch 之后 `getReader()` 自己切行。
不用 SSE（EventSource 只认 `text/event-stream`，也不能带 header，token 只能裸奔在 URL 上），
也不用 WebSocket（多一套握手和心跳，而这里要的只是一条不断行的管道）。
页面上限 500 条：日志的价值在「刚刚发生了什么」，堆几千条只会让页面卡。

**不做日志轮转。** `-log` 的语义是"记到我说的那个文件"，悄悄改名或截断，
按位置去找的人会以为丢了一段。要清就显式 `wr log -clear`，清空这个动作本身也留一行。

**不做 MCP 协议层。** 见上。

**超时杀整棵进程树。** POSIX 用独立进程组按组 SIGKILL，Windows 用 `taskkill /F /T`。
实测 `sleep 300 & sleep 300 & wait` 这类语法级 fork 会留下孤儿，所以主进程退出后
还会确认整组清空再返回。job 的 kill 走同一条路径。

**`write` 默认允许新建。** agent 十次里九次是在建新文件，每次都要记得加 `-create`
是最没意义的摩擦。反过来，覆盖已有文件要 `-overwrite`（CLI 默认 true，想保护就显式关）。
删目录要 `-r`：少一个 flag 比多一次误删便宜得多。

**`old` 必须唯一命中。** `edit` 的替换单元出现 0 次或多次都报错，
宁可让你补上下文，也不要改错地方。

**`put` 一律走裸流。** `write`/`edit` 的 `content` 是 JSON 里的字符串，非法 UTF-8
字节到达服务端会被换成 U+FFFD——拿它传二进制，文件必然坏，而且每个坏字节膨胀成 3 字节。
所以 `put` 不再按 8MiB 分流，任何大小都直接 `PUT /raw`。
传二进制只走 `put` 或 `sync`，`write` 留给文本。

## 目录

代码行数按 `wc -l` 统计（含注释与空行），功能代码与测试分开。
每次提交 PR 都该同步这张表——`TestReadmeDirTableInSync` 会拿磁盘上的真实行数对账（合计行也查，别只改分行），
忘了更新它会直接挂。表格比任何「架构图」都更容易过期，所以只留两个数字。

| 子目录 | 作用 | 功能 | 测试 |
|---|---|---:|---:|
| `internal/audit/` | JSONL 审计日志：落点解析、追加写、清空、失败原因摘录 | 208 | 243 |
| `internal/client/` | 瘦客户端：一个 net/http 封装 | 335 | 122 |
| `internal/proto/` | 线格式 + 唯一的版本号 | 451 | 0 |
| `internal/server/` | HTTP 端点、执行、文件工具、搜索、后台任务、同步、转发、日志页面、openapi、超时与进程树 | 5187 | 4418 |
| `internal/xfer/` | 沙箱路径解析、原子写、删除/搬移 | 285 | 0 |
| `internal/shellx/` | 各 shell 的 args / 默认值 / 路径探测 / 编码前缀 | 328 | 129 |
| `internal/policy/` | 命令黑名单：注册表、首段动作（含沙箱边界、空设备与根 /tmp 放行、引号与通配按词切）、改权限、模式四层 | 1007 | 467 |
| `internal/webapp/` | 内置页面：`index.html` 离线看日志、`log.html` 实时看日志（页面是 HTML，这里只数 embed 那几行） | 21 | 0 |
| **合计** | | **7822** | **5379** |

测试集中在 `internal/server/`：`Exec`（超时杀树、输出截断）、`Edit`（唯一命中）、
`xfer.Resolve`（`..` / 软链外逃）、`.gitignore` 作用域、job 生命周期、sync 增量判定——
这些恰恰是最容易出错、又最难靠肉眼发现的地方。

```
cmd/wr/            命令行入口（serve + 客户端子命令 + sync、fwd、log）
internal/webapp/   内置页面：index.html 离线看、log.html 实时看（embed 进二进制）
internal/audit/    JSONL 审计日志：落点解析、追加写、清空、失败原因摘录
internal/shellx/   各 shell 的 args / 默认值 / 路径探测
internal/server/   HTTP 端点、执行、文件工具、搜索、后台任务、同步、转发、日志页面、openapi、超时与进程树
internal/client/   瘦客户端
internal/policy/   命令黑名单
internal/xfer/     沙箱路径解析、原子写、删除/搬移
internal/proto/    线格式 + 版本号
```
