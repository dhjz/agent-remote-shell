//go:build !windows

package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"cnb.cool/yyx946/tteist/internal/proto"
)

func putFile(t *testing.T, abs, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func item(p, content string) proto.SyncItem {
	return proto.SyncItem{Path: p, Size: int64(len(content)), ModTime: time.Now().Unix()}
}

// 远端没有的一律要传；远端一样的跳过；大小不同要传。
func TestSyncPlanBasics(t *testing.T) {
	s := newTestServer(t)
	putFile(t, filepath.Join(s.root.Path(), "dist/a.js"), "aaaa")
	putFile(t, filepath.Join(s.root.Path(), "dist/b.js"), "bb")

	resp, err := s.SyncPlan(proto.SyncPlanReq{
		Dir: "dist",
		Want: []proto.SyncItem{
			item("a.js", "aaaa"), // 大小一样、mtime 刚写 → 跳过
			item("b.js", "bbbb"), // 大小不同 → 传
			item("c.js", "ccc"),  // 远端没有 → 传
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Same) != 1 || resp.Same[0].Path != "a.js" {
		t.Fatalf("same = %+v", resp.Same)
	}
	if len(resp.Upload) != 2 {
		t.Fatalf("upload = %+v，应该是 b.js 和 c.js", resp.Upload)
	}
	// b.js 传给 4 字节，c.js 3 字节
	if resp.UploadBytes != 7 {
		t.Errorf("upload_bytes = %d，期望 7", resp.UploadBytes)
	}
}

// mtime 容差：FAT 只到 2 秒，差 1 秒不该重传；差得多才传。
func TestSyncPlanMtimeTolerance(t *testing.T) {
	s := newTestServer(t)
	abs := filepath.Join(s.root.Path(), "d/x.bin")
	putFile(t, abs, "1234")
	now := time.Now()
	if err := os.Chtimes(abs, now, now); err != nil {
		t.Fatal(err)
	}

	older := proto.SyncItem{Path: "x.bin", Size: 4, ModTime: now.Add(-time.Second).Unix()}
	resp, err := s.SyncPlan(proto.SyncPlanReq{Dir: "d", Want: []proto.SyncItem{older}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Upload) != 0 {
		t.Errorf("1 秒差在容差内，不该传：%+v", resp.Upload)
	}

	newer := proto.SyncItem{Path: "x.bin", Size: 4, ModTime: now.Add(60 * time.Second).Unix()}
	resp, err = s.SyncPlan(proto.SyncPlanReq{Dir: "d", Want: []proto.SyncItem{newer}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Upload) != 1 {
		t.Errorf("mtime 差 60 秒应重传：%+v", resp.Upload)
	}
}

// checksum 模式：大小和 mtime 都一样，但内容不同 —— 这是最容易被漏掉的那种。
func TestSyncPlanChecksum(t *testing.T) {
	s := newTestServer(t)
	abs := filepath.Join(s.root.Path(), "d/x.txt")
	putFile(t, abs, "abcd")
	now := time.Now()
	_ = os.Chtimes(abs, now, now)

	// 本地内容变了但大小、mtime 碰巧一样（改一个字符）
	localSHA, err := fileSHA(abs)
	if err != nil {
		t.Fatal(err)
	}
	wrong := proto.SyncItem{Path: "x.txt", Size: 4, ModTime: now.Unix(), SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}
	resp, err := s.SyncPlan(proto.SyncPlanReq{Dir: "d", Want: []proto.SyncItem{wrong}, Checksum: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Upload) != 1 {
		t.Fatalf("sha 不同必须传：%+v", resp)
	}

	ok := proto.SyncItem{Path: "x.txt", Size: 4, ModTime: now.Unix(), SHA256: localSHA}
	resp, err = s.SyncPlan(proto.SyncPlanReq{Dir: "d", Want: []proto.SyncItem{ok}, Checksum: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Upload) != 0 || len(resp.Same) != 1 {
		t.Fatalf("sha 相同应跳过：%+v", resp)
	}
}

// prune 只删「远端有、本地没有」的；不开 prune 就一个都不删。
func TestSyncPlanPrune(t *testing.T) {
	s := newTestServer(t)
	putFile(t, filepath.Join(s.root.Path(), "d/keep.txt"), "k")
	putFile(t, filepath.Join(s.root.Path(), "d/stale.txt"), "s")
	putFile(t, filepath.Join(s.root.Path(), "d/sub/old.txt"), "o")

	resp, err := s.SyncPlan(proto.SyncPlanReq{Dir: "d", Want: []proto.SyncItem{item("keep.txt", "k")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Delete) != 0 {
		t.Fatalf("没开 prune 不应产生删除：%v", resp.Delete)
	}

	resp, err = s.SyncPlan(proto.SyncPlanReq{Dir: "d", Want: []proto.SyncItem{item("keep.txt", "k")}, Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Delete) != 2 {
		t.Fatalf("应该删 2 个：%v", resp.Delete)
	}
	if resp.Delete[0] != "stale.txt" || resp.Delete[1] != "sub/old.txt" {
		t.Errorf("删除清单应有序：%v", resp.Delete)
	}
}

// 同步根目录不存在是正常情况（首次部署），不能报错。
func TestSyncPlanCreatesMissingDir(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.SyncPlan(proto.SyncPlanReq{Dir: "new/deep", Want: []proto.SyncItem{item("a.txt", "a")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Upload) != 1 {
		t.Fatalf("首次同步应全量上传：%+v", resp.Upload)
	}
	if fi, err := os.Stat(filepath.Join(s.root.Path(), "new/deep")); err != nil || !fi.IsDir() {
		t.Fatal("目标目录应被建出来")
	}
}

// .git 不参与同步，路径外逃也拦。
func TestSyncPlanRejectsDangerousPaths(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.SyncPlan(proto.SyncPlanReq{Dir: "d", Want: []proto.SyncItem{{Path: ".git/config", Size: 1}}}); err == nil {
		t.Fatal(".git 下的路径必须拒绝")
	}
	if _, err := s.SyncPlan(proto.SyncPlanReq{Dir: "d", Want: []proto.SyncItem{{Path: "../x", Size: 1}}}); err == nil {
		t.Fatal(".. 必须拒绝")
	}
}

// apply：建目录、删文件、并顺手收敛因此变空的父目录。
func TestSyncApplyMkdirAndDelete(t *testing.T) {
	s := newTestServer(t)
	putFile(t, filepath.Join(s.root.Path(), "d/sub/old.txt"), "o")
	putFile(t, filepath.Join(s.root.Path(), "d/keep.txt"), "k")

	resp, err := s.SyncApply(proto.SyncApplyReq{
		Dir: "d", Mkdirs: []string{"a/b"}, Delete: []string{"sub/old.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Created != 1 || resp.Deleted != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	if fi, err := os.Stat(filepath.Join(s.root.Path(), "d/a/b")); err != nil || !fi.IsDir() {
		t.Fatal("a/b 应被建出来")
	}
	if _, err := os.Stat(filepath.Join(s.root.Path(), "d/sub")); !os.IsNotExist(err) {
		t.Error("删空后的 sub 目录应被收敛掉")
	}
	if _, err := os.Stat(filepath.Join(s.root.Path(), "d/keep.txt")); err != nil {
		t.Error("keep.txt 不该被删")
	}
}

// apply 不允许删同步根目录本身——这是最容易写成 rm -rf 的一行。
func TestSyncApplyRefusesRootDelete(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.SyncApply(proto.SyncApplyReq{Dir: "d", Delete: []string{"."}}); err == nil {
		t.Fatal("删同步根必须拒绝")
	}
	if _, err := s.SyncApply(proto.SyncApplyReq{Dir: "d", Delete: []string{"../x"}}); err == nil {
		t.Fatal(".. 必须拒绝")
	}
}

// 端到端的语义：plan → apply → /raw PUT 之后，再 plan 一次应该全 skip。
func TestSyncRoundTrip(t *testing.T) {
	s := newTestServer(t)
	want := []proto.SyncItem{item("a/b/c.txt", "hello"), item("d.txt", "world")}

	plan, err := s.SyncPlan(proto.SyncPlanReq{Dir: "out", Want: want})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Upload) != 2 {
		t.Fatalf("首次应全传：%+v", plan.Upload)
	}
	if _, err := s.SyncApply(proto.SyncApplyReq{Dir: "out", Mkdirs: neededPaths(plan.Upload)}); err != nil {
		t.Fatal(err)
	}
	for _, it := range plan.Upload {
		abs := filepath.Join(s.root.Path(), "out", filepath.FromSlash(it.Path))
		content := map[string]string{"a/b/c.txt": "hello", "d.txt": "world"}[it.Path]
		putFile(t, abs, content)
		// 服务端写完后 mtime 是「现在」，和清单里的 now 在容差内
	}

	// 清单带上服务端实际的 mtime/size，模拟第二次 sync
	var again []proto.SyncItem
	for _, it := range want {
		fi, err := os.Stat(filepath.Join(s.root.Path(), "out", filepath.FromSlash(it.Path)))
		if err != nil {
			t.Fatal(err)
		}
		again = append(again, proto.SyncItem{Path: it.Path, Size: fi.Size(), ModTime: fi.ModTime().Unix()})
	}
	plan2, err := s.SyncPlan(proto.SyncPlanReq{Dir: "out", Want: again, Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan2.Upload) != 0 || len(plan2.Delete) != 0 {
		t.Fatalf("第二轮应无事可做：%+v", plan2)
	}
}

// neededPaths 是测试里对 neededDirs 的镜像：只算文件所在目录及其祖先。
func neededPaths(items []proto.SyncItem) []string {
	set := map[string]bool{}
	for _, it := range items {
		d := filepath.ToSlash(filepath.Dir(it.Path))
		for d != "." && d != "/" && d != "" {
			set[d] = true
			d = filepath.ToSlash(filepath.Dir(d))
		}
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	return out
}
