package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the CLI contract for a committed job record that names
// its owning circuit under two recognized spellings (canonical "circuit"
// plus an ASCII case variant or a JSON-escaped spelling). The owning circuit
// must never be chosen by key order, so every data command fails at read
// time with exit code 3, names the job and the circuit field, prints no
// record, and leaves data.json byte-for-byte in place.

// corruptJobCircuitCLI rewrites the committed j1 record so its circuit field
// appears under both the canonical spelling and extra (a raw `"key": value`
// member), returning the damaged bytes now on disk.
func corruptJobCircuitCLI(t *testing.T, dir, extra string) []byte {
	t.Helper()
	path := filepath.Join(dir, "data.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	field := `"circuit": "mul"`
	if !strings.Contains(string(raw), field) {
		t.Fatalf("committed file does not carry the circuit field %q", field)
	}
	bad := []byte(strings.Replace(string(raw), field, field+`, `+extra, 1))
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	return bad
}

// TestCLIJobCircuitDuplicateExitCode: a second recognized circuit spelling —
// an ASCII case variant or a JSON-escaped spelling, even with a byte-identical
// value — makes reads, submissions and unrelated writes all exit 3 without
// touching the file, and the read-failure message names the job and field.
func TestCLIJobCircuitDuplicateExitCode(t *testing.T) {
	cases := []struct {
		name  string
		extra string
	}{
		{"ASCII variant, same value", `"CIRCUIT": "mul"`},
		{"mixed case variant, same value", `"CiRcUiT": "mul"`},
		{"escaped key spelling", `"circui\u0074": "mul"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			seedBoundJobCLI(t, dir)
			bad := corruptJobCircuitCLI(t, dir, tc.extra)

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

			// The failure names the job whose circuit field is duplicated.
			r := callCLI(t, "job-get", "--dir", dir, "--id", "j1")
			if !strings.Contains(r.err, `"j1"`) || !strings.Contains(r.err, "circuit") {
				t.Fatalf("error does not name the damaged job and field: %q", r.err)
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

// TestCLIJobCircuitSingleSpellingLoads: a job record carrying its circuit
// under one non-canonical but recognized spelling still loads and queries
// normally.
func TestCLIJobCircuitSingleSpellingLoads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	seedBoundJobCLI(t, dir)
	path := filepath.Join(dir, "data.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	field := `"circuit": "mul"`
	if !strings.Contains(string(raw), field) {
		t.Fatalf("committed file does not carry the circuit field %q", field)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), field, `"CIRCUIT": "mul"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	r := callCLI(t, "job-get", "--dir", dir, "--id", "j1")
	if r.code != 0 {
		t.Fatalf("single CIRCUIT spelling must load: code=%d err=%s", r.code, r.err)
	}
	if !strings.Contains(r.out, "circuit=mul") {
		t.Fatalf("job did not read back against circuit mul: %q", r.out)
	}
}
