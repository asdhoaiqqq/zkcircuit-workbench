package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the CLI contract for a non-ASCII lookalike spelling of
// the compiled-artifact binding field: "compiled_haſh" (U+017F long s,
// written directly or as the JSON escape u017f) is matched onto
// compiled_hash by encoding/json's Unicode-folded tag matching, but it is
// not an accepted spelling of the binding. A record carrying it — even with
// a legal hash or an empty string, and even beside a correctly spelled
// compiled_hash — is data corruption: every data command fails at open with
// exit code 3, prints no record, names the damaged job and the compiled_hash
// field, and leaves data.json byte-for-byte in place.

// cliLongS is one literal long-s rune; cliJSONEscape is one literal
// backslash, so the escaped spelling keeps its backslash in source.
const cliLongS = "ſ"
const cliJSONEscape = `\`

// rewriteJobFieldCLI replaces the committed j1 binding field with an
// arbitrary raw JSON fragment and returns the damaged bytes now on disk.
func rewriteJobFieldCLI(t *testing.T, dir, hash, fragment string) []byte {
	t.Helper()
	path := filepath.Join(dir, "data.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	field := `"compiled_hash": "` + hash + `"`
	if !strings.Contains(string(raw), field) {
		t.Fatalf("committed file does not carry the bound field %q", field)
	}
	bad := []byte(strings.Replace(string(raw), field, fragment, 1))
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	return bad
}

// TestCLIJobCompiledHashLookalikeExitCode: a long-s spelling of the binding
// key, direct or escaped, with null / a legal hash / an empty string as its
// value, makes every data command exit 3 without touching the file.
func TestCLIJobCompiledHashLookalikeExitCode(t *testing.T) {
	direct := "compiled_ha" + cliLongS + "h"
	escaped := "compiled_ha" + cliJSONEscape + "u017f" + "h"
	cases := []struct {
		name    string
		keyText string
		value   string
	}{
		{"direct long s, null", direct, "null"},
		{"direct long s, legal hash", direct, `"deadbeef"`},
		{"direct long s, empty string", direct, `""`},
		{"escaped long s, null", escaped, "null"},
		{"escaped long s, legal hash", escaped, `"deadbeef"`},
		{"escaped long s, empty string", escaped, `""`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			hash := seedBoundJobCLI(t, dir)
			bad := rewriteJobFieldCLI(t, dir, hash, `"`+tc.keyText+`": `+tc.value)

			commands := [][]string{
				{"job-get", "--dir", dir, "--id", "j1"},
				{"job-list", "--dir", dir},
				{"job-submit", "--dir", dir, "--id", "j2", "--name", "mul", "--version", "1"},
				{"circuit-list", "--dir", dir},
				{"circuit-create", "--dir", dir, "--name", "other", "--version", "1"},
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
			}

			// The failure names the damaged job and the canonical binding field.
			r := callCLI(t, "job-get", "--dir", dir, "--id", "j1")
			if !strings.Contains(r.err, `"j1"`) || !strings.Contains(r.err, "compiled_hash") {
				t.Fatalf("error does not name the damaged job and field: %q", r.err)
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
}

// TestCLIJobCompiledHashLookalikeBesideCanonical: a long-s spelling together
// with a correctly spelled compiled_hash is refused in either key order and
// whether the two values agree — even when both carry the real artifact
// hash. The CLI never selects one of the values and continues.
func TestCLIJobCompiledHashLookalikeBesideCanonical(t *testing.T) {
	direct := "compiled_ha" + cliLongS + "h"
	escaped := "compiled_ha" + cliJSONEscape + "u017f" + "h"
	cases := []struct {
		name     string
		fragment func(hash string) string
	}{
		{"canonical then direct, same hash", func(h string) string {
			return `"compiled_hash": "` + h + `", "` + direct + `": "` + h + `"`
		}},
		{"direct then canonical, null vs hash", func(h string) string {
			return `"` + direct + `": null, "compiled_hash": "` + h + `"`
		}},
		{"canonical then escaped, same hash", func(h string) string {
			return `"compiled_hash": "` + h + `", "` + escaped + `": "` + h + `"`
		}},
		{"escaped then canonical, empty vs hash", func(h string) string {
			return `"` + escaped + `": "", "compiled_hash": "` + h + `"`
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			hash := seedBoundJobCLI(t, dir)
			bad := rewriteJobFieldCLI(t, dir, hash, tc.fragment(hash))

			for _, args := range [][]string{
				{"job-get", "--dir", dir, "--id", "j1"},
				{"job-list", "--dir", dir},
			} {
				r := callCLI(t, args...)
				if r.code != 3 || r.out != "" ||
					!strings.Contains(r.err, "read failed") || !strings.Contains(r.err, "compiled_hash") {
					t.Fatalf("%s: want exit 3 read failure naming compiled_hash with no output, got code=%d out=%q err=%q",
						args[0], r.code, r.out, r.err)
				}
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

// TestCLIJobCompiledHashASCIICaseVariantStillReads: only non-ASCII
// lookalikes are refused; an ASCII case variant (including a JSON-escaped
// one) still reads the binding as before.
func TestCLIJobCompiledHashASCIICaseVariantStillReads(t *testing.T) {
	cases := []struct {
		name    string
		keyText string
	}{
		{"upper case", "COMPILED_HASH"},
		{"escaped underscore", "compiled" + cliJSONEscape + "u005f" + "hash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			hash := seedBoundJobCLI(t, dir)
			rewriteJobFieldCLI(t, dir, hash, `"`+tc.keyText+`": "`+hash+`"`)

			r := callCLI(t, "job-get", "--dir", dir, "--id", "j1")
			if r.code != 0 {
				t.Fatalf("ASCII spelling %q must load: code=%d err=%s", tc.keyText, r.code, r.err)
			}
			if !strings.Contains(r.out, "compiled_hash="+hash) {
				t.Fatalf("binding under %q not displayed: %q", tc.keyText, r.out)
			}
		})
	}
}
