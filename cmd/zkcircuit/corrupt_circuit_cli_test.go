package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the CLI contract for a damaged committed circuit
// record: when name/version/constraints/public_inputs/private_inputs/frozen/
// description is missing, null, mistyped or duplicated, every data command
// must fail at read time with exit code 3, name the damaged field, print no
// success output, and leave data.json byte-for-byte in place.

// cliFieldDamage damages one top-level field of the first circuit record
// through the generic JSON representation.
func cliFieldDamage(t *testing.T, dir string, mutate func(map[string]any)) []byte {
	t.Helper()
	path := filepath.Join(dir, "data.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	mutate(cliCircuit(env))
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

// cliDuplicateField rewrites the first circuit record's JSON text so that
// field appears a second time with extra. escaped spells one byte of the
// field key as a \u00XX escape.
func cliDuplicateField(t *testing.T, dir, field, extra string, escaped bool) []byte {
	t.Helper()
	path := filepath.Join(dir, "data.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Circuits []json.RawMessage `json:"circuits"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	rec := []byte(wire.Circuits[0])
	var compact bytes.Buffer
	if err := json.Compact(&compact, rec); err != nil {
		t.Fatal(err)
	}
	key := `"` + field + `"`
	if escaped {
		key = "\"\\u" + toHex(field[0]) + field[1:] + "\""
	}
	dup := append([]byte(nil), compact.Bytes()[:len(compact.Bytes())-1]...)
	dup = append(dup, []byte(","+key+":"+extra+"}")...)
	bad := bytes.Replace(raw, rec, dup, 1)
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	return bad
}

func toHex(b byte) string {
	const hex = "0123456789abcdef"
	return string([]byte{hex[b>>4], hex[b&0xf]})
}

// assertCLIReadFailure runs commands against a damaged directory and checks
// the exit-3 contract: the error names both the damaged field and the
// circuit record, stdout stays empty and data.json is never rewritten.
func assertCLIReadFailure(t *testing.T, dir string, bad []byte, field string, frozen bool, commands ...[]string) {
	t.Helper()
	if len(commands) == 0 {
		commands = [][]string{
			{"circuit-list", "--dir", dir},
			{"circuit-get", "--dir", dir, "--name", "mul", "--version", "1"},
			{"job-list", "--dir", dir},
		}
		if frozen {
			commands = append(commands,
				[]string{"circuit-compile", "--dir", dir, "--name", "mul", "--version", "1"})
		} else {
			commands = append(commands,
				[]string{"circuit-freeze", "--dir", dir, "--name", "mul", "--version", "1"},
				[]string{"circuit-update", "--dir", dir, "--name", "mul", "--version", "1",
					"--constraints", "2"})
		}
	}
	for _, args := range commands {
		r := callCLI(t, args...)
		if r.code != 3 {
			t.Fatalf("%s: want exit 3, got %d (out=%q err=%q)", args[0], r.code, r.out, r.err)
		}
		if r.out != "" {
			t.Fatalf("%s printed a success record despite corruption: %q", args[0], r.out)
		}
		for _, want := range []string{"read failed", "data corrupt", field} {
			if !strings.Contains(r.err, want) {
				t.Fatalf("%s error missing %q: %s", args[0], want, r.err)
			}
		}
	}
	left, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(bad) {
		t.Fatalf("data.json changed after refused commands")
	}
}

// TestCLICorruptCircuitFieldExitCode damages each required field and drives
// representative reads and mutations through the CLI in both draft and
// frozen+compiled states.
func TestCLICorruptCircuitFieldExitCode(t *testing.T) {
	damage := []struct {
		name   string
		field  string
		mutate func(map[string]any)
	}{
		{"missing name", "name", func(c map[string]any) { delete(c, "name") }},
		{"null name", "name", func(c map[string]any) { c["name"] = nil }},
		{"numeric name", "name", func(c map[string]any) { c["name"] = 7 }},
		{"missing version", "version", func(c map[string]any) { delete(c, "version") }},
		{"null version", "version", func(c map[string]any) { c["version"] = nil }},
		{"string version", "version", func(c map[string]any) { c["version"] = "1" }},
		{"missing constraints", "constraints", func(c map[string]any) { delete(c, "constraints") }},
		{"null constraints", "constraints", func(c map[string]any) { c["constraints"] = nil }},
		{"missing public_inputs", "public_inputs", func(c map[string]any) { delete(c, "public_inputs") }},
		{"null public_inputs", "public_inputs", func(c map[string]any) { c["public_inputs"] = nil }},
		{"wrong public_inputs", "public_inputs", func(c map[string]any) { c["public_inputs"] = true }},
		{"missing private_inputs", "private_inputs", func(c map[string]any) { delete(c, "private_inputs") }},
		{"null private_inputs", "private_inputs", func(c map[string]any) { c["private_inputs"] = nil }},
		{"missing frozen", "frozen", func(c map[string]any) { delete(c, "frozen") }},
		{"null frozen", "frozen", func(c map[string]any) { c["frozen"] = nil }},
		{"string frozen", "frozen", func(c map[string]any) { c["frozen"] = "false" }},
		{"missing description", "description", func(c map[string]any) { delete(c, "description") }},
		{"null description", "description", func(c map[string]any) { c["description"] = nil }},
		{"numeric description", "description", func(c map[string]any) { c["description"] = 1 }},
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
					seedCorruptibleStore(t, dir, frozen, func(env map[string]any) {})
					bad := cliFieldDamage(t, dir, tc.mutate)
					assertCLIReadFailure(t, dir, bad, tc.field, frozen)
				})
			}
		})
	}
}

