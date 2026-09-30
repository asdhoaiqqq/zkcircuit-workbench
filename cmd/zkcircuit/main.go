// Command zkcircuit is the 零知识电路工程工作台 entry point.
package main

import (
	"fmt"
	"os"

	"github.com/asdhoaiqqq/zkcircuit-workbench/zkcircuit"
)

func main() {
	command := "demo"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	switch command {
	case "demo":
		runDemo()
	case "version":
		fmt.Println("zkcircuit 0.1.0")
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println("usage: zkcircuit [demo|version|help]")
}

func runDemo() {
	circuit := zkcircuit.Circuit{Name: "transfer-range", Version: 4, Constraints: 18432, PublicInputs: 2, PrivateInputs: 5, Frozen: true}
	jobs := []zkcircuit.Job{
		{ID: "job-1", Circuit: "transfer-range", Version: 4, Kind: "prove", Attempt: 1},
		{ID: "job-2", Circuit: "transfer-range", Version: 3, Kind: "prove", Attempt: 1},
	}
	for _, job := range jobs {
		if err := zkcircuit.Validate(circuit, job, true); err != nil {
			fmt.Printf("job=%s refused: %v\n", job.ID, err)
			continue
		}
		fmt.Printf("job=%s accepted against %s v%d\n", job.ID, circuit.Name, circuit.Version)
	}
	cheap := zkcircuit.Circuit{Name: "merkle-inclusion", Version: 1, Constraints: 4096, Frozen: true}
	fmt.Println("witness cost order:", zkcircuit.WitnessCost([]zkcircuit.Circuit{circuit, cheap}))
}
