package server

import (
	"net/http"
	"reflect"
	"sort"
	"strings"

	"cnb.cool/yyx946/tteist/internal/proto"
)

// OpenAPI 手写而不是代码生成：端点就十来个，反射出来的 schema
// 还要额外维护一份 struct tag 规则，得不偿失。
//
// 唯一不能手写的是「哪些字段必填」：它必须和 decode() 的校验同一份来源
// （proto 的 struct tag），否则文档写必填、服务端却放行，等于给 agent 挖坑。
// 唯一要守的纪律是——加了端点就加到 spec() 里，
// TestOpenAPICoversRoutes 会用 Handler() 的路由表反过来验证，漏了直接挂。
func (s *Server) spec() map[string]any {
	paths := map[string]any{}

	b := &schemaBuilder{defs: map[string]any{}}
	op := func(method, path, summary, desc string, auth bool, in, out any) {
		entry, _ := paths[path].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
			paths[path] = entry
		}
		o := map[string]any{
			"summary":     summary,
			"description": desc,
			"responses": map[string]any{
				"200": b.jsonResponse(summary+" 成功", out),
				"400": b.jsonResponse("参数或执行出错", proto.Error{}),
			},
		}
		if in != nil {
			o["requestBody"] = map[string]any{
				"required": true,
				"content":  map[string]any{"application/json": map[string]any{"schema": b.requestSchema(reflect.TypeOf(in))}},
			}
		}
		if auth {
			o["security"] = []any{map[string]any{"bearer": []any{}}}
			o["responses"].(map[string]any)["401"] = b.jsonResponse("token 无效", proto.Error{})
		}
		entry[strings.ToLower(method)] = o
	}

	// 根和存活探测都是「门铃」：不鉴权、不递任何地图。响应体是纯文本，
	// 所以这里不走 op() 的 JSON 模板，手写两条。
	paths["/"] = map[string]any{"get": map[string]any{
		"summary":     "根路径（不返回任何内容）",
		"description": "空 body 的 204。不鉴权，也不给目录/清单——服务默认监听网卡，\"/\" 是扫描器敲的第一扇门。",
		"responses": map[string]any{
			"204": map[string]any{"description": "无内容"},
		},
	}}
	paths["/healthz"] = map[string]any{"get": map[string]any{
		"summary":     "存活探测",
		"description": "不鉴权，body 就是两个字符 `ok`（text/plain，不带版本号）。版本号走 `/info`。",
		"responses": map[string]any{
			"200": map[string]any{"description": "ok", "content": map[string]any{"text/plain": map[string]any{"schema": map[string]any{"type": "string", "example": "ok"}}}},
		},
	}}
	op("GET", "/info", "服务元信息",
		"沙箱目录、OS、可用 shell 列表、已配置的转发目标（fwd）。起手先调这个确定 -shell 用哪个、\n"+
			"以及 /fwd/<name>/ 这条路能不能走、名字填什么。", true, nil, proto.InfoResp{})

	op("POST", "/exec", "执行一条命令",
		"一次性执行，超时后杀整棵进程树。命令失败体现在 exit_code，不是 HTTP 错误。\n"+
			"需要长驻（dev server）用 /job/start。", true, proto.ExecRequest{}, proto.ExecResponse{})

	op("POST", "/read", "按行读文件", "返回带行号的内容；大文件用 offset/limit 分段。", true, proto.ReadReq{}, proto.ReadResp{})
	op("POST", "/write", "整文件写入",
		"默认允许新建文件；覆盖已有文件需 overwrite=true。改几行请用 /edit，别整文件传。",
		true, proto.WriteReq{}, proto.WriteResp{})
	op("POST", "/edit", "局部替换", "old 必须在文件中恰好出现一次，0 次或多处都报错——宁可让你补上下文，也不改错地方。", true, proto.EditReq{}, proto.EditResp{})
	op("POST", "/delete", "删除文件或目录", "目录非空时必须 recursive=true。写操作一律拒绝 .git 下的路径。", true, proto.DeleteReq{}, proto.DeleteResp{})
	op("POST", "/move", "改名 / 搬移", "跨盘时自动退化为 copy+delete。", true, proto.MoveReq{}, proto.MoveResp{})
	op("POST", "/mkdir", "建目录", "等价 mkdir -p。", true, proto.MkdirReq{}, proto.MkdirResp{})

	op("POST", "/glob", "按通配符找文件",
		"支持 ** / * / ? / {a,b}，如 '**/*.go'。默认按 .gitignore 过滤，并跳过 .git、node_modules。\n"+
			"respect_ignore=false 可连被忽略的也一起找。", true, proto.GlobReq{}, proto.GlobResp{})
	op("POST", "/grep", "搜索文件内容",
		"RE2 正则，逐行匹配——pattern 不能跨行，(?s) 也无效；多行结构请用 /exec 加 rg/pcre。\n"+
			"目录侧按 .gitignore 剪枝（被忽略的目录根本不进），文件侧并发扫描。\n"+
			"结果按 (file, line) 排序后截断，同一查询结果可复现。\n"+
			"limit 缺省 50，0 和负数都会被拒；truncated=true 时用 total_matches/total_files 看全量。\n"+
			"skipped 是被跳过的东西（二进制/超大/硬剪枝）的计数——没命中不代表没搜过。\n"+
			"非 UTF-8 的文件照样按字节匹配，但命中行会带 encoding=lossy（text 里的 U+FFFD 是替换出来的）。\n"+
			"定位阶段优先 files_only=true：同样一次全库扫描，返回体只有文件列表。", true, proto.GrepReq{}, proto.GrepResp{})
	op("POST", "/list", "列目录", "带 size 和 mtime，免得靠猜判断哪个是构建产物。", true, proto.ListReq{}, proto.ListResp{})

	op("POST", "/sync/plan", "增量同步：比对清单",
		"rsync 语义的上半段：客户端把本地文件清单（path/size/mtime，可选 sha256）发过来，\n"+
			"服务端返回 upload（需要传）/ same（跳过）/ delete（prune 时远端多余的）。\n"+
			"判断顺序 size → mtime(容差 2 秒) → sha256，代价从低到高；checksum=true 时全算 sha256。\n"+
			"上传的文件内容走 PUT /raw，不在这条 JSON 里。", true, proto.SyncPlanReq{}, proto.SyncPlanResp{})
	op("POST", "/sync/apply", "增量同步：建目录与删除",
		"按 plan 的结果落地：建好 upload 要用到的目录、删掉 delete 里的文件并收敛空目录。\n"+
			"先调它再 PUT 文件——传一半断了也不会留下半套结构。", true, proto.SyncApplyReq{}, proto.SyncApplyResp{})

	op("POST", "/job/start", "起一个后台任务", "stdout+stderr 合并写入 "+jobDirName+"/<id>.log；timeout_ms=0 表示长驻。上限 "+itoa64(maxJobs)+" 个。", true, proto.JobStartReq{}, proto.JobInfo{})
	op("POST", "/job/logs", "取后台任务日志",
		"三种取法：从头、tail=N 要末尾 N 行、since=offset 只要新增。\n"+
			"wait_ms>0 时长轮询：没新增就挂着等，进程退出立即返回。", true, proto.JobLogsReq{}, proto.JobLogsResp{})
	op("POST", "/job/list", "列后台任务", "按启动时间倒序，含运行状态、exit_code、日志路径。", true, struct{}{}, proto.JobListResp{})
	op("POST", "/job/kill", "停后台任务", "杀整棵进程树；drop=true 同时从列表移除（日志文件保留）。", true, map[string]any{"id": "job id", "drop": false}, proto.JobInfo{})

	op("POST", "/log", "读审计日志",
		"一行一条 JSONL，字段对齐 winreach-mcp 的审计格式：time / decision / tool / command / cwd /\n"+
			"exitCode / durationMs，外加 wr 的 path / targets / bytes / remote。\n"+
			"三种取法同 /job/logs：从头、tail=N 要末尾 N 行、after=偏移只要新增。\n"+
			"日志常在沙箱外，也可能早已超过 /read 的上限，所以单开这个端点而不是让 /read 去够它。\n"+
			"没开日志（服务端 -no-log）时返回 400。", true, proto.LogReq{}, proto.LogResp{})
	op("POST", "/log/clear", "清空审计日志",
		"清空文件内容。清空这个动作本身也会写进日志（先留痕再截断）。", true, struct{}{}, proto.LogClearResp{})

	// 转发不是「一种新能力」，是「让 agent 只认一个地址」的那层垫子。
	// 目标清单只认服务端启动参数，所以这里只有一个只读的列表端点；
	// 清单本身要 token，打到上游那两路不要——理由见 server.go 的 Handler()。
	paths["/fwd"] = map[string]any{"get": map[string]any{
		"summary": "已配置的转发目标",
		"description": "列出 -fwd 配好的上游（简写 name=5173 补成 http://127.0.0.1:5173，spec 回显原始写法，\n" +
			"path 给出两条入口各自的路径规则）。两种转发都是一样的透传（含 query、websocket 升级、流式响应），\n" +
			"并且**不校验 wr 的 token**（浏览器里点开页面时没有 header 可带）：\n" +
			"- `/fwd/<name>/…` 剥掉入口段：`/fwd/web/x` → 上游 `/x`\n" +
			"- `/fwds/<name>/…` 原样转发：`/fwds/web/x` → 上游 `/fwds/web/x`\n" +
			"清单本身还要 token：它回显的是 wr 自己的配置。\n" +
			"目标只能由服务端启动参数指定，请求里填不了 URL——否则 wr 就成了开放代理。",
		"security": []any{map[string]any{"bearer": []any{}}},
		"responses": map[string]any{
			"200": b.jsonResponse("转发目标清单", proto.FwdResp{}),
			"401": b.jsonResponse("token 无效", proto.Error{}),
		},
	}}
	op("POST", "/fwd/call", "转发一次调用，结果按 JSON 回来",
		"给「只有一个 JSON 通道」的 agent 用：选一个已配置的上游，带上方法/路径/body，\n"+
			"回 {status, headers, body}。非 UTF-8 的响应体放 body_b64，超过 8MiB 截断。\n"+
			"**不跟随重定向**：上游回 302 就给出 302 和它的 Location，别让 agent 以为\n"+
			"自己请求的那个地址是 200。有浏览器/能load URL 的 agent 更该直接用 `/fwd/<name>/...`。",
		false, proto.FwdReq{}, proto.FwdResult{})

	paths["/fwd/{name}/{path}"] = map[string]any{"get": map[string]any{
		"summary": "转发到上游，剥掉入口段（任意方法）",
		"description": "`/fwd/<name>/rest` → `<url>/rest`，**只剥一层前缀**（所以 rest 里再出现 `/fwd/` 也只当普通路径）。\n" +
			"上游按自己的根跑（base:'/'）时用这条。要让入口段也一起过去，用 `/fwds/<name>/...`。\n" +
			"方法、body、header（除 wr 的 Authorization/X-WR-Token）、query 里的 token、\n" +
			"websocket 升级、SSE 流式响应都会处理。**不校验 wr 的 token。**\n\n" +
			"wr 自己拍的板只有三种：上游连不上回 502；上游 302 落在 /fwd/<name>/ 上时替它剥一层再要一次，\n" +
			"绕回走过的路径才回 502 并说明是回环（vite 的 base:'/fwd/<name>/' 因此能直接开）；\n" +
			"上游对不存在的路径回 200 而对自己的根不回 200 时改报 404（SPA 对任何路径都回 200 则照旧 200）。\n" +
			"名字不存在时回空的 200（不列出已配置的名字——这条路径免 token）。转发根路径时附带 X-Forwarded-Host/Proto/Prefix。",
		"parameters": []any{
			map[string]any{"name": "name", "in": "path", "required": true, "description": "启动参数里配的名字", "schema": map[string]any{"type": "string"}},
			map[string]any{"name": "path", "in": "path", "required": false, "description": "透传给上游的路径", "schema": map[string]any{"type": "string"}},
		},
		"responses": map[string]any{
			"200": map[string]any{"description": "上游的原始响应（可能是任意 Content-Type）；名字没匹配上时是空 body 的 200"},
			"404": b.jsonResponse("上游确实没有这个路径", proto.Error{}),
			"502": b.jsonResponse("上游连不上 / 超时 / 302 绕回 wr 入口（转发回环）", proto.Error{}),
		},
	}}

	paths["/fwds/{name}/{path}"] = map[string]any{"get": map[string]any{
		"summary": "转发到上游，路径原样带上（任意方法）",
		"description": "`/fwds/<name>/rest` → `<url>/fwds/<name>/rest`：入口段也一起打给上游。\n" +
			"上游按 `base:'/fwds/<name>/'` 跑（页面里写死带前缀的绝对路径）时用这条，\n" +
			"静态资源和 HMR 地址不用改就落回同一个前缀。\n" +
			"与 `/fwd/<name>/...` 只差这一处：入口段剥不剥。其余（方法、body、header、query、websocket、\n" +
			"SSE、不校验 wr 的 token、502/404/回环判定）都一样。\n" +
			"结尾带斜杠的路径会被 301 到干净形式。",
		"parameters": []any{
			map[string]any{"name": "name", "in": "path", "required": true, "description": "启动参数里配的名字", "schema": map[string]any{"type": "string"}},
			map[string]any{"name": "path", "in": "path", "required": false, "description": "原样转给上游的路径（含入口段）", "schema": map[string]any{"type": "string"}},
		},
		"responses": map[string]any{
			"200": map[string]any{"description": "上游的原始响应（可能是任意 Content-Type）；名字没匹配上时是空 body 的 200"},
			"404": b.jsonResponse("上游确实没有这个路径", proto.Error{}),
			"502": b.jsonResponse("上游连不上 / 超时 / 302 绕回 wr 入口（转发回环）", proto.Error{}),
		},
	}}

	// 自描述本身也是端点：不放进来就等于「文档里没有文档」。
	// 都挂在 /api 下，而且都要 token：接口清单是「这台机器能干什么」的元信息，
	// 和 /info 同级；根路径 / 回空 204，扫描器顺手敲的那个地址上什么也没有。
	paths["/api"] = map[string]any{"get": map[string]any{
		"summary":     "接口目录页（HTML）",
		"description": "给人看的接口列表，返回 HTML 不是 JSON。**要 token**（`?token=` 最省事）——它自己会去 fetch `/api/openapi.json`，那份同样要 token。",
		"security":    []any{map[string]any{"bearer": []any{}}},
		"responses": map[string]any{
			"200": map[string]any{"description": "接口目录页", "content": map[string]any{"text/html": map[string]any{"schema": map[string]any{"type": "string"}}}},
			"401": b.jsonResponse("token 无效", proto.Error{}),
		},
	}}
	for _, p := range []string{"/api/openapi.json", "/api/doc"} {
		paths[p] = map[string]any{"get": map[string]any{
			"summary":     "接口清单（OpenAPI 3.0）",
			"description": "返回本文档的机器可读版。\"机器可读\"说的是格式（JSON 而不是 HTML），**不是免鉴权**：它和 /info 同级的元信息，不该白给。`?token=` 或 header 都行。",
			"x-note":      "带 ?token= 的 curl 一样能拿；不带会 401 token 无效。",
			"security":    []any{map[string]any{"bearer": []any{}}},
			"responses": map[string]any{
				"200": map[string]any{"description": "OpenAPI 文档", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object"}}}},
				"401": b.jsonResponse("token 无效", proto.Error{}),
			},
		}}
	}

	paths["/raw"] = map[string]any{
		"get": map[string]any{
			"summary":     "大文件下载",
			"description": "query 参数 path（相对沙箱根）。body 是文件本身，不是 JSON。`.git` 下（任意一层）一律 400：worktree 的 .git 文件指向仓外，.git/config 可能带凭据。",
			"security":    []any{map[string]any{"bearer": []any{}}},
			"parameters":  []any{queryParam("path", "相对沙箱根的路径", true)},
			"responses": map[string]any{
				"200": map[string]any{"description": "文件内容", "content": map[string]any{"application/octet-stream": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}}}},
				"400": b.jsonResponse("路径非法", proto.Error{}),
				"401": b.jsonResponse("token 无效", proto.Error{}),
			},
		},
		"put": map[string]any{
			"summary":     "大文件上传（上限 512MiB）",
			"description": "body 是文件本身，Content-Length 必填。写入走临时文件 + rename，不会留半截。`.git` 下一律 400。",
			"security":    []any{map[string]any{"bearer": []any{}}},
			"parameters":  []any{queryParam("path", "相对沙箱根的路径", true)},
			"responses": map[string]any{
				"200": b.jsonResponse("写入成功", proto.WriteResp{}),
				"400": b.jsonResponse("路径非法", proto.Error{}),
				"401": b.jsonResponse("token 无效", proto.Error{}),
				"413": b.jsonResponse("超过 512MiB", proto.Error{}),
			},
		},
	}

	openapi := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "wr —— 远程高效协同工具",
			"version":     Version,
			"description": "单二进制、零依赖的远端工作区 API：跑命令、读写文件、搜代码、管后台任务。\n所有 POST 端点收发的都是 JSON，/raw 收发裸流。鉴权用 `Authorization: Bearer <token>`、`X-WR-Token`，或 URL 上的 `?token=`（header 优先）。",
		},
		"servers": []any{map[string]any{"url": "/", "description": "当前服务"}},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearer": map[string]any{"type": "http", "scheme": "bearer"},
				// 与 bearer 等价：浏览器点开、curl 拼 URL 时不必再带 header。
				"tokenQuery": map[string]any{
					"type": "apiKey", "in": "query", "name": "token",
					"description": "等价于 Authorization: Bearer <token>；header 优先于本参数",
				},
			},
			"schemas": b.schemas(),
		},
		"paths": paths,
	}
	if s.cfg.Git != "" {
		openapi["info"].(map[string]any)["description"] = openapi["info"].(map[string]any)["description"].(string) +
			"\n\n当前沙箱对应 git 仓库位置：" + s.cfg.GitAt
	}
	return openapi
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	noIndex(w)
	writeJSON(w, 200, s.spec())
}

