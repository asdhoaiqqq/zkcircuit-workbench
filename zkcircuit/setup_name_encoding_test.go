package zkcircuit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the exact-readback rule for the owning-circuit value on a
// saved trusted-setup record — the same rule a circuit's own name and a job's
// circuit already carry. The "name" string a setup saves is the sole
// authority for which frozen circuit the setup belongs to, so its raw bytes
// must denote that circuit exactly.
//
// encoding/json silently rewrites two damage shapes to U+FFFD:
//   - invalid UTF-8 bytes in the literal portions of the token, and
//   - \uXXXX escapes forming an unpaired surrogate.
//
// Without the rule a setup record carrying such a token could, on read,
// confirm the setup of a genuinely different frozen circuit whose name
// contains "�", and a proof job against that circuit would pass the trusted
// setup gate. The read must instead be refused as data corruption:
// ErrDataCorrupt naming the setup record by its 1-based position and the name
// field, no partial results, the file kept byte-for-byte, and the failure
// never reported as "unknown circuit" or "missing setup". The rule covers the
// canonical "name", ASCII letter-case variants and JSON-escaped spellings of
// the key. Direct Chinese, emoji, an actually committed "�", a legal
// surrogate pair and the literal six-character text \uD800 all stay ordinary
// values matched exactly.

