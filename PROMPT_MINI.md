## 你是谁

远端机器上干活，手上只有 `wr` 这套 HTTP 接口。一个请求能办的别拆三个，改三行别覆盖整文件。**连接信息见 `remote_info.md`。**

## 铁律
- 文件操作全留远端，本机不留文件、不提交分支和 PR，保证远端代码安全
- 不在远端装 playwright 等调试插件，调试一律走 fwd 由你本地连过去

## 装一次
`command -v wr` 有输出就跳过：装进 `/usr/local/bin`（`sudo install -m 755 wr /usr/local/bin/wr`），无 sudo 就把 `~/.local/bin` 加进 PATH；Windows 放 `%LOCALAPPDATA%\wr` 再进用户 PATH。开新终端，`wr version` 出版本号再往下。

## 连接
```bash
echo 'export WR_URL=http://<host>:7573' >> ~/.bashrc   # zsh 用 ~/.zshrc
echo 'export WR_TOKEN=<token>'          >> ~/.bashrc   # 没 token 就不写
source ~/.bashrc && wr info
```
`WR_URL`/`WR_TOKEN` 只在**本机** shell 生效，临时换用 `-url`/`-token`；远端命令的环境来自服务端进程，要额外变量用 `wr exec -env K=V`，`WR_ROOT` 恒等于沙箱根。

`wr info`（`GET /info`）给出 `root`/`shells`/`git`/`fwd`：
- `root` 是沙箱根，所有路径相对它
- Windows 默认 `bash`（~105ms，够用）；`gitbash` 走 `-lc`（~2.6s）只在要别名/手感时用；`powershell` ~455ms、`cmd` 只认 Windows 原生写法；别在 bash 里套 powershell（翻倍）。中文输出服务端已钉 UTF-8，不用 `chcp`/`-Encoding`
- `git` 为空就不是仓库，先在 `root` 下找准子目录

## 只有这里才有的三条
1. 路径相对沙箱根：`src/main.go` ✅，`/abs/x` ❌，`../x` ❌
2. `.git` 只读不写：要 `git add` 就在 `exec` 里跑，别写 `.git` 里的文件
3. `HTTP 200` ≠ 成功：看 `exit_code` 和 `stderr`；超时是 `124`，进程树已被杀，别重试同一条

## 默认姿势
| 场景 | 用 | 别用 |
|---|---|---|
| 看代码 | `/read` + `offset`/`limit` | 整文件拉了再截 |
| 改几行 | `/edit`，`old` 带上文保唯一命中 | `/write` 覆盖 |
| 新建/大文件 | `/write`；二进制走 `PUT /raw` | `exec` heredoc、JSON `content` 塞二进制 |
| 找文件/找内容 | `/glob`、`/grep`（默认按 `.gitignore` 剪枝） | `ls -R`、`grep -r` |
| 看目录 | `/list`（要往下才 `recursive`） | `exec ls` |
| 构建/测试/git | `/exec`，给够 `timeout_ms`（默认 120s，上限 30min） | 猜结果 |
| dev server | `/job/start` + `/job/logs -until '关键字'` | `wait_ms` 干等、`nohup` |
| 访问 wr 背后的服务 | `/fwd`、`/fwds`、`/fwd/call` | 找第二个地址 |
| 部署一批文件 | `/sync/plan` → `/sync/apply` → `PUT /raw`（`wr sync`） | 循环 `wr put` |
| 不知道字段 | `GET /api/openapi.json`（要 token）、`/api/doc` | 猜 |

## 起手模板
```bash
wr info
wr grep 'func main' -glob '*.go' -l && wr read go.mod -offset 1 -limit 20
wr read internal/xfer/xfer.go -offset 40 -limit 60        # 先读目标段
wr edit internal/xfer/xfer.go -old '<带上下文的原文>' -new '<新内容>'
wr exec -timeout 600 'go build ./... && go test ./...'     # 再验证
wr exec 'go build ./...'                                  # 起服务前单独编一次
wr job start 'go run ./cmd/wr serve -p 8080'
wr job logs <id> -until 'Listening on'                    # 命中即返回
wr exec 'curl -s localhost:8080/healthz'
wr job kill <id> -drop -rm-log                            # 一条收干净
```
`-until` 命中就返回；进程先退出也立刻返回（打 `exit=...`）；都没发生就等满 `-timeout`（缺省 10s）并告诉你没等到。

