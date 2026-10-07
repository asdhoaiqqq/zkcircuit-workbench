package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLI contract for a committed job record whose circuit value would not read
// back unchanged: invalid UTF-8 bytes in the raw string token, or \uXXXX
// escapes forming an unpaired surrogate — under the canonical "circuit" key,
// an ASCII case variant, or a JSON-escaped spelling of either. The damage
// decodes to U+FFFD under encoding/json, so the job could read as owned by a
// circuit the saved bytes never named and borrow that circuit's frozen
// version, trusted setup and compiled artifact. Every data command fails at
// open with exit code 3, prints no record, names the damaged job and its
// circuit field (never a "circuit not found" misdiagnosis), and leaves
// data.json byte-for-byte in place.

// TestCLIJobCircuitEncodingCorruptExitCode: a damaged circuit token on j1 —
// invalid UTF-8 bytes or an unpaired surrogate escape, under any recognized
// spelling of the key, including damage whose U+FFFD-repaired form is a
// circuit that genuinely exists — makes every data command exit 3 with no
// output and leaves data.json byte-for-byte in place.
func TestCLIJobCircuitEncodingCorruptExitCode(t *testing.T) {
	escC := cliJSONEscape + "u0063ircuit"
	cases := []struct {
		name     string
		fragment string
	}{
		{"invalid UTF-8 byte repairs onto alpha itself", `"circuit": "alp` + "\xff" + `ha"`},
		{"lone high surrogate after legal prefix", `"circuit": "alp` + "ha" + cliJSONEscape + `uD800"`},
		{"lone low surrogate", `"circuit": "` + cliJSONEscape + `uDC00lpha"`},
		{"case variant key with damaged value", `"CIRCUIT": "alp` + "\xff" + `ha"`},
		{"escaped key with lone surrogate", `"` + escC + `": "alpha` + cliJSONEscape + `uD800"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			seedTwoCircuitJobCLI(t, dir)
			bad := rewriteJobCircuitCLI(t, dir, `"circuit": "alpha"`, tc.fragment)

			commands := [][]string{
				{"job-get", "--dir", dir, "--id", "j1"},
				{"job-list", "--dir", dir},
				{"job-submit", "--dir", dir, "--id", "j2", "--name", "alpha", "--version", "1"},
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
				if !strings.Contains(r.err, `"j1"`) || !strings.Contains(r.err, "circuit") {
					t.Fatalf("%s: error does not name the damaged job and circuit field: %q", args[0], r.err)
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

// TestCLIJobCircuitLegalUnicodeStillReads: a circuit whose name carries
// Chinese, an emoji or a genuinely committed "�" keeps its exact value
// through the CLI: create, freeze, setup, submit and job-get all agree on
// the name as written.
func TestCLIJobCircuitLegalUnicodeStillReads(t *testing.T) {
	for _, name := range []string{"电路", "emoji-😀", "has�mark"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			must := func(what string, r cliResult) {
				t.Helper()
				if r.code != 0 {
					t.Fatalf("%s: code=%d err=%s", what, r.code, r.err)
				}
			}
			must("create", callCLI(t, "circuit-create", "--dir", dir, "--name", name, "--version", "1",
				"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1"))
			must("freeze", callCLI(t, "circuit-freeze", "--dir", dir, "--name", name, "--version", "1"))
			must("setup", callCLI(t, "setup-record", "--dir", dir, "--name", name, "--version", "1"))
			must("submit", callCLI(t, "job-submit", "--dir", dir, "--id", "j1", "--name", name, "--version", "1"))

			r := callCLI(t, "job-get", "--dir", dir, "--id", "j1")
			if r.code != 0 {
				t.Fatalf("job-get: code=%d err=%s", r.code, r.err)
			}
			if !strings.Contains(r.out, "circuit="+name) {
				t.Fatalf("owning circuit did not read back as %q: %q", name, r.out)
			}
		})
	}
}
