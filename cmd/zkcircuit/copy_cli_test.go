package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedFrozenCircuit creates and freezes version 1 of "c" in dir, returning
// the create output for reference.
func seedFrozenCircuit(t *testing.T, dir string) {
	t.Helper()
	if code, out, errOut := runCapture(t, "circuit-create", "--dir", dir, "--name", "c", "--version", "1",
		"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1",
		"--description", "源版本"); code != 0 {
		t.Fatalf("create: code=%d out=%q err=%q", code, out, errOut)
	}
	defPath := filepath.Join(t.TempDir(), "def.json")
	def := `{"modulus":"7","constraints":[{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`
	if err := os.WriteFile(defPath, []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := runCapture(t, "constraint-import", "--dir", dir, "--name", "c", "--version", "1",
		"--file", defPath); code != 0 {
		t.Fatalf("import: code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := runCapture(t, "circuit-freeze", "--dir", dir, "--name", "c", "--version", "1"); code != 0 {
		t.Fatalf("freeze: code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestCLICircuitCopy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	seedFrozenCircuit(t, dir)

	code, out, errOut := runCapture(t, "circuit-copy", "--dir", dir, "--name", "c", "--version", "1", "--to-version", "2")
	if code != 0 {
		t.Fatalf("copy: code=%d err=%q", code, errOut)
	}
	for _, want := range []string{
		"name=c", "version=2", "constraints=1", "public_inputs=1", "private_inputs=1",
		"frozen=false", `description="源版本"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("copy output missing %q: %q", want, out)
		}
	}

	// The new draft is visible to get and list.
	code, out, _ = runCapture(t, "circuit-get", "--dir", dir, "--name", "c", "--version", "2")
	if code != 0 || !strings.Contains(out, "version=2") || !strings.Contains(out, "frozen=false") {
		t.Fatalf("get copied: code=%d out=%q", code, out)
	}
	code, out, _ = runCapture(t, "circuit-list", "--dir", dir)
	if code != 0 || strings.Count(out, "circuit:") != 2 {
		t.Fatalf("list after copy: code=%d out=%q", code, out)
	}

	// Repeating the identical copy returns the existing draft, exit 0, no
	// new record.
	code, out, errOut = runCapture(t, "circuit-copy", "--dir", dir, "--name", "c", "--version", "1", "--to-version", "2")
	if code != 0 || !strings.Contains(out, "version=2") {
		t.Fatalf("idempotent re-copy: code=%d out=%q err=%q", code, out, errOut)
	}
	_, out, _ = runCapture(t, "circuit-list", "--dir", dir)
	if strings.Count(out, "circuit:") != 2 {
		t.Fatalf("re-copy added records: %q", out)
	}

	// The copied draft accepts the ordinary draft operations.
	code, _, errOut = runCapture(t, "circuit-update", "--dir", dir, "--name", "c", "--version", "2",
		"--description", "修改后")
	if code != 0 {
		t.Fatalf("update copy: code=%d err=%q", code, errOut)
	}
	// … and now the copy request conflicts with the changed draft.
	code, _, errOut = runCapture(t, "circuit-copy", "--dir", dir, "--name", "c", "--version", "1", "--to-version", "2")
	if code != 1 || !strings.Contains(errOut, "conflict") {
		t.Fatalf("changed target: code=%d err=%q", code, errOut)
	}
	// The source is untouched.
	_, out, _ = runCapture(t, "circuit-get", "--dir", dir, "--name", "c", "--version", "1")
	if !strings.Contains(out, "frozen=true") || !strings.Contains(out, `description="源版本"`) {
		t.Fatalf("source changed: %q", out)
	}
}

func TestCLICircuitCopyUsageErrors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	seedFrozenCircuit(t, dir)

	cases := []struct {
		name string
		args []string
	}{
		{"missing to-version", []string{"circuit-copy", "--dir", dir, "--name", "c", "--version", "1"}},
		{"zero to-version", []string{"circuit-copy", "--dir", dir, "--name", "c", "--version", "1", "--to-version", "0"}},
		{"negative to-version", []string{"circuit-copy", "--dir", dir, "--name", "c", "--version", "1", "--to-version", "-2"}},
		{"same version", []string{"circuit-copy", "--dir", dir, "--name", "c", "--version", "1", "--to-version", "1"}},
		{"missing name", []string{"circuit-copy", "--dir", dir, "--version", "1", "--to-version", "2"}},
		{"missing version", []string{"circuit-copy", "--dir", dir, "--name", "c", "--to-version", "2"}},
		{"missing dir", []string{"circuit-copy", "--name", "c", "--version", "1", "--to-version", "2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, _ := runCapture(t, tc.args...)
			if code != 2 {
				t.Fatalf("want exit 2, got %d", code)
			}
		})
	}
	// No target records were left behind by the rejected calls.
	_, out, _ := runCapture(t, "circuit-list", "--dir", dir)
	if strings.Count(out, "circuit:") != 1 {
		t.Fatalf("usage errors left records: %q", out)
	}
}

func TestCLICircuitCopyRuleFailures(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")

	// Unknown source: not found, exit 1.
	code, _, errOut := runCapture(t, "circuit-copy", "--dir", dir, "--name", "ghost", "--version", "1", "--to-version", "2")
	if code != 1 || !strings.Contains(errOut, "not found") {
		t.Fatalf("missing source: code=%d err=%q", code, errOut)
	}

	// Unfrozen source: not frozen, exit 1.
	if code, _, errOut := runCapture(t, "circuit-create", "--dir", dir, "--name", "c", "--version", "1",
		"--constraints", "1"); code != 0 {
		t.Fatalf("create: code=%d err=%q", code, errOut)
	}
	code, _, errOut = runCapture(t, "circuit-copy", "--dir", dir, "--name", "c", "--version", "1", "--to-version", "2")
	if code != 1 || !strings.Contains(errOut, "not frozen") {
		t.Fatalf("draft source: code=%d err=%q", code, errOut)
	}

	// Frozen target conflicts even when content matches.
	if code, _, errOut := runCapture(t, "circuit-freeze", "--dir", dir, "--name", "c", "--version", "1"); code != 0 {
		t.Fatalf("freeze: code=%d err=%q", code, errOut)
	}
	if code, _, errOut := runCapture(t, "circuit-copy", "--dir", dir, "--name", "c", "--version", "1", "--to-version", "2"); code != 0 {
		t.Fatalf("copy: code=%d err=%q", code, errOut)
	}
	if code, _, errOut := runCapture(t, "circuit-freeze", "--dir", dir, "--name", "c", "--version", "2"); code != 0 {
		t.Fatalf("freeze target: code=%d err=%q", code, errOut)
	}
	code, _, errOut = runCapture(t, "circuit-copy", "--dir", dir, "--name", "c", "--version", "1", "--to-version", "2")
	if code != 1 || !strings.Contains(errOut, "conflict") {
		t.Fatalf("frozen target: code=%d err=%q", code, errOut)
	}
}
