package main

import (
	"fmt"
	"os"

	"goagent"
	"goagent/tools"
)

// 基础示例：多轮思考 + 工具执行
func main() {
	question := "请查看当前目录下的文件列表，然后读取其中任意一个文件的内容并告诉我"
	if len(os.Args) > 1 && os.Args[1] != "" {
		question = os.Args[1]
	}
	fmt.Println("问题:", question)

	answer, err := goagent.Ask(question).
		Config(goagent.WithMaxIterations(30)).
		Tools(tools.Bash{}).
		Chat()
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
	fmt.Println("回答:", answer)
}
