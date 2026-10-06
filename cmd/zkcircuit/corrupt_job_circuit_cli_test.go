package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLI contract for an ambiguous owning circuit on a committed job record.
// Every spelling an ordinary read would take as the job's circuit —
// canonical "circuit", an ASCII letter-case variant such as "CIRCUIT" or
// "CiRcUiT", or a JSON-escaped spelling of either — is one circuit field.
// Two of them on one record make the owning circuit depend on key order, so
// every data command fails at open with exit code 3, prints no record,
// names the damaged job and its circuit field, and leaves data.json
// byte-for-byte in place. A single recognized spelling still reads.

// seedTwoCircuitJobCLI drives the CLI through two legal create + freeze +
// setup sequences (alpha and beta) and one unbound job-submit owned by
// alpha, reproducing the ambiguity that matters: j1 spelled against either
// circuit has a valid owner.
func seedTwoCircuitJobCLI(t *testing.T, dir string) {
	t.Helper()
	must := func(what string, r cliResult) {
		t.Helper()
		if r.code != 0 {
			t.Fatalf("%s: code=%d err=%s", what, r.code, r.err)
		}
	}
	for _, name := range []string{"alpha", "beta"} {
		must("create "+name, callCLI(t, "circuit-create", "--dir", dir, "--name", name, "--version", "1",
			"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1"))
		must("freeze "+name, callCLI(t, "circuit-freeze", "--dir", dir, "--name", name, "--version", "1"))
		must("setup "+name, callCLI(t, "setup-record", "--dir", dir, "--name", name, "--version", "1"))
	}
	must("submit", callCLI(t, "job-submit", "--dir", dir, "--id", "j1", "--name", "alpha", "--version", "1"))
}

// rewriteJobCircuitCLI scopes a literal replacement to the committed j1 job:
// it replaces the first old occurring after the j1 id marker, so no other
// record's fields are ever touched. It returns the bytes now on disk.
func rewriteJobCircuitCLI(t *testing.T, dir, old, fragment string) []byte {
	t.Helper()
	path := filepath.Join(dir, "data.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	marker := `"id": "j1"`
	at := strings.Index(string(raw), marker)
	if at < 0 {
		t.Fatalf("committed file does not carry the j1 marker %q", marker)
	}
	rel := strings.Index(string(raw[at:]), old)
	if rel < 0 {
		t.Fatalf("j1 job does not carry %q after its id marker", old)
	}
	pos := at + rel
	bad := []byte(string(raw[:pos]) + fragment + string(raw[pos+len(old):]))
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	return bad
}

// TestCLIJobCircuitAmbiguousExitCode: two recognized circuit spellings on j1
// — ASCII variants or JSON-escaped spellings, equal or differing values, in
// either key order — make every data command exit 3 with no output and leave
// data.json byte-for-byte in place.
func TestCLIJobCircuitAmbiguousExitCode(t *testing.T) {
	escC := cliJSONEscape + "u0063ircuit"
	cases := []struct {
		name     string
		fragment string
	}{
		{"canonical plus upper, same value", `"circuit": "alpha", "CIRCUIT": "alpha"`},
		{"canonical plus upper, differing", `"circuit": "alpha", "CIRCUIT": "beta"`},
		{"upper then canonical", `"CIRCUIT": "beta", "circuit": "alpha"`},
		{"mixed case", `"circuit": "alpha", "CiRcUiT": "alpha"`},
		{"escaped exact spelling", `"circuit": "alpha", "` + escC + `": "alpha"`},
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

// TestCLIJobCircuitSingleSpellingStillReads: one recognized spelling of
// circuit, canonical or an ASCII case variant, direct or JSON-escaped, keeps
// the job owned by alpha.
func TestCLIJobCircuitSingleSpellingStillReads(t *testing.T) {
	escC := cliJSONEscape + "u0063ircuit"
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"upper case", "CIRCUIT"},
		{"mixed case", "CiRcUiT"},
		{"escaped letter", escC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			seedTwoCircuitJobCLI(t, dir)
			rewriteJobCircuitCLI(t, dir, `"circuit": "alpha"`, `"`+tc.key+`": "alpha"`)

			r := callCLI(t, "job-get", "--dir", dir, "--id", "j1")
			if r.code != 0 {
				t.Fatalf("single spelling %q must load: code=%d err=%s", tc.key, r.code, r.err)
			}
			if !strings.Contains(r.out, "circuit=alpha") || !strings.Contains(r.out, "version=1") {
				t.Fatalf("single spelling %q not read as owned by alpha v1: %q", tc.key, r.out)
			}
		})
	}
}
