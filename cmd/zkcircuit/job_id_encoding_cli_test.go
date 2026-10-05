package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the CLI contract of the proof-job id identity rule.
//
// job-submit with an id that is not complete, valid UTF-8 exits 1 (the
// business-rule failure code), prints no accepted record and commits nothing
// — the submitted bytes would otherwise be silently rewritten to "�" when the
// job is saved, and a query by the original bytes would miss it.
//
// A committed data.json whose job id carries invalid UTF-8 bytes or an
// unpaired \uXXXX surrogate escape (including under an ASCII case or
// JSON-escaped id key) is data corruption: every data command exits 3, no
// partial query result is printed, no modification commits, and the file
// stays byte-for-byte in place. Legal Chinese, emoji, a real "�" and the
// literal text \uD800 submit and read back under their exact id.

// seedJobTarget drives the CLI through create + freeze + setup for mul@1 so a
// job-submit meets every gate but the id encoding itself.
func seedJobTarget(t *testing.T, dir string) {
	t.Helper()
	must := func(what string, r cliResult) {
		t.Helper()
		if r.code != 0 {
			t.Fatalf("%s: code=%d err=%s", what, r.code, r.err)
		}
	}
	must("create", callCLI(t, "circuit-create", "--dir", dir, "--name", "mul", "--version", "1",
		"--constraints", "1"))
	must("freeze", callCLI(t, "circuit-freeze", "--dir", dir, "--name", "mul", "--version", "1"))
	must("setup", callCLI(t, "setup-record", "--dir", dir, "--name", "mul", "--version", "1"))
}

func TestCLISubmitRejectsInvalidUTF8ID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	seedJobTarget(t, dir)
	if r := callCLI(t, "job-submit", "--dir", dir, "--id", "keep", "--name", "mul",
		"--version", "1", "--attempt", "1"); r.code != 0 {
		t.Fatalf("seed job: code=%d err=%s", r.code, r.err)
	}
	dataPath := filepath.Join(dir, "data.json")
	before, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}

	bad := []struct {
		label string
		id    string
	}{
		{"lone continuation byte", "job\x80id"},
		{"truncated multi-byte", "trunc\xe4\xb8"},
		{"invalid byte 0xff", "bad\xff"},
		{"encoded surrogate half", "pair\xed\xa0\x80"},
		{"truncated at end", "end\xc3"},
	}
	for _, tc := range bad {
		t.Run(tc.label, func(t *testing.T) {
			r := callCLI(t, "job-submit", "--dir", dir, "--id", tc.id, "--name", "mul",
				"--version", "1", "--attempt", "1")
			if r.code != 1 {
				t.Fatalf("want exit 1, got %d (out=%q err=%q)", r.code, r.out, r.err)
			}
			if strings.Contains(r.out, "job accepted") {
				t.Fatalf("an accepted record was printed for a refused submit: %q", r.out)
			}
			if !strings.Contains(strings.ToLower(r.err), "utf-8") {
				t.Fatalf("error does not explain the id encoding is invalid: %q", r.err)
			}
			after, err := os.ReadFile(dataPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("refused submit modified data.json\nwant: %q\n got: %q", before, after)
			}
		})
	}

	// The previously committed job is still there, under its own id.
	r := callCLI(t, "job-get", "--dir", dir, "--id", "keep")
	if r.code != 0 || !strings.Contains(r.out, "id=keep") {
		t.Fatalf("existing job damaged by refused submits: code=%d out=%q err=%q", r.code, r.out, r.err)
	}
	r = callCLI(t, "job-list", "--dir", dir)
	if r.code != 0 || strings.Count(r.out, "job:") != 1 || !strings.Contains(r.out, "id=keep") {
		t.Fatalf("job list after refused submits: code=%d out=%q", r.code, r.out)
	}
}

