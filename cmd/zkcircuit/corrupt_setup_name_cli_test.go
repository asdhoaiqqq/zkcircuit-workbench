package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLI contract for an ambiguous owning-circuit name on a committed
// trusted-setup record. With a and b both frozen at version 1 and a trusted
// setup recorded only for a, every spelling an ordinary read would take as the
// setup's circuit name — canonical "name", an ASCII letter-case variant such
// as "NAME" or "NaMe", or a JSON-escaped spelling of either — is one name
// field. Two of them on one record make the owning circuit depend on key
// order, so every data command fails at open with exit code 3, prints no
// record, names the damaged setup record and its name field, and leaves
// data.json byte-for-byte in place. A single recognized spelling still reads,
// and the other circuit's proof job still reports a missing setup.

// seedTwoNamedFrozenSetupCLI drives the CLI through two frozen circuits a and
// b (both at version 1) with a trusted setup recorded on a only (b is
// deliberately unset).
func seedTwoNamedFrozenSetupCLI(t *testing.T, dir string) {
	t.Helper()
	must := func(what string, r cliResult) {
		t.Helper()
		if r.code != 0 {
			t.Fatalf("%s: code=%d err=%s", what, r.code, r.err)
		}
	}
	for _, n := range []string{"a", "b"} {
		must("create "+n, callCLI(t, "circuit-create", "--dir", dir, "--name", n,
			"--version", "1", "--constraints", "1"))
		must("freeze "+n, callCLI(t, "circuit-freeze", "--dir", dir, "--name", n, "--version", "1"))
	}
	must("setup a", callCLI(t, "setup-record", "--dir", dir, "--name", "a", "--version", "1"))
}

// rewriteSetupNameCLI scopes a literal replacement to the committed setups
// array: it replaces the first old occurring after the "setups" marker, so the
// shared circuit "name" fields are never touched. It returns the bytes now on
// disk.
func rewriteSetupNameCLI(t *testing.T, dir, old, fragment string) []byte {
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

// TestCLISetupNameAmbiguousExitCode: two recognized name spellings on the a
// setup record — ASCII variants, JSON-escaped spellings, equal or differing
// values, even when one names a circuit that does not exist — make every data
// command exit 3 with no output and leave data.json byte-for-byte in place.
func TestCLISetupNameAmbiguousExitCode(t *testing.T) {
	escN := cliJSONEscape + "u006eame"
	escUpperN := cliJSONEscape + "u004e" + "AME"
	cases := []struct {
		name     string
		fragment string
	}{
		{"canonical plus upper, same value", `"name": "a", "NAME": "a"`},
		{"canonical plus upper, differing", `"name": "a", "NAME": "b"`},
		{"upper then canonical", `"NAME": "b", "name": "a"`},
		{"two non-canonical variants", `"NAME": "b", "Name": "a"`},
		{"mixed case", `"name": "a", "NaMe": "a"`},
		{"escaped exact spelling", `"name": "a", "` + escN + `": "a"`},
		{"escaped uppercase spelling", `"name": "a", "` + escUpperN + `": "b"`},
		{"byte-identical duplicate", `"name": "a", "name": "a"`},
		{"second names a circuit that does not exist", `"name": "a", "NAME": "ghost"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			seedTwoNamedFrozenSetupCLI(t, dir)
			bad := rewriteSetupNameCLI(t, dir, `"name": "a"`, tc.fragment)

			commands := [][]string{
				{"setup-get", "--dir", dir, "--name", "a", "--version", "1"},
				{"setup-get", "--dir", dir, "--name", "b", "--version", "1"},
				{"setup-record", "--dir", dir, "--name", "b", "--version", "1"},
				{"job-submit", "--dir", dir, "--id", "jb", "--name", "b", "--version", "1"},
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
				if !strings.Contains(r.err, "setup record #1") || !strings.Contains(r.err, "name") {
					t.Fatalf("%s: error does not name the damaged setup record and name field: %q", args[0], r.err)
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

// TestCLISetupNameDuplicateInterspersed: the two spellings need not be
// adjacent; the version sitting between them still yields exit 3.
func TestCLISetupNameDuplicateInterspersed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	seedTwoNamedFrozenSetupCLI(t, dir)
	old := `"name": "a",
      "version": 1`
	fragment := `"name": "a",
      "version": 1,
      "NAME": "b"`
	bad := rewriteSetupNameCLI(t, dir, old, fragment)

	r := callCLI(t, "setup-get", "--dir", dir, "--name", "a", "--version", "1")
	if r.code != 3 || r.out != "" {
		t.Fatalf("interspersed duplicate: code=%d out=%q err=%q", r.code, r.out, r.err)
	}
	if !strings.Contains(r.err, "setup record #1") || !strings.Contains(r.err, "name") {
		t.Fatalf("error does not name the setup record/name: %q", r.err)
	}
	left, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(bad) {
		t.Fatalf("data.json changed after refused command")
	}
}

// TestCLISetupNameSingleSpellingStillReads: one recognized spelling of the
// circuit name, an ASCII variant or a JSON-escaped spelling, keeps the setup
// bound to a; b has no setup, so its proof job still exits 1 with "trusted
// setup missing".
func TestCLISetupNameSingleSpellingStillReads(t *testing.T) {
	escN := cliJSONEscape + "u006eame"
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"upper case", "NAME"},
		{"mixed case", "NaMe"},
		{"escaped letter", escN},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			seedTwoNamedFrozenSetupCLI(t, dir)
			rewriteSetupNameCLI(t, dir, `"name": "a"`, `"`+tc.key+`": "a"`)

			r := callCLI(t, "setup-get", "--dir", dir, "--name", "a", "--version", "1")
			if r.code != 0 || !strings.Contains(r.out, "name=a version=1 present=true") {
				t.Fatalf("single spelling %q not read as a's setup: code=%d out=%q err=%q",
					tc.key, r.code, r.out, r.err)
			}
			// The setup stays exclusive to a: a b job is still gated.
			missing := callCLI(t, "job-submit", "--dir", dir, "--id", "jb",
				"--name", "b", "--version", "1")
			if missing.code != 1 || !strings.Contains(missing.err, "trusted setup missing") {
				t.Fatalf("b job under single spelling %q: code=%d err=%q", tc.key, missing.code, missing.err)
			}
			// And a continues to pass the gate.
			ok := callCLI(t, "job-submit", "--dir", dir, "--id", "ja",
				"--name", "a", "--version", "1")
			if ok.code != 0 || !strings.Contains(ok.out, "job accepted") {
				t.Fatalf("a job under single spelling %q: code=%d err=%q", tc.key, ok.code, ok.err)
			}
		})
	}
}

// TestCLISetupNameValueMatchingStillExact: the name value is matched exactly;
// a single spelling whose value differs only by case or surrounding spaces
// names no committed circuit and the data fails integrity validation (exit 3),
// exactly as before — no case folding or trimming.
func TestCLISetupNameValueMatchingStillExact(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"case differs", "A"},
		{"trailing space", "a "},
		{"leading space", " a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			seedTwoNamedFrozenSetupCLI(t, dir)
			rewriteSetupNameCLI(t, dir, `"name": "a"`, `"name": "`+tc.value+`"`)
			r := callCLI(t, "setup-get", "--dir", dir, "--name", "a", "--version", "1")
			if r.code != 3 {
				t.Fatalf("value %q: want exit 3, got %d (err=%q)", tc.value, r.code, r.err)
			}
		})
	}
}
