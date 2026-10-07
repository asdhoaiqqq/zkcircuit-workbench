package zkcircuit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the exact-readback rule for the owning-circuit name on a
// saved trusted-setup record — the same rule a circuit's own name and a job's
// circuit name already carry. The "name" a setup saves is the sole authority
// for which frozen circuit version the setup belongs to, so its raw bytes
// must denote that circuit exactly.
//
// encoding/json silently rewrites two damage shapes to U+FFFD:
//   - invalid UTF-8 bytes in the literal portions of the token, and
//   - \uXXXX escapes forming an unpaired surrogate.
//
// Without the rule a setup saved with such a token could, on read, collide
// with a genuinely different frozen circuit whose name contains "�" — and
// when that version had no setup of its own, a proof job for that circuit
// would pass the trusted-setup gate against a setup that never belonged to
// it. The read must instead be refused as data corruption: ErrDataCorrupt
// naming the setup record by its 1-based position and the name field, no
// partial results, the file kept byte-for-byte, and the failure never
// reported as "unknown circuit" or "missing setup". The rule covers the
// canonical "name", ASCII letter-case variants and JSON-escaped spellings of
// the key. Direct Chinese, emoji, an actually committed "�", a legal
// surrogate pair and the literal six-character text \uD800 all stay ordinary
// values matched exactly.