func (b *schemaBuilder) jsonResponse(desc string, v any) map[string]any {
	return map[string]any{
		"description": desc,
		"content":     map[string]any{"application/json": map[string]any{"schema": b.schemaOf(v)}},
	}
}

func queryParam(name, desc string, required bool) map[string]any {
	return map[string]any{"name": name, "in": "query", "required": required, "description": desc, "schema": map[string]any{"type": "string"}}
}

// schemaBuilder 每次生成 spec 时新建一个，不共用全局状态：
// 共用的 defs 会在并发 /openapi.json 请求之间互相污染。
type schemaBuilder struct {
	defs map[string]any
}

// schemaOf 把 Go 结构体反射成 OpenAPI schema。
// 只覆盖 wr 实际用到的类型：string/int/bool/切片/结构体指针；
// 遇到不认识的就退化成 object，不为了完美把反射器写成一门小语言。
func (b *schemaBuilder) schemaOf(v any) map[string]any {
	t := reflect.TypeOf(v)
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == nil {
		return map[string]any{"type": "object"}
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": b.schemaOf(reflect.Zero(t.Elem()).Interface())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": true}
	}
	if t.Kind() != reflect.Struct {
		return map[string]any{"type": "object"}
	}

	name := t.Name()
	if name == "" {
		// 匿名 struct（例如 /job/kill 的入参）
		return b.structSchema(t)
	}
	if b.defs[name] != nil {
		return map[string]any{"$ref": "#/components/schemas/" + name}
	}
	b.defs[name] = map[string]any{} // 占位，防自引用死循环
	b.defs[name] = b.structSchema(t)
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

