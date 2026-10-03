package main

import (
	"bytes"
	"encoding/json"
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

// A stored constraint definition damaged after import — here an empty side
// deleted, which leaves the canonical form and the compiled artifact hash
// unchanged — must still fail every data command with exit code 3, print no
// success record and leave the file untouched.
func TestCLICorruptDefinitionRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	r := func(args ...string) (int, string, string) { return runCapture(t, args...) }

	defFile := filepath.Join(t.TempDir(), "def.json")
	def := `{"modulus":"7","constraints":[{"a":[{"wire":0,"coeff":"1"},{"wire":1,"coeff":"1"}],"b":[],"c":[]}]}`
	if err := os.WriteFile(defFile, []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := r("circuit-create", "--dir", dir, "--name", "c", "--version", "1",
		"--constraints", "1", "--public-inputs", "1"); code != 0 {
		t.Fatal("seed create")
	}
	if code, _, _ := r("constraint-import", "--dir", dir, "--name", "c", "--version", "1",
		"--file", defFile); code != 0 {
		t.Fatal("seed import")
	}
	if code, _, _ := r("circuit-freeze", "--dir", dir, "--name", "c", "--version", "1"); code != 0 {
		t.Fatal("seed freeze")
	}
	if code, _, _ := r("circuit-compile", "--dir", dir, "--name", "c", "--version", "1"); code != 0 {
		t.Fatal("seed compile")
	}

	dataFile := filepath.Join(dir, "data.json")
	original, err := os.ReadFile(dataFile)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(original, &env); err != nil {
		t.Fatal(err)
	}
	circuit := env["circuits"].([]any)[0].(map[string]any)
	constraint := circuit["definition"].(map[string]any)["constraints"].([]any)[0].(map[string]any)
	delete(constraint, "b")
	bad, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataFile, bad, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"circuit-list", "--dir", dir},
		{"circuit-get", "--dir", dir, "--name", "c", "--version", "1"},
		{"circuit-compile", "--dir", dir, "--name", "c", "--version", "1"},
	} {
		code, out, errOut := r(args...)
		if code != 3 || !strings.Contains(errOut, "read failed") || out != "" {
			t.Fatalf("%v: code=%d out=%q err=%q", args, code, out, errOut)
		}
	}
	after, err := os.ReadFile(dataFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatal("corrupt file was modified or overwritten")
	}
}
