package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/asdhoaiqqq/zkcircuit-workbench/zkcircuit"
)

// Process exit codes for the data commands.
const (
	exitOK    = 0
	exitRule  = 1 // business rule failure (not found, conflict, frozen, …)
	exitUsage = 2 // bad command line
	exitData  = 3 // storage cannot be read safely (corrupt/truncated/bad format)
)

// dataCommands is the set of commands backed by a persistent data directory.
var dataCommands = map[string]bool{
	"circuit-create":    true,
	"circuit-update":    true,
	"circuit-freeze":    true,
	"circuit-get":       true,
	"circuit-list":      true,
	"setup-record":      true,
	"setup-get":         true,
	"job-submit":        true,
	"job-get":           true,
	"job-list":          true,
	"constraint-import": true,
	"circuit-compile":   true,
	"input-check":       true,
}

// dispatchDataCommand runs cmd with args. handled is false when cmd is not a
// data command, so the caller keeps the historical "unknown command" path.
func dispatchDataCommand(cmd string, args []string) (code int, handled bool) {
	if !dataCommands[cmd] {
		return exitUsage, false
	}
	return runDataCommand(cmd, args), true
}

func runDataCommand(cmd string, args []string) int {
	fs, f := newFlagSet(cmd, os.Stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	// Record which value flags were explicitly provided, so circuit-update
	// can distinguish "omitted" from "given a default value".
	fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "constraints":
			f.constraintsSet = true
		case "public-inputs":
			f.publicInSet = true
		case "private-inputs":
			f.privateInSet = true
		case "description":
			f.descriptionSet = true
		}
	})
	if len(fs.Args()) > 0 {
		fmt.Fprintf(os.Stderr, "error: %s does not accept positional arguments (got %q)\n", cmd, fs.Args()[0])
		return exitUsage
	}
	if f.dir == "" {
		fmt.Fprintf(os.Stderr, "error: --dir is required for %s\n", cmd)
		return exitUsage
	}

	switch cmd {
	case "circuit-create":
		return cmdCircuitCreate(f)
	case "circuit-update":
		return cmdCircuitUpdate(f)
	case "circuit-freeze":
		return cmdCircuitFreeze(f)
	case "circuit-get":
		return cmdCircuitGet(f)
	case "circuit-list":
		return cmdCircuitList(f)
	case "setup-record":
		return cmdSetupRecord(f)
	case "setup-get":
		return cmdSetupGet(f)
	case "job-submit":
		return cmdJobSubmit(f)
	case "job-get":
		return cmdJobGet(f)
	case "job-list":
		return cmdJobList(f)
	case "constraint-import":
		return cmdConstraintImport(f)
	case "circuit-compile":
		return cmdCircuitCompile(f)
	case "input-check":
		return cmdInputCheck(f)
	}
	return exitUsage
}

func requireNameVersion(cmd string, f *cliFlags) (bool, int) {
	if f.name == "" {
		fmt.Fprintf(os.Stderr, "error: --name is required for %s\n", cmd)
		return false, exitUsage
	}
	if f.version <= 0 {
		fmt.Fprintf(os.Stderr, "error: --version must be a positive integer for %s\n", cmd)
		return false, exitUsage
	}
	return true, exitOK
}

func openStore(f *cliFlags) (*zkcircuit.Store, int) {
	store, err := zkcircuit.Open(f.dir)
	if err != nil {
		return nil, reportStoreError(err)
	}
	return store, exitOK
}

// reportStoreError prints a failed storage/domain call and returns the exit
// code. Rule failures are 1; unreadable data is 3.
func reportStoreError(err error) int {
	if errors.Is(err, zkcircuit.ErrDataCorrupt) {
		fmt.Fprintf(os.Stderr, "error: read failed: %v\n", err)
		return exitData
	}
	var domainErr zkcircuit.StoreError
	if errors.As(err, &domainErr) {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return exitRule
	}
	fmt.Fprintf(os.Stderr, "error: storage failure: %v\n", err)
	return exitData
}

func circuitFromFlags(f *cliFlags) zkcircuit.Circuit {
	return zkcircuit.Circuit{
		Name: f.name, Version: f.version,
		Constraints: f.constraints, PublicInputs: f.publicIn, PrivateInputs: f.privateIn,
		Description: f.description,
	}
}

