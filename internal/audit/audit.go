// Package audit 把「谁干了什么」写成 append-only JSONL。
//
// 格式对齐 winreach-mcp 的审计日志：一行一个 JSON 对象，字段名统一小驼峰
// （time / decision / tool / command / cwd / exitCode / durationMs …），
// 好让两边的日志能用同一套 grep 和同一个 SIEM 管道处理。
// wr 自己的东西（path / targets / bytes / remote）另加，不占人家已有字段的语义。
package audit

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 三种结论。failed 不单列：命令非零退出算 error，不是「服务出错」，
// 而是「这次动作没按预期完成」——grep error 就该一把捞出所有异常。
const (
	Allowed = "allowed"
	Blocked = "blocked"
	Error   = "error"
)

// DefaultFile 是不指定落点时用的文件名，落在 wr start 的执行目录下。
const DefaultFile = ".wr.log"

// Entry 是一行审计。omitempty 是为了让日志短：只留这次动作用到的字段。
type Entry struct {
	Time     string   `json:"time"`
	Decision string   `json:"decision"`
	Tool     string   `json:"tool"`
	Command  string   `json:"command,omitempty"`
	Shell    string   `json:"shell,omitempty"`
	Cwd      string   `json:"cwd,omitempty"`
	Path     string   `json:"path,omitempty"`
	Pattern  string   `json:"pattern,omitempty"`
	Targets  []string `json:"targets,omitempty"`
	Session  string   `json:"sessionId,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Error    string   `json:"error,omitempty"` // 命令自己吐的失败原因，见 Excerpt
	ExitCode *int     `json:"exitCode,omitempty"`
	Duration *int64   `json:"durationMs,omitempty"`
	Bytes    *int64   `json:"bytes,omitempty"`
	Count    *int     `json:"count,omitempty"`
	Remote   string   `json:"remote,omitempty"`
}

// Logger 是所有埋点唯一的口子。nil 安全：调用方不必判断有没有开日志。
type Logger struct {
	mu   sync.Mutex
	f    *os.File
	path string

	warned bool // 写失败只嚷一次，别把 stderr 刷满
}

// Resolve 把 -log 的取值翻成绝对路径。相对路径（含裸文件名）按 base 算，
// 也就是 wr start 被执行的目录——说"记到 wr.log"，找它时一定站在那个目录里。
func Resolve(base, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("日志文件名不能为空")
	}
	if filepath.IsAbs(name) {
		return filepath.Clean(name), nil
	}
	abs, err := filepath.Abs(filepath.Join(base, name))
	if err != nil {
		return "", err
	}
	return abs, nil
}

// Open 打开（不存在则创建）审计文件。目录会自动建。
// 永远是 O_APPEND：重启一次就少一段日志的审计等于没有审计。
func Open(path string) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Logger{f: f, path: path}, nil
}

// Path 是日志的绝对路径，横幅和 /info 都要把它打出来——落点不该靠猜。
func (l *Logger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Log 写一行。error 一律不外抛：审计失败不该让用户的操作失败，
// 但也绝不能静默——第一次出问题就在 stderr 上留一句。
func (l *Logger) Log(e Entry) {
	if l == nil {
		return
	}
	if e.Time == "" {
		e.Time = time.Now().Format(time.RFC3339Nano)
	}
	b, err := json.Marshal(e)
	if err != nil {
		l.warn(err)
		return
	}
	b = append(b, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	// 整行一次 Write：并发时不会出现两行各写一半绞在一起。
	if _, err := l.f.Write(b); err != nil {
		l.warn(err)
	}
}

func (l *Logger) warn(err error) {
	if l.warned {
		return
	}
	l.warned = true
	log.Printf("wr: 审计日志写 %s 失败（后续不再提示）：%v", l.path, err)
}

func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// Truncate 清空日志内容。append-only 说的是正常写入路径：
// 明着喊「清空」的时候不照做，只会让人自己 rm 掉整个文件。
func (l *Logger) Truncate() error {
	if l == nil {
		return fmt.Errorf("未开启审计日志")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	_, err := l.f.Seek(0, 0)
	return err
}

// Excerpt 从命令输出里摘出「失败长什么样」的开头一段，给审计的 error 字段用。
//
// 为什么只留开头：一次失败的根因通常在头几行（编译错误、ENOENT、权限拒绝），
// 而完整输出可能几 MB——把内容抄进日志就违背了「审计只记动作、不记内容」。
// 几十个字符足够认出是哪一类错，又不会让日志变成第二份 stderr。
//
// 为什么优先 stderr：stdout 常是正常输出，stderr 才是报错。但「只剩 stdout」
// 也远比空着强（命令把错误打到 stdout 的不少），所以 stderr 空时退回 stdout。
// 没有输出就返回空串，,omitempty 下这条字段直接不出现——空白不等于「没失败」。
const excerptMax = 120

func Excerpt(stderr, stdout string) string {
	text := stderr
	if strings.TrimSpace(text) == "" {
		text = stdout
	}
	line := firstNonBlankLine(text)
	if line == "" {
		return ""
	}
	if r := []rune(line); len(r) > excerptMax {
		return string(r[:excerptMax]) + "…"
	}
	return line
}

// firstNonBlankLine 找第一行有内容的：go / python 的报错前面常挂着空行，
// 摘到空行等于没摘。按行找而不是整段 trim，是为了别把前导缩进吃掉。
func firstNonBlankLine(s string) string {
	// 换行和回车一起当分隔符：Windows 的 CRLF 会带 \r，
	// 进度条那类「用 \r 刷新同一行」的输出更是全靠它切。
	for _, line := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// Int 和 Duration 是取值助手：omitempty 之下，0 和「没这个字段」
// 分不开——退出码 0 恰恰是最有意义的那种，得明确给指针。
// Count 同理：搜了 0 个命中是结论，不是「没搜」。
func Int(v int) *int { return &v }

// Count 是结果规模（命中行数 / 文件数 / 目录条目数）。
// 只读动作也进审计，但绝不留返回内容——「读了 main.go 前 80 行」
// 和「把 main.go 全文抄走了」在日志里是同一行，靠的就是这个数去分辨。
func Count(n int) *int { return &n }

func Duration(ms int64) *int64 { return &ms }

// Bytes 是返回体/产物的字节数。exec 记的是 stdout+stderr，读类动作
// （read/glob/grep/list）记的是整个响应序列化后的大小——一个数就够分辨
// 「看了一眼」和「抄走了一大片」，内容本身永远不进日志。
func Bytes(n int64) *int64 { return &n }
