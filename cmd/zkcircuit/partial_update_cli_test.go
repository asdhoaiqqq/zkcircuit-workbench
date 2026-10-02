package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// buildZkcircuit builds the CLI binary into a temp file for subprocess tests.
func buildZkcircuit(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "zkcircuit")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build zkcircuit: %v\n%s", err, out)
	}
	return bin
}

func runBin(t *testing.T, bin, dir string, args ...string) (int, string, string) {
	t.Helper()
	// args[0] is the command; --dir is inserted after it.
	cmdArgs := append([]string{args[0], "--dir", dir}, args[1:]...)
	cmd := exec.Command(bin, cmdArgs...)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run %v: %v", args, err)
		}
	}
	return code, outBuf.String(), errBuf.String()
}

func TestCLICircuitUpdatePartial(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	type res struct {
		code int
		out  string
		err  string
	}
	call := func(args ...string) res {
		c, o, e := runCapture(t, args...)
		return res{c, o, e}
	}

	// Seed the exact fixture: 2 constraints, 1 public, 3 private, "初稿".
	if r := call("circuit-create", "--dir", dir, "--name", "c", "--version", "1",
		"--constraints", "2", "--public-inputs", "1", "--private-inputs", "3",
		"--description", "初稿"); r.code != 0 {
		t.Fatalf("create: %+v", r)
	}

	// Description-only update must leave all three counts intact.
	r := call("circuit-update", "--dir", dir, "--name", "c", "--version", "1",
		"--description", "改后")
	if r.code != 0 {
		t.Fatalf("description-only update: %+v", r)
	}
	if !strings.Contains(r.out, "constraints=2") || !strings.Contains(r.out, "public_inputs=1") ||
		!strings.Contains(r.out, "private_inputs=3") || !strings.Contains(r.out, `description="改后"`) {
		t.Fatalf("description-only update changed fields: %q", r.out)
	}

	// Explicit zero input counts must zero them, not be treated as omitted.
	r = call("circuit-update", "--dir", dir, "--name", "c", "--version", "1",
		"--public-inputs", "0", "--private-inputs", "0")
	if r.code != 0 || !strings.Contains(r.out, "public_inputs=0") || !strings.Contains(r.out, "private_inputs=0") {
		t.Fatalf("explicit zero counts: %+v", r)
	}
	// Constraints and description untouched.
	if !strings.Contains(r.out, "constraints=2") || !strings.Contains(r.out, `description="改后"`) {
		t.Fatalf("zero update leaked: %q", r.out)
	}

	// Explicit empty description must clear it.
	r = call("circuit-update", "--dir", dir, "--name", "c", "--version", "1",
		"--description", "")
	if r.code != 0 || !strings.Contains(r.out, `description=""`) {
		t.Fatalf("explicit empty description: %+v", r)
	}
	if !strings.Contains(r.out, "constraints=2") || !strings.Contains(r.out, "public_inputs=0") ||
		!strings.Contains(r.out, "private_inputs=0") {
		t.Fatalf("empty description update leaked: %q", r.out)
	}

	// No modifiable fields: success, full record, nothing written.
	r = call("circuit-update", "--dir", dir, "--name", "c", "--version", "1")
	if r.code != 0 {
		t.Fatalf("no-op update: %+v", r)
	}
	if !strings.Contains(r.out, "constraints=2") || !strings.Contains(r.out, "frozen=false") {
		t.Fatalf("no-op update returned wrong record: %q", r.out)
	}
	dataBefore, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	// A second no-op must not change the file.
	if r := call("circuit-update", "--dir", dir, "--name", "c", "--version", "1"); r.code != 0 {
		t.Fatalf("second no-op: %+v", r)
	}
	dataAfter, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(dataBefore) != string(dataAfter) {
		t.Fatal("no-op update wrote to data.json")
	}

	// Unknown version -> not found (exit 1), even with no fields.
	if r := call("circuit-update", "--dir", dir, "--name", "ghost", "--version", "1"); r.code != 1 ||
		!strings.Contains(r.err, "not found") {
		t.Fatalf("unknown version: %+v", r)
	}

	// Freeze, then update -> frozen (exit 1), even with no fields.
	if r := call("circuit-freeze", "--dir", dir, "--name", "c", "--version", "1"); r.code != 0 {
		t.Fatalf("freeze: %+v", r)
	}
	if r := call("circuit-update", "--dir", dir, "--name", "c", "--version", "1",
		"--description", "x"); r.code != 1 || !strings.Contains(r.err, "frozen") {
		t.Fatalf("frozen update: %+v", r)
	}
	if r := call("circuit-update", "--dir", dir, "--name", "c", "--version", "1"); r.code != 1 ||
		!strings.Contains(r.err, "frozen") {
		t.Fatalf("frozen no-op: %+v", r)
	}

	// Reopen: the last committed state is the frozen record with the empty
	// description and zero counts.
	if r := call("circuit-get", "--dir", dir, "--name", "c", "--version", "1"); r.code != 0 ||
		!strings.Contains(r.out, "frozen=true") || !strings.Contains(r.out, `description=""`) ||
		!strings.Contains(r.out, "public_inputs=0") || !strings.Contains(r.out, "private_inputs=0") {
		t.Fatalf("state after reopen: %+v", r)
	}
}

