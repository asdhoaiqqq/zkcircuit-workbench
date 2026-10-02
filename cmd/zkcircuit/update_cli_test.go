package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// cliRes is one command invocation result.
type cliRes struct {
	code int
	out  string
	err  string
}

// TestCLIUpdateIsPartial covers the draft circuit-update partial-modification
// rules end to end through the command line.
func TestCLIUpdateIsPartial(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	call := func(args ...string) cliRes {
		c, o, e := runCapture(t, args...)
		return cliRes{c, o, e}
	}

	// Seed: 2 constraints, 1 public, 3 private, description 初稿.
	if r := call("circuit-create", "--dir", dir, "--name", "c", "--version", "1",
		"--constraints", "2", "--public-inputs", "1", "--private-inputs", "3",
		"--description", "初稿"); r.code != 0 {
		t.Fatalf("seed create: %+v", r)
	}

	// Description-only update must not reset counts to the flag defaults
	// (8192 / 0 / 0).
	r := call("circuit-update", "--dir", dir, "--name", "c", "--version", "1",
		"--description", "修订")
	if r.code != 0 || !strings.Contains(r.out, "constraints=2") ||
		!strings.Contains(r.out, "public_inputs=1") ||
		!strings.Contains(r.out, "private_inputs=3") ||
		!strings.Contains(r.out, `description="修订"`) {
		t.Fatalf("description-only update: %+v", r)
	}

	// Explicit zero inputs clear them; omitted fields survive.
	r = call("circuit-update", "--dir", dir, "--name", "c", "--version", "1",
		"--public-inputs", "0", "--private-inputs", "0")
	if r.code != 0 || !strings.Contains(r.out, "public_inputs=0") ||
		!strings.Contains(r.out, "private_inputs=0") ||
		!strings.Contains(r.out, "constraints=2") ||
		!strings.Contains(r.out, `description="修订"`) {
		t.Fatalf("explicit zero inputs: %+v", r)
	}

	// Explicit empty description clears it.
	r = call("circuit-update", "--dir", dir, "--name", "c", "--version", "1",
		"--description", "")
	if r.code != 0 || !strings.Contains(r.out, `description=""`) ||
		!strings.Contains(r.out, "constraints=2") {
		t.Fatalf("empty description: %+v", r)
	}

	// No modifiable field: succeeds and returns the current record.
	r = call("circuit-update", "--dir", dir, "--name", "c", "--version", "1")
	if r.code != 0 || !strings.Contains(r.out, "constraints=2") ||
		!strings.Contains(r.out, "public_inputs=0") ||
		!strings.Contains(r.out, "private_inputs=0") ||
		!strings.Contains(r.out, `description=""`) {
		t.Fatalf("field-less update: %+v", r)
	}

	// An illegal supplied field fails with exit 1 and changes nothing.
	if r := call("circuit-update", "--dir", dir, "--name", "c", "--version", "1",
		"--constraints", "0"); r.code != 1 || !strings.Contains(r.err, "constraints") {
		t.Fatalf("zero constraints should fail: %+v", r)
	}
	if r := call("circuit-get", "--dir", dir, "--name", "c", "--version", "1"); r.code != 0 ||
		!strings.Contains(r.out, "constraints=2") {
		t.Fatalf("rejected update leaked: %+v", r)
	}

	// Unknown version and frozen version keep their usual verdicts.
	if r := call("circuit-update", "--dir", dir, "--name", "c", "--version", "9",
		"--description", "x"); r.code != 1 || !strings.Contains(r.err, "not found") {
		t.Fatalf("unknown version: %+v", r)
	}
	if r := call("circuit-freeze", "--dir", dir, "--name", "c", "--version", "1"); r.code != 0 {
		t.Fatalf("freeze: %+v", r)
	}
	if r := call("circuit-update", "--dir", dir, "--name", "c", "--version", "1",
		"--description", "late"); r.code != 1 || !strings.Contains(r.err, "frozen") {
		t.Fatalf("frozen update: %+v", r)
	}
	if r := call("circuit-update", "--dir", dir, "--name", "c", "--version", "1"); r.code != 1 ||
		!strings.Contains(r.err, "frozen") {
		t.Fatalf("field-less frozen update: %+v", r)
	}

	// The record visible after all operations matches the final state.
	r = call("circuit-get", "--dir", dir, "--name", "c", "--version", "1")
	if r.code != 0 || !strings.Contains(r.out, "constraints=2") ||
		!strings.Contains(r.out, "public_inputs=0") ||
		!strings.Contains(r.out, "private_inputs=0") ||
		!strings.Contains(r.out, `description=""`) ||
		!strings.Contains(r.out, "frozen=true") {
		t.Fatalf("final record wrong: %+v", r)
	}
}

