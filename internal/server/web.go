package server

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"cnb.cool/yyx946/tteist/internal/proto"
	"cnb.cool/yyx946/tteist/internal/webapp"
)

// 页面挂在 /log，数据挂在 /log/stream。
//
// 为什么不是根路径：根路径一直是 404，那是给扫描器的答案。"/log" 是人知道
// 自己要什么才会敲的地址，启动横幅里也直接给全了。
const (
	logPagePath   = "/log"
	logStreamPath = "/log/stream"
	logPageFile   = "log.html"
)

// 实时窗口的几个边界：默认回 500 行，最多 5000 行，两次检查间隔 500ms。
// 500ms 是「人眼觉得实时」和「别把磁盘读穿」之间的那个点；500 行够回到
// 几分钟前，又不至于让浏览器一上来就渲染几千个节点（页面上限也是 500）。
const (
	streamTailDefault = 500
	streamTailMax     = 5000
	streamPollEvery   = 500 * time.Millisecond
	// 每轮最多推这么多。取 1MiB 是因为一次 tail 的上限就是 1MiB：历史那段
	// 永远能在一轮里追平，不会出现「历史还没送完就开始跟增量」的错位。
	streamChunkLimit = maxLogChunk
)

// handleLogPage 送出内置的 log.html。文件走 embed：单 exe 发行，
// 没有「页面丢了」这种失败模式。
func (s *Server) handleLogPage(w http.ResponseWriter, r *http.Request) {
	page, err := webapp.FS.Open(logPageFile)
	if err != nil {
		http.Error(w, "页面未打包进本二进制", http.StatusInternalServerError)
		return
	}
	defer page.Close()
	data, err := io.ReadAll(page)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	noIndex(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// handleLogStream 是给 log.html 用的实时流。
//
// 为什么不是 SSE：EventSource 只认 text/event-stream，且不能自定义 header，
// token 只能塞在 URL 上。这里要的东西比 SSE 少得多——一条不断行的管道，
// 按行 push 就够了。所以回 text/plain 的裸 body：浏览器 fetch 之后
// resp.body.getReader() 边读边切行，鉴权照旧走 Authorization header。
//
// 语义和 /log 一致：先 tail 一段历史，然后一直挂着等新增。客户端断开
// （刷新、关标签、网络抖动）就地退出，不留 goroutine。
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	if s.aud == nil {
		writeErr(w, 400, errNoAuditLog.Error())
		return
	}
	logPath := s.aud.Path()
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	if tail <= 0 {
		tail = streamTailDefault
	}
	if tail > streamTailMax {
		tail = streamTailMax
	}
	// after 只在断线重连时有用：客户端自己记偏移可以直接续上，
	// 页面用 tail 更省事，所以两条路都留着。
	offset, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "这个连接不支持流式输出")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // 前面有 nginx 之类时别攒着
	noIndex(w)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// 先送历史：/log 那套取法现成，保证 wr log 看到的和页面看到的是同一段。
	// 先取历史再记 offset，期间新写的行会在下面的循环里补上，不漏也不重。
	if offset <= 0 {
		resp, err := s.Log(proto.LogReq{Tail: tail})
		if err != nil {
			return
		}
		if resp.Output != "" {
			if _, err := io.WriteString(w, resp.Output); err != nil {
				return
			}
			flusher.Flush()
		}
		offset = resp.Offset
	}

	ticker := time.NewTicker(streamPollEvery)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
		data, next, err := readLogChunk(logPath, offset, streamChunkLimit)
		if err != nil {
			return // 文件被删 / 权限变了：收线，页面自己会重连
		}
		offset = next
		if len(data) == 0 {
			continue
		}
		if _, err := w.Write(data); err != nil {
			return
		}
		flusher.Flush()
	}
}

// readLogChunk 从 offset 往后读一段整行。
//
// 单独拎出来是为了能被测：流式页面的正确性全压在「半行不许推出去」这一条上，
// 半行 JSON 到了浏览器只会被当坏行。返回的 next 只到最后一个换行为止，
// 剩下那截交给下一轮——那时它多半已经写完了。
func readLogChunk(path string, offset, limit int64) (data []byte, next int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	size := fi.Size()
	if size < offset {
		// 文件被清空（wr log -clear）或换了个文件：从头再来。
		// 否则 offset 永远大于 size，页面就再也收不到东西了。
		offset = 0
	}
	if size == offset {
		return nil, offset, nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	buf, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, offset, err
	}
	i := bytes.LastIndexByte(buf, '\n')
	if i < 0 {
		return nil, offset, nil // 一行都没写完，等下一轮
	}
	return buf[:i+1], offset + int64(i+1), nil
}

// bannerLogURL 拼横幅里那行「点开就能实时看」的地址。
func bannerLogURL(host, token string) string {
	return withToken("http", host, logPagePath, token)
}
