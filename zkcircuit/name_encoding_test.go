package zkcircuit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the circuit-name identity rule: a name is the version's
// identity, so it must keep one exact value across submit, save and read.
//
// On the write path a name that is not complete, valid UTF-8 (a lone
// continuation byte, a truncated multi-byte character, an overlong form) is
// refused as ErrInvalidArgument before anything is committed — encoding/json
// would otherwise silently rewrite those bytes to U+FFFD at save time and
// the circuit would be stored under a different name than the caller asked
// for.
//
// On the read path a committed record whose raw name token carries invalid
// UTF-8 bytes, or \uXXXX escapes forming an unpaired surrogate, is data
// corruption: the whole directory read fails with ErrDataCorrupt naming the
// record and the name field, no partial results are returned, and the file
// stays byte-for-byte in place. Legal Chinese, emoji, an actually committed
// "�", a legal surrogate pair and the literal six characters \uD800 are all
// ordinary names and keep their exact value.

// rawEnvelope builds a complete committed envelope around one circuit record
// whose name is the given raw JSON token (already quoted/escaped by the
// caller), plus optionally a second, always-legal circuit.
func rawEnvelope(nameJSON string, withSecond bool) []byte {
	var b strings.Builder
	b.WriteString(`{"format":1,"circuits":[`)
	if withSecond {
		b.WriteString(`{"name":"ok","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":false,"description":""},`)
	}
	b.WriteString(`{"name":`)
	b.WriteString(nameJSON)
	b.WriteString(`,"version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":false,"description":""}],"setups":[],"jobs":[]}`)
	return []byte(b.String())
}

// plantRawEnvelope plants raw as the committed data.json of a fresh directory.
func plantRawEnvelope(t *testing.T, raw []byte) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bench")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dirDataFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCreateCircuitRejectsInvalidUTF8Name(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Seed an existing circuit with a definition; a refused create must leave
	// it, and the committed file, exactly as they were.
	seedDraftWithDef(t, s, "good", 1, 1, 1, `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`)
	dataPath := filepath.Join(dir, dirDataFile)
	before, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}

	bad := map[string]string{
		"lone continuation byte":   "bad\x80name",
		"truncated multi-byte":     "trunc\xe4\xb8",
		"overlong encoding":        "over\xc0\xaf",
		"invalid byte 0xff":        "bad\xff",
		"encoded surrogate half":   "pair\xed\xa0\x80",
		"truncated at end":         "end\xc3",
		"continuation then ASCII":  "bad\x80x",
		"valid prefix then damage": "电路\xff",
	}
	for label, name := range bad {
		t.Run(label, func(t *testing.T) {
			_, err := s.CreateCircuit(Circuit{Name: name, Version: 1, Constraints: 1})
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("want ErrInvalidArgument, got %v", err)
			}
		})
	}

	after, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("refused creates modified data.json\nwant: %q\n got: %q", before, after)
	}
	list, err := s.ListCircuits()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "good" {
		t.Fatalf("existing circuit damaged by refused creates: %+v", list)
	}
	if _, err := s.GetCircuit("good", 1); err != nil {
		t.Fatalf("existing circuit unreadable after refused creates: %v", err)
	}
}

func TestCreateCircuitValidUnicodeNamesRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{
		"电路",            // Chinese
		"emoji 😀 name",  // emoji
		"has�mark",      // an actually committed replacement character
		`literal\uD800`, // the literal six characters \uD800 — no escape
		"mixed 电路😀�mix", // everything together
	}
	for _, name := range names {
		if _, err := s.CreateCircuit(Circuit{Name: name, Version: 1, Constraints: 1}); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen and read every name back by its exact submitted value.
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	list, err := s2.ListCircuits()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != len(names) {
		t.Fatalf("want %d circuits, got %+v", len(names), list)
	}
	for _, name := range names {
		got, err := s2.GetCircuit(name, 1)
		if err != nil {
			t.Fatalf("name %q did not round-trip: %v", name, err)
		}
		if got.Name != name {
			t.Fatalf("name changed across save and read: want %q, got %q", name, got.Name)
		}
	}
}