## 请求长什么样
鉴权：`Authorization: Bearer <token>`、`X-WR-Token: <token>` 或 `?token=`。错误统一 `{"error":"..."}`：400 参数/路径非法，401 token 不对，404 job 不存在；未知字段或拼错会 400 并提示最像的字段名。
```bash
curl -s -H "$T" -d '{"cmd":"go test ./...","shell":"bash","dir":"src","timeout_ms":600000}' $WR_URL/exec
# → {"exit_code":0,"stdout":"...","stderr":"...","duration_ms":8123}
curl -s -H "$T" -d '{"path":"main.go","offset":1,"limit":200}' $WR_URL/read    # 1 基行号，>2MiB 改 offset/limit
curl -s -H "$T" -d '{"path":"main.go","edits":[{"old":"a","new":"b"}]}' $WR_URL/edit   # old 必须恰好一次
curl -s -H "$T" -d '{"pattern":"func main","glob":"*.go","context":2}' $WR_URL/grep     # glob 吃任意深度
curl -s -H "$T" -d '{"dir":"internal/xfer","limit":200}' $WR_URL/list                  # 字段是 dir；只一层
curl -s -X PUT --data-binary @app.exe "$WR_URL/raw?path=build/app.exe" -H "$T"         # ≤512MiB
curl -s -H "$T" -d '{"path":"dist","recursive":true}' $WR_URL/delete   # 删目录要 recursive
curl -s -H "$T" -d '{"from":"a.go","to":"internal/a.go"}' $WR_URL/move     # 跨盘自动 copy+delete
curl -s -H "$T" -d '{"path":"internal/job"}' $WR_URL/mkdir                  # 自带 -p
```
`exec` 单流上限 1MiB（超了截尾），要全量就重定向到文件再 `/read`。`/exec` 的 `dir` 是工作目录，`env` 是 `K=V`。

换行同一套字节：写进去的 CRLF 读回来还在，`crlf:true` 提示是 Windows 换行，别以为工具吃掉了 `\r`。

路径校验按「会不会弄坏别人的东西」判：
- 空设备放行：`> /dev/null`、`> NUL`
- 根 `/tmp` 放行读写删（`go build -o /tmp/x`、`rm -rf /tmp/*`），但 `rm -rf /tmp` 本身拦；`~/tmp`、`/var/tmp` 不算
- `/proc`、`~`、`/etc` 拦；临时文件写沙箱或 `/tmp`，别写家目录
- 带空格加引号，服务端按「词」认：`rm -rf "dir with space"` 是一个参数
- 通配看「最多碰到哪儿」：`rm -f src/*.go`、`rm -rf build/*` 放行；`rm -rf ../*`、`rm -rf *`、`rm -rf *.*` 拦

## 后台任务
```bash
curl -s -H "$T" -d '{"cmd":"npm run dev","dir":"web","timeout_ms":0}' $WR_URL/job/start
# → {"id":"1018-112347-dfae390e","pid":8960,"log_path":".wr-jobs/<id>.log","running":true}
curl -s -H "$T" -d '{"id":"<id>","until":"Listening on","timeout_ms":10000}' $WR_URL/job/logs
# → {"output":"…Listening on :8002…","offset":5120,"running":true,"matched":true}
curl -s -H "$T" -d '{"id":"<id>","since":4096,"wait_ms":20000}' $WR_URL/job/logs   # 下次 since=offset
curl -s -H "$T" -d '{"id":"<id>","drop":true,"drop_log":true}' $WR_URL/job/kill
```
- `wait_ms>0` 是长轮询：没新增就挂着，进程退出立刻返回，别自己 sleep
- `until` 是 RE2 按行匹配；等满还没命中是 `matched:false` 且进程可能还在跑——跟「起好了」不是一回事
- 退出码 `124` 超时、`130` 被杀；stdout+stderr 合并成一条时间线
- `/job/list` 找还活着的；上限 16 并发；重启后日志在，job 记录不保留

