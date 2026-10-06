package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLI contract for an ambiguous identity id on a committed job record. Every
// spelling an ordinary read would take as the job's id — canonical "id", an
// ASCII letter-case variant ("ID", "Id", "iD"), or a JSON-escaped spelling
// of either — is one id field. Two of them on one record make the job's
// identity depend on key order, so every data command fails at open with
// exit code 3, prints no record, locates the record by its 1-based position
// and names the id field without quoting a candidate id, and leaves
// data.json byte-for-byte in place. A single recognized spelling still
// reads.

// seedJobCLI drives the CLI through a legal create + freeze + setup sequence
// for circuit c and one unbound job-submit j1.
func seedJobCLI(t *testing.T, dir string) {
	t.Helper()
	must := func(what string, r cliResult) {
		t.Helper()
		if r.code != 0 {
			t.Fatalf("%s: code=%d err=%s", what, r.code, r.err)
		}
	}
	must("create", callCLI(t, "circuit-create", "--dir", dir, "--name", "c", "--version", "1",
		"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1"))
	must("freeze", callCLI(t, "circuit-freeze", "--dir", dir, "--name", "c", "--version", "1"))
	must("setup", callCLI(t, "setup-record", "--dir", dir, "--name", "c", "--version", "1"))
	must("submit", callCLI(t, "job-submit", "--dir", dir, "--id", "j1", "--name", "c", "--version", "1"))
}

// TestCLIJobIDAmbiguousExitCode: two recognized id spellings on the j1
// record — ASCII variants or JSON-escaped spellings, equal or differing
// values, in either key order, with or without a canonical "id" among them —
// make every data command exit 3 with no output and leave data.json
// byte-for-byte in place. The failure locates the record positionally and
// never quotes a candidate id as the job's identity.
func TestCLIJobIDAmbiguousExitCode(t *testing.T) {
	escID := cliJSONEscape + `u0049` + cliJSONEscape + `u0044`
	cases := []struct {
		name     string
		fragment string
	}{
		{"canonical plus upper, same value", `"id": "j1", "ID": "j1"`},
		{"canonical plus upper, differing", `"id": "j1", "ID": "j2"`},
		{"upper then canonical", `"ID": "j2", "id": "j1"`},
		{"mixed case Id", `"id": "j1", "Id": "j1"`},
		{"mixed case iD, differing", `"id": "j1", "iD": "j2"`},
		{"case variants only, no canonical", `"ID": "j1", "Id": "j1"`},
		{"escaped upper", `"id": "j1", "` + escID + `": "j1"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			seedJobCLI(t, dir)
			bad := rewriteJobCircuitCLI(t, dir, `"id": "j1"`, tc.fragment)

			commands := [][]string{
				{"job-get", "--dir", dir, "--id", "j1"},
				{"job-list", "--dir", dir},
				{"job-submit", "--dir", dir, "--id", "j3", "--name", "c", "--version", "1"},
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
				if !strings.Contains(r.err, "job record #1") || !strings.Contains(r.err, `"id"`) {
					t.Fatalf("%s: error does not locate the record and name the id field: %q", args[0], r.err)
				}
				if strings.Contains(r.err, `"j1"`) || strings.Contains(r.err, `"j2"`) {
					t.Fatalf("%s: error quotes a candidate id as the job's identity: %q", args[0], r.err)
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

// TestCLIJobIDSingleSpellingStillReads: one recognized spelling of id,
// canonical or an ASCII case variant, direct or JSON-escaped, keeps the job
// readable under its one id.
func TestCLIJobIDSingleSpellingStillReads(t *testing.T) {
	escID := cliJSONEscape + `u0049` + cliJSONEscape + `u0044`
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"upper case", "ID"},
		{"mixed case Id", "Id"},
		{"mixed case iD", "iD"},
		{"escaped upper", escID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			seedJobCLI(t, dir)
			rewriteJobCircuitCLI(t, dir, `"id": "j1"`, `"`+tc.key+`": "j1"`)

			r := callCLI(t, "job-get", "--dir", dir, "--id", "j1")
			if r.code != 0 {
				t.Fatalf("single spelling %q must load: code=%d err=%s", tc.key, r.code, r.err)
			}
			if !strings.Contains(r.out, "id=j1") || !strings.Contains(r.out, "circuit=c") {
				t.Fatalf("single spelling %q not read as job j1 of circuit c: %q", tc.key, r.out)
			}
		})
	}
}
