package zkcircuit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the exact-readback rule for the owning-circuit value on a
// saved proof job — the same rule the job id and a circuit's own name already
// carry. The "circuit" string a job saves is the sole authority for which
// frozen version, trusted setup and compiled artifact the job belongs to, so
// its raw bytes must denote that circuit exactly.
//
// encoding/json silently rewrites two damage shapes to U+FFFD:
//   - invalid UTF-8 bytes in the literal portions of the token, and
//   - \uXXXX escapes forming an unpaired surrogate.
//
// Without the rule a job saved with such a token could, on read, collide
// with a genuinely different circuit whose name contains "�" — and when that
// version is frozen, has a trusted setup and a compiled artifact, the job
// would keep succeeding against the wrong circuit. The read must instead be
// refused as data corruption: ErrDataCorrupt naming the job (by its id when
// legible, else by record position) and the circuit field, no partial
// results, the file kept byte-for-byte, and the failure never reported as
// "unknown circuit" or "missing setup". The rule covers the canonical
// "circuit", ASCII letter-case variants and JSON-escaped spellings of the
// key. Direct Chinese, emoji, an actually committed "�", a legal surrogate
// pair and the literal six-character text \uD800 all stay ordinary values
// matched exactly.

// rawJobCircuitEnvelope builds a complete committed envelope with the frozen,
// setup-bearing circuit "c" v1 and one job j1 whose circuit value is the
// given raw JSON string token (already quoted/escaped by the caller).
func rawJobCircuitEnvelope(circuitJSON string) []byte {
	return []byte(`{"format":1,"circuits":[` +
		`{"name":"c","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
		`],"setups":[{"name":"c","version":1}],"jobs":[` +
		`{"id":"j1","circuit":` + circuitJSON + `,"version":1,"kind":"prove","attempt":1}]}`)
}

// rawJobCircuitRecordEnvelope wraps a single fully hand-written job record
// around the legal "c" v1 circuit, so the caller controls key spellings.
func rawJobCircuitRecordEnvelope(record string) []byte {
	return []byte(`{"format":1,"circuits":[` +
		`{"name":"c","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
		`],"setups":[{"name":"c","version":1}],"jobs":[` + record + `]}`)
}

const legalJobTail = `,"version":1,"kind":"prove","attempt":1}`

// The motivating collision: a genuinely frozen, setup-bearing circuit named
// "电路\xef\xbf\xbd" ("电路�") exists, and a job saved its circuit with
// bytes encoding/json repairs onto exactly that name. Even though the
// repaired name exists, the version is frozen and the setup is present, the
// read must fail as encoding corruption — never continue against that
// circuit — and a legal job sharing the file must not produce a partial
// result.
func TestCorruptJobCircuitLookalikeNameRefused(t *testing.T) {
	// Both damage shapes decode to "电路�": a trailing invalid byte and an
	// unpaired high-surrogate escape after the intact Chinese prefix.
	damaged := map[string]string{
		"invalid trailing byte": "\"电路\xff\"",
		"truncated multibyte":   "\"电路\xe4\xb8\"",
		"lone high surrogate":   `"电路\uD800"`,
		"lone low surrogate":    `"电路\uDC00"`,
	}
	for label, jobCircuit := range damaged {
		t.Run(label, func(t *testing.T) {
			raw := []byte(`{"format":1,"circuits":[` +
				`{"name":"电路�","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
				`],"setups":[{"name":"电路�","version":1}],"jobs":[` +
				`{"id":"j-legal","circuit":"电路�","version":1,"kind":"prove","attempt":1},` +
				`{"id":"j-bad","circuit":` + jobCircuit + `,"version":1,"kind":"prove","attempt":1}]}`)
			dir := plantRawEnvelope(t, raw)

			s, err := Open(dir)
			if err == nil {
				s.Close()
				t.Fatalf("a job whose circuit repairs onto an existing frozen circuit was accepted")
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, `stored job record "j-bad"`) {
				t.Fatalf("error does not name the damaged job by id: %v", err)
			}
			if !strings.Contains(msg, `"circuit"`) {
				t.Fatalf("error does not name the circuit field: %v", err)
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
			// Reopening refuses identically, and the legal job is never
			// returned on its own.
			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("reopen: want ErrDataCorrupt, got %v", err)
			}
		})
	}
}

// The same collision with a compiled artifact: the damaged job is bound to a
// compiled_hash, and the real "电路�" version carries exactly that artifact.
// The encoding refusal must still come first — the job may not read as bound
// to the other circuit's compiled artifact, and the message must not be a
// hash mismatch.
func TestCorruptJobCircuitLookalikeWithBoundArtifactRefused(t *testing.T) {
	for label, fragment := range map[string]string{
		"invalid byte":   `"circuit": "电路` + "\xff" + `"`,
		"lone surrogate": `"circuit": "电路\uD800"`,
	} {
		t.Run(label, func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			h := compiledVersion(t, s, "电路�", 1, validDef)
			if _, err := s.SubmitJob(boundProveJob("j-legal", "电路�", 1, h)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SubmitJob(boundProveJob("j-bad", "电路�", 1, h)); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			bad := replaceInJob(t, dir, "j-bad", `"circuit": "电路�"`, fragment)

			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("bound lookalike job: want ErrDataCorrupt, got %v", err)
			} else if !strings.Contains(err.Error(), `"j-bad"`) ||
				!strings.Contains(err.Error(), `"circuit"`) {
				t.Fatalf("error does not name the job and circuit field: %v", err)
			}
			left, rerr := readDataFile(t, dir)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(left) != string(bad) {
				t.Fatalf("refused read modified data.json")
			}
		})
	}
}

// Invalid UTF-8 bytes in the circuit token are corruption under every shape,
// even when the repaired name would match no circuit at all.
func TestCorruptJobCircuitInvalidUTF8Refused(t *testing.T) {
	bad := map[string]string{
		"lone continuation byte":   "\"bad\x80name\"",
		"invalid byte 0xff":        "\"c\xff\"",
		"truncated multi-byte":     "\"trunc\xe4\xb8\"",
		"overlong encoding":        "\"over\xc0\xaf\"",
		"encoded surrogate half":   "\"pair\xed\xa0\x80\"",
		"valid prefix then damage": "\"电路\xff\"",
	}
	for label, circuitJSON := range bad {
		t.Run(label, func(t *testing.T) {
			raw := rawJobCircuitEnvelope(circuitJSON)
			dir := plantRawEnvelope(t, raw)

			s, err := Open(dir)
			if err == nil {
				s.Close()
				t.Fatalf("directory with an invalid-UTF-8 job circuit was accepted")
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, `stored job record "j1"`) || !strings.Contains(msg, `"circuit"`) {
				t.Fatalf("error does not name the job and the circuit field: %v", err)
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

// Unpaired \uXXXX surrogate escapes in the circuit token are corruption the
// same way, including high-then-anything and reversed pairs.
func TestCorruptJobCircuitUnpairedSurrogateRefused(t *testing.T) {
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
	for label, circuitJSON := range bad {
		t.Run(label, func(t *testing.T) {
			raw := rawJobCircuitEnvelope(circuitJSON)
			dir := plantRawEnvelope(t, raw)

			s, err := Open(dir)
			if err == nil {
				s.Close()
				t.Fatalf("directory with an unpaired-surrogate job circuit was accepted")
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, `stored job record "j1"`) || !strings.Contains(msg, `"circuit"`) {
				t.Fatalf("error does not name the job and the circuit field: %v", err)
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

// The rule applies to every accepted spelling of the circuit key: the
// canonical "circuit", an ASCII letter-case variant such as "CIRCUIT" or
// "CiRcUiT", and a JSON-\u-escaped spelling of either. A record without an
// id is located by its record position.
func TestCorruptJobCircuitSpellingVariantsRefused(t *testing.T) {
	escLowerC := jsonBackslash + "u0063ircuit" // circuit unescapes to "circuit"
	escUpperC := jsonBackslash + "u0043IRCUIT" // CIRCUIT unescapes to "CIRCUIT"
	cases := []struct {
		label    string
		record   string
		wantName string // how the damaged record is located in the message
	}{
		{"canonical key with invalid bytes", `{"id":"j1","circuit":"c` + "\xff" + `"` + legalJobTail, `stored job record "j1"`},
		{"canonical key with lone surrogate", `{"id":"j1","circuit":"c\uD800"` + legalJobTail, `stored job record "j1"`},
		{"upper case key with invalid bytes", `{"id":"j1","CIRCUIT":"c` + "\xff" + `"` + legalJobTail, `stored job record "j1"`},
		{"upper case key with lone surrogate", `{"id":"j1","CIRCUIT":"c\uD800"` + legalJobTail, `stored job record "j1"`},
		{"mixed case key with invalid bytes", `{"id":"j1","CiRcUiT":"c` + "\xff" + `"` + legalJobTail, `stored job record "j1"`},
		{`escaped lowercase key with bad bytes`, `{"id":"j1","` + escLowerC + `":"c` + "\xff" + `"` + legalJobTail, `stored job record "j1"`},
		{`escaped uppercase key with surrogate`, `{"id":"j1","` + escUpperC + `":"c\uD800"` + legalJobTail, `stored job record "j1"`},
		{"no id: located positionally, bad bytes", `{"circuit":"c` + "\xff" + `"` + legalJobTail, "job record #1"},
		{"no id: located positionally, surrogate", `{"CIRCUIT":"c\uD800"` + legalJobTail, "job record #1"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			raw := rawJobCircuitRecordEnvelope(tc.record)
			dir := plantRawEnvelope(t, raw)

			_, err := Open(dir)
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantName) ||
				!strings.Contains(err.Error(), `"circuit"`) {
				t.Fatalf("error does not name %s and the circuit field: %v", tc.wantName, err)
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

// Damage appearing after the directory is open is caught by the next read or
// modification alike: no partial job list, no committed mutation, and the
// file is never rewritten.
func TestOpenStoreRefusesAfterJobCircuitEncodingDamage(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedFrozenSource(t, s, "c", 1, 1, 1, 1, "")
	if _, err := s.SubmitJob(boundProveJob("j1", "c", 1, "")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Open a handle on the intact directory first; every operation reloads
	// under the lock, so damage written afterwards must be refused by the
	// very next read or modification.
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	bad := replaceInJob(t, dir, "j1", `"circuit": "c"`, `"circuit": "c`+"\xff"+`"`)

	if _, err := s2.GetJob("j1"); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetJob after damage: want ErrDataCorrupt, got %v", err)
	}
	if jobs, err := s2.ListJobs(); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("ListJobs after damage: want ErrDataCorrupt, got %d jobs err=%v", len(jobs), err)
	}
	if _, err := s2.SubmitJob(boundProveJob("j2", "c", 1, "")); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("SubmitJob after damage: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s2.CreateCircuit(Circuit{Name: "other", Version: 1, Constraints: 1}); !errors.Is(err, ErrDataCorrupt) {
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

// Legal circuit encodings read back under their exact value: direct Chinese,
// an emoji written as a surrogate-pair escape, an actually committed "�"
// (direct or �-escaped), and the literal text \uD800.
func TestLegalJobCircuitEncodingsReadBackExactly(t *testing.T) {
	cases := map[string]struct {
		nameJSON string // circuit record name token
		jobJSON  string // job circuit token
		want     string
	}{
		"direct Chinese": {`"电路"`, `"电路"`, "电路"},
		"escaped Chinese": {
			`"` + jsonBackslash + `u7535` + jsonBackslash + `u8DEF"`,
			`"` + jsonBackslash + `u7535` + jsonBackslash + `u8DEF"`,
			"电路",
		},
		"emoji pair written as escapes": {
			`"` + jsonBackslash + `uD83D` + jsonBackslash + `uDE00"`,
			`"` + jsonBackslash + `uD83D` + jsonBackslash + `uDE00"`,
			"😀",
		},
		"committed replacement char direct":  {`"has�mark"`, `"has�mark"`, "has�mark"},
		"committed replacement char escaped": {`"has` + jsonBackslash + `uFFFDmark"`, `"has` + jsonBackslash + `uFFFDmark"`, "has�mark"},
		"replacement char with Chinese":      {`"电路�"`, `"电路�"`, "电路�"},
		"literal backslash-u text": {
			`"` + `x` + jsonBackslash + jsonBackslash + `uD800"`,
			`"` + `x` + jsonBackslash + jsonBackslash + `uD800"`,
			`x\uD800`,
		},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			raw := []byte(`{"format":1,"circuits":[` +
				`{"name":` + tc.nameJSON + `,"version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
				`],"setups":[{"name":` + tc.nameJSON + `,"version":1}],"jobs":[` +
				`{"id":"j1","circuit":` + tc.jobJSON + `,"version":1,"kind":"prove","attempt":1}]}`)
			dir := plantRawEnvelope(t, raw)

			s, err := Open(dir)
			if err != nil {
				t.Fatalf("legal circuit encoding refused: %v", err)
			}
			defer s.Close()
			got, err := s.GetJob("j1")
			if err != nil {
				t.Fatalf("job did not read back: %v", err)
			}
			if got.Circuit != tc.want {
				t.Fatalf("circuit changed on read: want %q, got %q", tc.want, got.Circuit)
			}
		})
	}
}

// Legal Unicode circuit names also round-trip through the write path: submit
// against them, reopen, and get the job owned by the exact name.
func TestSubmitJobUnicodeCircuitRoundTrips(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"电路-1", "emoji 😀 c", "has�mark", `literal\uD800`}
	for _, name := range names {
		seedFrozenSource(t, s, name, 1, 1, 1, 1, "")
		if _, err := s.SubmitJob(boundProveJob("j-"+name, name, 1, "")); err != nil {
			t.Fatalf("submit against %q: %v", name, err)
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
		got, err := s2.GetJob("j-" + name)
		if err != nil {
			t.Fatalf("job for %q did not round-trip: %v", name, err)
		}
		if got.Circuit != name {
			t.Fatalf("circuit changed across save and read: want %q, got %q", name, got.Circuit)
		}
	}
}

// SubmitJob refuses an invalid-UTF-8 circuit with ErrInvalidArgument before
// anything is committed (the write-path counterpart of the read rule).
func TestSubmitJobRejectsInvalidUTF8Circuit(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedFrozenSource(t, s, "c", 1, 1, 1, 1, "")
	if _, err := s.SubmitJob(boundProveJob("j1", "c", 1, "")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}

	bad := map[string]string{
		"lone continuation byte":   "bad\x80name",
		"truncated multi-byte":     "trunc\xe4\xb8",
		"invalid byte 0xff":        "c\xff",
		"encoded surrogate half":   "pair\xed\xa0\x80",
		"valid prefix then damage": "电路\xff",
	}
	for label, circuit := range bad {
		t.Run(label, func(t *testing.T) {
			_, err := s.SubmitJob(boundProveJob("j-x", circuit, 1, ""))
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("want ErrInvalidArgument, got %v", err)
			}
			if !strings.Contains(err.Error(), "UTF-8") {
				t.Fatalf("error does not explain the circuit encoding is invalid: %v", err)
			}
		})
	}

	after, err := os.ReadFile(filepath.Join(dir, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("refused submissions modified data.json\nwant: %q\n got: %q", before, after)
	}
	if jobs, err := s.ListJobs(); err != nil || len(jobs) != 1 || jobs[0].ID != "j1" {
		t.Fatalf("existing job damaged by refused submissions: %+v %v", jobs, err)
	}
}

// Exact name matching is untouched: a value that differs only by letter case
// or surrounding whitespace still names no circuit and fails as it always
// did, rather than being treated as encoding damage or folded onto "c".
func TestJobCircuitExactMatchingUnchanged(t *testing.T) {
	cases := map[string]string{
		"case differs":   `"C"`,
		"trailing space": `"c "`,
		"leading space":  `" c"`,
	}
	for label, circuitJSON := range cases {
		t.Run(label, func(t *testing.T) {
			raw := rawJobCircuitEnvelope(circuitJSON)
			dir := plantRawEnvelope(t, raw)
			s, err := Open(dir)
			if err == nil {
				s.Close()
				t.Fatalf("an inexact circuit match was accepted")
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