func (b *schemaBuilder) structSchema(t reflect.Type) map[string]any {
	props := map[string]any{}
	var required []string
	b.fields(t, func(name string, f reflect.StructField) {
		props[name] = b.schemaOf(reflect.Zero(f.Type).Interface())
		if proto.IsRequired(f) {
			required = append(required, name)
		}
	})
	return objectSchema(props, required)
}

// requestSchema 是 structSchema 的严格版：必填字段直接 $ref 到共享定义，
// 不再内联一份 properties——两份 schema 迟早会飘，而「必填」这一条尤其不能飘。
// 区别只在零值：响应里 0 和空数组都是合法结果，入参里 "" 等于没填。
func (b *schemaBuilder) requestSchema(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return b.schemaOf(reflect.Zero(t).Interface())
	}
	name := t.Name()
	if name == "" {
		return b.structSchema(t) // 匿名 struct（/job/kill 的入参）没有定义名可引用
	}
	if b.defs[name] == nil {
		b.defs[name] = map[string]any{} // 占位，防自引用死循环
		b.defs[name] = b.structSchema(t)
	}
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

// fields 遍历一个请求/响应结构体里的 json 字段，跳过非导出和 `json:"-"`。
func (b *schemaBuilder) fields(t reflect.Type, fn func(name string, f reflect.StructField)) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" || tag == "" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		fn(name, f)
	}
}

