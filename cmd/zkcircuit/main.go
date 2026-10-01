// Command zkcircuit is the 零知识电路工程工作台 entry point.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

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
	case "circuit":
		runCircuit(os.Args[2:])
	case "setup":
		runSetup(os.Args[2:])
	case "job":
		runJob(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println("usage: zkcircuit [demo|version|help|<command>]")
	fmt.Println()
	fmt.Println("commands:")
	fmt.Println("  demo")
	fmt.Println("  version")
	fmt.Println("  help")
	fmt.Println("  circuit create -dir DIR -name NAME -version N [-desc TEXT] [-constraints N] [-public N] [-private N]")
	fmt.Println("  circuit update -dir DIR -name NAME -version N [-desc TEXT] [-constraints N] [-public N] [-private N]")
	fmt.Println("  circuit freeze -dir DIR -name NAME -version N")
	fmt.Println("  circuit list -dir DIR")
	fmt.Println("  setup register -dir DIR -name NAME -version N")
	fmt.Println("  setup list -dir DIR")
	fmt.Println("  job submit -dir DIR -id ID -name NAME -version N -attempt N [-kind prove]")
	fmt.Println("  job list -dir DIR")
}

// ---------------------------------------------------------------------------
// circuit
// ---------------------------------------------------------------------------

func runCircuit(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: circuit requires a subcommand: create|update|freeze|list")
		os.Exit(2)
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "create":
		circuitCreate(rest)
	case "update":
		circuitUpdate(rest)
	case "freeze":
		circuitFreeze(rest)
	case "list":
		circuitList(rest)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown circuit subcommand %q\n", sub)
		os.Exit(2)
	}
}

func circuitCreate(args []string) {
	fs := newFlagSet("circuit create")
	dir := fs.String("dir", "", "data directory")
	name := fs.String("name", "", "circuit name")
	version := fs.Int("version", 0, "circuit version")
	desc := fs.String("desc", "", "description")
	constraints := fs.Int("constraints", 0, "constraint count")
	public := fs.Int("public", 0, "public inputs")
	private := fs.Int("private", 0, "private inputs")
	parseFlags(fs, args)

	s := openStore(*dir)
	circuit, err := s.CreateCircuit(zkcircuit.Circuit{
		Name:          *name,
		Version:       *version,
		Description:   *desc,
		Constraints:   *constraints,
		PublicInputs:  *public,
		PrivateInputs: *private,
	})
	if err != nil {
		fatal(err)
	}
	fmt.Printf("circuit %s\n", formatCircuit(circuit))
}

func circuitUpdate(args []string) {
	fs := newFlagSet("circuit update")
	dir := fs.String("dir", "", "data directory")
	name := fs.String("name", "", "circuit name")
	version := fs.Int("version", 0, "circuit version")
	desc := fs.String("desc", "", "description")
	constraints := fs.Int("constraints", 0, "constraint count")
	public := fs.Int("public", 0, "public inputs")
	private := fs.Int("private", 0, "private inputs")
	parseFlags(fs, args)

	s := openStore(*dir)
	existing, err := s.GetCircuit(*name, *version)
	if err != nil {
		fatal(err)
	}
	updated := existing
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "desc":
			updated.Description = *desc
		case "constraints":
			updated.Constraints = *constraints
		case "public":
			updated.PublicInputs = *public
		case "private":
			updated.PrivateInputs = *private
		}
	})
	circuit, err := s.UpdateCircuit(updated)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("circuit %s\n", formatCircuit(circuit))
}

func circuitFreeze(args []string) {
	fs := newFlagSet("circuit freeze")
	dir := fs.String("dir", "", "data directory")
	name := fs.String("name", "", "circuit name")
	version := fs.Int("version", 0, "circuit version")
	parseFlags(fs, args)

	s := openStore(*dir)
	circuit, err := s.FreezeCircuit(*name, *version)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("circuit %s\n", formatCircuit(circuit))
}

func circuitList(args []string) {
	fs := newFlagSet("circuit list")
	dir := fs.String("dir", "", "data directory")
	parseFlags(fs, args)

	s := openStore(*dir)
	circuits, err := s.ListCircuits()
	if err != nil {
		fatal(err)
	}
	for _, c := range circuits {
		fmt.Printf("circuit %s\n", formatCircuit(c))
	}
}