func formatCircuit(c zkcircuit.Circuit) string {
	return fmt.Sprintf("name=%s version=%d constraints=%d public_inputs=%d private_inputs=%d frozen=%t description=%q",
		c.Name, c.Version, c.Constraints, c.PublicInputs, c.PrivateInputs, c.Frozen, c.Description)
}

func formatJob(j zkcircuit.Job) string {
	s := fmt.Sprintf("id=%s circuit=%s version=%d kind=%s attempt=%d", j.ID, j.Circuit, j.Version, j.Kind, j.Attempt)
	if j.Artifact != "" {
		s += " artifact=" + fmt.Sprintf("%q", j.Artifact)
	}
	if j.CompiledHash != "" {
		s += " compiled_hash=" + j.CompiledHash
	}
	return s
}

func cmdCircuitCreate(f *cliFlags) int {
	if ok, code := requireNameVersion("circuit-create", f); !ok {
		return code
	}
	store, code := openStore(f)
	if store == nil {
		return code
	}
	defer store.Close()

	c, err := store.CreateCircuit(circuitFromFlags(f))
	if err != nil {
		return reportStoreError(err)
	}
	fmt.Println("circuit version:", formatCircuit(c))
	return exitOK
}

func cmdCircuitUpdate(f *cliFlags) int {
	if ok, code := requireNameVersion("circuit-update", f); !ok {
		return code
	}
	store, code := openStore(f)
	if store == nil {
		return code
	}
	defer store.Close()

	// Only explicitly provided fields are changed; omitted fields keep their
	// stored values. A non-nil pointer — even to zero or the empty string —
	// replaces the field.
	patch := zkcircuit.PartialCircuit{}
	if f.constraintsSet {
		patch.Constraints = &f.constraints
	}
	if f.publicInSet {
		patch.PublicInputs = &f.publicIn
	}
	if f.privateInSet {
		patch.PrivateInputs = &f.privateIn
	}
	if f.descriptionSet {
		patch.Description = &f.description
	}

	c, err := store.UpdateCircuitPartial(f.name, f.version, patch)
	if err != nil {
		return reportStoreError(err)
	}
	fmt.Println("circuit updated:", formatCircuit(c))
	return exitOK
}

func cmdCircuitFreeze(f *cliFlags) int {
	if ok, code := requireNameVersion("circuit-freeze", f); !ok {
		return code
	}
	store, code := openStore(f)
	if store == nil {
		return code
	}
	defer store.Close()

	c, err := store.FreezeCircuit(f.name, f.version)
	if err != nil {
		return reportStoreError(err)
	}
	fmt.Println("circuit frozen:", formatCircuit(c))
	return exitOK
}

func cmdCircuitGet(f *cliFlags) int {
	if ok, code := requireNameVersion("circuit-get", f); !ok {
		return code
	}
	store, code := openStore(f)
	if store == nil {
		return code
	}
	defer store.Close()

	c, err := store.GetCircuit(f.name, f.version)
	if err != nil {
		return reportStoreError(err)
	}
	fmt.Println("circuit:", formatCircuit(c))
	return exitOK
}

func cmdCircuitList(f *cliFlags) int {
	store, code := openStore(f)
	if store == nil {
		return code
	}
	defer store.Close()

	circuits, err := store.ListCircuits()
	if err != nil {
		return reportStoreError(err)
	}
	for _, c := range circuits {
		fmt.Println("circuit:", formatCircuit(c))
	}
	return exitOK
}

func cmdSetupRecord(f *cliFlags) int {
	if ok, code := requireNameVersion("setup-record", f); !ok {
		return code
	}
	store, c := openStore(f)
	if store == nil {
		return c
	}
	defer store.Close()

	setup, err := store.RecordSetup(f.name, f.version)
	if err != nil {
		return reportStoreError(err)
	}
	fmt.Printf("trusted setup: name=%s version=%d\n", setup.Name, setup.Version)
	return exitOK
}

func cmdSetupGet(f *cliFlags) int {
	if ok, code := requireNameVersion("setup-get", f); !ok {
		return code
	}
	store, c := openStore(f)
	if store == nil {
		return c
	}
	defer store.Close()

	setup, found, err := store.GetSetup(f.name, f.version)
	if err != nil {
		return reportStoreError(err)
	}
	if !found {
		fmt.Fprintf(os.Stderr, "error: not found: no trusted setup registered for %q version %d\n", f.name, f.version)
		return exitRule
	}
	fmt.Printf("trusted setup: name=%s version=%d present=true\n", setup.Name, setup.Version)
	return exitOK
}

