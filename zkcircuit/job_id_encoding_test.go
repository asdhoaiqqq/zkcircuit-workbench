package zkcircuit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the proof-job id identity rule: a job id is the job's
// identity and must keep one exact value across submit, save, read and
// query — no replacement, trimming or case folding.
//
// On the write path an id that is not complete, valid UTF-8 (a lone
// continuation byte, a truncated multi-byte character, an encoded surrogate
// half) is refused as ErrInvalidArgument before anything is committed, even
// when the target version is frozen with a trusted setup and every other
// condition is legal. encoding/json would otherwise silently rewrite those
// bytes to U+FFFD at save time, so a query by the submitted bytes would miss
// the job and the job could collide with an id that genuinely contains "�".
//
// On the read path a committed job record whose raw id token carries invalid
// UTF-8 bytes, or \uXXXX escapes forming an unpaired surrogate, is data
// corruption: the whole directory read fails with ErrDataCorrupt naming the
// job record and the id field (located by record number when the id cannot be
// restored), no partial results are returned, no modification commits, and
// the file stays byte-for-byte in place. The same rule applies to the id
// written under an ASCII case spelling or restored from a JSON-escaped key.
// Legal Chinese, emoji, an actually committed "�", a legal surrogate pair and
// the literal six characters \uD800 are all ordinary ids and keep their exact
// value through submit, save, query and list.

// rawJobEnvelope builds a complete committed envelope with one frozen circuit
// c@1 carrying a trusted setup, an always-legal job "ok" and one job whose id
// is the given raw JSON token (already quoted/escaped by the caller).
func rawJobEnvelope(idJSON string) []byte {
	return []byte(`{"format":1,"circuits":[` +
		`{"name":"c","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":true,"description":""}` +
		`],"setups":[{"name":"c","version":1}],"jobs":[` +
		`{"id":"ok","circuit":"c","version":1,"kind":"prove","attempt":1},` +
		`{"id":` + idJSON + `,"circuit":"c","version":1,"kind":"prove","attempt":1}` +
		`]}`)
}

// rawJobEnvelopeKeyed is rawJobEnvelope but the damaged job's id is written
// under keyJSON (the raw, already quoted JSON member key) instead of "id".
func rawJobEnvelopeKeyed(keyJSON, idJSON string) []byte {
	return []byte(`{"format":1,"circuits":[` +
		`{"name":"c","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":true,"description":""}` +
		`],"setups":[{"name":"c","version":1}],"jobs":[` +
		`{"id":"ok","circuit":"c","version":1,"kind":"prove","attempt":1},` +
		`{` + keyJSON + `:` + idJSON + `,"circuit":"c","version":1,"kind":"prove","attempt":1}` +
		`]}`)
}

// TestSubmitJobRejectsInvalidUTF8ID: a non-blank id carrying invalid UTF-8 is
// refused as ErrInvalidArgument against a frozen, set-up version with every
// other condition legal; no job (under the original or a replaced id) is
// left, the existing records and data.json stay byte-for-byte as they were.
func TestSubmitJobRejectsInvalidUTF8ID(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	compiledVersion(t, s, "c", 1, validDef)
	if _, err := s.RecordSetup("c", 1); err != nil {
		t.Fatal(err)
	}
	// One legal job already present; the refused submissions must leave it
	// and the committed file exactly as they are.
	if _, err := s.SubmitJob(boundProveJob("keep", "c", 1, "")); err != nil {
		t.Fatal(err)
	}
	dataPath := filepath.Join(dir, dirDataFile)
	before, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}

	bad := map[string]string{
		"lone continuation byte":   "job\x80id",
		"truncated multi-byte":     "trunc\xe4\xb8",
		"overlong encoding":        "over\xc0\xaf",
		"invalid byte 0xff":        "bad\xff",
		"encoded surrogate half":   "pair\xed\xa0\x80",
		"truncated at end":         "end\xc3",
		"continuation then ASCII":  "bad\x80x",
		"valid prefix then damage": "任务\xff",
	}
	for label, id := range bad {
		t.Run(label, func(t *testing.T) {
			_, err := s.SubmitJob(boundProveJob(id, "c", 1, ""))
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("want ErrInvalidArgument, got %v", err)
			}
			if !strings.Contains(err.Error(), "UTF-8") {
				t.Fatalf("error does not explain the id encoding is invalid: %v", err)
			}
		})
	}

	after, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("refused submissions modified data.json\nwant: %q\n got: %q", before, after)
	}
	jobs, err := s.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "keep" {
		t.Fatalf("existing jobs damaged by refused submissions: %+v", jobs)
	}
	if _, err := s.GetJob("keep"); err != nil {
		t.Fatalf("existing job unreadable after refused submissions: %v", err)
	}
}

