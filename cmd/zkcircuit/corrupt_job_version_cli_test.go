package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLI contract for an ambiguous pinned-circuit version on a saved job. When a
// job record names the version field more than once across the spellings the
// reader recognizes (canonical, ASCII letter-case variant, JSON-unescaped, or
// "verſion" with U+017F long s), every data command must fail like any other
// read failure: exit code 3, nothing printed, the message locating the
// damaged job record and its version field, and data.json left byte-for-byte
// in place. A single version spelling (including a lone long-s spelling)
// keeps reading as before.

// cliLongVersion is "verſion" written directly; cliEscapedLongVersion is the
// same key with the long s JSON-escaped.
var cliLongVersion = "ver" + cliLongS + "ion"
var cliEscapedLongVersion = "ver" + cliJSONEscape + "u017f" + "ion"

// cliEscapedVersionUpper spells the ASCII variant "VERSION" with its capital
// V JSON-escaped; after unescaping it is the variant, not the canonical key.
var cliEscapedVersionUpper = cliJSONEscape + "u0056" + "ERSION"

// seedTwoVersionJobCLI drives the CLI through two frozen versions of "mul"
// (v1 and v2), each with its own trusted setup, and an unbound job "j1"
// pinned to v1 — the setup in which an ambiguous version used to read the job
// as v2.
func seedTwoVersionJobCLI(t *testing.T, dir string) {
	t.Helper()
	defPath := writeFile(t, filepath.Dir(dir), "def.json", cliCorruptDef)
	must := func(what string, r cliResult) {
		t.Helper()
		if r.code != 0 {
			t.Fatalf("%s: code=%d err=%s", what, r.code, r.err)
		}
	}
	for _, v := range []string{"1", "2"} {
		must("create v"+v, callCLI(t, "circuit-create", "--dir", dir, "--name", "mul", "--version", v,
			"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1"))
		must("import v"+v, callCLI(t, "constraint-import", "--dir", dir, "--name", "mul", "--version", v,
			"--file", defPath))
		must("freeze v"+v, callCLI(t, "circuit-freeze", "--dir", dir, "--name", "mul", "--version", v))
		must("setup v"+v, callCLI(t, "setup-record", "--dir", dir, "--name", "mul", "--version", v))
	}
	must("submit", callCLI(t, "job-submit", "--dir", dir, "--id", "j1", "--name", "mul", "--version", "1"))
}

// spliceCLIJob replaces the whole committed JSON object of job id with
// object, returning the bytes now on disk.
func spliceCLIJob(t *testing.T, dir, id, object string) []byte {
	t.Helper()
	path := filepath.Join(dir, "data.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	str := string(raw)
	marker := `"id": "` + id + `"`
	at := strings.Index(str, marker)
	if at < 0 {
		t.Fatalf("committed file does not carry job %q", id)
	}
	open := strings.LastIndex(str[:at], "{")
	closeRel := strings.Index(str[at:], "}")
	if open < 0 || closeRel < 0 {
		t.Fatalf("cannot locate object braces for job %q", id)
	}
	close := at + closeRel
	bad := []byte(str[:open] + object + str[close+1:])
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	return bad
}

// TestCLIJobVersionAmbiguityExitCode: every recognizable duplicate spelling
// of the version field makes every data command exit 3 without printing a
// record, names the damaged job and the version field, and leaves the file
// untouched.
func TestCLIJobVersionAmbiguityExitCode(t *testing.T) {
	job := func(members string) string { return `{` + members + `}` }
	cases := []struct {
		name   string
		object string
		named  bool // the message is expected to name the id "j1"
	}{
		{"canonical then upper, differing values",
			job(`"id": "j1", "circuit": "mul", "version": 1, "VERSION": 2, "kind": "prove", "attempt": 1`), true},
		{"canonical then upper, identical values",
			job(`"id": "j1", "circuit": "mul", "version": 1, "VERSION": 1, "kind": "prove", "attempt": 1`), true},
		{"upper then canonical",
			job(`"id": "j1", "circuit": "mul", "VERSION": 2, "version": 1, "kind": "prove", "attempt": 1`), true},
		{"duplicate spread among other fields",
			job(`"VERSION": 2, "id": "j1", "kind": "prove", "circuit": "mul", "attempt": 1, "version": 1`), true},
		{"direct long s beside canonical",
			job(`"id": "j1", "circuit": "mul", "version": 1, "` + cliLongVersion + `": 2, "kind": "prove", "attempt": 1`), true},
		{"escaped long s before canonical",
			job(`"id": "j1", "` + cliEscapedLongVersion + `": 1, "circuit": "mul", "version": 2, "kind": "prove", "attempt": 1`), true},
		{"escaped ASCII variant beside canonical",
			job(`"id": "j1", "circuit": "mul", "version": 1, "` + cliEscapedVersionUpper + `": 2, "kind": "prove", "attempt": 1`), true},
		{"two byte-identical canonical keys",
			job(`"id": "j1", "circuit": "mul", "version": 1, "version": 2, "kind": "prove", "attempt": 1`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			seedTwoVersionJobCLI(t, dir)
			bad := spliceCLIJob(t, dir, "j1", tc.object)

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
				if !strings.Contains(r.err, "version") {
					t.Fatalf("%s error does not name the version field: %q", args[0], r.err)
				}
			}

			// The failure locates the damaged job record: by id for folded
			// spellings, and by its jobs-array position for two exact keys.
			r := callCLI(t, "job-get", "--dir", dir, "--id", "j1")
			if tc.named {
				if !strings.Contains(r.err, `"j1"`) {
					t.Fatalf("error does not name the damaged job: %q", r.err)
				}
			} else if !strings.Contains(strings.ToLower(r.err), "job") {
				t.Fatalf("error does not locate the damaged job record: %q", r.err)
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

// TestCLIJobVersionLoneSpellingStillReads: a single version spelling keeps
// the existing behavior — an ASCII case variant and a lone long-s spelling
// both read the job pinned to v1.
func TestCLIJobVersionLoneSpellingStillReads(t *testing.T) {
	job := func(key string) string {
		return `{"id": "j1", "circuit": "mul", "` + key + `": 1, "kind": "prove", "attempt": 1}`
	}
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"upper case", "VERSION"},
		{"direct long s", cliLongVersion},
		{"escaped long s", cliEscapedLongVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			seedTwoVersionJobCLI(t, dir)
			spliceCLIJob(t, dir, "j1", job(tc.key))

			r := callCLI(t, "job-get", "--dir", dir, "--id", "j1")
			if r.code != 0 {
				t.Fatalf("lone spelling %q must load: code=%d err=%s", tc.key, r.code, r.err)
			}
			if !strings.Contains(r.out, "circuit=mul") || !strings.Contains(r.out, "version=1") {
				t.Fatalf("lone %q not read pinned to v1: %q", tc.key, r.out)
			}
			if strings.Contains(r.out, "version=2") {
				t.Fatalf("lone %q drifted onto v2: %q", tc.key, r.out)
			}
		})
	}
}
