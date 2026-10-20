// Package webapp 是 wr 自带的两张页面，编译进二进制。
//
//	index.html  离线看日志：拖一个 .wr.log 进去，靠浏览器文件句柄实时跟着读，
//	            不需要服务端，也不外发任何字节。这一张可以单独拷到任何静态服务上。
//	log.html    在线看日志：数据来自 GET /log/stream，wr start 的横幅会把这个地址打出来。
//
// 两张页面共用同一套字段定义（internal/audit 的 JSONL），index.html 是「已经有文件」，
// log.html 是「文件在服务端」。所以放一个目录、一份 embed，别让两边漂开。
package webapp

import (
	"embed"
	"io/fs"
)

//go:embed index.html log.html
var files embed.FS

// FS 是去掉目录前缀的页面文件系统。直接用 embed.FS 取 "index.html" 得写全路径，
// 这里转成 fs.FS 让服务端按 /log 这类路由名去挑文件。
var FS, _ = fs.Sub(files, ".")
