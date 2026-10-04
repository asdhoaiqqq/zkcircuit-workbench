package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// These tests cover the CLI contract for a non-ASCII lookalike spelling of
// one of data.json's top-level fields: "circuitſ", "setupſ", "jobſ" and
// "artifactſ" (U+017F long s, written directly or as the JSON escape u017f)
// are matched onto the canonical fields by encoding/json's Unicode-folded
// tag matching, but they are not accepted spellings. Such a member — alone
// or beside the standard / an ASCII upper-case spelling, with a legal value,
// an empty array or null — is data corruption: every data command fails at
// open with exit code 3, prints nothing on stdout, names both the standard
// field and the actual spelling, and leaves data.json byte-for-byte in
// place. Genuinely unknown names such as "note" or "cİrcuits" (U+0130) are
// unaffected.

// cliEnvLongS is one literal long-s rune; cliEnvEscape is one literal
// backslash so an escaped key token keeps its backslash in source.
const cliEnvLongS = "ſ"
const cliEnvEscape = `\`

// longSToken returns the full JSON key token naming field under its long-s
// spelling, directly written or as the u017f escape (assembled literally so
// strconv.Quote cannot double the backslash).
func longSToken(field string, escaped bool) string {
	if escaped {
		return `"` + field[:len(field)-1] + cliEnvEscape + "u017f" + `"`
	}
	return strconv.Quote(field[:len(field)-1] + cliEnvLongS)
}

// replaceTopLevelMember re-emits a compact envelope renaming one canonical
// key to keyToken and, when val is non-empty, replacing its value.
func replaceTopLevelMember(t *testing.T, compact []byte, canonical, keyToken, val string) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(compact))
	dec.UseNumber()
	if open, err := dec.Token(); err != nil || open != json.Delim('{') {
		t.Fatalf("envelope is not a JSON object: %v", err)
	}
	type kv struct {
		key string
		val json.RawMessage
	}
	var members []kv
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		var rv json.RawMessage
		if err := dec.Decode(&rv); err != nil {
			t.Fatal(err)
		}
		members = append(members, kv{key: keyTok.(string), val: rv})
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			buf.WriteByte(',')
		}
		token := strconv.Quote(m.key)
		value := m.val
		if m.key == canonical {
			token = keyToken
			if val != "" {
				value = json.RawMessage(val)
			}
		}
		buf.WriteString(token)
		buf.WriteByte(':')
		var vc bytes.Buffer
		if err := json.Compact(&vc, value); err != nil {
			t.Fatal(err)
		}
		buf.Write(vc.Bytes())
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

// assertCLILookalikeCorrupt drives query and mutating commands against a
// directory carrying bad bytes and requires exit 3, empty stdout, a read
// failure naming the canonical field and the long-s spelling, and a
// byte-for-byte untouched file.
func assertCLILookalikeCorrupt(t *testing.T, dir string, bad []byte, canonical string) {
	t.Helper()
	path := filepath.Join(dir, "data.json")
	actual := strconv.Quote(canonical[:len(canonical)-1] + cliEnvLongS)
	commands := [][]string{
		{"circuit-list", "--dir", dir},
		{"circuit-get", "--dir", dir, "--name", "mul", "--version", "1"},
		{"job-list", "--dir", dir},
		{"job-get", "--dir", dir, "--id", "j1"},
		// Mutating commands must refuse before committing.
		{"circuit-create", "--dir", dir, "--name", "new", "--version", "2",
			"--constraints", "1", "--description", "d"},
		{"circuit-freeze", "--dir", dir, "--name", "mul", "--version", "1"},
		{"job-submit", "--dir", dir, "--id", "j2", "--name", "mul", "--version", "1"},
	}
	for _, args := range commands {
		r := callCLI(t, args...)
		if r.code != 3 {
			t.Fatalf("%s: want exit 3, got %d (out=%q err=%q)", args[0], r.code, r.out, r.err)
		}
		if r.out != "" {
			t.Fatalf("%s printed output despite corruption: %q", args[0], r.out)
		}
		err := r.err
		if !strings.Contains(err, "read failed") || !strings.Contains(err, "data corrupt") {
			t.Fatalf("%s: unexpected error text %q", args[0], err)
		}
		if !strings.Contains(err, "non-ASCII spelling") ||
			!strings.Contains(err, strconv.Quote(canonical)) ||
			!strings.Contains(err, actual) {
			t.Fatalf("%s: error must name field %q and spelling %s, got %q",
				args[0], canonical, actual, err)
		}
	}
	left, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(bad) {
		t.Fatalf("data.json changed after refused commands")
	}
}

// TestCLILongSLookalikeBesideCanonicalExitCode: an appended long-s member —
// before or after the standard key, direct or u017f-escaped, for each of the
// four fields, carrying [] or null — makes every command exit 3 without
// touching the file. This is the "empty circuitſ hides real circuits"
// failure mode.
func TestCLILongSLookalikeBesideCanonicalExitCode(t *testing.T) {
	for _, field := range []string{"circuits", "setups", "jobs", "artifacts"} {
		for _, escaped := range []bool{false, true} {
			for _, val := range []string{"[]", "null"} {
				for _, before := range []bool{false, true} {
					name := field + "/escaped=" + strconv.FormatBool(escaped) +
						"/" + val + "/before=" + strconv.FormatBool(before)
					t.Run(name, func(t *testing.T) {
						dir := filepath.Join(t.TempDir(), "bench")
						seedBoundJobCLI(t, dir)
						compact := compactDataFile(t, dir)
						bad := appendTopLevelMember(t, compact, longSToken(field, escaped)+":"+val, before)
						bad = append(bad, '\n')
						if err := os.WriteFile(filepath.Join(dir, "data.json"), bad, 0o644); err != nil {
							t.Fatal(err)
						}
						assertCLILookalikeCorrupt(t, dir, bad, field)
					})
				}
			}
		}
	}
}

// TestCLILongSLookalikeAloneExitCode: the standard key replaced entirely by a
// lone long-s spelling must be refused whether its value is the field's real
// records, an empty array or null — a legal-record value is not ignored as an
// unknown member, and []/null do not read as "no records".
func TestCLILongSLookalikeAloneExitCode(t *testing.T) {
	cases := []struct {
		field string
		val   string // "" keeps the seeded real value
	}{
		{"circuits", ""}, {"circuits", "[]"}, {"circuits", "null"},
		{"setups", "[]"}, {"jobs", "null"}, {"artifacts", "[]"},
	}
	for _, escaped := range []bool{false, true} {
		for _, tc := range cases {
			name := "escaped=" + strconv.FormatBool(escaped) + "/" + tc.field
			if tc.val != "" {
				name += "=" + tc.val
			}
			t.Run(name, func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "bench")
				seedBoundJobCLI(t, dir)
				compact := compactDataFile(t, dir)
				bad := replaceTopLevelMember(t, compact, tc.field, longSToken(tc.field, escaped), tc.val)
				bad = append(bad, '\n')
				if err := os.WriteFile(filepath.Join(dir, "data.json"), bad, 0o644); err != nil {
					t.Fatal(err)
				}
				assertCLILookalikeCorrupt(t, dir, bad, tc.field)
			})
		}
	}
}

// TestCLILongSLookalikeBesideUpperVariantExitCode: a long-s member together
// with an ASCII upper-case spelling (so no lowercase key is present) is
// refused in either position; the read may not select one of the values.
func TestCLILongSLookalikeBesideUpperVariantExitCode(t *testing.T) {
	for _, before := range []bool{false, true} {
		t.Run("before="+strconv.FormatBool(before), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			seedBoundJobCLI(t, dir)
			compact := compactDataFile(t, dir)
			upper := rewriteTopLevelKeys(t, compact, map[string]string{
				"circuits": `"CIRCUITS"`,
			})
			bad := appendTopLevelMember(t, upper, longSToken("circuits", false)+":[]", before)
			bad = append(bad, '\n')
			if err := os.WriteFile(filepath.Join(dir, "data.json"), bad, 0o644); err != nil {
				t.Fatal(err)
			}
			assertCLILookalikeCorrupt(t, dir, bad, "circuits")
		})
	}
}

// TestCLIUnknownLookalikeBoundaryStillReads pins the boundary: "note" and
// "cİrcuits" (U+0130, which does not fold) stay ordinary unknown members and
// the directory still serves its records, while swapping in "circuitſ"
// (U+017F) in the same shape is an exit-3 failure.
func TestCLIUnknownLookalikeBoundaryStillReads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	seedBoundJobCLI(t, dir)
	compact := compactDataFile(t, dir)
	withUnknown := appendTopLevelMember(t, compact, `"note":1`, false)
	withUnknown = appendTopLevelMember(t, withUnknown, `"cİrcuits":[]`, false)
	withUnknown = append(withUnknown, '\n')
	if err := os.WriteFile(filepath.Join(dir, "data.json"), withUnknown, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := callCLI(t, "circuit-list", "--dir", dir); r.code != 0 ||
		!strings.Contains(r.out, "name=mul") {
		t.Fatalf("unknown keys wrongly rejected: code=%d out=%q err=%q", r.code, r.out, r.err)
	}

	dir2 := filepath.Join(t.TempDir(), "bench")
	seedBoundJobCLI(t, dir2)
	compact2 := compactDataFile(t, dir2)
	bad := appendTopLevelMember(t, compact2, `"note":1`, false)
	bad = appendTopLevelMember(t, bad, longSToken("circuits", false)+":[]", false)
	bad = append(bad, '\n')
	if err := os.WriteFile(filepath.Join(dir2, "data.json"), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	assertCLILookalikeCorrupt(t, dir2, bad, "circuits")
}
