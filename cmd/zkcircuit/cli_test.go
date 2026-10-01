package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capture runs fn with os.Stdout/os.Stderr redirected, returning what was
// written. The data commands never share these fds with other goroutines in
// these tests.
func capture(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wOut, wErr

	var outBuf, errBuf bytes.Buffer
	doneOut := make(chan struct{})
	doneErr := make(chan struct{})
	go func() { defer close(doneOut); outBuf.ReadFrom(rOut) }()
	go func() { defer close(doneErr); errBuf.ReadFrom(rErr) }()

	fn()

	wOut.Close()
	wErr.Close()
	os.Stdout, os.Stderr = origOut, origErr
	<-doneOut
	<-doneErr
	rOut.Close()
	rErr.Close()
	return outBuf.String(), errBuf.String()
}

func runCapture(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	code := -999
	out, errOut := capture(t, func() { code = run(args) })
	return code, out, errOut
}

func TestCLIPreservedCommands(t *testing.T) {
	code, out, _ := runCapture(t, "version")
	if code != 0 || strings.TrimSpace(out) != "zkcircuit 0.1.0" {
		t.Fatalf("version: code=%d out=%q", code, out)
	}
	for _, helpArg := range []string{"help", "-h", "--help"} {
		code, out, _ = runCapture(t, helpArg)
		if code != 0 || strings.TrimSpace(out) != "usage: zkcircuit [demo|version|help]" {
			t.Fatalf("%s: code=%d out=%q", helpArg, code, out)
		}
	}
	code, out, _ = runCapture(t, "demo")
	if code != 0 || !strings.Contains(out, "witness cost order:") {
		t.Fatalf("demo changed: code=%d out=%q", code, out)
	}
	code, _, errOut := runCapture(t, "bogus-command")
	if code != 2 || !strings.Contains(errOut, `unknown command "bogus-command"`) {
		t.Fatalf("unknown command: code=%d err=%q", code, errOut)
	}
}

func TestCLIFullLifecycle(t *testing.T) {
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

	// Missing --dir is a usage error.
	if r := call("circuit-create", "--name", "c", "--version", "1"); r.code != 2 {
		t.Fatalf("missing dir: code=%d err=%s", r.code, r.err)
	}

	if r := call("circuit-create", "--dir", dir, "--name", "transfer", "--version", "1",
		"--constraints", "2048", "--public-inputs", "2", "--private-inputs", "4",
		"--description", "first"); r.code != 0 || !strings.Contains(r.out, "frozen=false") {
		t.Fatalf("create: %+v", r)
	}
	// Invalid counts fail with exit 1.
	if r := call("circuit-create", "--dir", dir, "--name", "bad", "--version", "1",
		"--constraints", "0"); r.code != 1 || !strings.Contains(r.err, "constraints") {
		t.Fatalf("bad constraints: %+v", r)
	}
	// Idempotent re-create.
	if r := call("circuit-create", "--dir", dir, "--name", "transfer", "--version", "1",
		"--description", "first"); r.code != 0 {
		t.Fatalf("idempotent create: %+v", r)
	}
	// Conflicting description.
	if r := call("circuit-create", "--dir", dir, "--name", "transfer", "--version", "1",
		"--description", "second"); r.code != 1 || !strings.Contains(r.err, "conflict") {
		t.Fatalf("conflicting create: %+v", r)
	}
	// Update the draft, then freeze.
	if r := call("circuit-update", "--dir", dir, "--name", "transfer", "--version", "1",
		"--constraints", "4096", "--description", "grown"); r.code != 0 || !strings.Contains(r.out, "constraints=4096") {
		t.Fatalf("draft update: %+v", r)
	}
	if r := call("circuit-freeze", "--dir", dir, "--name", "transfer", "--version", "1"); r.code != 0 || !strings.Contains(r.out, "frozen=true") {
		t.Fatalf("freeze: %+v", r)
	}
	if r := call("circuit-freeze", "--dir", dir, "--name", "transfer", "--version", "1"); r.code != 0 {
		t.Fatalf("re-freeze: %+v", r)
	}
	if r := call("circuit-update", "--dir", dir, "--name", "transfer", "--version", "1",
		"--constraints", "1"); r.code != 1 || !strings.Contains(r.err, "frozen") {
		t.Fatalf("frozen update: %+v", r)
	}

	// Job gates: no setup yet.
	if r := call("job-submit", "--dir", dir, "--id", "j1", "--name", "transfer",
		"--version", "1", "--attempt", "1"); r.code != 1 || !strings.Contains(r.err, "trusted setup missing") {
		t.Fatalf("job without setup: %+v", r)
	}
	if r := call("setup-record", "--dir", dir, "--name", "transfer", "--version", "1"); r.code != 0 {
		t.Fatalf("setup record: %+v", r)
	}
	if r := call("setup-record", "--dir", dir, "--name", "transfer", "--version", "1"); r.code != 0 {
		t.Fatalf("setup repeat: %+v", r)
	}
	if r := call("job-submit", "--dir", dir, "--id", "j1", "--name", "transfer",
		"--version", "1", "--attempt", "1"); r.code != 0 || !strings.Contains(r.out, "job accepted") {
		t.Fatalf("job submit: %+v", r)
	}
	if r := call("job-submit", "--dir", dir, "--id", "j1", "--name", "transfer",
		"--version", "1", "--attempt", "2"); r.code != 1 || !strings.Contains(r.err, "conflict") {
		t.Fatalf("job conflict: %+v", r)
	}
	if r := call("job-submit", "--dir", dir, "--id", "j2", "--name", "ghost",
		"--version", "1", "--attempt", "1"); r.code != 1 || !strings.Contains(r.err, "not found") {
		t.Fatalf("job unknown circuit: %+v", r)
	}
	if r := call("job-submit", "--dir", dir, "--id", "j3", "--name", "transfer",
		"--version", "1", "--attempt", "0"); r.code != 2 {
		t.Fatalf("bad attempt should be usage error: %+v", r)
	}

	// Queries.
	if r := call("job-get", "--dir", dir, "--id", "j1"); r.code != 0 || !strings.Contains(r.out, "version=1") {
		t.Fatalf("job get: %+v", r)
	}
	if r := call("job-get", "--dir", dir, "--id", "missing"); r.code != 1 || !strings.Contains(r.err, "not found") {
		t.Fatalf("job get missing: %+v", r)
	}
	if r := call("job-list", "--dir", dir); r.code != 0 || strings.Count(r.out, "job:") != 1 {
		t.Fatalf("job list: %+v", r)
	}
	if r := call("circuit-list", "--dir", dir); r.code != 0 || !strings.Contains(r.out, "transfer") {
		t.Fatalf("circuit list: %+v", r)
	}
	if r := call("setup-get", "--dir", dir, "--name", "transfer", "--version", "1"); r.code != 0 || !strings.Contains(r.out, "present=true") {
		t.Fatalf("setup get: %+v", r)
	}
	if r := call("setup-get", "--dir", dir, "--name", "transfer", "--version", "2"); r.code != 1 {
		t.Fatalf("setup get absent: %+v", r)
	}
}