## 同步一批文件
```bash
wr sync ./dist -to build --dry-run   # 先看传什么删什么
wr sync ./dist -to build             # 只传 size/mtime 变了的
wr sync ./dist -to build -delete     # 远端多出来的也删
wr sync . -to repo -checksum         # mtime 不可信时按内容比
```
判定顺序：远端没有→传；大小不同→传；给了 sha256/`-checksum`→按内容；mtime 在 ±2 秒内→跳过。**不是「本地更新才传」**，别按新旧推理。

## 访问别的服务（`/fwd`）
上游由服务端 `-fwd name=端口` 挂进来，你只记名字，拿不到第二个地址。清单只看带 token 的地方：`wr info` 的 `fwd`、`GET /fwd`。空数组=没配，别猜名字；也别靠敲错名字试错（免 token 的入口只回空 200，不列清单）。
```bash
$WR_URL/fwd/web/src/main.ts    → 上游 /src/main.ts       # 剥入口段，base:'/'
$WR_URL/fwds/web/src/main.ts   → 上游 /fwds/web/src/main.ts  # 原样带，base:'/fwds/web/'
curl -s -H "$T" -d '{"target":"web","method":"POST","path":"/api/reload","body":"{}"}' $WR_URL/fwd/call
# → {"target":"web","status":200,"headers":[...],"body":"...","bytes":183,"duration_ms":12}
```
- 两条入口只差入口段要不要跟过去：SPA 相对路径用 `/fwd`，页面写死带前缀绝对路径用 `/fwds`，否则静态资源 404
- query、流式、websocket（vite HMR、Playwright CDP-over-ws）都走这条路，因为同 origin——浏览器能 load URL 就用它调试
- `target` 只能填配过的名字，填不了 URL；名字错 `/fwd/call` 回 400、`/fwd/<name>/` 回空 200
- 上游不可达 `502`（不是你错）；`/fwd/call` 回包上限 8MiB，非 UTF-8 给 `body_b64`
- wr 的 token 不漏给上游（只摘值等于 wr 口令那条），上游自己的凭据原样过
- 改自己仓库的代码走 `/read` `/edit`；`/fwd` 是「看别人」，不是传文件

## 别做这些
- 不读 `.git`、`node_modules`（搜索已默认跳过）
- 不用 `exec` 写几百行文件（引号必翻车，用 `/write`）
- 不整文件覆盖来「改一行」
- 不在一条 `exec` 里串「构建+起服务+等待」（超时杀整棵树）
- 不指望持久 shell：每次 `exec` 都是新进程，靠 `dir`/`env`
- 不把 token 打进日志、提交信息；`?token=` 别贴公开渠道
- 每步都在审计日志里（写动作全量、只读记路径+条数不记内容，被拒也记），别去改或清；`/log/clear` 本身留痕

## 出问题按这个顺序查
1. `curl -s $WR_URL/healthz` 通不通（免鉴权，只回 `ok`；版本看 `wr info`）
2. `wr info` 的 `root` 跟你以为的是不是同一个目录
3. 400：路径出沙箱、`old` 没唯一命中、目录没带 `recursive`、字段名拼错、必填没填（都会点名）
4. 401：token 带没带、服务端是不是现场生成了另一个
5. 124：命令确实慢，改 `timeout_ms` 或换 `/job/start`
6. 命令找不到：`shell` 选错或 PATH 不同；要别名才用 `gitbash`，别为了找 `go` 换 shell
7. 还不服：`GET /api/openapi.json?token=<token>` 对字段，别猜
