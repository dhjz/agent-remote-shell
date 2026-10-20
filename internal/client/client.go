// Package client 是 wr 的瘦客户端：一个 net/http 封装，30 行真活。
// 它的价值不在「能调」，而在「在 B 机器上开发时手感像本地工具」。
package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cnb.cool/yyx946/tteist/internal/proto"
)

type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

func New(url, token string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	return &Client{URL: strings.TrimRight(url, "/"), Token: token, HTTP: &http.Client{Timeout: timeout}}
}

func (c *Client) post(path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", c.URL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("连不上 %s: %w", c.URL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var e proto.Error
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("%s", httpFailure(resp.StatusCode, body))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// httpFailure 把「不是 wr 自己回的错」说清楚。
//
// 504 / 502 / 503 是中间的 nginx、LB 回的：命令可能压根没跑到（也可能跑完了
// 只是回程被掐），跟 exit_code 124（wr 自己超时并收了尸）是两件事。
// 早先两者都吐 `HTTP 504: ...`，一回长命令撞上去就容易被读成「命令超时」，
// 于是去调 timeout_ms——调大了照样撞 504，因为坏的是链路。
func httpFailure(code int, body []byte) string {
	msg := fmt.Sprintf("HTTP %d: %s", code, strings.TrimSpace(string(body)))
	switch code {
	case http.StatusGatewayTimeout, http.StatusBadGateway, http.StatusServiceUnavailable:
		return msg + "（这是网关/LB 回的，不是 wr：命令可能没跑到，也可能跑完了回程被掐。" +
			"命令超时 wr 会自己回 exit_code:124。别只顾着调大 timeout_ms，先看链路）"
	}
	return msg
}

func (c *Client) Exec(req proto.ExecRequest) (proto.ExecResponse, error) {
	var out proto.ExecResponse
	// HTTP 超时要比服务端允许的最大值长，否则网络层先断，看不到 exit code
	c.HTTP.Timeout = time.Duration(req.TimeoutMS)*time.Millisecond + 30*time.Second
	if req.TimeoutMS == 0 {
		c.HTTP.Timeout = 15 * time.Minute
	}
	err := c.post("/exec", req, &out)
	return out, err
}

func (c *Client) Read(req proto.ReadReq) (proto.ReadResp, error) {
	var out proto.ReadResp
	err := c.post("/read", req, &out)
	return out, err
}

func (c *Client) Write(req proto.WriteReq) (proto.WriteResp, error) {
	var out proto.WriteResp
	err := c.post("/write", req, &out)
	return out, err
}

func (c *Client) Edit(req proto.EditReq) (proto.EditResp, error) {
	var out proto.EditResp
	err := c.post("/edit", req, &out)
	return out, err
}

func (c *Client) Delete(req proto.DeleteReq) (proto.DeleteResp, error) {
	var out proto.DeleteResp
	err := c.post("/delete", req, &out)
	return out, err
}

func (c *Client) Move(req proto.MoveReq) (proto.MoveResp, error) {
	var out proto.MoveResp
	err := c.post("/move", req, &out)
	return out, err
}

func (c *Client) Mkdir(req proto.MkdirReq) (proto.MkdirResp, error) {
	var out proto.MkdirResp
	err := c.post("/mkdir", req, &out)
	return out, err
}

func (c *Client) JobStart(req proto.JobStartReq) (proto.JobInfo, error) {
	var out proto.JobInfo
	err := c.post("/job/start", req, &out)
	return out, err
}

func (c *Client) JobLogs(req proto.JobLogsReq) (proto.JobLogsResp, error) {
	var out proto.JobLogsResp
	// 长轮询可能等一会儿，别让 HTTP 超时比服务端的 wait 短
	if req.Wait > 0 {
		c.HTTP.Timeout = time.Duration(req.Wait)*time.Millisecond + 30*time.Second
	}
	err := c.post("/job/logs", req, &out)
	return out, err
}

func (c *Client) JobList() (proto.JobListResp, error) {
	var out proto.JobListResp
	err := c.post("/job/list", struct{}{}, &out)
	return out, err
}

// JobKill 停掉一个后台任务。drop 同时从列表移除，dropLog 连日志文件一起删。
func (c *Client) JobKill(id string, drop, dropLog bool) (proto.JobInfo, error) {
	var out proto.JobInfo
	req := map[string]any{"id": id, "drop": drop}
	if dropLog {
		req["drop_log"] = true
	}
	err := c.post("/job/kill", req, &out)
	return out, err
}

func (c *Client) Glob(req proto.GlobReq) (proto.GlobResp, error) {
	var out proto.GlobResp
	err := c.post("/glob", req, &out)
	return out, err
}

func (c *Client) Grep(req proto.GrepReq) (proto.GrepResp, error) {
	var out proto.GrepResp
	err := c.post("/grep", req, &out)
	return out, err
}

func (c *Client) Fwd() (proto.FwdResp, error) {
	var out proto.FwdResp
	req, err := http.NewRequest("GET", c.URL+"/fwd", nil)
	if err != nil {
		return out, err
	}
	return out, c.do(req, &out)
}

// FwdCall 把一次请求打到服务端配好的上游，结果按 JSON 回来。
// 和 GET /fwd/<name>/... 是同一条链的两个出口：那个给能 load URL 的 agent，
// 这个给只有文本通道的。
func (c *Client) FwdCall(req proto.FwdReq) (proto.FwdResult, error) {
	var out proto.FwdResult
	return out, c.post("/fwd/call", req, &out)
}

func (c *Client) Info() (proto.InfoResp, error) {
	var out proto.InfoResp
	req, err := http.NewRequest("GET", c.URL+"/info", nil)
	if err != nil {
		return out, err
	}
	err = c.do(req, &out)
	return out, err
}

// Put 上传本地文件。所有大小一律走 /raw 裸流。
//
// 曾经这里按 8MiB 分流：小文件塞进 /write 的 JSON content。这是错的——
// WriteReq.Content 是 Go string，JSON 解码时非法 UTF-8 字节会被换成
// U+FFFD（ef bf bd），二进制文件（exe/zip/png）就此损坏，且每坏一字节膨胀
// 3 字节。文本也没占到便宜：分片传输成本大致相当，不值得为它开一条坏路径。
func (c *Client) Put(rel, local string, overwrite bool) (proto.WriteResp, error) {
	var out proto.WriteResp
	if fi, err := os.Stat(local); err == nil && fi.IsDir() {
		return out, fmt.Errorf("%s 是目录，批量上传用 wr sync", local)
	}
	// /raw 本身不校验 overwrite；要守住「手滑刷掉别人的文件」，先问一句。
	if !overwrite {
		if _, err := c.Read(proto.ReadReq{Path: rel, Offset: 1, Limit: 1}); err == nil {
			return out, fmt.Errorf("%s 已存在，且 overwrite=false", rel)
		}
	}
	return c.PutRaw(rel, local)
}

// Get 指定 -o 时落盘，否则返回内容。
func (c *Client) Get(rel, local string) (proto.ReadResp, error) {
	var out proto.ReadResp
	if local == "" {
		return c.Read(proto.ReadReq{Path: rel})
	}
	req, err := http.NewRequest("GET", c.URL+"/raw?path="+urlEscape(rel), nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return out, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return out, err
	}
	// 先落临时文件再 rename：下载断在中途不会留半截文件，
	// 否则下次 build 用的就是那个残缺产物，问题极难查。
	tmp, err := os.CreateTemp(filepath.Dir(local), ".wr-*")
	if err != nil {
		return out, err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return out, err
	}
	if err := os.Rename(tmp.Name(), local); err != nil {
		return out, err
	}
	out.Path, out.Content = rel, fmt.Sprintf("%d bytes", n)
	return out, nil
}

func urlEscape(s string) string {
	return url.QueryEscape(filepath.ToSlash(s))
}

// List 列目录。带 size 与 mtime，客户端自己决定怎么排版。
func (c *Client) List(dir string, limit int) (proto.ListResp, error) {
	var out proto.ListResp
	err := c.post("/list", proto.ListReq{Dir: dir, Limit: limit}, &out)
	return out, err
}

// OpenAPI 拉服务端自描述，agent 可以先看一眼有哪些端点再决定调什么。
func (c *Client) Log(req proto.LogReq) (proto.LogResp, error) {
	var out proto.LogResp
	err := c.post("/log", req, &out)
	return out, err
}

func (c *Client) LogClear() (proto.LogClearResp, error) {
	var out proto.LogClearResp
	err := c.post("/log/clear", struct{}{}, &out)
	return out, err
}

func (c *Client) OpenAPI() (map[string]any, error) {
	var out map[string]any
	req, err := http.NewRequest("GET", c.URL+"/api/openapi.json", nil)
	if err != nil {
		return out, err
	}
	err = c.do(req, &out)
	return out, err
}

func (c *Client) SyncPlan(req proto.SyncPlanReq) (proto.SyncPlanResp, error) {
	var out proto.SyncPlanResp
	err := c.post("/sync/plan", req, &out)
	return out, err
}

func (c *Client) SyncApply(req proto.SyncApplyReq) (proto.SyncApplyResp, error) {
	var out proto.SyncApplyResp
	err := c.post("/sync/apply", req, &out)
	return out, err
}

// PutRaw 直接 PUT 裸流到 /raw，不经过 JSON。
// 同步时每个文件都走这条路：批量上传不该为小文件付 base64 的税。
func (c *Client) PutRaw(rel, local string) (proto.WriteResp, error) {
	var out proto.WriteResp
	f, err := os.Open(local)
	if err != nil {
		return out, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return out, err
	}
	req, err := http.NewRequest("PUT", c.URL+"/raw?path="+urlEscape(rel), f)
	if err != nil {
		return out, err
	}
	req.ContentLength = fi.Size()
	c.HTTP.Timeout = 15 * time.Minute
	return out, c.do(req, &out)
}