func TestCLIConstraintLifecycle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	r := func(args ...string) (int, string, string) { return runCapture(t, args...) }

	// Create a draft version with 2 constraints, 1 public, 1 private.
	if code, _, errOut := r("circuit-create", "--dir", dir, "--name", "mul", "--version", "1",
		"--constraints", "2", "--public-inputs", "1", "--private-inputs", "1",
		"--description", "demo"); code != 0 {
		t.Fatalf("create: %s", errOut)
	}

	// Missing --file is a usage error.
	if code, _, _ := r("constraint-import", "--dir", dir, "--name", "mul", "--version", "1"); code != 2 {
		t.Fatalf("import without --file: want exit 2, got %d", code)
	}

	defFile := filepath.Join(t.TempDir(), "def.json")
	os.WriteFile(defFile, []byte(`{
		"modulus": "2147483647",
		"constraints": [
			{"a":[{"wire":0,"coeff":"3"},{"wire":1,"coeff":"2"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"},{"wire":2,"coeff":"2"}]},
			{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":1,"coeff":"1"}],"c":[{"wire":1,"coeff":"1"}]}
		]
	}`), 0o644)
	if code, out, errOut := r("constraint-import", "--dir", dir, "--name", "mul", "--version", "1",
		"--file", defFile); code != 0 || !strings.Contains(out, "constraints=2") {
		t.Fatalf("import: code=%d out=%q err=%q", code, out, errOut)
	}

	// Illegal definition: count mismatch is rejected, state unchanged.
	badFile := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(badFile, []byte(`{"modulus":"2","constraints":[]}`), 0o644)
	if code, _, _ := r("constraint-import", "--dir", dir, "--name", "mul", "--version", "1",
		"--file", badFile); code != 1 {
		t.Fatalf("bad import: want exit 1, got %d", code)
	}

	// Compile before freeze: not frozen.
	if code, _, _ := r("compile", "--dir", dir, "--name", "mul", "--version", "1"); code != 1 {
		t.Fatalf("compile draft: want exit 1, got %d", code)
	}
	if code, _, errOut := r("circuit-freeze", "--dir", dir, "--name", "mul", "--version", "1"); code != 0 {
		t.Fatalf("freeze: %s", errOut)
	}
	code, out, errOut := r("compile", "--dir", dir, "--name", "mul", "--version", "1")
	if code != 0 || !strings.Contains(out, "compiled:") || !strings.Contains(out, "artifact=") {
		t.Fatalf("compile: code=%d out=%q err=%q", code, out, errOut)
	}
	// Idempotent compile.
	code, out2, _ := r("compile", "--dir", dir, "--name", "mul", "--version", "1")
	if code != 0 || out2 != out {
		t.Fatalf("recompile differs: %q vs %q", out, out2)
	}
	artifact := strings.TrimSpace(strings.Split(out, "artifact=")[1])

	// Check a satisfying witness: p=0, q=6.
	witFile := filepath.Join(t.TempDir(), "wit.json")
	os.WriteFile(witFile, []byte(`{"public":["0"],"private":["6"]}`), 0o644)
	code, out, errOut = r("check", "--dir", dir, "--name", "mul", "--version", "1",
		"--artifact", artifact, "--input", witFile)
	if code != 0 || !strings.Contains(out, "satisfied:") {
		t.Fatalf("check satisfied: code=%d out=%q err=%q", code, out, errOut)
	}
	// The output must never carry witness values or private data.
	if strings.Contains(out, "private") || strings.Contains(out, "failed") {
		t.Fatalf("check output leaked private data: %q", out)
	}

	// Failing witness: p=5, q=7 fails constraint 1.
	os.WriteFile(witFile, []byte(`{"public":["5"],"private":["7"]}`), 0o644)
	code, out, _ = r("check", "--dir", dir, "--name", "mul", "--version", "1",
		"--artifact", artifact, "--input", witFile)
	if code != 0 ||
		!strings.Contains(out, "not satisfied:") || !strings.Contains(out, "failed_constraint=1") {
		t.Fatalf("check not satisfied: code=%d out=%q", code, out)
	}

	// Bad witness format: missing private array.
	badWit := filepath.Join(t.TempDir(), "badwit.json")
	os.WriteFile(badWit, []byte(`{"public":["0"]}`), 0o644)
	if code, _, _ := r("check", "--dir", dir, "--name", "mul", "--version", "1",
		"--artifact", artifact, "--input", badWit); code != 1 {
		t.Fatalf("bad witness: want exit 1, got %d", code)
	}

	// Wrong artifact hash.
	if code, _, _ := r("check", "--dir", dir, "--name", "mul", "--version", "1",
		"--artifact", strings.Repeat("0", 64), "--input", witFile); code != 1 {
		t.Fatalf("wrong artifact: want exit 1, got %d", code)
	}

	// Reopen: definition and artifact survive.
	if code, out, _ := r("check", "--dir", dir, "--name", "mul", "--version", "1",
		"--artifact", artifact, "--input", witFile); code != 0 || !strings.Contains(out, "not satisfied:") {
		t.Fatalf("check after reopen: code=%d out=%q", code, out)
	}
}

