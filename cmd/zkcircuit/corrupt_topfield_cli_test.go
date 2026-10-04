package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// These tests cover the CLI contract for the five data.json top-level
// fields (format, circuits, setups, jobs, artifacts): two spellings of one
// field that differ only in ASCII letter case — one of them possibly a JSON
// escape spelling — make the directory unreadable. Every data command then
// exits 3 (read failure), prints nothing on stdout and leaves data.json
// byte-for-byte in place; a single upper/mixed/escaped spelling still reads
// with its existing compatibility.

// compactDataFile reads and compacts data.json.
func compactDataFile(t *testing.T, dir string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	return compact.Bytes()
}

// appendTopLevelMember returns compact envelope JSON with one extra member
// inserted either right after the opening brace (before=true) or right
// before the closing brace (before=false). member is the raw "key":value
// text. The result is verified to parse.
func appendTopLevelMember(t *testing.T, compact []byte, member string, before bool) []byte {
	t.Helper()
	if len(compact) == 0 || compact[0] != '{' || compact[len(compact)-1] != '}' {
		t.Fatalf("envelope is not a JSON object: %s", compact)
	}
	var out []byte
	if before {
		out = append(out, '{')
		out = append(out, member...)
		out = append(out, ',')
		out = append(out, compact[1:]...)
	} else {
		out = append(out, compact[:len(compact)-1]...)
		out = append(out, ',')
		out = append(out, member...)
		out = append(out, '}')
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("crafted envelope is not valid JSON: %v (%s)", err, out)
	}
	return out
}

// rewriteTopLevelKeys re-emits a compact envelope replacing the key token of
// each canonical key named in renames with the supplied full JSON key token
// (e.g. "FORMAT" or the escaped "JOBS"); values are copied compacted
// and stay byte-identical.
func rewriteTopLevelKeys(t *testing.T, compact []byte, renames map[string]string) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(compact))
	dec.UseNumber()
	open, err := dec.Token()
	if err != nil || open != json.Delim('{') {
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
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			t.Fatal(err)
		}
		members = append(members, kv{key: keyTok.(string), val: val})
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			buf.WriteByte(',')
		}
		token := strconv.Quote(m.key)
		if rep, ok := renames[m.key]; ok {
			token = rep
		}
		buf.WriteString(token)
		buf.WriteByte(':')
		var vc bytes.Buffer
		if err := json.Compact(&vc, m.val); err != nil {
			t.Fatal(err)
		}
		buf.Write(vc.Bytes())
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

// TestCLICaseVariantTopLevelDuplicateExitCode drives each duplicated
// case-variant top-level field through the CLI: queries and a mutation must
// exit 3 with empty stdout, name the canonical lowercase field, and leave
// the damaged file untouched.
func TestCLICaseVariantTopLevelDuplicateExitCode(t *testing.T) {
	// Escaped spellings decode (ASCII uppercase) to CIRCUITS / FORMAT.
	escCircuits := fmt.Sprintf(`"\u%04xIRCUITS"`, 'C')
	escFormat := fmt.Sprintf(`"\u%04xORMAT"`, 'F')

	cases := []struct {
		name      string
		key       string // full JSON key token of the extra member
		val       string // its raw JSON value
		canonical string
		before    bool // extra member precedes the canonical one
	}{
		{"circuits then CIRCUITS empty", `"CIRCUITS"`, "[]", "circuits", false},
		{"CIRCUITS empty then circuits", `"CIRCUITS"`, "[]", "circuits", true},
		{"circuits then escaped CIRCUITS", escCircuits, "[]", "circuits", false},
		{"format then FORMAT 999", `"FORMAT"`, "999", "format", false},
		{"FORMAT 999 masked by later format 1", `"FORMAT"`, "999", "format", true},
		{"FORMAT 1 then format", `"FORMAT"`, "1", "format", true},
		{"escaped FORMAT 1 then format", escFormat, "1", "format", true},
		{"setups then SETUPS null", `"SETUPS"`, "null", "setups", false},
		{"SETUPS null then setups", `"SETUPS"`, "null", "setups", true},
		{"jobs then JOBS", `"JOBS"`, "[]", "jobs", false},
		{"JOBS then jobs", `"JOBS"`, "[]", "jobs", true},
		{"artifacts then ARTIFACTS", `"ARTIFACTS"`, "[]", "artifacts", false},
		{"ARTIFACTS then artifacts", `"ARTIFACTS"`, "[]", "artifacts", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			seedBoundJobCLI(t, dir)
			compact := compactDataFile(t, dir)
			bad := appendTopLevelMember(t, compact, tc.key+":"+tc.val, tc.before)
			bad = append(bad, '\n')
			path := filepath.Join(dir, "data.json")
			if err := os.WriteFile(path, bad, 0o644); err != nil {
				t.Fatal(err)
			}

			commands := [][]string{
				{"circuit-list", "--dir", dir},
				{"circuit-get", "--dir", dir, "--name", "mul", "--version", "1"},
				{"job-list", "--dir", dir},
				{"job-get", "--dir", dir, "--id", "j1"},
				// Mutating commands must refuse before committing.
				{"circuit-create", "--dir", dir, "--name", "new", "--version", "2",
					"--constraints", "1", "--description", "d"},
				{"circuit-freeze", "--dir", dir, "--name", "new", "--version", "2"},
			}
			for _, args := range commands {
				r := callCLI(t, args...)
				if r.code != 3 {
					t.Fatalf("%s: want exit 3, got %d (out=%q err=%q)", args[0], r.code, r.out, r.err)
				}
				if r.out != "" {
					t.Fatalf("%s printed success output despite corruption: %q", args[0], r.out)
				}
				if !strings.Contains(r.err, "read failed") || !strings.Contains(r.err, "data corrupt") {
					t.Fatalf("%s: unexpected error text %q", args[0], r.err)
				}
				if !strings.Contains(r.err, "duplicate top-level field") ||
					!strings.Contains(r.err, strconv.Quote(tc.canonical)) {
					t.Fatalf("%s: error does not name duplicated top-level field %q: %q",
						args[0], tc.canonical, r.err)
				}
			}

			left, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(left) != string(bad) {
				t.Fatalf("data.json changed after refused commands")
			}
		})
	}
}

