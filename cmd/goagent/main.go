// goagent 默认 CLI：流式对话，将思考流与工具执行过程打印到屏幕
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/DotNetAge/goagent"
	"github.com/DotNetAge/goagent/tools"
)

func main() {
	question := strings.Join(os.Args[1:], " ")
	if question == "" {
		// 支持从标准输入传入问题（如 echo "问题" | goagent）
		data, err := io.ReadAll(os.Stdin)
		if err == nil {
			question = strings.TrimSpace(string(data))
		}
	}
	if question == "" {
		fmt.Fprintln(os.Stderr, "用法: goagent <问题>，或通过标准输入传入问题")
		os.Exit(1)
	}

	fmt.Println("问题:", question)
	fmt.Print("思考: ")

	// 思考流增量一收到立即打印（SSE 增量本身是模型生成的自然片段）
	err := goagent.Ask(question).
		Tools(tools.Bash{}).
		ChatStream(goagent.Callbacks{
			ThinkCallback: func(delta string) { fmt.Print(delta) },
			BeforeToolExec: func(tool goagent.ToolCall, args json.RawMessage) {
				fmt.Printf("\n正在执行 %s, %s\n", tool.Name(), formatArgs(args))
			},
			AfterToolExec: func(tool goagent.ToolCall, result string, err error) {
				status := "成功"
				if err != nil {
					status = "失败: " + err.Error()
				}
				fmt.Printf("[工具] 执行结束: %s (%s)\n", tool.Name(), status)
				if r := strings.TrimSpace(result); r != "" {
					r = strings.ReplaceAll(r, "\n", " ")
					if runes := []rune(r); len(runes) > 120 {
						// 按 rune 截断，避免多字节字符被切成乱码
						r = string(runes[:120]) + "..."
					}
					fmt.Printf("      结果: %s\n", r)
				}
			},
			Finished: func(answer string) {
				fmt.Println("\n\n回答:", answer)
			},
		})
	if err != nil {
		fmt.Fprintln(os.Stderr, "\n错误:", err)
		os.Exit(1)
	}
}

// formatArgs 将工具参数 JSON 转为 key=value 列表，用 ", " 连接成一行输出
func formatArgs(args json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(args, &m); err == nil && len(m) > 0 {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%v", k, m[k]))
		}
		return strings.Join(parts, ", ")
	}
	return string(args)
}