func TestCLICompileWithoutDefinition(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	r := func(args ...string) (int, string, string) { return runCapture(t, args...) }

	if code, _, _ := r("circuit-create", "--dir", dir, "--name", "c", "--version", "1",
		"--constraints", "1"); code != 0 {
		t.Fatal("create")
	}
	if code, _, _ := r("circuit-freeze", "--dir", dir, "--name", "c", "--version", "1"); code != 0 {
		t.Fatal("freeze")
	}
	if code, _, errOut := r("compile", "--dir", dir, "--name", "c", "--version", "1"); code != 1 ||
		!strings.Contains(errOut, "constraint definition missing") {
		t.Fatalf("compile without definition: code=%d err=%q", code, errOut)
	}
	// Check on a frozen version without artifact reports artifact missing.
	witFile := filepath.Join(t.TempDir(), "w.json")
	os.WriteFile(witFile, []byte(`{"public":[],"private":[]}`), 0o644)
	if code, _, errOut := r("check", "--dir", dir, "--name", "c", "--version", "1",
		"--artifact", strings.Repeat("0", 64), "--input", witFile); code != 1 ||
		!strings.Contains(errOut, "artifact missing") {
		t.Fatalf("check without artifact: code=%d err=%q", code, errOut)
	}
}

func TestCLIPersistenceAndCorruption(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	r := func(args ...string) (int, string, string) { return runCapture(t, args...) }

	if code, _, _ := r("circuit-create", "--dir", dir, "--name", "c", "--version", "1",
		"--constraints", "32", "--description", "d"); code != 0 {
		t.Fatal("seed create")
	}
	if code, _, _ := r("circuit-freeze", "--dir", dir, "--name", "c", "--version", "1"); code != 0 {
		t.Fatal("seed freeze")
	}
	if code, _, _ := r("setup-record", "--dir", dir, "--name", "c", "--version", "1"); code != 0 {
		t.Fatal("seed setup")
	}
	if code, out, _ := r("circuit-get", "--dir", dir, "--name", "c", "--version", "1"); code != 0 && !strings.Contains(out, "frozen=true") {
		t.Fatalf("state after reopen lost or wrong: code=%d out=%q", code, out)
	}

	// Corrupt the committed file: every data command must report a read
	// failure with exit code 3 and leave the broken file in place.
	dataFile := filepath.Join(dir, "data.json")
	original, err := os.ReadFile(dataFile)
	if err != nil {
		t.Fatal(err)
	}
	bad := append(bytes.Clone(original), []byte("{{not-json")...)
	if err := os.WriteFile(dataFile, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := r("circuit-list", "--dir", dir)
	if code != 3 || !strings.Contains(errOut, "read failed") {
		t.Fatalf("corrupt file: code=%d err=%q", code, errOut)
	}
	after, err := os.ReadFile(dataFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatal("corrupt file was modified or overwritten")
	}
}
