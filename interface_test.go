package dtmx

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dtmx 是**独立组件仓**，不是 launchx 的子包。它被 launchx 以
// `replace => ../dtmx` 消费 —— 一旦反向 import launchx 就成环，
// MVS 直接解析失败；更糟的是能解析但把示例/装配面拖进组件。
//
// 为什么用 AST 而不是 grep：本判据要求「零命中」，而本仓的注释与 README 里会
// 大量出现 "launchx" 字样（说明为什么不能依赖它），grep 会把它们算成命中 ——
// 恒红或恒绿的判据都会被整片忽略。
func TestDtmxDoesNotDependOnLaunchx(t *testing.T) {
	forbidden := []string{
		"github.com/gospacex/launchx", // 反向依赖会与 replace 成环
		"github.com/gospacex/configx", // 纯库不读配置中心
		"github.com/gospacex/dbx",     // 句柄由调用方注入
		"github.com/cloudwego/",       // RPC/HTTP 框架属调用方
		"gorm.io/gorm",                // ORM 属调用方
	}
	var bad []string
	scanned := 0
	impl := 0

	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// 只扫本仓源码；跳过隐藏目录（.git 等）与子模块目录
			if path != "." && strings.HasPrefix(filepath.Base(path), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		scanned++
		if !strings.HasSuffix(path, "_test.go") {
			impl++
		}

		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			// 解析失败不该静默放过：那意味着这条守卫根本没在看你以为它在看的文件
			t.Errorf("解析 %s 失败：%v", path, perr)
			return nil
		}
		for _, imp := range f.Imports {
			lit := strings.Trim(imp.Path.Value, `"`)
			for _, prefix := range forbidden {
				if strings.HasPrefix(lit, prefix) {
					bad = append(bad, path+"  import "+lit)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk 失败：%v", err)
	}
	if len(bad) > 0 {
		t.Fatalf("dtmx 出现 %d 处禁用依赖，组件仓必须保持独立（扫过 %d 个文件）：\n  %s",
			len(bad), scanned, strings.Join(bad, "\n  "))
	}
	// 防空洞绿：本仓只有测试文件、一个实现文件都没有时，上面的 import 判据
	// 无从违反，会「因为没东西可看」而通过。故显式要求实现文件下限
	// （三件套：result.go / barrier.go / payload.go）。
	if impl < 3 {
		t.Fatalf("只扫到 %d 个实现 .go 文件（应 ≥3：result/barrier/payload）—— 组件仓是空的，上面的 import 判据无从违反，属空洞绿", impl)
	}
	t.Logf("dtmx 依赖面干净：扫过 %d 个 .go 文件（实现 %d），无禁用依赖", scanned, impl)
}
