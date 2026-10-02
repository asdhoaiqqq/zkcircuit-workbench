package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLICircuitCopyLifecycle(t *testing.T) {
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

	// Create a draft with counts matching the definition, import, freeze.
	if r := call("circuit-create", "--dir", dir, "--name", "mul", "--version", "1",
		"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1",
		"--description", "original"); r.code != 0 {
		t.Fatalf("create: %+v", r)
	}
	if r := call("constraint-import", "--dir", dir, "--name", "mul", "--version", "1",
		"--file", defPath); r.code != 0 {
		t.Fatalf("import: %+v", r)
	}
	if r := call("circuit-freeze", "--dir", dir, "--name", "mul", "--version", "1"); r.code != 0 {
		t.Fatalf("freeze: %+v", r)
	}

	// Copy to a new draft version.
	r := call("circuit-copy", "--dir", dir, "--name", "mul", "--version", "1", "--to-version", "2")
	if r.code != 0 {
		t.Fatalf("copy: %+v", r)
	}
	if !strings.Contains(r.out, "circuit copied:") ||
		!strings.Contains(r.out, "name=mul") ||
		!strings.Contains(r.out, "version=2") ||
		!strings.Contains(r.out, "constraints=1") ||
		!strings.Contains(r.out, "public_inputs=1") ||
		!strings.Contains(r.out, "private_inputs=1") ||
		!strings.Contains(r.out, "frozen=false") ||
		!strings.Contains(r.out, `description="original"`) {
		t.Fatalf("copy output wrong: %q", r.out)
	}

	// The copied draft appears in list and get.
	if r := call("circuit-list", "--dir", dir); r.code != 0 ||
		!strings.Contains(r.out, "version=1") || !strings.Contains(r.out, "version=2") {
		t.Fatalf("list after copy: %+v", r)
	}
	if r := call("circuit-get", "--dir", dir, "--name", "mul", "--version", "2"); r.code != 0 ||
		!strings.Contains(r.out, "frozen=false") || !strings.Contains(r.out, `description="original"`) {
		t.Fatalf("get after copy: %+v", r)
	}

	// The copied draft can be updated, re-imported, frozen and compiled.
	if r := call("circuit-update", "--dir", dir, "--name", "mul", "--version", "2",
		"--description", "modified"); r.code != 0 || !strings.Contains(r.out, `description="modified"`) {
		t.Fatalf("update copied draft: %+v", r)
	}
	if r := call("circuit-freeze", "--dir", dir, "--name", "mul", "--version", "2"); r.code != 0 {
		t.Fatalf("freeze copied draft: %+v", r)
	}
	r2 := call("circuit-compile", "--dir", dir, "--name", "mul", "--version", "2")
	if r2.code != 0 || !strings.Contains(r2.out, "hash=") {
		t.Fatalf("compile copied draft: %+v", r2)
	}
	// The target's hash must differ from the source's hash.
	r1 := call("circuit-compile", "--dir", dir, "--name", "mul", "--version", "1")
	hash1 := strings.TrimSpace(strings.SplitN(strings.SplitAfter(r1.out, "hash=")[1], "\n", 2)[0])
	hash2 := strings.TrimSpace(strings.SplitN(strings.SplitAfter(r2.out, "hash=")[1], "\n", 2)[0])
	if hash1 == hash2 {
		t.Fatalf("source and target hashes must differ, both %s", hash1)
	}

	// The source is untouched.
	if r := call("circuit-get", "--dir", dir, "--name", "mul", "--version", "1"); r.code != 0 ||
		!strings.Contains(r.out, "frozen=true") || !strings.Contains(r.out, `description="original"`) {
		t.Fatalf("source changed after copy: %+v", r)
	}
}

func TestCLICircuitCopyErrors(t *testing.T) {
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

	// Missing --dir -> usage error.
	if r := call("circuit-copy", "--name", "c", "--version", "1", "--to-version", "2"); r.code != 2 {
		t.Fatalf("missing dir: code=%d err=%s", r.code, r.err)
	}
	// Missing --name -> usage error.
	if r := call("circuit-copy", "--dir", dir, "--version", "1", "--to-version", "2"); r.code != 2 {
		t.Fatalf("missing name: code=%d err=%s", r.code, r.err)
	}
	// Missing --version -> usage error.
	if r := call("circuit-copy", "--dir", dir, "--name", "c", "--to-version", "2"); r.code != 2 {
		t.Fatalf("missing version: code=%d err=%s", r.code, r.err)
	}
	// Missing --to-version -> usage error.
	if r := call("circuit-copy", "--dir", dir, "--name", "c", "--version", "1"); r.code != 2 {
		t.Fatalf("missing to-version: code=%d err=%s", r.code, r.err)
	}
	// Same version -> usage error.
	if r := call("circuit-copy", "--dir", dir, "--name", "c", "--version", "1", "--to-version", "1"); r.code != 2 {
		t.Fatalf("same version: code=%d err=%s", r.code, r.err)
	}
	// Zero to-version -> usage error.
	if r := call("circuit-copy", "--dir", dir, "--name", "c", "--version", "1", "--to-version", "0"); r.code != 2 {
		t.Fatalf("zero to-version: code=%d err=%s", r.code, r.err)
	}
	// Unknown source -> not found (exit 1).
	if r := call("circuit-copy", "--dir", dir, "--name", "ghost", "--version", "1", "--to-version", "2"); r.code != 1 ||
		!strings.Contains(r.err, "not found") {
		t.Fatalf("unknown source: code=%d err=%s", r.code, r.err)
	}

	// Create a draft (not frozen) source.
	if r := call("circuit-create", "--dir", dir, "--name", "c", "--version", "1",
		"--constraints", "1", "--description", "d"); r.code != 0 {
		t.Fatalf("create: %+v", r)
	}
	// Unfrozen source -> not frozen (exit 1).
	if r := call("circuit-copy", "--dir", dir, "--name", "c", "--version", "1", "--to-version", "2"); r.code != 1 ||
		!strings.Contains(r.err, "not frozen") {
		t.Fatalf("unfrozen source: code=%d err=%s", r.code, r.err)
	}

	// Freeze the source.
	if r := call("circuit-freeze", "--dir", dir, "--name", "c", "--version", "1"); r.code != 0 {
		t.Fatalf("freeze: %+v", r)
	}
	// First copy succeeds.
	if r := call("circuit-copy", "--dir", dir, "--name", "c", "--version", "1", "--to-version", "2"); r.code != 0 {
		t.Fatalf("first copy: %+v", r)
	}
	// Freeze the target.
	if r := call("circuit-freeze", "--dir", dir, "--name", "c", "--version", "2"); r.code != 0 {
		t.Fatalf("freeze target: %+v", r)
	}
	// Copy to a frozen target -> conflict (exit 1).
	if r := call("circuit-copy", "--dir", dir, "--name", "c", "--version", "1", "--to-version", "2"); r.code != 1 ||
		!strings.Contains(r.err, "conflict") {
		t.Fatalf("frozen target: code=%d err=%s", r.code, r.err)
	}
}

