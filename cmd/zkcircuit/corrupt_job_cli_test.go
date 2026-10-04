package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the CLI contract for a damaged compiled-artifact binding
// on a committed job record: a compiled_hash that is present but not a JSON
// string (null above all) is data corruption, not an unbound job. Every data
// command fails at open with exit code 3 (the existing read-failure code),
// prints no record, and leaves data.json byte-for-byte in place.

// seedBoundJobCLI drives the CLI through a legal create + import + freeze +
// setup + compile + bound job-submit and returns the artifact hash.
func seedBoundJobCLI(t *testing.T, dir string) string {
	t.Helper()
	defPath := writeFile(t, filepath.Dir(dir), "def.json", cliCorruptDef)
	must := func(what string, r cliResult) {
		t.Helper()
		if r.code != 0 {
			t.Fatalf("%s: code=%d err=%s", what, r.code, r.err)
		}
	}
	must("create", callCLI(t, "circuit-create", "--dir", dir, "--name", "mul", "--version", "1",
		"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1"))
	must("import", callCLI(t, "constraint-import", "--dir", dir, "--name", "mul", "--version", "1",
		"--file", defPath))
	must("freeze", callCLI(t, "circuit-freeze", "--dir", dir, "--name", "mul", "--version", "1"))
	must("setup", callCLI(t, "setup-record", "--dir", dir, "--name", "mul", "--version", "1"))
	compiled := callCLI(t, "circuit-compile", "--dir", dir, "--name", "mul", "--version", "1")
	must("compile", compiled)
	hash := ""
	for _, field := range strings.Fields(compiled.out) {
		if strings.HasPrefix(field, "hash=") {
			hash = strings.TrimPrefix(field, "hash=")
		}
	}
	if hash == "" {
		t.Fatalf("compile output carries no hash: %q", compiled.out)
	}
	must("submit", callCLI(t, "job-submit", "--dir", dir, "--id", "j1", "--name", "mul", "--version", "1",
		"--hash", hash))
	return hash
}

// corruptBoundJobCLI rewrites the committed j1 binding field to the given
// raw JSON value and returns the damaged bytes now on disk.
func corruptBoundJobCLI(t *testing.T, dir, hash, replacement string) []byte {
	t.Helper()
	path := filepath.Join(dir, "data.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	field := `"compiled_hash": "` + hash + `"`
	if !strings.Contains(string(raw), field) {
		t.Fatalf("committed file does not carry the bound field %q", field)
	}
	bad := []byte(strings.Replace(string(raw), field, `"compiled_hash": `+replacement, 1))
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	return bad
}

// TestCLICorruptJobCompiledHashExitCode: a non-string compiled_hash makes
// reads, submissions and unrelated writes all exit 3 without touching the
// file, and the read-failure message names the damaged job.
func TestCLICorruptJobCompiledHashExitCode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	hash := seedBoundJobCLI(t, dir)
	bad := corruptBoundJobCLI(t, dir, hash, "null")

	commands := [][]string{
		{"job-get", "--dir", dir, "--id", "j1"},
		{"job-list", "--dir", dir},
		{"job-submit", "--dir", dir, "--id", "j2", "--name", "mul", "--version", "1"},
		{"circuit-list", "--dir", dir},
		{"circuit-create", "--dir", dir, "--name", "other", "--version", "1"},
	}
	for _, args := range commands {
		r := callCLI(t, args...)
		if r.code != 3 {
			t.Fatalf("%s: want exit 3, got %d (out=%q err=%q)", args[0], r.code, r.out, r.err)
		}
		if r.out != "" {
			t.Fatalf("%s printed a record despite corruption: %q", args[0], r.out)
		}
		if !strings.Contains(r.err, "read failed") || !strings.Contains(r.err, "data corrupt") {
			t.Fatalf("%s: unexpected error text %q", args[0], r.err)
		}
	}

	// The failure names the job whose binding field is damaged.
	r := callCLI(t, "job-get", "--dir", dir, "--id", "j1")
	if !strings.Contains(r.err, `"j1"`) || !strings.Contains(r.err, "compiled_hash") {
		t.Fatalf("error does not name the damaged job and field: %q", r.err)
	}

	left, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(bad) {
		t.Fatalf("data.json changed after refused commands")
	}
}

// TestCLIJobCompiledHashShapes: the two legal expressions of the binding keep
// working at the CLI — an omitted field and an explicit empty string both
// read as unbound — while every other JSON type exits 3.
func TestCLIJobCompiledHashShapes(t *testing.T) {
	t.Run("explicit empty string stays unbound", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "bench")
		hash := seedBoundJobCLI(t, dir)
		corruptBoundJobCLI(t, dir, hash, `""`)

		r := callCLI(t, "job-get", "--dir", dir, "--id", "j1")
		if r.code != 0 {
			t.Fatalf("explicit empty binding must load: code=%d err=%s", r.code, r.err)
		}
		if strings.Contains(r.out, "compiled_hash") {
			t.Fatalf("explicit empty hash displayed as bound: %q", r.out)
		}
	})

	t.Run("non-string values are corrupt", func(t *testing.T) {
		for _, value := range []string{"5", "true", `["x"]`, `{"h":"x"}`} {
			dir := filepath.Join(t.TempDir(), "bench")
			hash := seedBoundJobCLI(t, dir)
			corruptBoundJobCLI(t, dir, hash, value)
			r := callCLI(t, "job-list", "--dir", dir)
			if r.code != 3 || !strings.Contains(r.err, "read failed") {
				t.Fatalf("compiled_hash=%s: want exit 3 read failure, got code=%d err=%q", value, r.code, r.err)
			}
		}
	})
}

// TestCLIJobCompiledHashDuplicateExitCode: a repeated compiled_hash key —
// including one repeated through a JSON escape spelling — exits 3.
func TestCLIJobCompiledHashDuplicateExitCode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	hash := seedBoundJobCLI(t, dir)
	path := filepath.Join(dir, "data.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	field := `"compiled_hash": "` + hash + `"`
	duplicate := field + `, "compiled\u005fhash": "` + hash + `"`
	bad := []byte(strings.Replace(string(raw), field, duplicate, 1))
	// Sanity: the damaged file really does name the key twice after JSON
	// unescaping, so the test exercises the duplicate path.
	var probe struct {
		Jobs []map[string]json.RawMessage `json:"jobs"`
	}
	if err := json.Unmarshal(bad, &probe); err != nil || len(probe.Jobs) != 1 {
		t.Fatalf("damaged file does not decode: %v", err)
	}
	if _, ok := probe.Jobs[0]["compiled_hash"]; !ok {
		t.Fatalf("escaped duplicate key did not decode to compiled_hash")
	}
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}

	r := callCLI(t, "job-list", "--dir", dir)
	if r.code != 3 || !strings.Contains(r.err, "read failed") || !strings.Contains(r.err, "compiled_hash") {
		t.Fatalf("duplicate compiled_hash: want exit 3 read failure naming the field, got code=%d err=%q", r.code, r.err)
	}
	left, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(bad) {
		t.Fatalf("data.json changed after refused command")
	}
}
