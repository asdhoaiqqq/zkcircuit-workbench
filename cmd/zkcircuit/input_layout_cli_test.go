package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLI contract for the input-layout ceiling: declaring public+private+1
// slots beyond the platform int is a rule failure (exit 1) naming the input
// layout, prints no success record and saves nothing; a committed record
// already carrying such a layout fails every read with exit 3.

func TestCLICreateRejectsUnrepresentableInputLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	max := fmt.Sprintf("%d", math.MaxInt)

	code, out, errOut := runCapture(t, "circuit-create", "--dir", dir,
		"--name", "c", "--version", "1", "--constraints", "1",
		"--public-inputs", max, "--private-inputs", "1")
	if code != 1 {
		t.Fatalf("overflowing layout: code=%d out=%q err=%q", code, out, errOut)
	}
	if !strings.Contains(errOut, "input layout") {
		t.Fatalf("error should name the input layout: %q", errOut)
	}
	if strings.Contains(out, "circuit version:") {
		t.Fatalf("rejected create printed a success record: %q", out)
	}
	// Nothing was saved — not even a wrapped-around smaller layout.
	code, out, _ = runCapture(t, "circuit-list", "--dir", dir)
	if code != 0 || strings.Contains(out, "circuit:") {
		t.Fatalf("rejected create left a record behind: code=%d out=%q", code, out)
	}

	// The largest representable layout still works from the command line.
	maxMinusOne := fmt.Sprintf("%d", math.MaxInt-1)
	code, out, _ = runCapture(t, "circuit-create", "--dir", dir,
		"--name", "edge", "--version", "1", "--constraints", "1",
		"--public-inputs", maxMinusOne, "--private-inputs", "0")
	if code != 0 || !strings.Contains(out, "public_inputs="+maxMinusOne) {
		t.Fatalf("boundary create: code=%d out=%q", code, out)
	}
	code, out, _ = runCapture(t, "circuit-get", "--dir", dir,
		"--name", "edge", "--version", "1")
	if code != 0 || !strings.Contains(out, "public_inputs="+maxMinusOne) {
		t.Fatalf("boundary get: code=%d out=%q", code, out)
	}
}

func TestCLIUpdateRejectsUnrepresentableInputLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	max := fmt.Sprintf("%d", math.MaxInt)

	if code, _, _ := runCapture(t, "circuit-create", "--dir", dir,
		"--name", "c", "--version", "1", "--constraints", "1",
		"--public-inputs", "0", "--private-inputs", "1",
		"--description", "初稿"); code != 0 {
		t.Fatal("seed create")
	}
	// Only the public count is given; the kept private count still counts
	// toward the layout, and the description riding along is refused too.
	code, out, errOut := runCapture(t, "circuit-update", "--dir", dir,
		"--name", "c", "--version", "1",
		"--public-inputs", max, "--description", "改后")
	if code != 1 || !strings.Contains(errOut, "input layout") {
		t.Fatalf("overflowing update: code=%d out=%q err=%q", code, out, errOut)
	}
	code, out, _ = runCapture(t, "circuit-get", "--dir", dir,
		"--name", "c", "--version", "1")
	if code != 0 || !strings.Contains(out, `description="初稿"`) ||
		!strings.Contains(out, "public_inputs=0") {
		t.Fatalf("rejected update leaked into the record: code=%d out=%q", code, out)
	}
}

func TestCLIReadRejectsUnrepresentableInputLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")

	if code, _, _ := runCapture(t, "circuit-create", "--dir", dir,
		"--name", "c", "--version", "1", "--constraints", "1",
		"--public-inputs", "1", "--private-inputs", "1"); code != 0 {
		t.Fatal("seed create")
	}
	// Damage the committed record into an unrepresentable layout.
	path := filepath.Join(dir, "data.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(raw), `"public_inputs": 1`,
		fmt.Sprintf(`"public_inputs": %d`, math.MaxInt), 1)
	if bad == string(raw) {
		t.Fatal("seed file did not contain the expected public_inputs field")
	}
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"circuit-list", "--dir", dir},
		{"circuit-get", "--dir", dir, "--name", "c", "--version", "1"},
		{"job-list", "--dir", dir},
	} {
		code, out, errOut := runCapture(t, args...)
		if code != 3 || !strings.Contains(errOut, "read failed") {
			t.Fatalf("%v: code=%d out=%q err=%q", args, code, out, errOut)
		}
		if out != "" {
			t.Fatalf("%v: partial query output on corrupt data: %q", args, out)
		}
	}
	// The original file is left in place, not shrunk or rewritten.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != bad {
		t.Fatal("corrupt data file was modified")
	}
}
