package zkcircuit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the circuit-name identity rule: a name is committed,
// stored and read back as exactly one value. Names carrying bytes that are
// not complete, legal UTF-8 are refused at creation (ErrInvalidArgument,
// nothing committed); a committed name whose raw JSON string could not
// survive a lossless decode — invalid UTF-8 bytes or an unpaired-surrogate
// \uXXXX escape — is data corruption (ErrDataCorrupt), never a name that
// silently comes back with U+FFFD substitutions. Legal Unicode, a genuine
// "�" and the literal text \uD800 stay ordinary names.

// rawCircuitRecord renders one complete circuit record around a raw JSON
// string literal for the name, so tests can place bytes in the file that no
// encoder would emit.
func rawCircuitRecord(nameLiteral string, version int) string {
	return `{"name":` + nameLiteral +
		`,"version":` + itoa(version) +
		`,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":false,"description":""}`
}

// writeCircuitEnvelope commits a hand-built data.json (plus trailing
// newline, matching the writer) and returns the exact bytes written.
func writeCircuitEnvelope(t *testing.T, dir string, records ...string) []byte {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"format":1,"circuits":[` + strings.Join(records, ",") +
		`],"setups":[],"jobs":[]}` + "\n"
	raw := []byte(content)
	if err := os.WriteFile(filepath.Join(dir, dirDataFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return raw
}

// requireDataFileBytes asserts data.json is byte-for-byte the given content.
func requireDataFileBytes(t *testing.T, dir string, want []byte) {
	t.Helper()
	left, err := os.ReadFile(filepath.Join(dir, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(want) {
		t.Fatalf("data.json changed\nwant: %q\n got: %q", want, left)
	}
}

// TestCreateCircuitRejectsInvalidUTF8Name: a name that is not complete,
// legal UTF-8 fails the whole create with ErrInvalidArgument before anything
// is staged — no record, no rewritten name on disk, existing state intact.
func TestCreateCircuitRejectsInvalidUTF8Name(t *testing.T) {
	names := map[string]string{
		"lone continuation byte":   "bad\x80name",
		"lone continuation at end": "bad\xa0",
		"truncated two-byte":       "bad\xc2",
		"truncated three-byte":     "电\xe5\xad",    // 电 followed by a cut-off character
		"truncated at end":         "\xe7\x94",     // 电 missing its last byte
		"overlong encoding":        "\xc0\xaf",     // overlong '/'
		"surrogate as UTF-8":       "\xed\xa0\x80", // U+D800 encoded as UTF-8
		"byte inside CJK":          "电\xe5\xadn",
	}
	for label, name := range names {
		t.Run(label, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			// A healthy record already committed must survive the refusal.
			if _, err := s.CreateCircuit(Circuit{Name: "good", Version: 1, Constraints: 1}); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(dir, dirDataFile))
			if err != nil {
				t.Fatal(err)
			}

			_, err = s.CreateCircuit(Circuit{Name: name, Version: 1, Constraints: 1})
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("want ErrInvalidArgument, got %v", err)
			}
			// The refused name was neither committed nor rewritten into the
			// file; the healthy record is byte-for-byte intact.
			requireDataFileBytes(t, dir, before)
			list, err := s.ListCircuits()
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 1 || list[0].Name != "good" {
				t.Fatalf("refused create left records behind: %+v", list)
			}
		})
	}
}

// TestCreateCircuitInvalidUTF8NameFreshDirectory: on a fresh directory the
// refusal happens before the first commit, so no data.json appears at all.
func TestCreateCircuitInvalidUTF8NameFreshDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.CreateCircuit(Circuit{Name: "bad\x80", Version: 1, Constraints: 1}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, dirDataFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused create wrote a data file: stat err=%v", err)
	}
}

// TestValidUnicodeNamesRoundTrip: legal Chinese, emoji, a genuine "�" and
// the literal text \uD800 are ordinary names — created, committed, reloaded
// and found again under exactly the submitted string.
func TestValidUnicodeNamesRoundTrip(t *testing.T) {
	names := []string{
		"电路",
		"🚀 发射",
		"replacement � inside", // a genuine U+FFFD is a legal character
		`\uD800`,               // the six literal characters, not an escape
		"  padded  ",           // surrounding spaces are part of the name
	}
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range names {
		if _, err := s.CreateCircuit(Circuit{Name: name, Version: i + 1, Constraints: 1}); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	for i, name := range names {
		got, err := s2.GetCircuit(name, i+1)
		if err != nil {
			t.Fatalf("get %q: %v", name, err)
		}
		if got.Name != name {
			t.Fatalf("name drifted: want %q, got %q", name, got.Name)
		}
	}
	list, err := s2.ListCircuits()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != len(names) {
		t.Fatalf("want %d circuits, got %d", len(names), len(list))
	}
}

// TestStoredNameInvalidUTF8Refused: a committed name whose raw bytes are not
// legal UTF-8 is data corruption — the whole directory refuses to read, the
// error names the circuit record's name field, and the file is left
// byte-for-byte in place even with a healthy record beside the damaged one.
func TestStoredNameInvalidUTF8Refused(t *testing.T) {
	// The literals carry the damaged bytes themselves (interpreted Go string
	// escapes), which is exactly what a corrupted or hand-edited file holds.
	names := map[string]string{
		"lone continuation byte": "\"bad\x80name\"",
		"truncated three-byte":   "\"\xe7\x94\"",
		"overlong encoding":      "\"\xc0\xaf\"",
		"surrogate as UTF-8":     "\"\xed\xa0\x80\"",
	}
	for label, literal := range names {
		t.Run(label, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			bad := writeCircuitEnvelope(t, dir,
				rawCircuitRecord(`"good"`, 9),
				rawCircuitRecord(literal, 1))

			s, err := Open(dir)
			if err == nil {
				s.Close()
				t.Fatalf("directory with an invalid-UTF-8 name opened")
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), "circuit record") || !strings.Contains(err.Error(), `"name"`) {
				t.Fatalf("error must name the circuit record's name field: %v", err)
			}
			requireDataFileBytes(t, dir, bad)
		})
	}
}

// TestStoredNameUnpairedSurrogateRefused: a \uXXXX escape naming an
// unpaired surrogate would decode as U+FFFD — the read is refused as data
// corruption instead of continuing with the substituted name.
func TestStoredNameUnpairedSurrogateRefused(t *testing.T) {
	literals := map[string]string{
		"high at end":              `"电路\uD800"`,
		"high before plain char":   `"ok\uD800x"`,
		"high before non-pair":     `"\uD800A"`,
		"two highs":                `"\uD800\uD800"`,
		"lone low":                 `"\uDC00solo"`,
		"low then high reversed":   `"\uDC00\uD800"`,
		"high before other escape": `"\uD800\n"`,
	}
	for label, literal := range literals {
		t.Run(label, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			bad := writeCircuitEnvelope(t, dir,
				rawCircuitRecord(`"good"`, 9),
				rawCircuitRecord(literal, 1))

			_, err := Open(dir)
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), "circuit record") || !strings.Contains(err.Error(), `"name"`) {
				t.Fatalf("error must name the circuit record's name field: %v", err)
			}
			requireDataFileBytes(t, dir, bad)
		})
	}
}

// TestCorruptNamePoisonsWholeDirectory: with the damage introduced after a
// clean open, every operation on the directory fails with ErrDataCorrupt —
// no partial query results, no committed modification — and the file stays
// byte-for-byte as the tamper left it.
func TestCorruptNamePoisonsWholeDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.CreateCircuit(Circuit{Name: "good", Version: 9, Constraints: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "mul", Version: 1, Constraints: 1}); err != nil {
		t.Fatal(err)
	}

	// External tamper while the handle is open: rewrite the second record's
	// name with an unpaired-surrogate escape.
	raw, err := os.ReadFile(filepath.Join(dir, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	bad := []byte(strings.Replace(string(raw), `"mul"`, `"电\uD800"`, 1))
	if string(bad) == string(raw) {
		t.Fatal("setup: name not found in committed file")
	}
	if err := os.WriteFile(filepath.Join(dir, dirDataFile), bad, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.ListCircuits(); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("ListCircuits: want ErrDataCorrupt, got %v", err)
	}
	// The healthy record must not be returned as a partial result.
	if _, err := s.GetCircuit("good", 9); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetCircuit on the healthy record: want ErrDataCorrupt, got %v", err)
	}
	// Modifications must not commit.
	if _, err := s.CreateCircuit(Circuit{Name: "new", Version: 1, Constraints: 1}); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("CreateCircuit: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.FreezeCircuit("good", 9); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("FreezeCircuit: want ErrDataCorrupt, got %v", err)
	}
	requireDataFileBytes(t, dir, bad)
}

// TestStoredNameLegalUnicodeAccepted: the legal half of the read rule — a
// paired surrogate escape decodes to its character, an escaped backslash
// keeps the literal text \uD800, and a genuine "�" stays as committed.
func TestStoredNameLegalUnicodeAccepted(t *testing.T) {
	cases := []struct {
		literal string // raw JSON string literal in the file
		want    string // the Go string the record must read back as
	}{
		{`"emoji 😀"`, "emoji \U0001F600"},            // character written directly
		{`"emoji \ud83d\ude00"`, "emoji \U0001F600"}, // legal surrogate pair
		{`"emoji \uD83D\uDE00"`, "emoji \U0001F600"}, // uppercase hex pair
		{`"电路"`, "电路"},
		{`"literal \\uD800"`, `literal \uD800`},
		{`"real �"`, "real �"},
	}
	for _, tc := range cases {
		t.Run(tc.literal, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			writeCircuitEnvelope(t, dir, rawCircuitRecord(tc.literal, 3))

			s, err := Open(dir)
			if err != nil {
				t.Fatalf("legal name refused: %v", err)
			}
			defer s.Close()
			got, err := s.GetCircuit(tc.want, 3)
			if err != nil {
				t.Fatalf("get by the exact decoded name: %v", err)
			}
			if got.Name != tc.want {
				t.Fatalf("name drifted: want %q, got %q", tc.want, got.Name)
			}
			// A commit round-trip keeps the same name value.
			if _, err := s.FreezeCircuit(tc.want, 3); err != nil {
				t.Fatal(err)
			}
			again, err := s.GetCircuit(tc.want, 3)
			if err != nil || again.Name != tc.want || !again.Frozen {
				t.Fatalf("round-trip drifted: %+v err=%v", again, err)
			}
		})
	}
}

// TestStoredReplacementCharNameReadsAsCommitted: a name previously committed
// as a legal "�" is read at its current value — the lost bytes are neither
// guessed nor restored, and the name keeps working for every operation.
func TestStoredReplacementCharNameReadsAsCommitted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.CreateCircuit(Circuit{Name: "old � name", Version: 1, Constraints: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.GetCircuit("old � name", 1)
	if err != nil {
		t.Fatalf("the committed � name must read back as itself: %v", err)
	}
	if got.Name != "old � name" {
		t.Fatalf("name drifted: %q", got.Name)
	}
}
