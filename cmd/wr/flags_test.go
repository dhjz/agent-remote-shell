package main

import (
	"flag"
	"testing"
)

// 短名必须真能落到长名那个变量上，否则就是「写了不生效」。
func TestShortAliasSetsLongFlag(t *testing.T) {
	fs := newFlagSet("t", flag.ContinueOnError)
	token := fs.String("token", "", "")
	port := fs.Int("port", 7573, "")
	fs.short("t", "token")
	fs.short("p", "port")

	if err := fs.Parse([]string{"-t", "secret", "-p=8080"}); err != nil {
		t.Fatal(err)
	}
	if *token != "secret" || *port != 8080 {
		t.Fatalf("-t/-p 没生效: token=%q port=%d", *token, *port)
	}
}

// -timeout 和 -t 同前缀，不能被短名改写误伤。
func TestShortAliasLeavesLongNames(t *testing.T) {
	fs := newFlagSet("t", flag.ContinueOnError)
	timeout := fs.Int64("timeout", 120, "")
	fs.short("t", "timeout")

	if err := fs.Parse([]string{"-timeout", "7"}); err != nil {
		t.Fatal(err)
	}
	if *timeout != 7 {
		t.Fatalf("timeout=%d", *timeout)
	}
	if err := fs.Parse([]string{"-t", "9"}); err != nil {
		t.Fatal(err)
	}
	if *timeout != 9 {
		t.Fatalf("-t 没落到 timeout: %d", *timeout)
	}
}

// 位置参数混在标志里时，短名的值不能被当成位置参数——
// `wr start -r /repo -p 80` 里的 /repo 属于 -r。
func TestShortAliasValueNotTakenAsPositional(t *testing.T) {
	fs := newFlagSet("t", flag.ContinueOnError)
	root := fs.String("root", ".", "")
	port := fs.Int("port", 7573, "")
	fs.short("r", "root")
	fs.short("p", "port")

	pos := parse(fs, []string{"-r", "/repo", "leftover", "-p", "80"})
	if len(pos) != 1 || pos[0] != "leftover" {
		t.Fatalf("位置参数被拆错: %v", pos)
	}
	if *root != "/repo" || *port != 80 {
		t.Fatalf("root=%q port=%d", *root, *port)
	}
}

// 同一个短名挂两处，是名单写错了，编译期发现不了，运行到才炸太晚——
// 这里把「会 panic」本身钉住，改短名表时能立刻知道撞了。
func TestShortAliasDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("同一短名挂两个长名应当 panic")
		}
	}()
	fs := newFlagSet("t", flag.ContinueOnError)
	fs.String("content", "", "")
	fs.String("create", "", "")
	fs.shorts("content", "create")
}

// 短名只给 wr start。客户端子命令一律长名——这条不写测试就靠自觉，
// 加一个人的「顺手」就漏回来了，所以钉住常用的几个短名都不认。
func TestClientSubcommandsRejectShortFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"exec", []string{"-t", "60", "echo hi"}},
		{"exec", []string{"-d", "src", "echo hi"}},
		{"read", []string{"-o", "1", "main.go"}},
		{"grep", []string{"-g", "*.go", "func main"}},
		{"ls", []string{"-l"}},
		{"sync", []string{"-c", "./dist"}},
	}
	for _, c := range cases {
		fs := newFlagSet(c.name, flag.ContinueOnError)
		var cmd string
		switch c.name {
		case "exec":
			fs.Int64("timeout", 120, "")
			fs.String("dir", "", "")
			cmd = "exec"
		case "read":
			fs.Int("offset", 1, "")
			cmd = "read"
		case "grep":
			fs.String("glob", "", "")
			cmd = "grep"
		case "ls":
			fs.Bool("long", false, "")
			cmd = "ls"
		case "sync":
			fs.Bool("checksum", false, "")
			cmd = "sync"
		}
		fs.SetOutput(discard{})
		if err := fs.Parse(c.args); err == nil {
			t.Fatalf("%s 不该认短名: %v（这会话里只给 wr start 挂短名）", cmd, c.args)
		}
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