func cmdJobSubmit(f *cliFlags) int {
	if f.id == "" {
		fmt.Fprintln(os.Stderr, "error: --id is required for job-submit")
		return exitUsage
	}
	if f.name == "" {
		fmt.Fprintln(os.Stderr, "error: --name is required for job-submit")
		return exitUsage
	}
	if f.version <= 0 {
		fmt.Fprintln(os.Stderr, "error: --version must be a positive integer for job-submit")
		return exitUsage
	}
	if f.attempt <= 0 {
		fmt.Fprintln(os.Stderr, "error: --attempt must be a positive integer for job-submit")
		return exitUsage
	}
	store, code := openStore(f)
	if store == nil {
		return code
	}
	defer store.Close()

	job := zkcircuit.Job{ID: f.id, Circuit: f.name, Version: f.version, Kind: f.kind, Attempt: f.attempt, CompiledHash: f.hash}
	stored, err := store.SubmitJob(job)
	if err != nil {
		return reportStoreError(err)
	}
	fmt.Println("job accepted:", formatJob(stored))
	return exitOK
}

func cmdJobGet(f *cliFlags) int {
	if f.id == "" {
		fmt.Fprintln(os.Stderr, "error: --id is required for job-get")
		return exitUsage
	}
	store, code := openStore(f)
	if store == nil {
		return code
	}
	defer store.Close()

	job, err := store.GetJob(f.id)
	if err != nil {
		return reportStoreError(err)
	}
	fmt.Println("job:", formatJob(job))
	return exitOK
}

func cmdJobList(f *cliFlags) int {
	store, code := openStore(f)
	if store == nil {
		return code
	}
	defer store.Close()

	jobs, err := store.ListJobs()
	if err != nil {
		return reportStoreError(err)
	}
	for _, j := range jobs {
		fmt.Println("job:", formatJob(j))
	}
	return exitOK
}

func cmdConstraintImport(f *cliFlags) int {
	if ok, code := requireNameVersion("constraint-import", f); !ok {
		return code
	}
	if f.file == "" {
		fmt.Fprintln(os.Stderr, "error: --file is required for constraint-import")
		return exitUsage
	}
	store, code := openStore(f)
	if store == nil {
		return code
	}
	defer store.Close()

	def, err := store.ImportConstraints(f.name, f.version, f.file)
	if err != nil {
		return reportStoreError(err)
	}
	fmt.Printf("constraints imported: name=%s version=%d modulus=%s constraints=%d\n",
		f.name, f.version, def.Modulus, len(def.Constraints))
	return exitOK
}

func cmdCircuitCompile(f *cliFlags) int {
	if ok, code := requireNameVersion("circuit-compile", f); !ok {
		return code
	}
	store, code := openStore(f)
	if store == nil {
		return code
	}
	defer store.Close()

	artifact, err := store.CompileCircuit(f.name, f.version)
	if err != nil {
		return reportStoreError(err)
	}
	fmt.Printf("artifact: name=%s version=%d modulus=%d constraints=%d hash=%s\n",
		artifact.Name, artifact.Version, artifact.Modulus, artifact.Constraints, artifact.Hash)
	return exitOK
}

// cmdInputCheck prints the verdict on stdout without ever echoing private
// values. Both satisfied and unsatisfied assignments are successful
// evaluations and exit 0; the satisfied= field carries the verdict (with
// first_failure set when false). Only gating, input-format and data problems
// use the non-zero error exits.
func cmdInputCheck(f *cliFlags) int {
	if ok, code := requireNameVersion("input-check", f); !ok {
		return code
	}
	if f.file == "" {
		fmt.Fprintln(os.Stderr, "error: --file is required for input-check")
		return exitUsage
	}
	if f.hash == "" {
		fmt.Fprintln(os.Stderr, "error: --hash is required for input-check")
		return exitUsage
	}
	store, code := openStore(f)
	if store == nil {
		return code
	}
	defer store.Close()

	result, err := store.CheckInputFile(f.name, f.version, f.hash, f.file)
	if err != nil {
		return reportStoreError(err)
	}
	if result.Satisfied {
		fmt.Printf("input check: satisfied=true name=%s version=%d hash=%s\n",
			f.name, f.version, result.Hash)
		return exitOK
	}
	fmt.Printf("input check: satisfied=false name=%s version=%d hash=%s first_failure=%d\n",
		f.name, f.version, result.Hash, result.FirstFailure)
	return exitOK
}
