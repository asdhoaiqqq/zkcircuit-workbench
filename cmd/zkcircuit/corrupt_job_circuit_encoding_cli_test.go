package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLI contract for the owning-circuit exact-readback rule on a saved proof
// job. The circuit a job saves decides which frozen version, trusted setup
// and compiled artifact it belongs to, so a committed data.json whose job
// circuit value carries invalid UTF-8 bytes or an unpaired \uXXXX surrogate
// escape is data corruption even when the repaired name happens to exist, is
// frozen and has a setup: every data command exits 3, prints no partial
// result, names the damaged job and its circuit field, and leaves data.json
// byte-for-byte in place. The rule covers the canonical "circuit", ASCII
// letter-case variants and JSON-escaped spellings of the key.
//
// job-submit with a circuit name that is not complete, valid UTF-8 exits 1
// (the business-rule failure code) before anything is committed. Legal
// Chinese, emoji and a genuine "�" round-trip exactly.

// rawJobCircuitLookalikeEnvelopeCLI builds a committed envelope with one
// genuinely frozen, setup-bearing circuit named "电路�" plus two jobs: one
// legal job owned by that circuit and one whose circuit token is the given
// raw JSON string that decodes onto the same name only by replacement.
func rawJobCircuitLookalikeEnvelopeCLI(damagedCircuitJSON string) []byte {
	return []byte(`{"format":1,"circuits":[` +
		`{"name":"电路�","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
		`],"setups":[{"name":"电路�","version":1}],"jobs":[` +
		`{"id":"j-legal","circuit":"电路�","version":1,"kind":"prove","attempt":1},` +
		`{"id":"j-bad","circuit":` + damagedCircuitJSON + `,"version":1,"kind":"prove","attempt":1}]}`)
}

// rawJobCircuitKeyEnvelopeCLI builds a committed envelope around the legal
// frozen circuit "c" v1 and one fully hand-written job record, so the caller
// controls the spelling of the circuit key.
func rawJobCircuitKeyEnvelopeCLI(record string) []byte {
	return []byte(`{"format":1,"circuits":[` +
		`{"name":"c","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
		`],"setups":[{"name":"c","version":1}],"jobs":[` + record + `]}`)
}

func TestCLICorruptJobCircuitExitCode3(t *testing.T) {
	damage := []struct {
		label       string
		circuitJSON string
	}{
		{"invalid UTF-8 bytes", "\"电路\xff\""},
		{"truncated multibyte", "\"电路\xe4\xb8\""},
		{"lone continuation byte", "\"bad\x80\""},
		{"unpaired high surrogate", `"电路\uD800"`},
		{"unpaired low surrogate", `"电路\uDC00"`},
		{"reversed pair", `"\uDC00\uD800"`},
	}
	for _, tc := range damage {
		t.Run(tc.label, func(t *testing.T) {
			raw := rawJobCircuitLookalikeEnvelopeCLI(tc.circuitJSON)
			dir := plantRawDataFile(t, raw)

			// Every data command fails at the read, including the get for the
			// still-legal job: no partial results and no new job either.
			commands := [][]string{
				{"job-get", "--dir", dir, "--id", "j-legal"},
				{"job-get", "--dir", dir, "--id", "j-bad"},
				{"job-list", "--dir", dir},
				{"job-submit", "--dir", dir, "--id", "j2", "--name", "电路�", "--version", "1"},
				{"circuit-list", "--dir", dir},
			}
			for _, args := range commands {
				r := callCLI(t, args...)
				if r.code != 3 {
					t.Fatalf("%v: want exit 3, got %d (out=%q err=%q)", args, r.code, r.out, r.err)
				}
				if r.out != "" {
					t.Fatalf("%v printed a record despite corruption: %q", args, r.out)
				}
				if !strings.Contains(r.err, "read failed") || !strings.Contains(r.err, "data corrupt") {
					t.Fatalf("%v: unexpected error text %q", args, r.err)
				}
				if !strings.Contains(r.err, "j-bad") || !strings.Contains(r.err, `"circuit"`) {
					t.Fatalf("%v error does not name the damaged job and circuit field: %q", args, r.err)
				}
				// The failure must not be disguised as a lookup/setup problem.
				if strings.Contains(r.err, "unknown circuit") || strings.Contains(r.err, "trusted setup") {
					t.Fatalf("%v: encoding damage masked as a lookup failure: %q", args, r.err)
				}
			}

			left, rerr := os.ReadFile(filepath.Join(dir, "data.json"))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(left) != string(raw) {
				t.Fatalf("data.json changed across refused commands\nwant: %q\n got: %q", raw, left)
			}
		})
	}
}

// The encoding rule applies to every accepted spelling of the circuit key;
// a record without an id is located by its record position.
func TestCLICorruptJobCircuitKeySpellingsExitCode3(t *testing.T) {
	escLowerC := cliJSONEscape + "u0063ircuit"
	escUpperC := cliJSONEscape + "u0043IRCUIT"
	jobTail := `,"version":1,"kind":"prove","attempt":1}`
	cases := []struct {
		label    string
		record   string
		wantName string
	}{
		{"upper case key, bad bytes", `{"id":"j1","CIRCUIT":"c` + "\xff" + `"` + jobTail, "j1"},
		{"mixed case key, surrogate", `{"id":"j1","CiRcUiT":"c\uD800"` + jobTail, "j1"},
		{"escaped lowercase key, bad bytes", `{"id":"j1","` + escLowerC + `":"c` + "\xff" + `"` + jobTail, "j1"},
		{"escaped uppercase key, surrogate", `{"id":"j1","` + escUpperC + `":"c\uD800"` + jobTail, "j1"},
		{"no id, bad bytes", `{"circuit":"c` + "\xff" + `"` + jobTail, "job record #1"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			raw := rawJobCircuitKeyEnvelopeCLI(tc.record)
			dir := plantRawDataFile(t, raw)

			r := callCLI(t, "job-list", "--dir", dir)
			if r.code != 3 {
				t.Fatalf("want exit 3, got %d (err=%q)", r.code, r.err)
			}
			if !strings.Contains(r.err, "read failed") || !strings.Contains(r.err, `"circuit"`) ||
				!strings.Contains(r.err, tc.wantName) {
				t.Fatalf("unexpected error text %q", r.err)
			}

			left, rerr := os.ReadFile(filepath.Join(dir, "data.json"))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(left) != string(raw) {
				t.Fatalf("data.json changed across the refused command")
			}
		})
	}
}