// TestCLIUpdatePreservesSeparateFieldEditsAndDefinition checks that distinct
// partial operations accumulate and that a description-only edit is allowed
// on a draft that already imported a constraint definition.
func TestCLIUpdatePreservesSeparateFieldEditsAndDefinition(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "bench")
	defPath := writeFile(t, work, "def.json", cliDef) // 1 constraint, wires 1..2
	call := func(args ...string) cliRes {
		c, o, e := runCapture(t, args...)
		return cliRes{c, o, e}
	}

	if r := call("circuit-create", "--dir", dir, "--name", "mul", "--version", "1",
		"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1",
		"--description", "初稿"); r.code != 0 {
		t.Fatalf("create: %+v", r)
	}
	if r := call("constraint-import", "--dir", dir, "--name", "mul", "--version", "1",
		"--file", defPath); r.code != 0 {
		t.Fatalf("import: %+v", r)
	}

	// Description alone must not be rejected as an illegal count change even
	// though a definition is present, and must keep the counts at 1/1/1.
	r := call("circuit-update", "--dir", dir, "--name", "mul", "--version", "1",
		"--description", "仅改描述")
	if r.code != 0 || !strings.Contains(r.out, "constraints=1") ||
		!strings.Contains(r.out, "public_inputs=1") ||
		!strings.Contains(r.out, "private_inputs=1") ||
		!strings.Contains(r.out, `description="仅改描述"`) {
		t.Fatalf("description update with imported definition: %+v", r)
	}

	// Two separate field edits from separate invocations both survive.
	if r := call("circuit-update", "--dir", dir, "--name", "mul", "--version", "1",
		"--public-inputs", "2", "--private-inputs", "2"); r.code != 0 {
		t.Fatalf("input-count update: %+v", r)
	}
	r = call("circuit-get", "--dir", dir, "--name", "mul", "--version", "1")
	if r.code != 0 || !strings.Contains(r.out, "public_inputs=2") ||
		!strings.Contains(r.out, "private_inputs=2") ||
		!strings.Contains(r.out, `description="仅改描述"`) {
		t.Fatalf("separate edits did not accumulate: %+v", r)
	}

	// A count change that would invalidate the definition is refused (exit 1)
	// and the definition remains usable.
	if r := call("circuit-update", "--dir", dir, "--name", "mul", "--version", "1",
		"--constraints", "2"); r.code != 1 || !strings.Contains(r.err, "illegal") {
		t.Fatalf("definition-breaking update: %+v", r)
	}
	if r := call("circuit-get", "--dir", dir, "--name", "mul", "--version", "1"); r.code != 0 ||
		!strings.Contains(r.out, "constraints=1") {
		t.Fatalf("refused count change leaked: %+v", r)
	}

	// Reopen (fresh process invocation) and confirm persistence.
	r = call("circuit-get", "--dir", dir, "--name", "mul", "--version", "1")
	if r.code != 0 || !strings.Contains(r.out, "constraints=1") ||
		!strings.Contains(r.out, "public_inputs=2") ||
		!strings.Contains(r.out, "private_inputs=2") ||
		!strings.Contains(r.out, `description="仅改描述"`) {
		t.Fatalf("state after reopen inconsistent: %+v", r)
	}
}
