// Command zkcircuit is the 零知识电路工程工作台 entry point.
package main

import (
	"fmt"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	command := "demo"
	if len(args) > 0 {
		command = args[0]
	}
	switch command {
	case "demo":
		runDemo()
		return 0
	case "version":
		fmt.Println("zkcircuit 0.1.0")
		return 0
	case "help", "-h", "--help":
		usageLine()
		return 0
	default:
		if code, handled := dispatchDataCommand(command, args[1:]); handled {
			return code
		}
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		usageLine()
		return 2
	}
}
