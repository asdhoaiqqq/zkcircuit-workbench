package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the CLI contract of the job-id identity rule, the job
// counterpart of the circuit-name rule in name_encoding_cli_test.go.
//
// job-submit with an id that is not complete, valid UTF-8 exits 1 (the
// business-rule failure code), prints no "job accepted" record and commits
// nothing — the submitted bytes would otherwise be silently rewritten to
// "�" when the record is saved, and a later job-get by the submitted id
// would miss it.
//
// A committed data.json whose job record id carries invalid UTF-8 bytes or an
// unpaired \uXXXX surrogate escape is data corruption: every data command
// exits 3, no partial query result is printed, no modification commits, and
// the file stays byte-for-byte in place.

// rawJobEnvelopeCLI builds a committed envelope with one frozen, setup-bearing
// circuit and one job whose id is the given raw JSON string token.
func rawJobEnvelopeCLI(idJSON string) []byte {
	return []byte(`{"format":1,"circuits":[` +
		`{"name":"c","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
		`],"setups":[{"name":"c","version":1}],"jobs":[` +
		`{"id":"j1","circuit":"c","version":1,"kind":"prove","attempt":1},` +
		`{"id":` + idJSON + `,"circuit":"c","version":1,"kind":"prove","attempt":1}` +
		`]}`)
}

func TestCLISubmitJobRejectsInvalidUTF8ID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	hash := seedBoundJobCLI(t, dir) // frozen circuit "mul" v1 with setup + job j1
	dataPath := filepath.Join(dir, "data.json")
	before, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}

	bad := []struct {
		label string
		id    string
	}{
		{"lone continuation byte", "bad\x80id"},
		{"truncated multi-byte", "trunc\xe4\xb8"},
		{"invalid byte 0xff", "bad\xff"},
		{"encoded surrogate half", "pair\xed\xa0\x80"},
	}
	for _, tc := range bad {
		t.Run(tc.label, func(t *testing.T) {
			r := callCLI(t, "job-submit", "--dir", dir, "--id", tc.id,
				"--name", "mul", "--version", "1", "--hash", hash)
			if r.code != 1 {
				t.Fatalf("want exit 1, got %d (out=%q err=%q)", r.code, r.out, r.err)
			}
			if strings.Contains(r.out, "job accepted:") {
				t.Fatalf("a success record was printed for a refused submit: %q", r.out)
			}
			if !strings.Contains(r.err, "UTF-8") {
				t.Fatalf("error does not explain the id encoding is invalid: %q", r.err)
			}
			after, rerr := os.ReadFile(dataPath)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(after) != string(before) {
				t.Fatalf("refused submit modified data.json\nwant: %q\n got: %q", before, after)
			}
		})
	}

	// The existing job is untouched; no repaired "�" id exists either.
	r := callCLI(t, "job-list", "--dir", dir)
	if r.code != 0 || !strings.Contains(r.out, "id=j1") {
		t.Fatalf("existing job damaged: code=%d out=%q err=%q", r.code, r.out, r.err)
	}
	if strings.Contains(r.out, "�") {
		t.Fatalf("a U+FFFD-repaired id was committed: %q", r.out)
	}
}

func TestCLICorruptJobIDExitCode3(t *testing.T) {
	damage := []struct {
		label  string
		idJSON string
	}{
		{"invalid UTF-8 bytes", "\"bad\xff\""},
		{"lone continuation byte", "\"bad\x80id\""},
		{"truncated multi-byte", "\"trunc\xe4\xb8\""},
		{"unpaired high surrogate", `"作业\uD800"`},
		{"unpaired low surrogate", `"\uDC00"`},
		{"ID case spelling, bad bytes", "\"bad\xff\""},
	}
	for _, tc := range damage {
		t.Run(tc.label, func(t *testing.T) {
			raw := rawJobEnvelopeCLI(tc.idJSON)
			if strings.HasPrefix(tc.label, "ID case") {
				raw = []byte(strings.Replace(string(raw), `"id":`+tc.idJSON,
					`"ID":`+tc.idJSON, 1))
			}
			dir := plantRawDataFile(t, raw)

			commands := [][]string{
				{"job-get", "--dir", dir, "--id", "j1"},
				{"job-list", "--dir", dir},
				{"job-submit", "--dir", dir, "--id", "j2", "--name", "c", "--version", "1"},
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
			}

			// The failure names the damaged record (by position when the id
			// cannot be recovered) and the id field.
			r := callCLI(t, "job-list", "--dir", dir)
			if !strings.Contains(r.err, "job record #2") || !strings.Contains(r.err, `"id"`) {
				t.Fatalf("error does not name the job record and id field: %q", r.err)
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

// Legal Unicode ids — Chinese, emoji, an actually committed "�", a surrogate
// pair and the literal six characters \uD800 — submit, save and read back
// under their exact value through the CLI.
func TestCLIUnicodeJobIDsRoundTrip(t *testing.T) {
	ids := []string{"作业-1", "emoji 😀 job", "has�mark", `literal\uD800`}
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			hash := seedBoundJobCLI(t, dir)
			r := callCLI(t, "job-submit", "--dir", dir, "--id", id,
				"--name", "mul", "--version", "1", "--hash", hash)
			if r.code != 0 {
				t.Fatalf("submit: code=%d err=%s", r.code, r.err)
			}
			if !strings.Contains(r.out, "id="+id) {
				t.Fatalf("submit output does not carry the exact id: %q", r.out)
			}
			r = callCLI(t, "job-get", "--dir", dir, "--id", id)
			if r.code != 0 || !strings.Contains(r.out, "id="+id) {
				t.Fatalf("id did not round-trip: code=%d out=%q err=%q", r.code, r.out, r.err)
			}
		})
	}
}