// rawSetupEnvelope builds a complete committed envelope with one frozen
// circuit whose name is the given raw JSON string token (already
// quoted/escaped by the caller) and one setup record whose name token is
// likewise caller-supplied. When withJob is true a proof job bound to the
// repaired circuit rides along: under the old behavior it passed the trusted
// setup gate once the setup name was repaired.
func rawSetupEnvelope(circuitJSON, setupJSON string, withJob bool) []byte {
	var b strings.Builder
	b.WriteString(`{"format":1,"circuits":[`)
	b.WriteString(`{"name":`)
	b.WriteString(circuitJSON)
	b.WriteString(`,"version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}`)
	b.WriteString(`],"setups":[{"name":`)
	b.WriteString(setupJSON)
	b.WriteString(`,"version":1}],"jobs":[`)
	if withJob {
		b.WriteString(`{"id":"j1","circuit":`)
		b.WriteString(circuitJSON)
		b.WriteString(`,"version":1,"kind":"prove","attempt":1}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// rawSetupRecordEnvelope wraps a single fully hand-written setup record
// around the legal frozen circuit named by circuitJSON, so the caller
// controls the spelling of the setup record's name key.
func rawSetupRecordEnvelope(circuitJSON, record string) []byte {
	return []byte(`{"format":1,"circuits":[` +
		`{"name":` + circuitJSON + `,"version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
		`],"setups":[` + record + `],"jobs":[]}`)
}

// The motivating collision: a genuinely frozen circuit named "电路�" exists
// and a proof job is bound to it, while the setup record's name token only
// decodes onto that name through U+FFFD replacement. Even though the
// repaired name exists, the version is frozen and everything the job's setup
// gate asks for is in place, the read must fail as encoding corruption —
// never confirm the setup — and the file must stay byte-for-byte in place.
func TestCorruptSetupNameLookalikeRefused(t *testing.T) {
	damaged := map[string]string{
		"invalid trailing byte": "\"电路\xff\"",
		"truncated multibyte":   "\"电路\xe4\xb8\"",
		"lone high surrogate":   `"电路\uD800"`,
		"lone low surrogate":    `"电路\uDC00"`,
	}
	for label, setupJSON := range damaged {
		t.Run(label, func(t *testing.T) {
			raw := rawSetupEnvelope(`"电路�"`, setupJSON, true)
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
			if !strings.Contains(msg, "setup record #1") {
				t.Fatalf("error does not locate the setup record: %v", err)
			}
			if !strings.Contains(msg, `"name"`) {
				t.Fatalf("error does not name the name field: %v", err)
			}
			if strings.Contains(msg, "unknown circuit") || strings.Contains(msg, "trusted setup missing") {
				t.Fatalf("encoding damage masked as a lookup failure: %v", err)
			}
			left, rerr := os.ReadFile(filepath.Join(dir, dirDataFile))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(left) != string(raw) {
				t.Fatalf("refused read modified data.json\nwant: %q\n got: %q", raw, left)
			}
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
	for label, setupJSON := range bad {
		t.Run(label, func(t *testing.T) {
			// The only circuit is "c"; the repaired name resolves to nothing.
			raw := rawSetupEnvelope(`"c"`, setupJSON, false)
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
			if !strings.Contains(msg, "invalid UTF-8") {
				t.Fatalf("error does not explain the name encoding is invalid UTF-8: %v", err)
			}
			if strings.Contains(msg, "belongs to unknown circuit") {
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
	for label, setupJSON := range bad {
		t.Run(label, func(t *testing.T) {
			raw := rawSetupEnvelope(`"c"`, setupJSON, false)
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

// The rule applies to every accepted spelling of the name key: the canonical
// "name", an ASCII letter-case variant such as "NAME" or "NaMe", and a
// JSON-\u-escaped spelling of either.
func TestCorruptSetupNameSpellingVariantsRefused(t *testing.T) {
	escLowerN := jsonBackslash + "u006eame" // name unescapes to "name"
	escUpperN := jsonBackslash + "u004eAME" // NAME unescapes to "NAME"
	cases := []struct {
		label  string
		record string
	}{
		{"canonical key with invalid bytes", `{"name":"c` + "\xff" + `","version":1}`},
		{"canonical key with lone surrogate", `{"name":"c\uD800","version":1}`},
		{"upper case key with invalid bytes", `{"NAME":"c` + "\xff" + `","version":1}`},
		{"upper case key with lone surrogate", `{"NAME":"c\uD800","version":1}`},
		{"mixed case key with invalid bytes", `{"NaMe":"c` + "\xff" + `","version":1}`},
		{"escaped lowercase key with bad bytes", `{"` + escLowerN + `":"c` + "\xff" + `","version":1}`},
		{"escaped uppercase key with surrogate", `{"` + escUpperN + `":"c\uD800","version":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			raw := rawSetupRecordEnvelope(`"c"`, tc.record)
			dir := plantRawEnvelope(t, raw)

			_, err := Open(dir)
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "setup record #1") || !strings.Contains(msg, `"name"`) {
				t.Fatalf("error does not name the setup record and the name field: %v", err)
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

// The damaged record is located by its 1-based position in the setups array;
// a legal record sharing the file is never returned on its own.
func TestCorruptSetupNameOnSecondRecordNamesPosition(t *testing.T) {
	raw := []byte(`{"format":1,"circuits":[` +
		`{"name":"c","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""},` +
		`{"name":"电路�","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
		`],"setups":[` +
		`{"name":"c","version":1},` +
		`{"name":"电路` + "\xff" + `","version":1}],"jobs":[]}`)
	dir := plantRawEnvelope(t, raw)

	_, err := Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	if !strings.Contains(err.Error(), "setup record #2") {
		t.Fatalf("error does not locate setup record #2: %v", err)
	}
	left, rerr := os.ReadFile(filepath.Join(dir, dirDataFile))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(left) != string(raw) {
		t.Fatalf("refused read modified data.json")
	}
}

// Damage appearing after the directory is open is caught by the next read or
// modification alike: no setup result, no committed registration or job, and
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
		t.Fatalf("SubmitJob after damage: want ErrDataCorrupt, got %v", err)
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

// RecordSetup refuses an invalid-UTF-8 name with ErrInvalidArgument before
// anything is committed (the write-path counterpart of the read rule).
func TestRecordSetupRejectsInvalidUTF8(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedFrozenSource(t, s, "c", 1, 1, 1, 1, "")
	if _, err := s.RecordSetup("c", 1); err != nil {
		t.Fatal(err)
	}
	before, rerr := os.ReadFile(filepath.Join(dir, dirDataFile))
	if rerr != nil {
		t.Fatal(rerr)
	}

	bad := map[string]string{
		"lone continuation byte":   "bad\x80name",
		"truncated multi-byte":     "trunc\xe4\xb8",
		"invalid byte 0xff":        "c\xff",
		"encoded surrogate half":   "pair\xed\xa0\x80",
		"valid prefix then damage": "电路\xff",
	}
	for label, name := range bad {
		t.Run(label, func(t *testing.T) {
			_, err := s.RecordSetup(name, 1)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("want ErrInvalidArgument, got %v", err)
			}
			if !strings.Contains(err.Error(), "UTF-8") {
				t.Fatalf("error does not explain the name encoding is invalid: %v", err)
			}
		})
	}

	after, rerr := os.ReadFile(filepath.Join(dir, dirDataFile))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(after) != string(before) {
		t.Fatalf("refused registrations modified data.json\nwant: %q\n got: %q", before, after)
	}
	if _, found, err := s.GetSetup("c", 1); err != nil || !found {
		t.Fatalf("existing setup damaged by refused registrations: found=%v err=%v", found, err)
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
		"direct Chinese":                     {`"电路"`, "电路"},
		"escaped Chinese":                    {`"` + jsonBackslash + `u7535` + jsonBackslash + `u8DEF"`, "电路"},
		"emoji pair written as escapes":      {`"` + jsonBackslash + `uD83D` + jsonBackslash + `uDE00"`, "😀"},
		"committed replacement char direct":  {`"has�mark"`, "has�mark"},
		"committed replacement char escaped": {`"has` + jsonBackslash + `uFFFDmark"`, "has�mark"},
		"replacement char with Chinese":      {`"电路�"`, "电路�"},
		"literal backslash-u text":           {`"` + jsonBackslash + jsonBackslash + `uD800"`, `\uD800`},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			raw := rawSetupEnvelope(tc.nameJSON, tc.nameJSON, false)
			dir := plantRawEnvelope(t, raw)

			s, err := Open(dir)
			if err != nil {
				t.Fatalf("legal setup name refused: %v", err)
			}
			defer s.Close()
			got, found, err := s.GetSetup(tc.want, 1)
			if err != nil || !found {
				t.Fatalf("setup for %q did not read back: found=%v err=%v", tc.want, found, err)
			}
			if got.Name != tc.want {
				t.Fatalf("name changed on read: want %q, got %q", tc.want, got.Name)
			}
			// A proof job against the exact name still passes the setup gate.
			if _, err := s.SubmitJob(boundProveJob("j1", tc.want, 1, "")); err != nil {
				t.Fatalf("job against legal setup %q: %v", tc.want, err)
			}
		})
	}
}

// Legal Unicode names also round-trip through the write path: freeze, record
// the setup, reopen, and get it owned by the exact name.
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
		if _, found, err := s2.GetSetup(name, 1); err != nil || !found {
			t.Fatalf("setup for %q did not round-trip: found=%v err=%v", name, found, err)
		}
	}
}

// Exact name matching is untouched: a value that differs only by letter case
// or surrounding whitespace still names no circuit and fails integrity
// validation as it always did, rather than being treated as encoding damage
// or folded onto "a".
func TestSetupNameExactMatchingUnchanged(t *testing.T) {
	for label, frag := range map[string]string{
		"case differs":   `"name": "A"`,
		"trailing space": `"name": "a "`,
		"leading space":  `"name": " a"`,
	} {
		t.Run(label, func(t *testing.T) {
			dir := seedTwoNamedFrozenSetupStore(t)
			replaceInSetup(t, dir, setupNameLine("a"), frag)
			_, err := Open(dir)
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if strings.Contains(err.Error(), "would not read back unchanged") {
				t.Fatalf("an exact-but-different name was misreported as encoding damage: %v", err)
			}
		})
	}
}
