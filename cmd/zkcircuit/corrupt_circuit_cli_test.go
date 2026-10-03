package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the CLI contract for a damaged circuit record field:
// every data command fails at open with exit code 3, the error names the
// damaged field, nothing is printed on stdout, and data.json is left
// byte-for-byte in place.

// TestCLICorruptCircuitRecordExitCode removes or nulls each required scalar
// field of the stored circuit record and drives reads and writes through the
// CLI: all must exit 3 naming the field, without touching the file.
func TestCLICorruptCircuitRecordExitCode(t *testing.T) {
	damage := []struct {
		name   string
		field  string
		mutate func(map[string]any)
	}{
		{"missing frozen", "frozen", func(env map[string]any) {
			delete(cliCircuit(env), "frozen")
		}},
		{"null frozen", "frozen", func(env map[string]any) {
			cliCircuit(env)["frozen"] = nil
		}},
		{"missing version", "version", func(env map[string]any) {
			delete(cliCircuit(env), "version")
		}},
		{"string constraints", "constraints", func(env map[string]any) {
			cliCircuit(env)["constraints"] = "1"
		}},
		{"null public inputs", "public_inputs", func(env map[string]any) {
			cliCircuit(env)["public_inputs"] = nil
		}},
		{"null description", "description", func(env map[string]any) {
			cliCircuit(env)["description"] = nil
		}},
	}

	for _, tc := range damage {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			bad := seedCorruptibleStore(t, dir, false, tc.mutate)

			commands := [][]string{
				{"circuit-list", "--dir", dir},
				{"circuit-get", "--dir", dir, "--name", "mul", "--version", "1"},
				{"circuit-freeze", "--dir", dir, "--name", "mul", "--version", "1"},
				{"circuit-compile", "--dir", dir, "--name", "mul", "--version", "1"},
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
				if !strings.Contains(r.err, `"`+tc.field+`"`) {
					t.Fatalf("%s: error does not name the damaged field %q: %q", args[0], tc.field, r.err)
				}
			}

			left, err := os.ReadFile(filepath.Join(dir, "data.json"))
			if err != nil {
				t.Fatal(err)
			}
			if string(left) != string(bad) {
				t.Fatal("data.json changed after refused commands")
			}
		})
	}
}

// TestCLIDuplicateCircuitFieldExitCode writes a duplicated "frozen" member
// directly (a parsed map cannot hold one): the CLI must refuse with exit 3.
func TestCLIDuplicateCircuitFieldExitCode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	bad := []byte(`{"format":1,"circuits":[{` +
		`"name":"mul","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,` +
		`"frozen":false,"description":"d","frozen":false}],"setups":[],"jobs":[]}` + "\n")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data.json"), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	r := callCLI(t, "circuit-list", "--dir", dir)
	if r.code != 3 {
		t.Fatalf("want exit 3, got %d (out=%q err=%q)", r.code, r.out, r.err)
	}
	if r.out != "" {
		t.Fatalf("printed a record despite corruption: %q", r.out)
	}
	if !strings.Contains(r.err, "data corrupt") || !strings.Contains(r.err, `"frozen"`) {
		t.Fatalf("error does not name the duplicated field: %q", r.err)
	}
	left, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(bad) {
		t.Fatal("data.json changed after refused command")
	}
}

// TestCLICircuitRecordZeroValuesStillLegal keeps the legitimate boundary at
// the CLI: explicit zero input counts, frozen:false and an empty description
// load and list normally.
func TestCLICircuitRecordZeroValuesStillLegal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	seedCorruptibleStore(t, dir, false, func(env map[string]any) {
		c := cliCircuit(env)
		c["public_inputs"] = 0
		c["private_inputs"] = 0
		c["frozen"] = false
		c["description"] = ""
		// Zero inputs cannot host the seeded definition's wires; drop it to
		// the counts-only state so the record itself is the legal boundary.
		delete(c, "definition")
	})

	r := callCLI(t, "circuit-get", "--dir", dir, "--name", "mul", "--version", "1")
	if r.code != 0 {
		t.Fatalf("explicit zero/false/empty values refused: code=%d err=%s", r.code, r.err)
	}
	if !strings.Contains(r.out, "frozen=false") || !strings.Contains(r.out, `description=""`) {
		t.Fatalf("stored zero values not reported: %q", r.out)
	}
}