func objectSchema(props map[string]any, required []string) map[string]any {
	out := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		sort.Strings(required)
		out["required"] = required
	}
	return out
}

// schemas 触发一遍所有协议类型的注册，再返回快照。
// 顺序无关（map 本来无序），但要在生成 spec 之后取，才能拿到完整定义。
func (b *schemaBuilder) schemas() map[string]any {
	probe := []any{
		proto.ExecRequest{}, proto.ExecResponse{}, proto.ReadReq{}, proto.ReadResp{},
		proto.WriteReq{}, proto.WriteResp{}, proto.Edit{}, proto.EditReq{}, proto.EditResp{},
		proto.GlobReq{}, proto.GlobResp{}, proto.GrepReq{}, proto.GrepMatch{}, proto.GrepResp{},
		proto.ListReq{}, proto.Entry{}, proto.ListResp{},
		proto.DeleteReq{}, proto.DeleteResp{}, proto.MoveReq{}, proto.MoveResp{},
		proto.MkdirReq{}, proto.MkdirResp{},
		proto.SyncItem{}, proto.SyncPlanReq{}, proto.SyncPlanResp{},
		proto.SyncApplyReq{}, proto.SyncApplyResp{},
		proto.JobStartReq{}, proto.JobLogsReq{}, proto.JobLogsResp{}, proto.JobInfo{}, proto.JobListResp{},
		proto.LogReq{}, proto.LogResp{}, proto.LogClearResp{},
		proto.InfoResp{}, proto.ShellInfo{}, proto.Error{},
	}
	for _, p := range probe {
		b.schemaOf(p)
	}
	out := make(map[string]any, len(b.defs))
	for k, v := range b.defs {
		out[k] = v
	}
	return out
}

