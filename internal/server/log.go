package server

import (
	"bytes"
	"errors"
	"io"
	"os"

	"cnb.cool/yyx946/tteist/internal/audit"
	"cnb.cool/yyx946/tteist/internal/proto"
)

// maxLogChunk 是单次 /log 的上限。和 /read 的 2MiB 对齐没必要更大：
// 审计日志的价值在「按时间顺序看发生了什么」，一次拉一段就够，
// 全量灌进上下文只会把真正要紧的那几行淹掉。
const maxLogChunk = 1 << 20

var errNoAuditLog = errors.New("未开启审计日志：服务端用 -log <文件> 或 -log 打开，默认落点是启动目录下的 .wr.log")

// Log 读审计日志。三种取法和 /job/logs 一致：从头、只要新增（after）、只要最后 N 行（tail）。
// 日志不在沙箱里，所以这个端点不碰 root——它只认服务端自己开的那个文件。
func (s *Server) Log(req proto.LogReq) (proto.LogResp, error) {
	var resp proto.LogResp
	if s.aud == nil {
		return resp, errNoAuditLog
	}
	resp.Path = s.aud.Path()
	f, err := os.Open(resp.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return resp, nil // 开了还没写第一条：空日志不是错误
		}
		return resp, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return resp, err
	}
	size := fi.Size()
	resp.Size = size

	limit := req.Limit
	if limit <= 0 {
		limit = maxLogChunk
	}
	if limit > maxLogChunk {
		limit = maxLogChunk
	}

	// tail 和 after 的截断方向是反的，别混用一套逻辑：
	//
	//   after 是「往后追」，追不完就退到最后一个换行，下次接着读，不丢不重；
	//   tail 是「只要最近这一段」，装不下就该扔最老的那头——从尾部往前留。
	//
	// 早先两种都按 after 处理，于是一份 1.7MiB 的日志被 tail=500 只要到 296 行，
	// 回的还不是最近 296 行，而是「往前数第 500 行之后」那 296 行；更糟的是
	// offset 停在了这段的末尾，中间那段日志页面上永远看不到，看着就像流卡死了。
	// skipPartial 只在 tail 时需要：tail 的起点是按行数从后往前数出来的，
	// 按字节上限再掐一次就可能掐在行中间。after 的起点是调用方自己记下的
	// 「上一个中断处」，那里一定是行首，硬扔一行反而会漏。
	start, skipPartial := int64(0), false
	switch {
	case req.After > 0:
		if req.After >= size {
			resp.Offset = size
			return resp, nil
		}
		start = req.After
	case req.Tail > 0:
		// tailOffset 数的是「第 n 个换行」，而文件末尾没有换行，所以要 n+1：
		// 倒数第 n 行的行首，正好是倒数第 n+1 个换行的后一个字节。
		start, skipPartial = tailOffset(f, size, req.Tail+1), true
		// 装不下：只留最后 limit 字节，尾巴才是「最近」的那段。
		if size-start > int64(limit) {
			start = size - int64(limit)
			resp.Truncated = true
		}
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return resp, err
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)))
	if err != nil {
		return resp, err
	}
	// 判断起点是不是行首，看的是它前面那个字节：换行（或文件开头）才算。
	// 看 data[0] 是错的——整行数据本来就从 '{' 开头。
	if skipPartial && start > 0 && len(data) > 0 {
		var prev [1]byte
		if _, err := f.ReadAt(prev[:], start-1); err == nil && prev[0] != '\n' {
			if i := bytes.IndexByte(data, '\n'); i >= 0 {
				start += int64(i) + 1
				data = data[i+1:]
			} else {
				start += int64(len(data))
				data = nil
			}
		}
	}
	// 超上限时退到最后一个换行：半行 JSON 送到客户端只会解析失败，
	// 下次用 offset 续读还能接上。
	if int64(len(data)) == int64(limit) && start+int64(len(data)) < size {
		if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
			data = data[:i+1]
			resp.Truncated = true
		}
	}
	resp.Output = string(data)
	resp.Offset = start + int64(len(data))
	return resp, nil
}

// ClearLog 清空日志。动作本身也留一行：清空审计这件事，
// 恰恰是最该在日志里留下痕迹的那种操作。
func (s *Server) ClearLog() (proto.LogClearResp, error) {
	var resp proto.LogClearResp
	if s.aud == nil {
		return resp, errNoAuditLog
	}
	resp.Path = s.aud.Path()
	if fi, err := os.Stat(resp.Path); err == nil {
		resp.Cleared = fi.Size()
	}
	s.audit(audit.Entry{Decision: audit.Allowed, Tool: "log_clear", Path: resp.Path})
	if err := s.aud.Truncate(); err != nil {
		return resp, err
	}
	// 截断会把刚写的那行也抹掉，补回来——不然「谁清的日志」永远查不到。
	s.audit(audit.Entry{Decision: audit.Allowed, Tool: "log_clear", Path: resp.Path, Reason: "日志已被清空"})
	return resp, nil
}