// rawSetupLookalikeEnvelope builds a committed envelope with the frozen
// circuit "电路�" v1 and one setup record whose name value is the given raw
// JSON string token (already quoted/escaped by the caller). The setup name
// token is the only thing the caller varies.
func rawSetupLookalikeEnvelope(setupNameJSON string) []byte {
	return []byte(`{"format":1,"circuits":[` +
		`{"name":"电路�","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
		`],"setups":[{"name":` + setupNameJSON + `,"version":1}],"jobs":[]}`)
}

// rawSetupRecordEnvelope wraps a fully hand-written setup record around the
// legal frozen circuit "c" v1, so the caller controls key spellings.
func rawSetupRecordEnvelope(record string) []byte {
	return []byte(`{"format":1,"circuits":[` +
		`{"name":"c","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
		`],"setups":[` + record + `],"jobs":[]}`)
}

const legalSetupTail = `,"version":1}`

// The motivating collision: a genuinely frozen circuit named "电路�" exists,
// and a setup saved its owning name with bytes encoding/json repairs onto
// exactly that name. Even though the repaired name exists and the version is
// frozen, the read must fail as encoding corruption — never confirm a setup
// for that circuit — and a legal setup record sharing the file must not
// produce a partial result.
func TestCorruptSetupNameLookalikeRefused(t *testing.T) {
	damaged := map[string]string{
		"invalid trailing byte": "\"电路\xff\"",
		"truncated multibyte":   "\"电路\xe4\xb8\"",
		"lone high surrogate":   `"电路\uD800"`,
		"lone low surrogate":    `"电路\uDC00"`,
	}
	for label, setupName := range damaged {
		t.Run(label, func(t *testing.T) {
			raw := []byte(`{"format":1,"circuits":[` +
				`{"name":"c","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""},` +
				`{"name":"电路�","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
				`],"setups":[{"name":"c","version":1},{"name":` + setupName + `,"version":1}],"jobs":[]}`)
			dir := plantRawEnvelope(t, raw)

			s, err := Open(dir)
			if err == nil {
				s.Close()
				t.Fatalf("a setup whose name repairs onto an existing frozen circuit was accepted")
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "setup record #2") {
				t.Fatalf("error does not name the damaged setup record by position: %v", err)
			}
			if !strings.Contains(msg, `"name"`) {
				t.Fatalf("error does not name the name field: %v", err)
			}
			if strings.Contains(msg, "unknown circuit") || strings.Contains(msg, "trusted setup") {
				t.Fatalf("encoding damage masked as a lookup failure: %v", err)
			}
			left, rerr := os.ReadFile(filepath.Join(dir, dirDataFile))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(left) != string(raw) {
				t.Fatalf("refused read modified data.json\nwant: %q\n got: %q", raw, left)
			}
			// Reopening refuses identically; the legal first setup is never
			// returned on its own.
			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("reopen: want ErrDataCorrupt, got %v", err)
			}
		})
	}
}

// Invalid UTF-8 bytes in the setup name token are corruption under every
// shape, even when the repaired name would match no circuit at all.
func TestCorruptSetupNameInvalidUTF8Refused(t *testing.T) {
	bad := map[string]string{
		"lone continuation byte":   "\"bad\x80name\"",
		"invalid byte 0xff":        "\"c\xff\"",
		"truncated multi-byte":     "\"trunc\xe4\xb8\"",
		"overlong encoding":        "\"over\xc0\xaf\"",
		"encoded surrogate half":   "\"pair\xed\xa0\x80\"",
		"valid prefix then damage": "\"电路\xff\"",
	}
	for label, setupName := range bad {
		t.Run(label, func(t *testing.T) {
			raw := rawSetupLookalikeEnvelope(setupName)
			dir := plantRawEnvelope(t, raw)

			s, err := Open(dir)
			if err == nil {
				s.Close()
				t.Fatalf("directory with an invalid-UTF-8 setup name was accepted")
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "setup record #1") || !strings.Contains(msg, `"name"`) {
				t.Fatalf("error does not name the setup record and the name field: %v", err)
			}
			if strings.Contains(msg, "unknown circuit") {
				t.Fatalf("encoding damage masked as an unknown-circuit failure: %v", err)
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

// Unpaired \uXXXX surrogate escapes in the setup name token are corruption
// the same way, including high-then-anything and reversed pairs.
func TestCorruptSetupNameUnpairedSurrogateRefused(t *testing.T) {
	bad := map[string]string{
		"lone high surrogate":            `"c\uD800"`,
		"lone low surrogate":             `"\uDC00"`,
		"high surrogate at end":          `"end\uD83D"`,
		"two high surrogates":            `"\uD800\uD800"`,
		"high then non-surrogate escape": `"\uD800A"`,
		"high then simple escape":        `"\uD800\n"`,
		"low then high (reversed pair)":  `"\uDC00\uD800"`,
		"low surrogate after valid pair": `"😀\uDE00"`,
		"escaped lowercase":              `"a\ud800"`,
	}
	for label, setupName := range bad {
		t.Run(label, func(t *testing.T) {
			raw := rawSetupLookalikeEnvelope(setupName)
			dir := plantRawEnvelope(t, raw)

			s, err := Open(dir)
			if err == nil {
				s.Close()
				t.Fatalf("directory with an unpaired-surrogate setup name was accepted")
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "setup record #1") || !strings.Contains(msg, `"name"`) {
				t.Fatalf("error does not name the setup record and the name field: %v", err)
			}
			if !strings.Contains(msg, "surrogate") {
				t.Fatalf("error does not explain the surrogate damage: %v", err)
			}
			left, rerr := os.ReadFile(filepath.Join(dir, dirDataFile))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(left) != string(raw) {
				t.Fatalf("refused read modified data.json")
			}
		})
	}
}

// The rule applies to every accepted spelling of the setup name key: the
// canonical "name", an ASCII letter-case variant such as "NAME" or "NaMe",
// and a JSON-\u-escaped spelling of either.
func TestCorruptSetupNameSpellingVariantsRefused(t *testing.T) {
	escLowerN := jsonBackslash + "u006eame" // name unescapes to "name"
	escUpperN := jsonBackslash + "u004eAME" // NAME unescapes to "NAME"
	cases := []struct {
		label  string
		record string
	}{
		{"canonical key with invalid bytes", `{"name":"c` + "\xff" + `"` + legalSetupTail},
		{"canonical key with lone surrogate", `{"name":"c\uD800"` + legalSetupTail},
		{"upper case key with invalid bytes", `{"NAME":"c` + "\xff" + `"` + legalSetupTail},
		{"upper case key with lone surrogate", `{"NAME":"c\uD800"` + legalSetupTail},
		{"mixed case key with invalid bytes", `{"NaMe":"c` + "\xff" + `"` + legalSetupTail},
		{"escaped lowercase key with bad bytes", `{"` + escLowerN + `":"c` + "\xff" + `"` + legalSetupTail},
		{"escaped uppercase key with surrogate", `{"` + escUpperN + `":"c\uD800"` + legalSetupTail},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			raw := rawSetupRecordEnvelope(tc.record)
			dir := plantRawEnvelope(t, raw)

			_, err := Open(dir)
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), "setup record #1") ||
				!strings.Contains(err.Error(), `"name"`) {
				t.Fatalf("error does not name setup record #1 and the name field: %v", err)
			}
			left, rerr := os.ReadFile(filepath.Join(dir, dirDataFile))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(left) != string(raw) {
				t.Fatalf("refused read modified data.json")
			}
		})
	}
}

// A damaged record on the second setup position is located there, and the
// first, legal setup is never returned on its own.
func TestCorruptSetupNameOnSecondRecordNamesPosition(t *testing.T) {
	raw := []byte(`{"format":1,"circuits":[` +
		`{"name":"c","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""},` +
		`{"name":"电路�","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
		`],"setups":[{"name":"c","version":1},{"name":"电路` + "\xff" + `","version":1}],"jobs":[]}`)
	dir := plantRawEnvelope(t, raw)

	_, err := Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	if !strings.Contains(err.Error(), "setup record #2") {
		t.Fatalf("error does not locate setup record #2: %v", err)
	}
}

// Damage appearing after the directory is open is caught by the next read or
// modification alike: no partial setup query, no committed setup or job, and
// the file is never rewritten.
func TestOpenStoreRefusesAfterSetupNameEncodingDamage(t *testing.T) {
	dir := seedTwoNamedFrozenSetupStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bad := replaceInSetup(t, dir, setupNameLine("a"), `"name": "a`+"\xff"+`"`)

	if _, found, err := s.GetSetup("a", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetSetup(a) after damage: found=%v want ErrDataCorrupt, got %v", found, err)
	}
	if _, found, err := s.GetSetup("b", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetSetup(b) after damage must not partially succeed: found=%v err=%v", found, err)
	}
	if _, err := s.RecordSetup("b", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("RecordSetup after damage: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.SubmitJob(boundProveJob("j-b", "b", 1, "")); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("SubmitJob for b after damage: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "other", Version: 1, Constraints: 1}); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("unrelated CreateCircuit after damage: want ErrDataCorrupt, got %v", err)
	}
	got, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(bad) {
		t.Fatalf("damaged file was rewritten by a refused operation")
	}
}

// Legal setup name encodings read back under their exact value: direct
// Chinese, an emoji written as a surrogate-pair escape, an actually committed
// "�" (direct or �-escaped), and the literal text \uD800.
func TestLegalSetupNameEncodingsReadBackExactly(t *testing.T) {
	cases := map[string]struct {
		nameJSON string
		want     string
	}{
		"direct Chinese": {`"电路"`, "电路"},
		"escaped Chinese": {
			`"` + jsonBackslash + `u7535` + jsonBackslash + `u8DEF"`,
			"电路",
		},
		"emoji pair written as escapes": {
			`"` + jsonBackslash + `uD83D` + jsonBackslash + `uDE00"`,
			"😀",
		},
		"committed replacement char direct":  {`"has�mark"`, "has�mark"},
		"committed replacement char escaped": {`"has` + jsonBackslash + `uFFFDmark"`, "has�mark"},
		"replacement char with Chinese":      {`"电路�"`, "电路�"},
		"literal backslash-u text": {
			`"` + `x` + jsonBackslash + jsonBackslash + `uD800"`,
			`x\uD800`,
		},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			raw := []byte(`{"format":1,"circuits":[` +
				`{"name":` + tc.nameJSON + `,"version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
				`],"setups":[{"name":` + tc.nameJSON + `,"version":1}],"jobs":[]}`)
			dir := plantRawEnvelope(t, raw)

			s, err := Open(dir)
			if err != nil {
				t.Fatalf("legal setup name encoding refused: %v", err)
			}
			defer s.Close()
			got, found, err := s.GetSetup(tc.want, 1)
			if err != nil || !found {
				t.Fatalf("setup did not read back under %q: found=%v err=%v", tc.want, found, err)
			}
			if got.Name != tc.want {
				t.Fatalf("setup name changed on read: want %q, got %q", tc.want, got.Name)
			}
			// The setup keeps gating a proof job for that exact circuit.
			if _, err := s.SubmitJob(boundProveJob("j1", tc.want, 1, "")); err != nil {
				t.Fatalf("a legal name's setup must pass the job gate: %v", err)
			}
		})
	}
}

// Legal Unicode setup names also round-trip through the write path: register
// the setup, reopen, and read it back owned by the exact name.
func TestRecordSetupUnicodeNameRoundTrips(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"电路-1", "emoji 😀 c", "has�mark", `literal\uD800`}
	for _, name := range names {
		seedFrozenSource(t, s, name, 1, 1, 1, 1, "")
		if _, err := s.RecordSetup(name, 1); err != nil {
			t.Fatalf("record setup for %q: %v", name, err)
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
	for _, name := range names {
		got, found, err := s2.GetSetup(name, 1)
		if err != nil || !found {
			t.Fatalf("setup for %q did not round-trip: found=%v err=%v", name, found, err)
		}
		if got.Name != name {
			t.Fatalf("setup name changed across save and read: want %q, got %q", name, got.Name)
		}
	}
}

// Exact name matching is untouched: a value that differs only by letter case
// or surrounding whitespace still names no circuit and fails as it always
// did, rather than being treated as encoding damage or folded onto "c".
func TestSetupNameExactMatchingUnchanged(t *testing.T) {
	cases := map[string]string{
		"case differs":   `"C"`,
		"trailing space": `"c "`,
		"leading space":  `" c"`,
	}
	for label, setupName := range cases {
		t.Run(label, func(t *testing.T) {
			raw := rawSetupRecordEnvelope(`{"name":` + setupName + legalSetupTail)
			dir := plantRawEnvelope(t, raw)
			s, err := Open(dir)
			if err == nil {
				s.Close()
				t.Fatalf("an inexact name match was accepted")
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if strings.Contains(err.Error(), "would not read back unchanged") {
				t.Fatalf("an exact-but-different name was misreported as encoding damage: %v", err)
			}
		})
	}
}
