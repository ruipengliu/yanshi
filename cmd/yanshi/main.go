// Command yanshi 是单二进制入口：serve 启动全部组件，chat 是命令行客户端。
package main

import (
	"fmt"
	"os"
)

const usage = `usage:
  yanshi serve [flags]   启动单进程服务（API + Worker）
  yanshi chat  [flags]   命令行对话客户端
  yanshi node  [flags]   模拟电脑上的 HostApp，把共享目录作为 Node 接入

运行 yanshi <command> -h 查看参数。`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "chat":
		err = chat(os.Args[2:])
	case "node":
		err = nodeCmd(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "yanshi:", err)
		os.Exit(1)
	}
}