func formatCircuit(c zkcircuit.Circuit) string {
	return fmt.Sprintf("name=%q version=%d description=%q constraints=%d public=%d private=%d frozen=%t",
		c.Name, c.Version, c.Description, c.Constraints, c.PublicInputs, c.PrivateInputs, c.Frozen)
}

// ---------------------------------------------------------------------------
// setup
// ---------------------------------------------------------------------------

func runSetup(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: setup requires a subcommand: register|list")
		os.Exit(2)
	}
	switch args[0] {
	case "register":
		setupRegister(args[1:])
	case "list":
		setupList(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "error: unknown setup subcommand %q\n", args[0])
		os.Exit(2)
	}
}

func setupRegister(args []string) {
	fs := newFlagSet("setup register")
	dir := fs.String("dir", "", "data directory")
	name := fs.String("name", "", "circuit name")
	version := fs.Int("version", 0, "circuit version")
	parseFlags(fs, args)

	s := openStore(*dir)
	su, err := s.RegisterTrustedSetup(*name, *version)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("setup circuit=%q version=%d setup_id=%s\n", su.Circuit, su.Version, su.SetupID)
}

func setupList(args []string) {
	fs := newFlagSet("setup list")
	dir := fs.String("dir", "", "data directory")
	parseFlags(fs, args)

	s := openStore(*dir)
	setups, err := s.ListSetups()
	if err != nil {
		fatal(err)
	}
	for _, su := range setups {
		fmt.Printf("setup circuit=%q version=%d setup_id=%s\n", su.Circuit, su.Version, su.SetupID)
	}
}

// ---------------------------------------------------------------------------
// job
// ---------------------------------------------------------------------------

func runJob(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: job requires a subcommand: submit|list")
		os.Exit(2)
	}
	switch args[0] {
	case "submit":
		jobSubmit(args[1:])
	case "list":
		jobList(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "error: unknown job subcommand %q\n", args[0])
		os.Exit(2)
	}
}

func jobSubmit(args []string) {
	fs := newFlagSet("job submit")
	dir := fs.String("dir", "", "data directory")
	id := fs.String("id", "", "job id")
	name := fs.String("name", "", "circuit name")
	version := fs.Int("version", 0, "circuit version")
	attempt := fs.Int("attempt", 0, "attempt number")
	kind := fs.String("kind", "prove", "job kind (only prove is accepted)")
	parseFlags(fs, args)

	s := openStore(*dir)
	job, err := s.SubmitJob(zkcircuit.Job{
		ID:      *id,
		Circuit: *name,
		Version: *version,
		Kind:    *kind,
		Attempt: *attempt,
	})
	if err != nil {
		fatal(err)
	}
	fmt.Printf("job id=%q circuit=%q version=%d kind=%s attempt=%d artifact=%s\n",
		job.ID, job.Circuit, job.Version, job.Kind, job.Attempt, job.Artifact)
}

func jobList(args []string) {
	fs := newFlagSet("job list")
	dir := fs.String("dir", "", "data directory")
	parseFlags(fs, args)

	s := openStore(*dir)
	jobs, err := s.ListJobs()
	if err != nil {
		fatal(err)
	}
	for _, job := range jobs {
		fmt.Printf("job id=%q circuit=%q version=%d kind=%s attempt=%d artifact=%s\n",
			job.ID, job.Circuit, job.Version, job.Kind, job.Attempt, job.Artifact)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: zkcircuit %s [flags]\n", name)
		fs.PrintDefaults()
	}
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) {
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if len(fs.Args()) > 0 {
		fmt.Fprintf(os.Stderr, "error: unexpected argument %q\n", strings.Join(fs.Args(), " "))
		os.Exit(2)
	}
}

func openStore(dir string) *zkcircuit.Store {
	if strings.TrimSpace(dir) == "" {
		fmt.Fprintln(os.Stderr, "error: -dir is required")
		os.Exit(2)
	}
	store, err := zkcircuit.Open(dir)
	if err != nil {
		fatal(err)
	}
	return store
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
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
