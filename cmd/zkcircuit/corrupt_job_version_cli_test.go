package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLI contract for an ambiguous pinned version on a committed job record.
// Every spelling an ordinary read would take as the job's version — canonical
// "version", an ASCII letter-case variant, a JSON-escaped spelling, or the
// Unicode-folded long-s "verſion" (U+017F) — is one version field. Two of
// them on one record make the bound version depend on key order, so every
// data command fails at open with exit code 3, prints no record, names the
// damaged job and its version field, and leaves data.json byte-for-byte in
// place. A single recognized spelling (ASCII or long-s) still reads.

// rewriteJobVersionCLI scopes a literal replacement to the committed j1 job:
// it replaces the first old occurring after the j1 id marker, so the shared
// circuit/setup/artifact "version" fields are never touched. It returns the
// bytes now on disk.
func rewriteJobVersionCLI(t *testing.T, dir, old, fragment string) []byte {
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

// TestCLIJobVersionAmbiguousExitCode: two recognized version spellings on j1 —
// ASCII variants, JSON-escaped spellings, or a long-s spelling beside a
// recognized one, equal or differing values — make every data command exit 3
// with no output and leave data.json byte-for-byte in place.
func TestCLIJobVersionAmbiguousExitCode(t *testing.T) {
	direct := "ver" + cliLongS + "ion"
	escapedLongS := "ver" + cliJSONEscape + "u017f" + "ion"
	escV := cliJSONEscape + "u0076ersion"
	cases := []struct {
		name     string
		fragment string
	}{
		{"canonical plus upper, same value", `"version": 1, "VERSION": 1`},
		{"canonical plus upper, differing", `"version": 1, "VERSION": 2`},
		{"upper then canonical", `"VERSION": 2, "version": 1`},
		{"mixed case", `"version": 1, "VeRsIoN": 1`},
		{"escaped exact spelling", `"version": 1, "` + escV + `": 1`},
		{"canonical plus direct long s", `"version": 1, "` + direct + `": 1`},
		{"direct long s then canonical, differing", `"` + direct + `": 2, "version": 1`},
		{"canonical plus escaped long s", `"version": 1, "` + escapedLongS + `": 1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			hash := seedBoundJobCLI(t, dir)
			bad := rewriteJobVersionCLI(t, dir, `"version": 1`, tc.fragment)

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
				if !strings.Contains(r.err, `"j1"`) || !strings.Contains(r.err, "version") {
					t.Fatalf("%s: error does not name the damaged job and version field: %q", args[0], r.err)
				}
			}

			// The compiled hash the job carries is still on disk verbatim.
			if !strings.Contains(string(bad), `"compiled_hash": "`+hash+`"`) {
				t.Fatalf("damaged fragment lost the bound compiled hash")
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

// TestCLIJobVersionSingleASCIISpellingStillReads: one recognized spelling of
// version, ASCII variant or a long-s fold, keeps the job pinned to v1 and
// preserves its compiled hash.
func TestCLIJobVersionSingleSpellingStillReads(t *testing.T) {
	directLongS := "ver" + cliLongS + "ion"
	escLongS := "ver" + cliJSONEscape + "u017f" + "ion"
	escS := "ver" + cliJSONEscape + "u0073" + "ion"
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"upper case", "VERSION"},
		{"mixed case", "VeRsIoN"},
		{"escaped letter", escS},
		{"direct long s", directLongS},
		{"escaped long s", escLongS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			hash := seedBoundJobCLI(t, dir)
			rewriteJobVersionCLI(t, dir, `"version": 1`, `"`+tc.key+`": 1`)

			r := callCLI(t, "job-get", "--dir", dir, "--id", "j1")
			if r.code != 0 {
				t.Fatalf("single spelling %q must load: code=%d err=%s", tc.key, r.code, r.err)
			}
			if !strings.Contains(r.out, "version=1") || !strings.Contains(r.out, "compiled_hash="+hash) {
				t.Fatalf("single spelling %q not read as v1 with binding: %q", tc.key, r.out)
			}
		})
	}
}