func TestCorruptNameInvalidUTF8Refused(t *testing.T) {
	bad := map[string]string{
		"lone continuation byte": "\"bad\x80name\"",
		"invalid byte 0xff":      "\"bad\xff\"",
		"truncated multi-byte":   "\"trunc\xe4\xb8\"",
		"overlong encoding":      "\"over\xc0\xaf\"",
		"encoded surrogate half": "\"pair\xed\xa0\x80\"",
	}
	for label, nameJSON := range bad {
		t.Run(label, func(t *testing.T) {
			// A second, fully legal circuit shares the directory: the read
			// must still fail outright rather than return partial results.
			raw := rawEnvelope(nameJSON, true)
			dir := plantRawEnvelope(t, raw)

			s, err := Open(dir)
			if err == nil {
				s.Close()
				t.Fatalf("directory with an invalid-UTF-8 circuit name was accepted")
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), "circuit record #2") ||
				!strings.Contains(err.Error(), `"name"`) {
				t.Fatalf("error does not name the circuit record and the name field: %v", err)
			}
			left, rerr := os.ReadFile(filepath.Join(dir, dirDataFile))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(left) != string(raw) {
				t.Fatalf("refused read modified data.json\nwant: %q\n got: %q", raw, left)
			}
		})
	}
}

func TestCorruptNameUnpairedSurrogateRefused(t *testing.T) {
	bad := map[string]string{
		"lone high surrogate":              `"电路\uD800"`,
		"lone low surrogate":               `"\uDC00"`,
		"high surrogate at end":            `"end\uD83D"`,
		"two high surrogates":              `"\uD800\uD800"`,
		"high then non-surrogate escape":   `"\uD800A"`,
		"high then simple escape":          `"\uD800\n"`,
		"low then high (reversed pair)":    `"\uDC00\uD800"`,
		"high then literal ASCII":          `"\uD800x"`,
		"low surrogate after valid pair":   `"😀\uDE00"`,
		"high surrogate after valid pair":  `"😀\uD83D"`,
		"valid pair then lone high":        `"😀\uD800"`,
		"high surrogate escaped lowercase": `"a\ud800"`,
	}
	for label, nameJSON := range bad {
		t.Run(label, func(t *testing.T) {
			raw := rawEnvelope(nameJSON, true)
			dir := plantRawEnvelope(t, raw)

			s, err := Open(dir)
			if err == nil {
				s.Close()
				t.Fatalf("directory with an unpaired-surrogate circuit name was accepted")
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), "circuit record #2") ||
				!strings.Contains(err.Error(), `"name"`) {
				t.Fatalf("error does not name the circuit record and the name field: %v", err)
			}
			left, rerr := os.ReadFile(filepath.Join(dir, dirDataFile))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(left) != string(raw) {
				t.Fatalf("refused read modified data.json\nwant: %q\n got: %q", raw, left)
			}
		})
	}
}

func TestLegalNameEncodingsReadBackExactly(t *testing.T) {
	cases := map[string]struct {
		nameJSON string
		want     string
	}{
		"surrogate pair decodes to its character": {`"\u7535\u8DEF\uD83D\uDE00"`, "电路😀"},
		"escaped backslash keeps literal text":    {`"\\uD800"`, `\uD800`},
		"committed replacement char direct":       {"\"has�mark\"", "has�mark"},
		"committed replacement char escaped":      {`"has\uFFFDmark"`, "has�mark"},
		"escaped Chinese":                         {`"\u7535\u8DEF"`, "电路"},
		"escaped emoji pair alone":                {`"\uD83D\uDE00"`, "😀"},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			raw := rawEnvelope(tc.nameJSON, false)
			dir := plantRawEnvelope(t, raw)

			s, err := Open(dir)
			if err != nil {
				t.Fatalf("legal name refused: %v", err)
			}
			defer s.Close()
			got, err := s.GetCircuit(tc.want, 1)
			if err != nil {
				t.Fatalf("name %q did not read back: %v", tc.want, err)
			}
			if got.Name != tc.want {
				t.Fatalf("name changed on read: want %q, got %q", tc.want, got.Name)
			}
		})
	}
}

// A directory whose committed name is corrupt refuses every operation —
// reads return nothing partial and mutations never commit — while the file
// stays byte-for-byte in place.
func TestCorruptNameDirectoryFullyRefused(t *testing.T) {
	raw := rawEnvelope(`"电路\uD800"`, true)
	dir := plantRawEnvelope(t, raw)

	if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("open: want ErrDataCorrupt, got %v", err)
	}
	// Every entry point opens the directory first, so with the file damaged
	// there is no handle to read or mutate through at all; confirm the file
	// is untouched after the refused attempts.
	left, err := os.ReadFile(filepath.Join(dir, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(raw) {
		t.Fatalf("data.json changed across refused opens\nwant: %q\n got: %q", raw, left)
	}
}
