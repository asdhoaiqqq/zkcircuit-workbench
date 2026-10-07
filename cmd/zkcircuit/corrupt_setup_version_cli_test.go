package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLI contract for an ambiguous version on a committed trusted-setup record.
// Every spelling an ordinary read would take as the setup's version —
// canonical "version", an ASCII letter-case variant, a JSON-escaped spelling,
// or the Unicode-folded long-s "verſion" (U+017F) — is one version field.
// Two of them on one record make the covered version depend on key order, so
// every data command fails at open with exit code 3, prints no record, names
// the damaged setup record and the version field, and leaves data.json
// byte-for-byte in place. A single recognized spelling (ASCII or long-s)
// still reads.

// seedTwoSetupCLI drives the CLI through a legal create + freeze + setup for
// two versions of circuit c, so a setup record spelling version both 1 and 2
// has a legal frozen target under either value.
func seedTwoSetupCLI(t *testing.T, dir string) {
	t.Helper()
	must := func(what string, r cliResult) {
		t.Helper()
		if r.code != 0 {
			t.Fatalf("%s: code=%d err=%s", what, r.code, r.err)
		}
	}
	for _, v := range []string{"1", "2"} {
		must("create v"+v, callCLI(t, "circuit-create", "--dir", dir, "--name", "c", "--version", v,
			"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1"))
		must("freeze v"+v, callCLI(t, "circuit-freeze", "--dir", dir, "--name", "c", "--version", v))
		must("setup v"+v, callCLI(t, "setup-record", "--dir", dir, "--name", "c", "--version", v))
	}
}

// rewriteSetupVersionCLI replaces the first old occurring after the "setups"
// marker — the first setup record is c@1, since the array is committed sorted
// — so the shared circuit "version" fields are never touched. It returns the
// bytes now on disk.
func rewriteSetupVersionCLI(t *testing.T, dir, old, fragment string) []byte {
	t.Helper()
	path := filepath.Join(dir, "data.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	marker := `"setups": [`
	at := strings.Index(string(raw), marker)
	if at < 0 {
		t.Fatalf("committed file does not carry the setups marker %q", marker)
	}
	rel := strings.Index(string(raw[at:]), old)
	if rel < 0 {
		t.Fatalf("first setup record does not carry %q after the setups marker", old)
	}
	pos := at + rel
	bad := []byte(string(raw[:pos]) + fragment + string(raw[pos+len(old):]))
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	return bad
}

// TestCLISetupVersionAmbiguousExitCode: two recognized version spellings on
// the c@1 setup record — ASCII variants, JSON-escaped spellings, or a long-s
// spelling beside a recognized one, equal or differing values — make every
// data command exit 3 with no output and leave data.json byte-for-byte in
// place.
func TestCLISetupVersionAmbiguousExitCode(t *testing.T) {
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
			seedTwoSetupCLI(t, dir)
			bad := rewriteSetupVersionCLI(t, dir, `"version": 1`, tc.fragment)

			commands := [][]string{
				{"setup-get", "--dir", dir, "--name", "c", "--version", "1"},
				{"setup-get", "--dir", dir, "--name", "c", "--version", "2"},
				{"setup-record", "--dir", dir, "--name", "c", "--version", "2"},
				{"job-submit", "--dir", dir, "--id", "j1", "--name", "c", "--version", "2"},
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
				if !strings.Contains(r.err, "setup record #1") || !strings.Contains(r.err, "version") {
					t.Fatalf("%s: error does not name the damaged setup record and version field: %q", args[0], r.err)
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

// TestCLISetupVersionSingleSpellingStillReads: one recognized spelling of
// version, ASCII variant or a long-s fold, keeps the c@1 setup registered
// and readable.
func TestCLISetupVersionSingleSpellingStillReads(t *testing.T) {
	directLongS := "ver" + cliLongS + "ion"
	escLongS := "ver" + cliJSONEscape + "u017f" + "ion"
	escS := "ver" + cliJSONEscape + "u0073" + "ion"
	for _, tc := range []struct {
		name     string
		fragment string
	}{
		{"upper case", `"VERSION": 1`},
		{"mixed case", `"VeRsIoN": 1`},
		{"escaped letter", `"` + escS + `": 1`},
		{"direct long s", `"` + directLongS + `": 1`},
		{"escaped long s", `"` + escLongS + `": 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			seedTwoSetupCLI(t, dir)
			rewriteSetupVersionCLI(t, dir, `"version": 1`, tc.fragment)

			r := callCLI(t, "setup-get", "--dir", dir, "--name", "c", "--version", "1")
			if r.code != 0 {
				t.Fatalf("single spelling %q must read: code=%d err=%q", tc.name, r.code, r.err)
			}
			if !strings.Contains(r.out, "name=c version=1") {
				t.Fatalf("single spelling %q read the wrong setup: %q", tc.name, r.out)
			}
		})
	}
}
