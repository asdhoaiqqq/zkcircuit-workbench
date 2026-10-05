package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the CLI contract of the circuit-name identity rule.
//
// circuit-create with a name that is not complete, valid UTF-8 exits 1 (the
// business-rule failure code), prints no success record and commits nothing —
// the submitted bytes would otherwise be silently rewritten to "�" when the
// record is saved, and the circuit would be stored under a different name.
//
// A committed data.json whose circuit record name carries invalid UTF-8
// bytes or an unpaired \uXXXX surrogate escape is data corruption: every
// data command exits 3, no partial query result is printed, no modification
// commits, and the file stays byte-for-byte in place.

// plantRawDataFile writes raw as the committed data.json of a fresh bench
// directory and returns the directory.
func plantRawDataFile(t *testing.T, raw []byte) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bench")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// rawEnvelopeNamed builds a committed envelope holding one legal circuit and
// one circuit whose name is the given raw JSON token.
func rawEnvelopeNamed(nameJSON string) []byte {
	return []byte(`{"format":1,"circuits":[` +
		`{"name":"ok","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":false,"description":""},` +
		`{"name":` + nameJSON + `,"version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":false,"description":""}` +
		`],"setups":[],"jobs":[]}`)
}

func TestCLICreateRejectsInvalidUTF8Name(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	if r := callCLI(t, "circuit-create", "--dir", dir, "--name", "good", "--version", "1",
		"--constraints", "1"); r.code != 0 {
		t.Fatalf("seed create: code=%d err=%s", r.code, r.err)
	}
	dataPath := filepath.Join(dir, "data.json")
	before, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}

	bad := []struct {
		label string
		name  string
	}{
		{"lone continuation byte", "bad\x80name"},
		{"truncated multi-byte", "trunc\xe4\xb8"},
		{"invalid byte 0xff", "bad\xff"},
		{"encoded surrogate half", "pair\xed\xa0\x80"},
	}
	for _, tc := range bad {
		t.Run(tc.label, func(t *testing.T) {
			r := callCLI(t, "circuit-create", "--dir", dir, "--name", tc.name, "--version", "1",
				"--constraints", "1")
			if r.code != 1 {
				t.Fatalf("want exit 1, got %d (out=%q err=%q)", r.code, r.out, r.err)
			}
			if strings.Contains(r.out, "circuit version:") {
				t.Fatalf("a success record was printed for a refused create: %q", r.out)
			}
			after, err := os.ReadFile(dataPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("refused create modified data.json\nwant: %q\n got: %q", before, after)
			}
		})
	}

	// The previously committed circuit is still there, under its own name.
	r := callCLI(t, "circuit-list", "--dir", dir)
	if r.code != 0 || !strings.Contains(r.out, "name=good") {
		t.Fatalf("existing circuit damaged by refused creates: code=%d out=%q err=%q", r.code, r.out, r.err)
	}
}

func TestCLICorruptNameExitCode3(t *testing.T) {
	damage := []struct {
		label    string
		nameJSON string
	}{
		{"invalid UTF-8 bytes", "\"bad\xff\""},
		{"lone continuation byte", "\"bad\x80name\""},
		{"unpaired high surrogate", `"电路\uD800"`},
		{"unpaired low surrogate", `"\uDC00"`},
	}
	for _, tc := range damage {
		t.Run(tc.label, func(t *testing.T) {
			raw := rawEnvelopeNamed(tc.nameJSON)
			dir := plantRawDataFile(t, raw)

			commands := [][]string{
				{"circuit-list", "--dir", dir},
				{"circuit-get", "--dir", dir, "--name", "ok", "--version", "1"},
				{"circuit-create", "--dir", dir, "--name", "new", "--version", "1", "--constraints", "1"},
				{"circuit-freeze", "--dir", dir, "--name", "ok", "--version", "1"},
			}
			for _, args := range commands {
				r := callCLI(t, args...)
				if r.code != 3 {
					t.Fatalf("%v: want exit 3, got %d (out=%q err=%q)", args, r.code, r.out, r.err)
				}
				if strings.Contains(r.out, "circuit:") || strings.Contains(r.out, "circuit version:") ||
					strings.Contains(r.out, "circuit frozen:") {
					t.Fatalf("%v: a success/partial record was printed: %q", args, r.out)
				}
			}
			left, err := os.ReadFile(filepath.Join(dir, "data.json"))
			if err != nil {
				t.Fatal(err)
			}
			if string(left) != string(raw) {
				t.Fatalf("data.json changed across refused commands\nwant: %q\n got: %q", raw, left)
			}
		})
	}
}

// Legal Unicode names — Chinese, emoji, an actually committed "�" and the
// literal six characters \uD800 — are ordinary names: they create, save and
// read back under their exact value.
func TestCLIUnicodeNamesRoundTrip(t *testing.T) {
	names := []string{"电路", "emoji 😀 name", "has�mark", `literal\uD800`}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			r := callCLI(t, "circuit-create", "--dir", dir, "--name", name, "--version", "1",
				"--constraints", "1")
			if r.code != 0 {
				t.Fatalf("create: code=%d err=%s", r.code, r.err)
			}
			r = callCLI(t, "circuit-get", "--dir", dir, "--name", name, "--version", "1")
			if r.code != 0 || !strings.Contains(r.out, "name="+name) {
				t.Fatalf("name did not round-trip: code=%d out=%q err=%q", r.code, r.out, r.err)
			}
		})
	}
}