// TestSubmitJobValidUnicodeIDsRoundTrip: Chinese, emoji, a real "�", the
// literal text \uD800 and a mixed id are ordinary ids that submit, save and
// read back under their exact value, and list order follows byte/string
// ordering with no normalization.
func TestSubmitJobValidUnicodeIDsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	compiledVersion(t, s, "c", 1, validDef)
	if _, err := s.RecordSetup("c", 1); err != nil {
		t.Fatal(err)
	}
	ids := []string{
		"任务",            // Chinese
		"job 😀",         // emoji
		"has�mark",      // an actually committed replacement character
		`literal\uD800`, // the literal six characters \uD800 — no escape
		"mixed 任务😀�mix", // everything together
	}
	for _, id := range ids {
		if _, err := s.SubmitJob(boundProveJob(id, "c", 1, "")); err != nil {
			t.Fatalf("submit %q: %v", id, err)
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
	for _, id := range ids {
		got, err := s2.GetJob(id)
		if err != nil {
			t.Fatalf("id %q did not round-trip: %v", id, err)
		}
		if got.ID != id {
			t.Fatalf("id changed across save and read: want %q, got %q", id, got.ID)
		}
	}
	// An id genuinely containing "�" must not collide with one whose invalid
	// bytes would have been replaced: the real replacement id is still its own
	// job, and no replacement-collapsed twin exists.
	if _, err := s2.GetJob("has�mark"); err != nil {
		t.Fatalf("replacement-character id not found exactly: %v", err)
	}
}

// TestCorruptJobIDInvalidUTF8Refused: a saved job id token with invalid UTF-8
// bytes is data corruption even with a legal job alongside; the open fails
// naming the job record (by number) and the id field, and the file is
// untouched.
func TestCorruptJobIDInvalidUTF8Refused(t *testing.T) {
	bad := map[string]string{
		"lone continuation byte":   "\"job\x80id\"",
		"invalid byte 0xff":        "\"bad\xff\"",
		"truncated multi-byte":     "\"trunc\xe4\xb8\"",
		"overlong encoding":        "\"over\xc0\xaf\"",
		"encoded surrogate half":   "\"pair\xed\xa0\x80\"",
		"truncated at end":         "\"end\xc3\"",
		"valid prefix then damage": "\"任务\xff\"",
	}
	for label, idJSON := range bad {
		t.Run(label, func(t *testing.T) {
			raw := rawJobEnvelope(idJSON)
			dir := plantRawEnvelope(t, raw)

			s, err := Open(dir)
			if err == nil {
				s.Close()
				t.Fatalf("directory with an invalid-UTF-8 job id was accepted")
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), "job record #2") ||
				!strings.Contains(err.Error(), `"id"`) {
				t.Fatalf("error does not name the job record and id field: %v", err)
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

// TestCorruptJobIDUnpairedSurrogateRefused: a saved id token with an unpaired
// high or low \uXXXX surrogate is data corruption; a correctly paired
// surrogate elsewhere in the directory does not mask it.
func TestCorruptJobIDUnpairedSurrogateRefused(t *testing.T) {
	bad := map[string]string{
		"lone high surrogate":            `"任务\uD800"`,
		"lone low surrogate":             `"\uDC00"`,
		"high surrogate at end":          `"end\uD83D"`,
		"two high surrogates":            `"\uD800\uD800"`,
		"high then non-surrogate escape": `"\uD800A"`,
		"high then simple escape":        `"\uD800\n"`,
		"low then high reversed":         `"\uDC00\uD800"`,
		"low after valid pair":           `"😀\uDE00"`,
		"high after valid pair":          `"😀\uD83D"`,
		"lowercase hex high":             `"a\ud800"`,
	}
	for label, idJSON := range bad {
		t.Run(label, func(t *testing.T) {
			raw := rawJobEnvelope(idJSON)
			dir := plantRawEnvelope(t, raw)

			s, err := Open(dir)
			if err == nil {
				s.Close()
				t.Fatalf("directory with an unpaired-surrogate job id was accepted")
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), "job record #2") ||
				!strings.Contains(err.Error(), `"id"`) {
				t.Fatalf("error does not name the job record and id field: %v", err)
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

// TestCorruptJobIDUnderCaseOrEscapedKeyRefused: the id identity rule applies
// to any spelling the ordinary decode fills ID from — an ASCII case variant
// or a JSON-escaped key — not only the canonical "id". The damaged record is
// located by number and the field is named.
func TestCorruptJobIDUnderCaseOrEscapedKeyRefused(t *testing.T) {
	cases := []struct {
		name    string
		keyJSON string
		idJSON  string
	}{
		{"upper case id, invalid bytes", `"ID"`, "\"bad\xff\""},
		{"mixed case id, lone surrogate", `"Id"`, `"\uDC00"`},
		{"escaped uppercase key", `"\u0049D"`, "\"bad\x80x\""},
		{"escaped lowercase key", `"\u0069d"`, `"end\uD800"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := rawJobEnvelopeKeyed(tc.keyJSON, tc.idJSON)
			dir := plantRawEnvelope(t, raw)

			s, err := Open(dir)
			if err == nil {
				s.Close()
				t.Fatalf("directory with a corrupt id under a case/escaped key was accepted")
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), "job record #2") ||
				!strings.Contains(err.Error(), `"id"`) {
				t.Fatalf("error does not name the job record and id field: %v", err)
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

// TestLegalJobIDEncodingsReadBackExactly: a surrogate pair decodes to its
// astral character, an escaped backslash keeps the literal six characters
// \uD800, and a committed "�" (direct or escaped) is an ordinary id; each is
// found by its exact decoded value.
func TestLegalJobIDEncodingsReadBackExactly(t *testing.T) {
	cases := map[string]struct {
		idJSON string
		want   string
	}{
		"surrogate pair decodes to emoji":    {`"job\uD83D\uDE00"`, "job😀"},
		"escaped backslash keeps literal":    {`"\\uD800"`, `\uD800`},
		"committed replacement char direct":  {"\"has�mark\"", "has�mark"},
		"committed replacement char escaped": {`"has\uFFFDmark"`, "has�mark"},
		"escaped Chinese":                    {`"\u4efb\u52a1"`, "任务"},
		"emoji pair alone":                   {`"\uD83D\uDE00"`, "😀"},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			raw := rawJobEnvelope(tc.idJSON)
			dir := plantRawEnvelope(t, raw)

			s, err := Open(dir)
			if err != nil {
				t.Fatalf("legal id refused: %v", err)
			}
			defer s.Close()
			got, err := s.GetJob(tc.want)
			if err != nil {
				t.Fatalf("id %q did not read back: %v", tc.want, err)
			}
			if got.ID != tc.want {
				t.Fatalf("id changed on read: want %q, got %q", tc.want, got.ID)
			}
			// The legal sibling job still lists; exact match, no folding.
			if _, err := s.GetJob("ok"); err != nil {
				t.Fatalf("sibling job lost: %v", err)
			}
		})
	}
}

// A directory whose committed job id is corrupt refuses every operation —
// reads return nothing partial and mutations never commit — while the file
// stays byte-for-byte in place.
func TestCorruptJobIDDirectoryFullyRefused(t *testing.T) {
	raw := rawJobEnvelope(`"任务\uD800"`)
	dir := plantRawEnvelope(t, raw)

	if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("open: want ErrDataCorrupt, got %v", err)
	}
	left, err := os.ReadFile(filepath.Join(dir, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(raw) {
		t.Fatalf("data.json changed across refused open\nwant: %q\n got: %q", raw, left)
	}
}

// Damage appearing after the directory is open is caught by the very next
// operation on a seeded store: reads return no partial job list, a new submit
// is not committed and an unrelated circuit change cannot rewrite the file.
func TestOpenStoreRefusesAfterJobIDCorruption(t *testing.T) {
	dir, h := seedBoundJobStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Rewrite the bound job's id token to carry invalid bytes.
	valid, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	marker := `"id": "j-bound"`
	if !strings.Contains(string(valid), marker) {
		t.Fatalf("committed file does not carry %q", marker)
	}
	bad := []byte(strings.Replace(string(valid), marker, `"id": "bad\xff"`, 1))
	writeDataFile(t, dir, bad)

	if _, err := s.GetJob("j-bound"); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetJob after corruption: want ErrDataCorrupt, got %v", err)
	}
	if jobs, err := s.ListJobs(); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("ListJobs after corruption: want ErrDataCorrupt, got %d jobs err=%v", len(jobs), err)
	}
	if _, err := s.SubmitJob(boundProveJob("j-new", "c", 1, h)); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("SubmitJob after corruption: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "other", Version: 1, Constraints: 1}); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("unrelated CreateCircuit after corruption: want ErrDataCorrupt, got %v", err)
	}
	got, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(bad) {
		t.Fatalf("damaged file was rewritten by a refused operation")
	}
}