func TestCLICircuitUpdateInvalidCountsRejected(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	type res struct {
		code int
		out  string
		err  string
	}
	call := func(args ...string) res {
		c, o, e := runCapture(t, args...)
		return res{c, o, e}
	}

	if r := call("circuit-create", "--dir", dir, "--name", "c", "--version", "1",
		"--constraints", "2", "--public-inputs", "1", "--private-inputs", "3",
		"--description", "初稿"); r.code != 0 {
		t.Fatalf("create: %+v", r)
	}

	// Zero constraints is invalid and refuses the whole change.
	if r := call("circuit-update", "--dir", dir, "--name", "c", "--version", "1",
		"--constraints", "0", "--description", "x"); r.code != 1 ||
		!strings.Contains(r.err, "constraints") {
		t.Fatalf("zero constraints: %+v", r)
	}
	// Negative input counts are invalid.
	if r := call("circuit-update", "--dir", dir, "--name", "c", "--version", "1",
		"--public-inputs", "-1"); r.code != 1 || !strings.Contains(r.err, "public input") {
		t.Fatalf("negative public: %+v", r)
	}
	if r := call("circuit-update", "--dir", dir, "--name", "c", "--version", "1",
		"--private-inputs", "-1"); r.code != 1 || !strings.Contains(r.err, "private input") {
		t.Fatalf("negative private: %+v", r)
	}

	// Everything preserved.
	if r := call("circuit-get", "--dir", dir, "--name", "c", "--version", "1"); r.code != 0 ||
		!strings.Contains(r.out, "constraints=2") || !strings.Contains(r.out, "public_inputs=1") ||
		!strings.Contains(r.out, "private_inputs=3") || !strings.Contains(r.out, `description="初稿"`) {
		t.Fatalf("rejected update leaked: %+v", r)
	}
}

func TestCLICircuitUpdateWithDefinition(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "bench")
	defPath := writeFile(t, work, "def.json", cliDef)
	type res struct {
		code int
		out  string
		err  string
	}
	call := func(args ...string) res {
		c, o, e := runCapture(t, args...)
		return res{c, o, e}
	}

	// Draft with counts matching the definition, then import.
	if r := call("circuit-create", "--dir", dir, "--name", "mul", "--version", "1",
		"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1",
		"--description", "smoke"); r.code != 0 {
		t.Fatalf("create: %+v", r)
	}
	if r := call("constraint-import", "--dir", dir, "--name", "mul", "--version", "1",
		"--file", defPath); r.code != 0 {
		t.Fatalf("import: %+v", r)
	}

	// A compatible adjustment is allowed even with a definition present.
	if r := call("circuit-update", "--dir", dir, "--name", "mul", "--version", "1",
		"--public-inputs", "2", "--private-inputs", "3"); r.code != 0 ||
		!strings.Contains(r.out, "public_inputs=2") || !strings.Contains(r.out, "private_inputs=3") {
		t.Fatalf("compatible adjustment: %+v", r)
	}

	// Shrinking below a referenced wire is refused wholesale. After the
	// compatible adjustment (2 public + 3 private, wires 1..5) zeroing both
	// inputs leaves only the constant wire, so wire 2 is out of range.
	if r := call("circuit-update", "--dir", dir, "--name", "mul", "--version", "1",
		"--public-inputs", "0", "--private-inputs", "0"); r.code != 1 || !strings.Contains(r.err, "definition") {
		t.Fatalf("shrink past wire: %+v", r)
	}
	// Drifting the constraint count is refused.
	if r := call("circuit-update", "--dir", dir, "--name", "mul", "--version", "1",
		"--constraints", "2"); r.code != 1 || !strings.Contains(r.err, "definition") {
		t.Fatalf("constraint drift: %+v", r)
	}
	// A good field alongside a bad field refuses the whole change.
	if r := call("circuit-update", "--dir", dir, "--name", "mul", "--version", "1",
		"--description", "good", "--public-inputs", "0", "--private-inputs", "0"); r.code != 1 {
		t.Fatalf("good with bad field: %+v", r)
	}

	// Counts and description preserved; definition still intact.
	if r := call("circuit-get", "--dir", dir, "--name", "mul", "--version", "1"); r.code != 0 ||
		!strings.Contains(r.out, "constraints=1") || !strings.Contains(r.out, "public_inputs=2") ||
		!strings.Contains(r.out, "private_inputs=3") || !strings.Contains(r.out, `description="smoke"`) {
		t.Fatalf("rejected update leaked: %+v", r)
	}
}

