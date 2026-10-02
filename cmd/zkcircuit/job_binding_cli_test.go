package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asdhoaiqqq/zkcircuit-workbench/zkcircuit"
)

const cliBindingDef = `{"modulus":"7","constraints":[
	{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`

// setupCompiledVersion drives the full lifecycle through the CLI and returns
// the artifact hash printed by circuit-compile.
func setupCompiledVersion(t *testing.T, dir, name string) string {
	t.Helper()
	defFile := filepath.Join(t.TempDir(), "def.json")
	if err := os.WriteFile(defFile, []byte(cliBindingDef), 0o644); err != nil {
		t.Fatal(err)
	}
	r := func(args ...string) (int, string, string) { return runCapture(t, args...) }
	if code, _, e := r("circuit-create", "--dir", dir, "--name", name, "--version", "1",
		"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1"); code != 0 {
		t.Fatalf("create: %s", e)
	}
	if code, _, e := r("constraint-import", "--dir", dir, "--name", name, "--version", "1", "--file", defFile); code != 0 {
		t.Fatalf("import: %s", e)
	}
	if code, _, e := r("circuit-freeze", "--dir", dir, "--name", name, "--version", "1"); code != 0 {
		t.Fatalf("freeze: %s", e)
	}
	if code, _, e := r("setup-record", "--dir", dir, "--name", name, "--version", "1"); code != 0 {
		t.Fatalf("setup: %s", e)
	}
	code, out, e := r("circuit-compile", "--dir", dir, "--name", name, "--version", "1")
	if code != 0 {
		t.Fatalf("compile: %s", e)
	}
	// output: artifact: name=... version=1 modulus=7 constraints=1 hash=<64>
	idx := strings.Index(out, "hash=")
	if idx < 0 {
		t.Fatalf("compile output has no hash: %q", out)
	}
	return strings.TrimSpace(out[idx+len("hash="):])
}

func TestCLIJobSubmitHashBinding(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	hash := setupCompiledVersion(t, dir, "c")
	r := func(args ...string) (int, string, string) { return runCapture(t, args...) }

	// Binding submission: the hash appears in the submission output, in
	// job-get and in job-list.
	code, out, _ := r("job-submit", "--dir", dir, "--id", "j1", "--name", "c", "--version", "1", "--attempt", "1", "--hash", hash)
	if code != 0 || !strings.Contains(out, "compiled_hash="+hash) {
		t.Fatalf("bound submit: code=%d out=%q", code, out)
	}
	code, out, _ = r("job-get", "--dir", dir, "--id", "j1")
	if code != 0 || !strings.Contains(out, "compiled_hash="+hash) {
		t.Fatalf("job-get binding: code=%d out=%q", code, out)
	}
	code, out, _ = r("job-list", "--dir", dir)
	if code != 0 || !strings.Contains(out, "compiled_hash="+hash) {
		t.Fatalf("job-list binding: code=%d out=%q", code, out)
	}

	// A wrong hash is a business failure, exit 1, and stores nothing.
	code, _, errOut := r("job-submit", "--dir", dir, "--id", "jbad", "--name", "c", "--version", "1", "--attempt", "1", "--hash", hash+"00")
	if code != 1 || !strings.Contains(errOut, "artifact mismatch") {
		t.Fatalf("wrong hash: code=%d err=%q", code, errOut)
	}
	code, _, _ = r("job-get", "--dir", dir, "--id", "jbad")
	if code != 1 {
		t.Fatalf("rejected job should not exist, code=%d", code)
	}

	// A frozen version with a setup but no artifact reports missing.
	if code, _, e := r("circuit-create", "--dir", dir, "--name", "bare", "--version", "1", "--constraints", "1"); code != 0 {
		t.Fatalf("bare create: %s", e)
	}
	if code, _, e := r("circuit-freeze", "--dir", dir, "--name", "bare", "--version", "1"); code != 0 {
		t.Fatalf("bare freeze: %s", e)
	}
	if code, _, e := r("setup-record", "--dir", dir, "--name", "bare", "--version", "1"); code != 0 {
		t.Fatalf("bare setup: %s", e)
	}
	code, _, errOut = r("job-submit", "--dir", dir, "--id", "jbare", "--name", "bare", "--version", "1", "--attempt", "1", "--hash", hash)
	if code != 1 || !strings.Contains(errOut, "compiled artifact missing") {
		t.Fatalf("missing artifact: code=%d err=%q", code, errOut)
	}

	// Unbound submission without --hash keeps the old behavior.
	code, out, _ = r("job-submit", "--dir", dir, "--id", "junbound", "--name", "bare", "--version", "1", "--attempt", "1")
	if code != 0 || strings.Contains(out, "compiled_hash") {
		t.Fatalf("unbound submit: code=%d out=%q", code, out)
	}
	code, out, _ = r("job-get", "--dir", dir, "--id", "junbound")
	if code != 0 || strings.Contains(out, "compiled_hash") {
		t.Fatalf("unbound job must not show a binding: code=%d out=%q", code, out)
	}

	// A changed hash on resubmission is a conflict even when the new hash
	// matches no artifact.
	code, _, errOut = r("job-submit", "--dir", dir, "--id", "j1", "--name", "c", "--version", "1", "--attempt", "1", "--hash", "does-not-exist")
	if code != 1 || !strings.Contains(errOut, "conflict") {
		t.Fatalf("conflict precedence: code=%d err=%q", code, errOut)
	}
	// Switching to unbound is also a conflict.
	code, _, errOut = r("job-submit", "--dir", dir, "--id", "j1", "--name", "c", "--version", "1", "--attempt", "1")
	if code != 1 || !strings.Contains(errOut, "conflict") {
		t.Fatalf("bound->unbound conflict: code=%d err=%q", code, errOut)
	}

	// Reopening the directory keeps the binding.
	s2, err := zkcircuit.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.GetJob("j1")
	if err != nil || got.CompiledHash != hash {
		t.Fatalf("binding lost after reopen: %+v %v", got, err)
	}
}

func TestCLIBoundJobTamperExitThree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	hash := setupCompiledVersion(t, dir, "c")
	r := func(args ...string) (int, string, string) { return runCapture(t, args...) }
	if code, _, e := r("job-submit", "--dir", dir, "--id", "j1", "--name", "c", "--version", "1", "--attempt", "1", "--hash", hash); code != 0 {
		t.Fatalf("bind: %s", e)
	}

	// Hand-edit the committed file: drop the artifact record entirely.
	dataFile := filepath.Join(dir, "data.json")
	raw, err := os.ReadFile(dataFile)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	delete(env, "artifacts")
	tampered, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataFile, tampered, 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := r("job-get", "--dir", dir, "--id", "j1")
	if code != 3 || !strings.Contains(errOut, "read failed") {
		t.Fatalf("tampered binding: code=%d err=%q", code, errOut)
	}
	// The file on disk is untouched by the failed reads.
	after, _ := os.ReadFile(dataFile)
	if string(after) != string(tampered) {
		t.Fatal("failed read modified the data file")
	}
}