func TestCLICircuitCopyIdempotent(t *testing.T) {
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
		"--constraints", "1", "--description", "d"); r.code != 0 {
		t.Fatalf("create: %+v", r)
	}
	if r := call("circuit-freeze", "--dir", dir, "--name", "c", "--version", "1"); r.code != 0 {
		t.Fatalf("freeze: %+v", r)
	}

	// First copy.
	r1 := call("circuit-copy", "--dir", dir, "--name", "c", "--version", "1", "--to-version", "2")
	if r1.code != 0 {
		t.Fatalf("first copy: %+v", r1)
	}
	dataBefore, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Second identical copy: same output, no write.
	r2 := call("circuit-copy", "--dir", dir, "--name", "c", "--version", "1", "--to-version", "2")
	if r2.code != 0 {
		t.Fatalf("second copy: %+v", r2)
	}
	if r2.out != r1.out {
		t.Fatalf("idempotent copy output differs:\n first: %q\nsecond: %q", r1.out, r2.out)
	}
	dataAfter, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(dataBefore) != string(dataAfter) {
		t.Fatal("idempotent copy wrote to data.json")
	}

	// Change the target's description -> conflict on re-copy.
	if r := call("circuit-update", "--dir", dir, "--name", "c", "--version", "2",
		"--description", "changed"); r.code != 0 {
		t.Fatalf("update target: %+v", r)
	}
	if r := call("circuit-copy", "--dir", dir, "--name", "c", "--version", "1", "--to-version", "2"); r.code != 1 ||
		!strings.Contains(r.err, "conflict") {
		t.Fatalf("changed target: code=%d err=%s", r.code, r.err)
	}
}

func TestCLICircuitCopyPersistence(t *testing.T) {
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

	// Seed a frozen source with a definition.
	if r := call("circuit-create", "--dir", dir, "--name", "c", "--version", "1",
		"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1",
		"--description", "d"); r.code != 0 {
		t.Fatalf("create: %+v", r)
	}
	defPath := writeFile(t, t.TempDir(), "def.json", cliDef)
	if r := call("constraint-import", "--dir", dir, "--name", "c", "--version", "1",
		"--file", defPath); r.code != 0 {
		t.Fatalf("import: %+v", r)
	}
	if r := call("circuit-freeze", "--dir", dir, "--name", "c", "--version", "1"); r.code != 0 {
		t.Fatalf("freeze: %+v", r)
	}

	// Copy.
	if r := call("circuit-copy", "--dir", dir, "--name", "c", "--version", "1", "--to-version", "2"); r.code != 0 {
		t.Fatalf("copy: %+v", r)
	}

	// Reopen in a fresh invocation: the copied draft and its definition survive.
	if r := call("circuit-get", "--dir", dir, "--name", "c", "--version", "2"); r.code != 0 ||
		!strings.Contains(r.out, "frozen=false") || !strings.Contains(r.out, `description="d"`) {
		t.Fatalf("get after reopen: %+v", r)
	}

	// The copied draft can be frozen and compiled after reopen.
	if r := call("circuit-freeze", "--dir", dir, "--name", "c", "--version", "2"); r.code != 0 {
		t.Fatalf("freeze after reopen: %+v", r)
	}
	if r := call("circuit-compile", "--dir", dir, "--name", "c", "--version", "2"); r.code != 0 ||
		!strings.Contains(r.out, "hash=") {
		t.Fatalf("compile after reopen: %+v", r)
	}

	// The source is intact after reopen.
	if r := call("circuit-get", "--dir", dir, "--name", "c", "--version", "1"); r.code != 0 ||
		!strings.Contains(r.out, "frozen=true") {
		t.Fatalf("source after reopen: %+v", r)
	}
}
