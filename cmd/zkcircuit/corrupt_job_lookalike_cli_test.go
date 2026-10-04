package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLI contract for the non-ASCII lookalike spelling of the compiled-artifact
// binding field. encoding/json folds "compiled_haſh" (long s, U+017F,
// literal or JSON-escaped) onto the compiled_hash struct tag; a null there
// used to open as an unbound job and vanished on the next save. Every such
// spelling is data corruption: every data command exits 3, prints nothing,
// names the damaged job and the compiled_hash field, and leaves data.json
// byte-for-byte in place — in whichever key order the lookalike and the
// standard spelling occur and whether or not their values agree.

// replaceJobBinding swaps j1's committed binding member in data.json for
// member and returns the damaged bytes now on disk.
func replaceJobBinding(t *testing.T, dir, hash, member string) []byte {
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
	bad := []byte(strings.Replace(string(raw), field, member, 1))
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	return bad
}

func assertReadFailure(t *testing.T, args []string, bad []byte, dir string) {
	t.Helper()
	r := callCLI(t, args...)
	if r.code != 3 {
		t.Fatalf("%v: want exit 3, got %d (out=%q err=%q)", args, r.code, r.out, r.err)
	}
	if r.out != "" {
		t.Fatalf("%v printed output despite corruption: %q", args, r.out)
	}
	if !strings.Contains(r.err, "read failed") || !strings.Contains(r.err, "data corrupt") {
		t.Fatalf("%v: unexpected error text %q", args, r.err)
	}
	if !strings.Contains(r.err, `"j1"`) || !strings.Contains(r.err, "compiled_hash") {
		t.Fatalf("%v: error does not name the damaged job and compiled_hash field: %q", args, r.err)
	}
	left, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(bad) {
		t.Fatalf("%v: data.json changed after the refused command", args)
	}
}

// Both on-disk spellings (literal rune and JSON escape) of a lookalike key,
// with every shape of value, fail the read the same way.
func TestCLICorruptJobCompiledHashLookalikeExitCode(t *testing.T) {
	literalLongS := "ſ"
	escapedLongS := "\\u017f" // the six ASCII bytes backslash-u-0-1-7-f
	cases := []struct {
		name   string
		member func(h string) string
	}{
		{"literal long s null", func(string) string {
			return `"compiled_ha` + literalLongS + `h": null`
		}},
		{"escaped long s null", func(string) string {
			return `"compiled_ha` + escapedLongS + `h": null`
		}},
		{"literal long s legal hash", func(h string) string {
			return `"compiled_ha` + literalLongS + `h": "` + h + `"`
		}},
		{"escaped long s empty string", func(string) string {
			return `"compiled_ha` + escapedLongS + `h": ""`
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			hash := seedBoundJobCLI(t, dir)
			bad := replaceJobBinding(t, dir, hash, tc.member(hash))

			for _, args := range [][]string{
				{"job-get", "--dir", dir, "--id", "j1"},
				{"job-list", "--dir", dir},
				{"circuit-list", "--dir", dir},
				// An unrelated modification must not rewrite the damage away.
				{"circuit-create", "--dir", dir, "--name", "other", "--version", "1"},
			} {
				assertReadFailure(t, args, bad, dir)
			}
		})
	}
}

// When the lookalike and the standard spelling coexist, the whole directory
// read fails in whichever order the keys appear and whether the two values
// agree; the command never picks one and shows a partial job list.
func TestCLICorruptJobCompiledHashLookalikeCoexistence(t *testing.T) {
	escapedLongS := "\\u017f"
	literalLongS := "ſ"
	cases := []struct {
		name   string
		member func(h string) string
	}{
		{"standard then literal, same value", func(h string) string {
			return `"compiled_hash": "` + h + `", "compiled_ha` + literalLongS + `h": "` + h + `"`
		}},
		{"literal then standard, differing value", func(h string) string {
			return `"compiled_ha` + literalLongS + `h": "deadbeef", "compiled_hash": "` + h + `"`
		}},
		{"escaped null then standard", func(h string) string {
			return `"compiled_ha` + escapedLongS + `h": null, "compiled_hash": "` + h + `"`
		}},
		{"standard then escaped null", func(h string) string {
			return `"compiled_hash": "` + h + `", "compiled_ha` + escapedLongS + `h": null`
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			hash := seedBoundJobCLI(t, dir)
			bad := replaceJobBinding(t, dir, hash, tc.member(hash))
			assertReadFailure(t, []string{"job-list", "--dir", dir}, bad, dir)
		})
	}
}

// The accepted ASCII letter-case spellings, including JSON-escaped ones,
// still load normally and display the binding.
func TestCLICompiledHashASCIIVariantsStillRead(t *testing.T) {
	cases := []struct {
		name   string
		member func(h string) string
	}{
		{"upper case", func(h string) string { return `"COMPILED_HASH": "` + h + `"` }},
		{"escaped underscore", func(h string) string {
			return "\"compiled\\u005fhash\": \"" + h + `"`
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			hash := seedBoundJobCLI(t, dir)
			replaceJobBinding(t, dir, hash, tc.member(hash))
			r := callCLI(t, "job-get", "--dir", dir, "--id", "j1")
			if r.code != 0 {
				t.Fatalf("ASCII case spelling must load: code=%d err=%s", r.code, r.err)
			}
			if !strings.Contains(r.out, "compiled_hash") || !strings.Contains(r.out, hash) {
				t.Fatalf("binding not displayed after ASCII spelling: %q", r.out)
			}
		})
	}
}
