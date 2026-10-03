package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the CLI contract for a damaged committed definition:
// every data command fails at open with exit code 3 (the existing read-
// failure code), prints no success record / artifact / input-check verdict,
// and leaves data.json byte-for-byte in place.
const cliCorruptDef = `{"modulus":"7","constraints":[
	{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`

type cliResult struct {
	code int
	out  string
	err  string
}

func callCLI(t *testing.T, args ...string) cliResult {
	t.Helper()
	code, out, errOut := runCapture(t, args...)
	return cliResult{code, out, errOut}
}

// seedCorruptibleStore drives the CLI through a legal create + import
// (optionally freeze + compile), applies mutate to the parsed data.json and
// returns the damaged bytes now on disk.
func seedCorruptibleStore(t *testing.T, dir string, freezeAndCompile bool, mutate func(map[string]any)) []byte {
	t.Helper()
	work := filepath.Dir(dir)
	defPath := writeFile(t, work, "def.json", cliCorruptDef)
	must := func(what string, r cliResult) {
		t.Helper()
		if r.code != 0 {
			t.Fatalf("%s: code=%d err=%s", what, r.code, r.err)
		}
	}
	must("create", callCLI(t, "circuit-create", "--dir", dir, "--name", "mul", "--version", "1",
		"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1"))
	must("import", callCLI(t, "constraint-import", "--dir", dir, "--name", "mul", "--version", "1",
		"--file", defPath))
	if freezeAndCompile {
		must("freeze", callCLI(t, "circuit-freeze", "--dir", dir, "--name", "mul", "--version", "1"))
		must("compile", callCLI(t, "circuit-compile", "--dir", dir, "--name", "mul", "--version", "1"))
	}

	path := filepath.Join(dir, "data.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	mutate(env)
	bad, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	bad = append(bad, '\n')
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	return bad
}

// TestCLICorruptDefinitionExitCode drives each shape of damage through the
// CLI: regardless of draft or frozen/compiled state, reads, compilation and
// input checks must exit 3 without touching the file.
func TestCLICorruptDefinitionExitCode(t *testing.T) {
	damage := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing side b", func(env map[string]any) {
			delete(cliConstraint(env), "b")
		}},
		{"missing side a", func(env map[string]any) {
			delete(cliConstraint(env), "a")
		}},
		{"null side a", func(env map[string]any) {
			cliConstraint(env)["a"] = nil
		}},
		{"null term object", func(env map[string]any) {
			cliConstraint(env)["a"].([]any)[0] = nil
		}},
		{"missing modulus", func(env map[string]any) {
			delete(cliDefinition(env), "modulus")
		}},
		{"null constraints", func(env map[string]any) {
			cliDefinition(env)["constraints"] = nil
		}},
		{"null wire", func(env map[string]any) {
			cliConstraint(env)["a"].([]any)[0].(map[string]any)["wire"] = nil
		}},
		{"missing coeff", func(env map[string]any) {
			delete(cliConstraint(env)["a"].([]any)[0].(map[string]any), "coeff")
		}},
	}

	for _, frozen := range []bool{false, true} {
		state := "draft"
		if frozen {
			state = "frozen-compiled"
		}
		t.Run(state, func(t *testing.T) {
			for _, tc := range damage {
				t.Run(tc.name, func(t *testing.T) {
					dir := filepath.Join(t.TempDir(), "bench")
					bad := seedCorruptibleStore(t, dir, frozen, tc.mutate)

					goodInput := writeFile(t, filepath.Dir(dir), "in.json", cliGoodInput)
					commands := [][]string{
						{"circuit-list", "--dir", dir},
						{"circuit-get", "--dir", dir, "--name", "mul", "--version", "1"},
						{"circuit-compile", "--dir", dir, "--name", "mul", "--version", "1"},
						{"input-check", "--dir", dir, "--name", "mul", "--version", "1",
							"--hash", strings.Repeat("0", 64), "--file", goodInput},
					}
					if frozen {
						commands = append(commands, []string{"job-list", "--dir", dir})
					} else {
						// A mutating command must likewise refuse before writing.
						commands = append(commands, []string{"circuit-freeze", "--dir", dir, "--name", "mul", "--version", "1"})
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
		})
	}
}

// TestCLIDefinitionNullStillCountsOnly keeps the legacy path working at the
// CLI: a record whose definition is null opens and reports a missing
// definition on compile (exit 1), never a read failure.
func TestCLIDefinitionNullStillCountsOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	seedCorruptibleStore(t, dir, false, func(env map[string]any) {
		cliCircuit(env)["definition"] = nil
	})

	if r := callCLI(t, "circuit-list", "--dir", dir); r.code != 0 {
		t.Fatalf("null definition must load: code=%d err=%s", r.code, r.err)
	}
	if r := callCLI(t, "circuit-freeze", "--dir", dir, "--name", "mul", "--version", "1"); r.code != 0 {
		t.Fatalf("freeze: %s", r.err)
	}
	if r := callCLI(t, "circuit-compile", "--dir", dir, "--name", "mul", "--version", "1"); r.code != 1 ||
		!strings.Contains(r.err, "constraint definition missing") {
		t.Fatalf("null definition compile: code=%d err=%s", r.code, r.err)
	}
}

func cliCircuit(env map[string]any) map[string]any {
	return env["circuits"].([]any)[0].(map[string]any)
}

func cliDefinition(env map[string]any) map[string]any {
	return cliCircuit(env)["definition"].(map[string]any)
}

func cliConstraint(env map[string]any) map[string]any {
	return cliDefinition(env)["constraints"].([]any)[0].(map[string]any)
}
