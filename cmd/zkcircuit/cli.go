package main

import (
	"flag"
	"fmt"
	"io"
)

func usageLine() {
	fmt.Println("usage: zkcircuit [demo|version|help]")
}

// cliFlags holds every flag the data commands accept.
type cliFlags struct {
	dir         string
	name        string
	version     int
	constraints int
	publicIn    int
	privateIn   int
	description string
	id          string
	attempt     int
	kind        string
}

// newFlagSet builds the shared flag set. Defaults are chosen so the bare
// required flags (--dir/--name/--version or --dir/--id) already produce a
// valid request.
func newFlagSet(cmd string, out io.Writer) (*flag.FlagSet, *cliFlags) {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {}
	var f cliFlags
	fs.StringVar(&f.dir, "dir", "", "数据目录")
	fs.StringVar(&f.name, "name", "", "电路名称")
	fs.IntVar(&f.version, "version", 0, "电路版本号（正整数）")
	fs.IntVar(&f.constraints, "constraints", 8192, "约束数量（必须大于 0）")
	fs.IntVar(&f.publicIn, "public-inputs", 0, "公开输入数量（不得为负）")
	fs.IntVar(&f.privateIn, "private-inputs", 0, "私有输入数量（不得为负）")
	fs.StringVar(&f.description, "description", "", "版本描述")
	fs.StringVar(&f.id, "id", "", "作业编号")
	fs.IntVar(&f.attempt, "attempt", 1, "尝试次数（正整数）")
	fs.StringVar(&f.kind, "kind", "prove", "作业类型（当前只接受 prove）")
	return fs, &f
}
