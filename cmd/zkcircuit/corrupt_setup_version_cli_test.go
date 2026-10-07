package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLI contract for an ambiguous circuit version on a committed trusted-setup
// record. A setup belongs exclusively to the frozen circuit version named in
// the record; every spelling an ordinary read would take as the setup's
// version — canonical "version", an ASCII letter-case variant, a JSON-escaped
// spelling, or the Unicode-folded long-s "verſion" (U+017F) — is one version
// field. Two of them on one record make the version depend on key order, so
// every data command fails at open with exit code 3, prints no record, names
// the damaged setup record and its version field, and leaves data.json
// byte-for-byte in place. A single recognized spelling (ASCII or long-s) still
// reads, and the other version's proof job still reports a missing setup.

// seedTwoFrozenSetupCLI drives the CLI through two frozen versions of circuit
// "mul" with a trusted setup recorded on v1 only (v2 deliberately unset).
func seedTwoFrozenSetupCLI(t *testing.T, dir string) {
	t.Helper()
	must := func(what string, r cliResult) {
		t.Helper()
		if r.code != 0 {
			t.Fatalf("%s: code=%d err=%s", what, r.code, r.err)
		}
	}
	for _, v := range []string{"1", "2"} {
		must("create "+v, callCLI(t, "circuit-create", "--dir", dir, "--name", "mul",
			"--version", v, "--constraints", "1"))
		must("freeze "+v, callCLI(t, "circuit-freeze", "--dir", dir, "--name", "mul", "--version", v))
	}
	must("setup v1", callCLI(t, "setup-record", "--dir", dir, "--name", "mul", "--version", "1"))
}

// rewriteSetupVersionCLI scopes a literal replacement to the committed setups
// array: it replaces the first old occurring after the "setups" marker, so the
// shared circuit "version" fields are never touched. It returns the bytes now
// on disk.
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
		t.Fatalf("committed file does not carry the setups array")
	}
	rel := strings.Index(string(raw[at:]), old)
	if rel < 0 {
		t.Fatalf("setups array does not carry %q after its marker", old)
	}
	pos := at + rel
	bad := []byte(string(raw[:pos]) + fragment + string(raw[pos+len(old):]))
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	return bad
}

// TestCLISetupVersionAmbiguousExitCode: two recognized version spellings on
// the v1 setup record — ASCII variants, JSON-escaped spellings, or a long-s
// spelling beside a recognized one, equal or differing values — make every
// data command exit 3 with no output and leave data.json byte-for-byte in
// place.
func TestCLISetupVersionAmbiguousExitCode(t *testing.T) {
	direct := "ver" + cliLongS + "ion"
	escapedLongS := "ver" + cliJSONEscape + "u017f" + "ion"
	escV := cliJSONEscape + "u0076ersion"
	escUpperV := cliJSONEscape + "u0056ERSION"
	cases := []struct {
		name     string
		fragment string
	}{
		{"canonical plus upper, same value", `"version": 1, "VERSION": 1`},
		{"canonical plus upper, differing", `"version": 1, "VERSION": 2`},
		{"upper then canonical", `"VERSION": 2, "version": 1`},
		{"two non-canonical variants", `"VERSION": 2, "Version": 1`},
		{"mixed case", `"version": 1, "VeRsIoN": 1`},
		{"escaped exact spelling", `"version": 1, "` + escV + `": 1`},
		{"escaped uppercase spelling", `"version": 1, "` + escUpperV + `": 2`},
		{"canonical plus direct long s", `"version": 1, "` + direct + `": 1`},
		{"direct long s then canonical, differing", `"` + direct + `": 2, "version": 1`},
		{"canonical plus escaped long s", `"version": 1, "` + escapedLongS + `": 1`},
		{"byte-identical duplicate", `"version": 1, "version": 1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			seedTwoFrozenSetupCLI(t, dir)
			bad := rewriteSetupVersionCLI(t, dir, `"version": 1`, tc.fragment)

			commands := [][]string{
				{"setup-get", "--dir", dir, "--name", "mul", "--version", "1"},
				{"setup-record", "--dir", dir, "--name", "mul", "--version", "2"},
				{"job-submit", "--dir", dir, "--id", "j2", "--name", "mul", "--version", "2"},
				{"job-list", "--dir", dir},
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

// TestCLISetupVersionDuplicateInterspersed: the two spellings need not be
// adjacent; the name sitting between them still yields exit 3.
func TestCLISetupVersionDuplicateInterspersed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	seedTwoFrozenSetupCLI(t, dir)
	old := `"name": "mul",
      "version": 1`
	fragment := `"version": 1,
      "name": "mul",
      "VERSION": 2`
	bad := rewriteSetupVersionCLI(t, dir, old, fragment)

	r := callCLI(t, "setup-get", "--dir", dir, "--name", "mul", "--version", "1")
	if r.code != 3 || r.out != "" {
		t.Fatalf("interspersed duplicate: code=%d out=%q err=%q", r.code, r.out, r.err)
	}
	if !strings.Contains(r.err, "setup record #1") || !strings.Contains(r.err, "version") {
		t.Fatalf("error does not name the setup record/version: %q", r.err)
	}
	left, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(bad) {
		t.Fatalf("data.json changed after refused command")
	}
}

// TestCLISetupVersionSingleSpellingStillReads: one recognized spelling of
// version, an ASCII variant or a long-s fold, keeps the setup bound to v1; v2
// has no setup, so its proof job still exits 1 with "trusted setup missing".
func TestCLISetupVersionSingleSpellingStillReads(t *testing.T) {
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
			seedTwoFrozenSetupCLI(t, dir)
			rewriteSetupVersionCLI(t, dir, `"version": 1`, `"`+tc.key+`": 1`)

			r := callCLI(t, "setup-get", "--dir", dir, "--name", "mul", "--version", "1")
			if r.code != 0 || !strings.Contains(r.out, "version=1 present=true") {
				t.Fatalf("single spelling %q not read as v1 setup: code=%d out=%q err=%q",
					tc.key, r.code, r.out, r.err)
			}
			// The setup stays exclusive to v1: a v2 job is still gated.
			missing := callCLI(t, "job-submit", "--dir", dir, "--id", "j2",
				"--name", "mul", "--version", "2")
			if missing.code != 1 || !strings.Contains(missing.err, "trusted setup missing") {
				t.Fatalf("v2 job under single spelling %q: code=%d err=%q", tc.key, missing.code, missing.err)
			}
			// And v1 continues to pass the gate.
			ok := callCLI(t, "job-submit", "--dir", dir, "--id", "j1",
				"--name", "mul", "--version", "1")
			if ok.code != 0 || !strings.Contains(ok.out, "job accepted") {
				t.Fatalf("v1 job under single spelling %q: code=%d err=%q", tc.key, ok.code, ok.err)
			}
		})
	}
}
