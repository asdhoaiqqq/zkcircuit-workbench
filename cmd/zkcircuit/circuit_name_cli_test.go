package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// These tests pin the CLI contract of the circuit-name identity rule: a
// create submitted with a name that is not complete, legal UTF-8 exits 1
// with no success record and nothing persisted; a committed name that can no
// longer be read back losslessly (invalid UTF-8 bytes or an unpaired
// surrogate escape in data.json) makes every command on the directory exit 3
// without partial results and without touching the file.

// TestCLICreateInvalidUTF8Name: the create entry refuses names carrying lone
// continuation bytes or truncated multi-byte characters with the rule-
// failure exit code, prints no "circuit version:" record and commits
// nothing — on a fresh directory no data.json appears at all, and beside
// existing records every byte of committed state is preserved.
func TestCLICreateInvalidUTF8Name(t *testing.T) {
	names := map[string]string{
		"lone continuation byte": "bad\x80name",
		"truncated two-byte":     "bad\xc2",
		"truncated three-byte":   "\xe7\x94",
	}
	for label, name := range names {
		t.Run(label, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")

			r := callCLI(t, "circuit-create", "--dir", dir, "--name", name, "--version", "1",
				"--constraints", "1")
			if r.code != 1 {
				t.Fatalf("want exit 1, got %d (out=%q err=%q)", r.code, r.out, r.err)
			}
			if r.out != "" {
				t.Fatalf("a success record was printed for a refused name: %q", r.out)
			}
			if !strings.Contains(r.err, "invalid argument") {
				t.Fatalf("unexpected error text %q", r.err)
			}
			if _, err := os.Stat(filepath.Join(dir, "data.json")); !os.IsNotExist(err) {
				t.Fatalf("refused create wrote a data file (stat err=%v)", err)
			}

			// Beside existing records the refusal still commits nothing.
			if r := callCLI(t, "circuit-create", "--dir", dir, "--name", "good", "--version", "1",
				"--constraints", "1"); r.code != 0 {
				t.Fatalf("seed create: code=%d err=%s", r.code, r.err)
			}
			before, err := os.ReadFile(filepath.Join(dir, "data.json"))
			if err != nil {
				t.Fatal(err)
			}
			r = callCLI(t, "circuit-create", "--dir", dir, "--name", name, "--version", "2",
				"--constraints", "1")
			if r.code != 1 || r.out != "" {
				t.Fatalf("want exit 1 and no output, got %d out=%q", r.code, r.out)
			}
			left, err := os.ReadFile(filepath.Join(dir, "data.json"))
			if err != nil {
				t.Fatal(err)
			}
			if string(left) != string(before) {
				t.Fatalf("refused create changed committed state")
			}
			r = callCLI(t, "circuit-list", "--dir", dir)
			if r.code != 0 || !strings.Contains(r.out, "name=good") || strings.Contains(r.out, "�") {
				t.Fatalf("existing records must survive untouched: code=%d out=%q", r.code, r.out)
			}
		})
	}
}

// TestCLICreateValidUnicodeNames: legal Chinese, emoji and the literal text
// \uD800 are ordinary names on the command line — created and read back
// under exactly the submitted string.
func TestCLICreateValidUnicodeNames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	names := []string{"电路", "🚀", "has � inside", `\uD800`}
	for i, name := range names {
		r := callCLI(t, "circuit-create", "--dir", dir, "--name", name,
			"--version", strconv.Itoa(i+1), "--constraints", "1")
		if r.code != 0 {
			t.Fatalf("create %q: code=%d err=%s", name, r.code, r.err)
		}
		if !strings.Contains(r.out, "name="+name+" ") {
			t.Fatalf("create %q echoed a different name: %q", name, r.out)
		}
	}
	for i, name := range names {
		r := callCLI(t, "circuit-get", "--dir", dir, "--name", name,
			"--version", strconv.Itoa(i+1))
		if r.code != 0 || !strings.Contains(r.out, "name="+name+" ") {
			t.Fatalf("get %q: code=%d out=%q err=%s", name, r.code, r.out, r.err)
		}
	}
}

// TestCLICorruptStoredNameExitCode: a committed name written with an
// unpaired surrogate escape ("电\uD800") or with invalid UTF-8 bytes makes
// every command on the directory exit 3 — no partial query results, no
// committed modification — and leaves data.json byte-for-byte in place.
func TestCLICorruptStoredNameExitCode(t *testing.T) {
	damage := []struct {
		name    string
		replace func(raw string) string
	}{
		{"unpaired surrogate escape", func(raw string) string {
			return strings.Replace(raw, `"mul"`, `"电\uD800"`, 1)
		}},
		{"invalid UTF-8 byte", func(raw string) string {
			return strings.Replace(raw, `"mul"`, "\"mu\x80l\"", 1)
		}},
	}
	for _, tc := range damage {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			for _, args := range [][]string{
				{"circuit-create", "--dir", dir, "--name", "mul", "--version", "1", "--constraints", "1"},
				{"circuit-create", "--dir", dir, "--name", "good", "--version", "9", "--constraints", "1"},
			} {
				if r := callCLI(t, args...); r.code != 0 {
					t.Fatalf("seed %s: code=%d err=%s", args[0], r.code, r.err)
				}
			}
			path := filepath.Join(dir, "data.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			bad := tc.replace(string(raw))
			if bad == string(raw) {
				t.Fatal("setup: name not found in committed file")
			}
			if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
				t.Fatal(err)
			}

			commands := [][]string{
				{"circuit-list", "--dir", dir},
				{"circuit-get", "--dir", dir, "--name", "good", "--version", "9"},
				{"circuit-create", "--dir", dir, "--name", "new", "--version", "1", "--constraints", "1"},
				{"circuit-freeze", "--dir", dir, "--name", "good", "--version", "9"},
			}
			for _, args := range commands {
				r := callCLI(t, args...)
				if r.code != 3 {
					t.Fatalf("%s: want exit 3, got %d (out=%q err=%q)", args[0], r.code, r.out, r.err)
				}
				if r.out != "" {
					t.Fatalf("%s printed a success record despite corruption: %q", args[0], r.out)
				}
				if !strings.Contains(r.err, "read failed") || !strings.Contains(r.err, "data corrupt") {
					t.Fatalf("%s: unexpected error text %q", args[0], r.err)
				}
				if !strings.Contains(r.err, `"name"`) {
					t.Fatalf("%s: error must name the damaged field: %q", args[0], r.err)
				}
			}

			left, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(left) != bad {
				t.Fatalf("data.json changed after refused commands")
			}
		})
	}
}
