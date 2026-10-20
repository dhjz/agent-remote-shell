package client

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cnb.cool/yyx946/tteist/internal/proto"
)

// 回归：#32。曾经 Put 对 ≤8MiB 的文件走 /write 的 JSON content，
// 而 string 往返会把手写的非法 UTF-8 字节换成 U+FFFD，二进制静默损坏。
// 这里钉死两件事：二进制必须逐字节一致，且根本不该碰 /write。
func TestPutBinaryKeepsBytes(t *testing.T) {
	raw := make([]byte, 0, 4096)
	for i := 0; i < 2048; i++ {
		raw = append(raw, byte(0x80+i%128)) // 全是非法 UTF-8 起始字节
	}

	var sawWrite bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/write":
			sawWrite = true
			http.Error(w, `{"error":"不该走这里"}`, 400)
		case r.URL.Path == "/raw" && r.Method == "PUT":
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(r.Body)
			got := buf.Bytes()
			if !bytes.Equal(got, raw) {
				t.Errorf("裸流内容不一致: %d bytes vs %d", len(got), len(raw))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"path": r.URL.Query().Get("path"), "bytes": len(got)})
		default:
			http.Error(w, "unexpected", 404)
		}
	}))
	defer srv.Close()

	local := filepath.Join(t.TempDir(), "r.bin")
	if err := os.WriteFile(local, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	c := New(srv.URL, "t", 0)
	resp, err := c.Put("r.bin", local, true)
	if err != nil {
		t.Fatal(err)
	}
	if sawWrite {
		t.Fatal("Put 不该再走 /write 的 JSON 通道")
	}
	if resp.Bytes != len(raw) {
		t.Fatalf("bytes=%d want %d", resp.Bytes, len(raw))
	}
}

// overwrite=false 时远端已有同名文件必须被拦住——改走裸流后这条语义得自己补。
func TestPutNoOverwrite(t *testing.T) {
	var put bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			put = true
			_ = json.NewEncoder(w).Encode(map[string]any{"path": "a.txt"})
			return
		}
		// /read 返回内容 = 远端已存在
		_ = json.NewEncoder(w).Encode(map[string]any{"content": "     1\tx\n"})
	}))
	defer srv.Close()

	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(srv.URL, "t", 0)
	if _, err := c.Put("a.txt", local, false); err == nil {
		t.Fatal("远端已存在且 overwrite=false，应当报错")
	}
	if put {
		t.Fatal("被拒绝时不该发 PUT")
	}
}

// 504 是网关回的，不是 wr 的命令超时；报错得把这两件事分开，
// 否则长命令撞一次 504 就会被读成「命令太长，调大 timeout_ms 就好」。
func TestGatewayTimeoutNotReportedAsCmdTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = w.Write([]byte("<html>504 Gateway Time-out</html>"))
	}))
	defer srv.Close()

	c := &Client{URL: srv.URL, Token: "0123456789abcdef0123", HTTP: srv.Client()}
	_, err := c.Exec(proto.ExecRequest{Cmd: "git commit --allow-empty -m x"})
	if err == nil {
		t.Fatal("504 该报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "504") || !strings.Contains(msg, "网关") {
		t.Errorf("504 该说清是网关回的：%s", msg)
	}
	if !strings.Contains(msg, "124") {
		t.Errorf("该指认「命令超时是 exit_code:124」：%s", msg)
	}
	// 400 之类 wr 自己的错不该被这段话污染
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"命令被策略拒绝：没写清删什么（\"rm\"）"}`))
	}))
	defer srv2.Close()
	c2 := &Client{URL: srv2.URL, Token: "0123456789abcdef0123", HTTP: srv2.Client()}
	if _, err := c2.Exec(proto.ExecRequest{Cmd: "rm -rf *"}); err == nil ||
		!strings.Contains(err.Error(), "没写清删什么") || strings.Contains(err.Error(), "网关") {
		t.Errorf("400 该原样透出 wr 的理由：%v", err)
	}
}
