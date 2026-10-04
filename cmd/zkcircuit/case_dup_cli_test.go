package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLI contract for a top-level field repeated under a different letter-case
// spelling (circuits + CIRCUITS, format + FORMAT, …): every data command
// fails at open with exit code 3, prints no success record, names the
// standard lowercase field, and leaves data.json byte-for-byte in place.

// seedCaseDupCLIStore drives the CLI through a legal create + freeze and
// returns the committed data.json bytes.
func seedCaseDupCLIStore(t *testing.T, dir string) []byte {
	t.Helper()
	must := func(what string, r cliResult) {
		t.Helper()
		if r.code != 0 {
			t.Fatalf("%s: code=%d err=%s", what, r.code, r.err)
		}
	}
	must("create", callCLI(t, "circuit-create", "--dir", dir, "--name", "mul", "--version", "1",
		"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1"))
	must("freeze", callCLI(t, "circuit-freeze", "--dir", dir, "--name", "mul", "--version", "1"))
	raw, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCLITopLevelCaseVariantDuplicateExitCode(t *testing.T) {
	damage := []struct {
		name    string
		replace func(raw string) string
		field   string
	}{
		{"circuits then CIRCUITS", func(raw string) string {
			return strings.Replace(raw, `"setups":`, `"CIRCUITS": [], "setups":`, 1)
		}, "circuits"},
		{"CIRCUITS then circuits", func(raw string) string {
			return strings.Replace(raw, `"circuits":`, `"CIRCUITS": [], "circuits":`, 1)
		}, "circuits"},
		{"format then FORMAT", func(raw string) string {
			return strings.Replace(raw, `"format": 1`, `"format": 1, "FORMAT": 999`, 1)
		}, "format"},
		{"FORMAT then format", func(raw string) string {
			return strings.Replace(raw, `"format": 1`, `"FORMAT": 999, "format": 1`, 1)
		}, "format"},
		{"escaped CIRCUITS", func(raw string) string {
			return strings.Replace(raw, `"setups":`, "\"\\u0043IRCUITS\": [], \"setups\":", 1)
		}, "circuits"},
	}
	for _, tc := range damage {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			raw := string(seedCaseDupCLIStore(t, dir))
			bad := tc.replace(raw)
			if bad == raw {
				t.Fatalf("replacement did not apply to committed file:\n%s", raw)
			}
			if err := os.WriteFile(filepath.Join(dir, "data.json"), []byte(bad), 0o644); err != nil {
				t.Fatal(err)
			}

			commands := [][]string{
				{"circuit-list", "--dir", dir},
				{"circuit-get", "--dir", dir, "--name", "mul", "--version", "1"},
				{"job-list", "--dir", dir},
				// Mutating commands must likewise refuse before writing.
				{"circuit-create", "--dir", dir, "--name", "other", "--version", "1", "--constraints", "1"},
				{"circuit-freeze", "--dir", dir, "--name", "mul", "--version", "1"},
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
				if !strings.Contains(r.err, `"`+tc.field+`"`) {
					t.Fatalf("%s: error does not name the standard field %q: %q", args[0], tc.field, r.err)
				}
			}

			left, err := os.ReadFile(filepath.Join(dir, "data.json"))
			if err != nil {
				t.Fatal(err)
			}
			if string(left) != bad {
				t.Fatalf("data.json changed after refused commands")
			}
		})
	}
}

// A single case-variant spelling of each top-level field keeps the CLI
// working: the committed circuit lists and reads back normally.
func TestCLITopLevelCaseVariantSingleFieldsLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	raw := string(seedCaseDupCLIStore(t, dir))
	renamed := strings.Replace(raw, `"format"`, `"FORMAT"`, 1)
	renamed = strings.Replace(renamed, `"circuits"`, `"Circuits"`, 1)
	renamed = strings.Replace(renamed, `"setups"`, `"SETUPS"`, 1)
	renamed = strings.Replace(renamed, `"jobs"`, `"Jobs"`, 1)
	if err := os.WriteFile(filepath.Join(dir, "data.json"), []byte(renamed), 0o644); err != nil {
		t.Fatal(err)
	}

	r := callCLI(t, "circuit-list", "--dir", dir)
	if r.code != 0 || !strings.Contains(r.out, "mul") {
		t.Fatalf("case-variant fields must list the circuit: code=%d out=%q err=%q", r.code, r.out, r.err)
	}
	r = callCLI(t, "circuit-get", "--dir", dir, "--name", "mul", "--version", "1")
	if r.code != 0 || !strings.Contains(r.out, "mul") {
		t.Fatalf("case-variant fields must read the circuit: code=%d out=%q err=%q", r.code, r.out, r.err)
	}
}