// rewriteJobID seeds two legal jobs and replaces the id token of job id with
// the given raw JSON token (and optionally a raw id member key), returning the
// damaged bytes on disk.
func rewriteJobID(t *testing.T, dir, id, keyJSON, idJSON string) []byte {
	t.Helper()
	path := filepath.Join(dir, "data.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := `"id": "` + id + `"`
	if !strings.Contains(string(raw), old) {
		t.Fatalf("committed file does not carry %q", old)
	}
	repl := keyJSON + ": " + idJSON
	bad := []byte(strings.Replace(string(raw), old, repl, 1))
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	return bad
}

func TestCLICorruptJobIDExitCode3(t *testing.T) {
	damage := []struct {
		label   string
		idJSON  string
		recordN string
	}{
		{"invalid byte", "\"bad\xff\"", "#2"},
		{"lone continuation byte", "\"job\x80id\"", "#2"},
		{"truncated multi-byte", "\"trunc\xe4\xb8\"", "#2"},
		{"unpaired high surrogate", `"job\uD800"`, "#2"},
		{"unpaired low surrogate", `"\uDC00"`, "#2"},
		{"high after valid pair", `"😀\uD83D"`, "#2"},
	}
	for _, tc := range damage {
		t.Run(tc.label, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			seedJobTarget(t, dir)
			must := func(r cliResult) {
				t.Helper()
				if r.code != 0 {
					t.Fatalf("seed job: code=%d err=%s", r.code, r.err)
				}
			}
			must(callCLI(t, "job-submit", "--dir", dir, "--id", "j1", "--name", "mul",
				"--version", "1", "--attempt", "1"))
			must(callCLI(t, "job-submit", "--dir", dir, "--id", "j2", "--name", "mul",
				"--version", "1", "--attempt", "1"))
			bad := rewriteJobID(t, dir, "j2", `"id"`, tc.idJSON)

			commands := [][]string{
				{"job-get", "--dir", dir, "--id", "j1"},
				{"job-list", "--dir", dir},
				{"job-submit", "--dir", dir, "--id", "j3", "--name", "mul", "--version", "1", "--attempt", "1"},
				{"circuit-list", "--dir", dir},
			}
			for _, args := range commands {
				r := callCLI(t, args...)
				if r.code != 3 {
					t.Fatalf("%v: want exit 3, got %d (out=%q err=%q)", args, r.code, r.out, r.err)
				}
				if r.out != "" {
					t.Fatalf("%v printed a partial record despite corruption: %q", args[0], r.out)
				}
				if !strings.Contains(r.err, "read failed") || !strings.Contains(r.err, "data corrupt") {
					t.Fatalf("%v unexpected error text %q", args[0], r.err)
				}
			}
			// The failure names the damaged record (by number) and the id field.
			r := callCLI(t, "job-list", "--dir", dir)
			if !strings.Contains(r.err, "job record "+tc.recordN) || !strings.Contains(r.err, `"id"`) {
				t.Fatalf("error does not name the job record and id field: %q", r.err)
			}
			left, err := os.ReadFile(filepath.Join(dir, "data.json"))
			if err != nil {
				t.Fatal(err)
			}
			if string(left) != string(bad) {
				t.Fatalf("data.json changed across refused commands")
			}
		})
	}
}

func TestCLICorruptJobIDUnderCaseOrEscapedKeyExitCode3(t *testing.T) {
	cases := []struct {
		label   string
		keyJSON string
		idJSON  string
	}{
		{"upper case id key", `"ID"`, "\"bad\xff\""},
		{"mixed case id key", `"Id"`, `"\uDC00"`},
		{"escaped uppercase key", `"\u0049D"`, "\"bad\x80x\""},
		{"escaped lowercase key", `"\u0069d"`, `"end\uD800"`},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			seedJobTarget(t, dir)
			if r := callCLI(t, "job-submit", "--dir", dir, "--id", "j1", "--name", "mul",
				"--version", "1", "--attempt", "1"); r.code != 0 {
				t.Fatalf("seed job: code=%d err=%s", r.code, r.err)
			}
			bad := rewriteJobID(t, dir, "j1", tc.keyJSON, tc.idJSON)

			r := callCLI(t, "job-list", "--dir", dir)
			if r.code != 3 || !strings.Contains(r.err, "read failed") ||
				!strings.Contains(r.err, `"id"`) {
				t.Fatalf("want exit 3 read failure naming the id field, got code=%d err=%q", r.code, r.err)
			}
			left, err := os.ReadFile(filepath.Join(dir, "data.json"))
			if err != nil {
				t.Fatal(err)
			}
			if string(left) != string(bad) {
				t.Fatalf("data.json changed after refused command")
			}
		})
	}
}

// Legal Unicode ids — Chinese, emoji, an actually committed "�" and the
// literal six characters \uD800 — are ordinary ids: they submit, save and read
// back under their exact value and appear in the list unchanged.
func TestCLIUnicodeJobIDsRoundTrip(t *testing.T) {
	ids := []string{"任务", "job 😀", "has�mark", `literal\uD800`}
	dir := filepath.Join(t.TempDir(), "bench")
	seedJobTarget(t, dir)
	for _, id := range ids {
		r := callCLI(t, "job-submit", "--dir", dir, "--id", id, "--name", "mul",
			"--version", "1", "--attempt", "1")
		if r.code != 0 {
			t.Fatalf("submit %q: code=%d err=%s", id, r.code, r.err)
		}
	}
	for _, id := range ids {
		r := callCLI(t, "job-get", "--dir", dir, "--id", id)
		if r.code != 0 || !strings.Contains(r.out, "id="+id) {
			t.Fatalf("id %q did not round-trip: code=%d out=%q err=%q", id, r.code, r.out, r.err)
		}
	}
	r := callCLI(t, "job-list", "--dir", dir)
	if r.code != 0 || strings.Count(r.out, "job:") != len(ids) {
		t.Fatalf("job list mismatch: code=%d out=%q", r.code, r.out)
	}
	for _, id := range ids {
		if !strings.Contains(r.out, "id="+id) {
			t.Fatalf("list missing exact id %q: %q", id, r.out)
		}
	}
}
