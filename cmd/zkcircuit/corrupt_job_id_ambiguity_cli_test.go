package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the CLI contract for an ambiguous job identity on a
// committed record: a job carrying two spellings of its id field (canonical
// "id" plus an ASCII letter-case or JSON-escaped spelling, including a pair
// made only of variants) makes its query number a function of key order
// (encoding/json keeps the last value), so it is data corruption rather than
// a missing id or a submit conflict. Every data command fails at read with
// exit code 3, prints nothing, locates the record by its 1-based position and
// names the id field, and leaves data.json byte-for-byte in place.

// corruptJobIDCLI rewrites the committed j1 id member with the given raw JSON
// text and returns the damaged bytes now on disk.
func corruptJobIDCLI(t *testing.T, dir, replacement string) []byte {
	t.Helper()
	path := filepath.Join(dir, "data.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	field := `"id": "j1"`
	if !strings.Contains(string(raw), field) {
		t.Fatalf("committed file does not carry the job id field %q", field)
	}
	bad := []byte(strings.Replace(string(raw), field, replacement, 1))
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	return bad
}

// TestCLICorruptJobIDAmbiguityExitCode: two recognized id spellings make
// reads, submissions and unrelated writes all exit 3 without touching the
// file or printing a record, and the failure locates the job positionally
// instead of treating a candidate value as its settled identity.
func TestCLICorruptJobIDAmbiguityExitCode(t *testing.T) {
	cases := []struct {
		name        string
		replacement string
	}{
		{"canonical plus ID, differing values", `"id": "j1", "ID": "j2"`},
		{"canonical plus ID, same value", `"id": "j1", "ID": "j1"`},
		{"variant only, ID beside Id", `"ID": "j1", "Id": "j2"`},
		{"canonical plus JSON-escaped ID", "\"id\": \"j1\", \"\\u0049\\u0044\": \"j2\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			seedBoundJobCLI(t, dir)
			bad := corruptJobIDCLI(t, dir, tc.replacement)

			commands := [][]string{
				{"job-get", "--dir", dir, "--id", "j1"},
				{"job-list", "--dir", dir},
				{"job-submit", "--dir", dir, "--id", "j3", "--name", "mul", "--version", "1"},
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

			// The failure locates the record by its array position and names
			// the id field, without quoting a candidate id as settled.
			r := callCLI(t, "job-list", "--dir", dir)
			if !strings.Contains(r.err, "job record #1") {
				t.Fatalf("error does not locate the record positionally: %q", r.err)
			}
			if !strings.Contains(r.err, `"id"`) {
				t.Fatalf("error does not name the id field: %q", r.err)
			}
			if strings.Contains(r.err, `stored job record "j1"`) {
				t.Fatalf("error labels the ambiguous record by a candidate id: %q", r.err)
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
