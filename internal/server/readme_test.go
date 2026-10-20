package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// README 的「目录」表要求每次 PR 更新，但没人会真的记得。
// 这里直接从磁盘数行数，和表里的数字对账：改了代码没更新表，测试就挂。
// 表是承诺，不是装饰。
var readmeRowRe = regexp.MustCompile("(?m)^\\| `([^`]+)/` .*?\\| (\\d+) \\| (\\d+) \\|$")

// 合计行单独认：它不带目录名，按分行相加得到，最容易被忘（改了三行，合计还是老的）。
var readmeTotalRe = regexp.MustCompile("(?m)^\\| \\*\\*合计\\*\\* \\|.*?\\| \\*\\*(\\d+)\\*\\* \\| \\*\\*(\\d+)\\*\\* \\|$")

func countLines(t *testing.T, dir string, isTest bool) int {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读不到 %s: %v", dir, err)
	}
	total := 0
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") != isTest {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		// 和 `wc -l` 对齐：换行符个数，末尾没换行的最后一行也算一行。
		total += strings.Count(string(data), "\n")
		if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
			total++
		}
	}
	return total
}

func TestReadmeDirTableInSync(t *testing.T) {
	data, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Skipf("读不到 README.md: %v", err)
	}
	rows := readmeRowRe.FindAllStringSubmatch(string(data), -1)
	if len(rows) == 0 {
		t.Fatal("README 里没找到目录行数表")
	}
	root, _ := filepath.Abs("../..")
	sumFunc, sumTest := 0, 0
	for _, m := range rows {
		dir, wantFunc, wantTest := m[1], m[2], m[3]
		gotFunc := countLines(t, filepath.Join(root, dir), false)
		gotTest := countLines(t, filepath.Join(root, dir), true)
		sumFunc += gotFunc
		sumTest += gotTest
		if strconv.Itoa(gotFunc) != wantFunc {
			t.Errorf("%s 功能行数：README 写 %s，实际 %d", dir, wantFunc, gotFunc)
		}
		if strconv.Itoa(gotTest) != wantTest {
			t.Errorf("%s 测试行数：README 写 %s，实际 %d", dir, wantTest, gotTest)
		}
	}
	// 合计是分行的和：分行都对、合计写错，一样算表过期。
	tm := readmeTotalRe.FindStringSubmatch(string(data))
	if tm == nil {
		t.Fatal("README 的目录表缺合计行")
	}
	if strconv.Itoa(sumFunc) != tm[1] {
		t.Errorf("合计功能行数：README 写 %s，实际 %d", tm[1], sumFunc)
	}
	if strconv.Itoa(sumTest) != tm[2] {
		t.Errorf("合计测试行数：README 写 %s，实际 %d", tm[2], sumTest)
	}
}