const docIndexHTML = `<!doctype html>
<html lang="zh">
<meta charset="utf-8">
<title>wr API</title>
<style>
  body{font:14px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif;margin:0;padding:24px;max-width:960px;color:#1f2328}
  h1{font-size:20px;margin:0 0 4px}
  .meta{color:#656d76;margin-bottom:20px}
  code,pre{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12.5px}
  .ep{border:1px solid #d0d7de;border-radius:6px;margin:10px 0;overflow:hidden}
  .hdr{display:flex;gap:10px;align-items:baseline;padding:8px 12px;background:#f6f8fa;cursor:pointer}
  .m{font-weight:600;text-transform:uppercase;font-size:11px;color:#0969da;min-width:38px}
  .p{font-family:ui-monospace,monospace;font-weight:600}
  .s{color:#656d76;margin-left:auto;text-align:right}
  .body{display:none;padding:10px 12px;border-top:1px solid #d0d7de}
  .open .body{display:block}
  .desc{color:#57606a;white-space:pre-wrap;margin-bottom:8px}
  a{color:#0969da}
</style>
<h1>wr API</h1>
<div class="meta">机器可读（指格式，不是免鉴权）：<a href="/api/openapi.json">/api/openapi.json</a> · 等价路径 <code>/api/doc</code> · 鉴权 <code>Authorization: Bearer &lt;token&gt;</code> 或 <code>?token=</code>（本页和接口清单一样要 token，直接点开即可）</div>
<div id="list">加载中…</div>
<script>
// 从地址栏 ?token= 取一次，之后每个接口都能点开试。
const TOKEN = new URLSearchParams(location.search).get('token')
// 文档本身要 token（和 /info 同级的元信息），带上地址栏拿到的那个。
const DOC_URL = '/api/openapi.json' + (TOKEN ? '?token=' + encodeURIComponent(TOKEN) : '')
fetch(DOC_URL).then(r => r.json()).then(doc => {
  const box = document.getElementById('list')
  box.innerHTML = ''
  const paths = Object.keys(doc.paths).sort()
  for (const p of paths) {
    for (const m of Object.keys(doc.paths[p])) {
      const op = doc.paths[p][m]
      const el = document.createElement('div')
      el.className = 'ep'
      // 只有 GET + query 传参的接口能直接点开；POST 得自己拼 body。
      const href = TOKEN && m === 'get' ? p + '?token=' + encodeURIComponent(TOKEN) : ''
      const title = href
        ? '<a href="' + href + '" target="_blank">' + p + '</a>'
        : p
      el.innerHTML = '<div class="hdr"><span class="m">' + m + '</span><span class="p">' + title +
        '</span><span class="s">' + (op.summary || '') + '</span></div>' +
        '<div class="body"><div class="desc">' + (op.description || '') + '</div>' +
        '<div>请求体：<code>' + JSON.stringify(op.requestBody ? op.requestBody.content['application/json'].schema : null) + '</code></div></div>'
      el.querySelector('.hdr').onclick = () => el.classList.toggle('open')
      box.appendChild(el)
    }
  }
}).catch(e => { document.getElementById('list').textContent = '读取 /api/openapi.json 失败：' + e })
</script>
</html>
`