// Two real subprocess clients operate on the same data directory
// concurrently: one changes the description, the other the input counts.
// Both committed fields must survive; a later freeze must see the merged
// record.
func TestCLICircuitUpdateConcurrentClients(t *testing.T) {
	bin := buildZkcircuit(t)
	dir := filepath.Join(t.TempDir(), "bench")

	// Seed the draft.
	if code, _, errOut := runBin(t, bin, dir, "circuit-create", "--name", "c", "--version", "1",
		"--constraints", "2", "--public-inputs", "1", "--private-inputs", "3",
		"--description", "初稿"); code != 0 {
		t.Fatalf("seed: %s", errOut)
	}

	var wg sync.WaitGroup
	errs := make(chan string, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		code, out, errOut := runBin(t, bin, dir, "circuit-update", "--name", "c", "--version", "1",
			"--description", "client-A")
		if code != 0 {
			errs <- errOut
			return
		}
		if !strings.Contains(out, `description="client-A"`) {
			errs <- "client-A output missing description: " + out
		}
	}()
	go func() {
		defer wg.Done()
		code, out, errOut := runBin(t, bin, dir, "circuit-update", "--name", "c", "--version", "1",
			"--public-inputs", "4", "--private-inputs", "5")
		if code != 0 {
			errs <- errOut
			return
		}
		if !strings.Contains(out, "public_inputs=4") || !strings.Contains(out, "private_inputs=5") {
			errs <- "client-B output missing counts: " + out
		}
	}()
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent client: %v", e)
	}

	// Both fields preserved, constraints untouched.
	_, out, _ := runBin(t, bin, dir, "circuit-get", "--name", "c", "--version", "1")
	if !strings.Contains(out, "constraints=2") || !strings.Contains(out, "public_inputs=4") ||
		!strings.Contains(out, "private_inputs=5") || !strings.Contains(out, `description="client-A"`) {
		t.Fatalf("merged record wrong: %q", out)
	}

	// Freeze after the merged updates; a later update is refused.
	if code, _, errOut := runBin(t, bin, dir, "circuit-freeze", "--name", "c", "--version", "1"); code != 0 {
		t.Fatalf("freeze: %s", errOut)
	}
	if code, _, _ := runBin(t, bin, dir, "circuit-update", "--name", "c", "--version", "1",
		"--description", "late"); code != 1 {
		t.Fatal("update after freeze should be refused")
	}

	// Reopen in a fresh process: the merged, frozen record is the state.
	_, out, _ = runBin(t, bin, dir, "circuit-get", "--name", "c", "--version", "1")
	if !strings.Contains(out, "frozen=true") || !strings.Contains(out, "constraints=2") ||
		!strings.Contains(out, "public_inputs=4") || !strings.Contains(out, "private_inputs=5") ||
		!strings.Contains(out, `description="client-A"`) {
		t.Fatalf("state after reopen: %q", out)
	}
}
