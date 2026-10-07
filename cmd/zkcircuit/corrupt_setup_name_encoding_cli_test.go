package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLI contract for the owning-circuit exact-readback rule on a saved trusted
// setup record. The name a setup saves decides which frozen circuit it
// belongs to, so a committed data.json whose setup name value carries
// invalid UTF-8 bytes or an unpaired \uXXXX surrogate escape is data
// corruption even when the repaired name happens to exist, the version is
// frozen and the setup gate would otherwise pass: every data command exits
// 3, prints no partial result, names the damaged setup record (by its 1-based
// position) and its name field, and leaves data.json byte-for-byte in place.
// The rule covers the canonical "name", ASCII letter-case variants and
// JSON-escaped spellings of the key.
//
// setup-record with a circuit name that is not complete, valid UTF-8 exits 1
// (the business-rule failure code) before anything is committed. Legal
// Chinese, emoji and a genuine "�" round-trip exactly.

// rawSetupLookalikeEnvelopeCLI builds a committed envelope with one genuinely
// frozen circuit named "电路�", a proof job bound to it, and one setup
// record whose name token is the given raw JSON string that decodes onto the
// same name only through replacement.
func rawSetupLookalikeEnvelopeCLI(damagedNameJSON string) []byte {
	return []byte(`{"format":1,"circuits":[` +
		`{"name":"电路�","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
		`],"setups":[{"name":` + damagedNameJSON + `,"version":1}],"jobs":[` +
		`{"id":"j1","circuit":"电路�","version":1,"kind":"prove","attempt":1}]}`)
}

// rawSetupKeyEnvelopeCLI builds a committed envelope around the legal frozen
// circuit "c" v1 and one fully hand-written setup record, so the caller
// controls the spelling of the name key.
func rawSetupKeyEnvelopeCLI(record string) []byte {
	return []byte(`{"format":1,"circuits":[` +
		`{"name":"c","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
		`],"setups":[` + record + `],"jobs":[]}`)
}

func TestCLICorruptSetupNameExitCode3(t *testing.T) {
	damage := []struct {
		label    string
		nameJSON string
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
			raw := rawSetupLookalikeEnvelopeCLI(tc.nameJSON)
			dir := plantRawDataFile(t, raw)

			// Every data command fails at the read, including queries for the
			// still-legal circuit and its job: no partial results and no new
			// setup or job either.
			commands := [][]string{
				{"setup-get", "--dir", dir, "--name", "电路�", "--version", "1"},
				{"setup-get", "--dir", dir, "--name", "c", "--version", "1"},
				{"setup-record", "--dir", dir, "--name", "电路�", "--version", "1"},
				{"job-submit", "--dir", dir, "--id", "j2", "--name", "电路�", "--version", "1"},
				{"job-list", "--dir", dir},
				{"job-get", "--dir", dir, "--id", "j1"},
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
				if !strings.Contains(r.err, "setup record #1") || !strings.Contains(r.err, `"name"`) {
					t.Fatalf("%v error does not name the damaged setup record and name field: %q", args, r.err)
				}
				// The failure must not be disguised as a lookup/setup problem.
				if strings.Contains(r.err, "belongs to unknown circuit") ||
					strings.Contains(r.err, "trusted setup missing") {
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

// The encoding rule applies to every accepted spelling of the name key; the
// damaged setup record is always located by its 1-based position.
func TestCLICorruptSetupNameKeySpellingsExitCode3(t *testing.T) {
	escLowerN := cliJSONEscape + "u006eame"
	escUpperN := cliJSONEscape + "u004eAME"
	cases := []struct {
		label  string
		record string
	}{
		{"upper case key, bad bytes", `{"NAME":"c` + "\xff" + `","version":1}`},
		{"mixed case key, surrogate", `{"NaMe":"c\uD800","version":1}`},
		{"escaped lowercase key, bad bytes", `{"` + escLowerN + `":"c` + "\xff" + `","version":1}`},
		{"escaped uppercase key, surrogate", `{"` + escUpperN + `":"c\uD800","version":1}`},
		{"canonical key, bad bytes", `{"name":"c` + "\xff" + `","version":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			raw := rawSetupKeyEnvelopeCLI(tc.record)
			dir := plantRawDataFile(t, raw)

			r := callCLI(t, "setup-get", "--dir", dir, "--name", "c", "--version", "1")
			if r.code != 3 {
				t.Fatalf("want exit 3, got %d (err=%q)", r.code, r.err)
			}
			if !strings.Contains(r.err, "read failed") ||
				!strings.Contains(r.err, "setup record #1") ||
				!strings.Contains(r.err, `"name"`) {
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

// A damaged record on the second setup slot is located by position #2.
func TestCLICorruptSetupNameSecondRecordPosition(t *testing.T) {
	raw := []byte(`{"format":1,"circuits":[` +
		`{"name":"c","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""},` +
		`{"name":"电路�","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
		`],"setups":[{"name":"c","version":1},{"name":"电路` + "\xff" + `","version":1}],"jobs":[]}`)
	dir := plantRawDataFile(t, raw)

	r := callCLI(t, "job-list", "--dir", dir)
	if r.code != 3 || !strings.Contains(r.err, "setup record #2") {
		t.Fatalf("want exit 3 naming setup record #2, got %d (err=%q)", r.code, r.err)
	}
	left, rerr := os.ReadFile(filepath.Join(dir, "data.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(left) != string(raw) {
		t.Fatalf("data.json changed across the refused command")
	}
}

// setup-record with an invalid-UTF-8 circuit name is a business-rule failure
// (exit 1), prints no success record and commits nothing.
func TestCLISetupRecordRejectsInvalidUTF8(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	seedTwoNamedFrozenSetupCLI(t, dir)
	dataPath := filepath.Join(dir, "data.json")
	before, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"bad\x80name", "a\xff", "电路\xff"} {
		r := callCLI(t, "setup-record", "--dir", dir, "--name", name, "--version", "1")
		if r.code != 1 {
			t.Fatalf("want exit 1, got %d (out=%q err=%q)", r.code, r.out, r.err)
		}
		if strings.Contains(r.out, "trusted setup:") {
			t.Fatalf("a success record was printed for a refused registration: %q", r.out)
		}
		if !strings.Contains(r.err, "UTF-8") {
			t.Fatalf("error does not explain the name encoding is invalid: %q", r.err)
		}
		after, rerr := os.ReadFile(dataPath)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if string(after) != string(before) {
			t.Fatalf("refused registration modified data.json")
		}
	}
}

// Legal Unicode circuit names — Chinese, emoji and a genuine "�" — freeze,
// take a setup and accept a job that reads back owned by the exact name.
func TestCLIUnicodeSetupNameRoundTrip(t *testing.T) {
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

			r := callCLI(t, "setup-get", "--dir", dir, "--name", name, "--version", "1")
			if r.code != 0 || !strings.Contains(r.out, "name="+name+" version=1 present=true") {
				t.Fatalf("setup did not read back under its exact name: code=%d out=%q err=%q",
					r.code, r.out, r.err)
			}
		})
	}
}