// TestCLIDuplicateCircuitFieldExitCode: a field appearing twice — same value,
// different value, or under a JSON-escaped spelling — is corruption even
// when both occurrences agree.
func TestCLIDuplicateCircuitFieldExitCode(t *testing.T) {
	cases := []struct {
		name   string
		field  string
		extra  string
		escape bool
	}{
		{"frozen differing", "frozen", "true", false},
		{"frozen same value", "frozen", "false", false},
		{"frozen escaped key", "frozen", "false", true},
		{"name same value", "name", `"mul"`, false},
		{"version same value", "version", "1", false},
		{"description same value", "description", `""`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			seedCorruptibleStore(t, dir, false, func(env map[string]any) {})
			bad := cliDuplicateField(t, dir, tc.field, tc.extra, tc.escape)
			assertCLIReadFailure(t, dir, bad, tc.field, false)
		})
	}
}

// TestCLIExplicitZeroFalseEmptyLoads: counts of 0, frozen:false and an empty
// description are legitimate explicit values — the directory loads and
// queries print them normally.
func TestCLIExplicitZeroFalseEmptyLoads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	r := callCLI(t, "circuit-create", "--dir", dir, "--name", "bare", "--version", "1",
		"--constraints", "1", "--public-inputs", "0", "--private-inputs", "0",
		"--description", "")
	if r.code != 0 {
		t.Fatalf("create: %s", r.err)
	}
	// Reload: the stored record must read back with the explicit zero/empty
	// values, not be mistaken for a damaged, field-less record.
	r = callCLI(t, "circuit-get", "--dir", dir, "--name", "bare", "--version", "1")
	if r.code != 0 {
		t.Fatalf("get: code=%d err=%s", r.code, r.err)
	}
	for _, want := range []string{"public_inputs=0", "private_inputs=0", "frozen=false", `description=""`} {
		if !strings.Contains(r.out, want) {
			t.Fatalf("output missing %q: %q", want, r.out)
		}
	}
	// The on-disk record explicitly carries frozen and description even at
	// their zero values.
	raw, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"frozen"`, `"description"`, `"public_inputs"`, `"private_inputs"`} {
		if !bytes.Contains(raw, []byte(key)) {
			t.Fatalf("data.json does not explicitly persist %s", key)
		}
	}
}