// job-submit with an invalid-UTF-8 circuit name is a business-rule failure
// (exit 1), prints no accepted record and commits nothing.
func TestCLISubmitJobRejectsInvalidUTF8Circuit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	hash := seedBoundJobCLI(t, dir) // frozen circuit "mul" v1 with setup + job j1
	dataPath := filepath.Join(dir, "data.json")
	before, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"bad\x80name", "c\xff", "电路\xff"} {
		r := callCLI(t, "job-submit", "--dir", dir, "--id", "jx",
			"--name", name, "--version", "1", "--hash", hash)
		if r.code != 1 {
			t.Fatalf("want exit 1, got %d (out=%q err=%q)", r.code, r.out, r.err)
		}
		if strings.Contains(r.out, "job accepted:") {
			t.Fatalf("a success record was printed for a refused submit: %q", r.out)
		}
		if !strings.Contains(r.err, "UTF-8") {
			t.Fatalf("error does not explain the circuit encoding is invalid: %q", r.err)
		}
		after, rerr := os.ReadFile(dataPath)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if string(after) != string(before) {
			t.Fatalf("refused submit modified data.json")
		}
	}
}

// Legal Unicode circuit names — Chinese, emoji and a genuine "�" — create,
// freeze, set up and accept a job that reads back owned by the exact name.
func TestCLIUnicodeJobCircuitRoundTrip(t *testing.T) {
	for _, name := range []string{"电路-1", "emoji 😀 c", "电路�"} {
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
			must("submit", callCLI(t, "job-submit", "--dir", dir, "--id", "j1",
				"--name", name, "--version", "1"))

			r := callCLI(t, "job-get", "--dir", dir, "--id", "j1")
			if r.code != 0 || !strings.Contains(r.out, "circuit="+name) {
				t.Fatalf("job did not read back under its exact circuit: code=%d out=%q err=%q",
					r.code, r.out, r.err)
			}
		})
	}
}
