package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the CLI contract for a damaged compiled-artifact binding
// on a stored job: if data.json carries compiled_hash as null (or another
// non-string type) every data command fails at read time with exit code 3
// (the existing read-failure code), prints no partial job output, accepts no
// new submission, and leaves data.json byte-for-byte in place — the anomaly
// must never be normalized away by a later unrelated write.

// seedBoundJobStore drives the CLI through a full frozen/setup/compiled
// circuit and submits a hash-bound job, returning the directory and hash.
func seedBoundJobStore(t *testing.T, dir string) string {
	t.Helper()
	work := filepath.Dir(dir)
	defPath := writeFile(t, work, "def.json", cliCorruptDef)
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
	for _, tok := range strings.Fields(compiled.out) {
		if strings.HasPrefix(tok, "hash=") {
			hash = strings.TrimPrefix(tok, "hash=")
		}
	}
	if hash == "" {
		t.Fatalf("could not read artifact hash from compile output %q", compiled.out)
	}
	must("job-submit", callCLI(t, "job-submit", "--dir", dir, "--id", "j-bound",
		"--name", "mul", "--version", "1", "--attempt", "1", "--hash", hash))
	return hash
}

func cliJob(env map[string]any, id string) map[string]any {
	for _, item := range env["jobs"].([]any) {
		job := item.(map[string]any)
		if job["id"] == id {
			return job
		}
	}
	return nil
}

// damageJobFile parses data.json, applies mutate to the named job record and
// writes the result back, returning the bytes now on disk.
func damageJobFile(t *testing.T, dir, id string, mutate func(map[string]any)) []byte {
	t.Helper()
	path := filepath.Join(dir, "data.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	job := cliJob(env, id)
	if job == nil {
		t.Fatalf("job %q not found", id)
	}
	mutate(job)
	bad, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	bad = append(bad, '\n')
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	return bad
}

// TestCLICorruptJobBindingExitCode drives each non-string compiled_hash shape
// through the CLI: reads fail, a new submission fails and an unrelated circuit
// write fails, all exit 3 with "read failed"/"data corrupt" and the job id,
// and the damaged file is never touched.
func TestCLICorruptJobBindingExitCode(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{"null", nil},
		{"number", float64(7)},
		{"boolean", true},
		{"array", []any{"x"}},
		{"object", map[string]any{"x": "y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			hash := seedBoundJobStore(t, dir)
			bad := damageJobFile(t, dir, "j-bound", func(job map[string]any) {
				job["compiled_hash"] = tc.value
			})

			commands := [][]string{
				{"job-get", "--dir", dir, "--id", "j-bound"},
				{"job-list", "--dir", dir},
				// A fresh, otherwise-valid submission must be refused at read.
				{"job-submit", "--dir", dir, "--id", "j-new", "--name", "mul",
					"--version", "1", "--attempt", "1", "--hash", hash},
				// An unrelated circuit change must not rewrite the directory.
				{"circuit-create", "--dir", dir, "--name", "other", "--version", "1",
					"--constraints", "1"},
			}
			for _, args := range commands {
				r := callCLI(t, args...)
				if r.code != 3 {
					t.Fatalf("%s: want exit 3, got %d (out=%q err=%q)", args[0], r.code, r.out, r.err)
				}
				if r.out != "" {
					t.Fatalf("%s printed output despite corruption: %q", args[0], r.out)
				}
				if !strings.Contains(r.err, "read failed") ||
					!strings.Contains(r.err, "data corrupt") ||
					!strings.Contains(r.err, "j-bound") {
					t.Fatalf("%s: unexpected error text %q", args[0], r.err)
				}
			}

			left, err := os.ReadFile(filepath.Join(dir, "data.json"))
			if err != nil {
				t.Fatal(err)
			}
			if string(left) != string(bad) {
				t.Fatalf("data.json changed after refused commands")
			}
		})
	}
}

// TestCLIExplicitEmptyJobBindingStaysUnbound keeps the two legal unbound
// encodings working at the CLI: a register-only job omits compiled_hash and a
// hand-written explicit "" also loads, both shown without compiled_hash.
func TestCLIExplicitEmptyJobBindingStaysUnbound(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	seedBoundJobStore(t, dir)
	// Add a register-only job through the CLI: it omits compiled_hash.
	if r := callCLI(t, "job-submit", "--dir", dir, "--id", "j-loose",
		"--name", "mul", "--version", "1", "--attempt", "1"); r.code != 0 {
		t.Fatalf("register-only submit: code=%d err=%s", r.code, r.err)
	}
	// Rewrite its record with an explicit empty compiled_hash: still legal.
	damageJobFile(t, dir, "j-loose", func(job map[string]any) {
		job["compiled_hash"] = ""
	})

	r := callCLI(t, "job-get", "--dir", dir, "--id", "j-loose")
	if r.code != 0 {
		t.Fatalf("explicit empty binding must load: code=%d err=%s", r.code, r.err)
	}
	if strings.Contains(r.out, "compiled_hash") {
		t.Fatalf("unbound job displays a binding: %q", r.out)
	}
	// The bound job still reports its exact hash.
	rb := callCLI(t, "job-get", "--dir", dir, "--id", "j-bound")
	if rb.code != 0 || !strings.Contains(rb.out, "compiled_hash=") {
		t.Fatalf("bound job lost its hash: code=%d out=%q err=%q", rb.code, rb.out, rb.err)
	}
}