// TestCLICaseVariantDuplicateEmptyArraysHideNoRecords pins the headline
// failure mode: later "CIRCUITS":[] / "JOBS":[] must not silently replace
// the populated arrays with empty lists, and a later "FORMAT":1 must not
// mask the canonical format value.
func TestCLICaseVariantDuplicateEmptyArraysHideNoRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	seedBoundJobCLI(t, dir)
	compact := compactDataFile(t, dir)
	damaged := appendTopLevelMember(t, compact, `"CIRCUITS":[]`, false)
	damaged = appendTopLevelMember(t, damaged, `"JOBS":[]`, false)
	damaged = appendTopLevelMember(t, damaged, `"FORMAT":1`, false)
	if err := os.WriteFile(filepath.Join(dir, "data.json"), append(damaged, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"circuit-list", "--dir", dir},
		{"job-list", "--dir", dir},
		{"circuit-get", "--dir", dir, "--name", "mul", "--version", "1"},
	} {
		r := callCLI(t, args...)
		if r.code != 3 || r.out != "" {
			t.Fatalf("%s: want exit 3 with no output, got code=%d out=%q err=%q",
				args[0], r.code, r.out, r.err)
		}
	}
}

// TestCLISingleCaseVariantTopFieldsReadNormally: a valid directory whose
// five keys are written under case variants (jobs through a JSON escape)
// keeps serving the version, setup, artifact hash and bound job, and a later
// commit rewrites the canonical lowercase spellings without losing state.
func TestCLISingleCaseVariantTopFieldsReadNormally(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	hash := seedBoundJobCLI(t, dir)
	compact := compactDataFile(t, dir)
	escJobs := fmt.Sprintf(`"\u%04xOBS"`, 'J') // decodes to "JOBS"
	renamed := rewriteTopLevelKeys(t, compact, map[string]string{
		"format":    `"FORMAT"`,
		"circuits":  `"CIRCUITS"`,
		"setups":    `"SETUPS"`,
		"jobs":      escJobs,
		"artifacts": `"ARTIFACTS"`,
	})
	if err := os.WriteFile(filepath.Join(dir, "data.json"), append(renamed, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	if r := callCLI(t, "circuit-list", "--dir", dir); r.code != 0 ||
		!strings.Contains(r.out, "name=mul") || !strings.Contains(r.out, "frozen=true") {
		t.Fatalf("upper-case circuit-list: code=%d out=%q err=%q", r.code, r.out, r.err)
	}
	if r := callCLI(t, "circuit-get", "--dir", dir, "--name", "mul", "--version", "1"); r.code != 0 ||
		!strings.Contains(r.out, "constraints=1") {
		t.Fatalf("upper-case circuit-get: code=%d out=%q err=%q", r.code, r.out, r.err)
	}
	if r := callCLI(t, "job-list", "--dir", dir); r.code != 0 ||
		!strings.Contains(r.out, "j1") || !strings.Contains(r.out, "compiled_hash="+hash) {
		t.Fatalf("escaped-key job-list: code=%d out=%q err=%q", r.code, r.out, r.err)
	}
	if r := callCLI(t, "job-get", "--dir", dir, "--id", "j1"); r.code != 0 ||
		!strings.Contains(r.out, "compiled_hash="+hash) {
		t.Fatalf("escaped-key job-get: code=%d out=%q err=%q", r.code, r.out, r.err)
	}

	// A mutation commits and writes the canonical lowercase keys back.
	if r := callCLI(t, "circuit-create", "--dir", dir, "--name", "second", "--version", "2",
		"--constraints", "1", "--description", "d"); r.code != 0 {
		t.Fatalf("mutation on case-variant directory: code=%d err=%q", r.code, r.err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, key := range []string{"format", "circuits", "setups", "jobs", "artifacts"} {
		if !strings.Contains(text, strconv.Quote(key)+":") {
			t.Fatalf("committed file lost canonical key %q", key)
		}
		if strings.Contains(text, strings.ToUpper(strconv.Quote(key))+":") {
			t.Fatalf("committed file kept upper-case spelling of %q", key)
		}
	}
	if r := callCLI(t, "circuit-list", "--dir", dir); r.code != 0 ||
		strings.Count(r.out, "circuit:") != 2 {
		t.Fatalf("state lost across canonicalization: code=%d out=%q", r.code, r.out)
	}
	if r := callCLI(t, "job-get", "--dir", dir, "--id", "j1"); r.code != 0 ||
		!strings.Contains(r.out, "compiled_hash="+hash) {
		t.Fatalf("artifact binding lost across canonicalization: code=%d out=%q", r.code, r.out)
	}
}
