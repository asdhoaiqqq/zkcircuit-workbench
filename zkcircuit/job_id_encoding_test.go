package zkcircuit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the job-id identity rule, the job counterpart of the
// circuit-name rule in name_encoding_test.go: an id is a job's identity, so it
// must keep one exact value across submit, save, read and lookup.
//
// On the write path an id that is not complete, valid UTF-8 (a lone
// continuation byte, a truncated multi-byte character, an encoded surrogate
// half) is refused as ErrInvalidArgument before anything is committed —
// encoding/json would otherwise silently rewrite those bytes to U+FFFD at
// save time, a later GetJob by the submitted bytes would miss the record, and
// it could share an identity with an id that genuinely contains "�".
//
// On the read path a committed job record whose raw id token carries invalid
// UTF-8 bytes, or \uXXXX escapes forming an unpaired surrogate — under the
// canonical "id" or an ASCII case/JSON-escaped spelling — is data corruption:
// the whole directory read fails with ErrDataCorrupt naming the job record
// (by position when the id cannot be recovered) and the id field, no partial
// results are returned, and the file stays byte-for-byte in place. Legal
// Chinese, emoji, an actually committed "�", a legal surrogate pair and the
// literal six characters \uD800 are all ordinary ids and keep their exact
// value.

// rawJobEnvelope builds a complete committed envelope around one legal job
// plus one job record whose id is the given raw JSON string token (already
// quoted/escaped by the caller). The frozen circuit c v1 has a trusted setup
// so the records are semantically legal apart from the damaged id.
func rawJobEnvelope(idJSON string) []byte {
	return []byte(`{"format":1,"circuits":[` +
		`{"name":"c","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
		`],"setups":[{"name":"c","version":1}],"jobs":[` +
		`{"id":"j1","circuit":"c","version":1,"kind":"prove","attempt":1},` +
		`{"id":` + idJSON + `,"circuit":"c","version":1,"kind":"prove","attempt":1}` +
		`]}`)
}

// SubmitJob refuses every shape of invalid UTF-8 in the id with
// ErrInvalidArgument, writes nothing, and leaves an already registered job
// readable under its own id.
func TestSubmitJobRejectsInvalidUTF8ID(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := compiledVersion(t, s, "c", 1, validDef)
	if _, err := s.SubmitJob(boundProveJob("j1", "c", 1, h)); err != nil {
		t.Fatal(err)
	}
	dataPath := filepath.Join(dir, dirDataFile)
	before, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}

	bad := map[string]string{
		"lone continuation byte":   "bad\x80id",
		"truncated multi-byte":     "trunc\xe4\xb8",
		"overlong encoding":        "over\xc0\xaf",
		"invalid byte 0xff":        "bad\xff",
		"encoded surrogate half":   "pair\xed\xa0\x80",
		"truncated at end":         "end\xc3",
		"continuation then ASCII":  "bad\x80x",
		"valid prefix then damage": "作业\xff",
	}
	for label, id := range bad {
		t.Run(label, func(t *testing.T) {
			_, err := s.SubmitJob(boundProveJob(id, "c", 1, h))
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
	// Neither the submitted bytes nor their U+FFFD-repaired form may exist as
	// a job; the existing job stays readable under its own id.
	if got, err := s.GetJob("j1"); err != nil || got.ID != "j1" {
		t.Fatalf("existing job damaged by refused submissions: %+v %v", got, err)
	}
	jobs, err := s.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != "j1" {
		t.Fatalf("refused submissions left jobs behind: %+v", jobs)
	}
}

// Legal Unicode ids submit, save, reopen and read back under their exact
// submitted value, and idempotent resubmit / conflicting resubmit keep their
// existing behavior on those ids.
func TestSubmitJobValidUnicodeIDsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := compiledVersion(t, s, "c", 1, validDef)
	ids := []string{
		"作业-1",          // Chinese
		"emoji 😀 job",   // emoji
		"has�mark",      // an actually committed replacement character
		`literal\uD800`, // the literal six characters \uD800 — no escape
		"mixed 作业😀�mix",
	}
	for _, id := range ids {
		if _, err := s.SubmitJob(boundProveJob(id, "c", 1, h)); err != nil {
			t.Fatalf("submit %q: %v", id, err)
		}
	}
	// Same id + same request is idempotent; same id + different request
	// conflicts — identity stays byte-exact for Unicode ids.
	if _, err := s.SubmitJob(boundProveJob(ids[0], "c", 1, h)); err != nil {
		t.Fatalf("idempotent resubmit %q: %v", ids[0], err)
	}
	conflicting := boundProveJob(ids[0], "c", 1, "")
	if _, err := s.SubmitJob(conflicting); !errors.Is(err, ErrConflict) {
		t.Fatalf("different request under same id: want ErrConflict, got %v", err)
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
	listed, err := s2.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != len(ids) {
		t.Fatalf("want %d jobs, got %+v", len(ids), listed)
	}
}

// A committed id with invalid UTF-8 bytes is data corruption even when legal
// jobs share the directory: the read fails outright, names the job record
// (positionally — the id cannot be recovered) and the id field, and leaves
// the file byte-for-byte in place.
func TestCorruptJobIDInvalidUTF8Refused(t *testing.T) {
	bad := map[string]string{
		"lone continuation byte":   "\"bad\x80id\"",
		"invalid byte 0xff":        "\"bad\xff\"",
		"truncated multi-byte":     "\"trunc\xe4\xb8\"",
		"overlong encoding":        "\"over\xc0\xaf\"",
		"encoded surrogate half":   "\"pair\xed\xa0\x80\"",
		"valid prefix then damage": "\"作业\xff\"",
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
				t.Fatalf("error does not name the job record and the id field: %v", err)
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

// A committed id whose JSON escapes form an unpaired surrogate is data
// corruption under the same rules as a circuit name.
func TestCorruptJobIDUnpairedSurrogateRefused(t *testing.T) {
	bad := map[string]string{
		"lone high surrogate":            `"作业\uD800"`,
		"lone low surrogate":             `"\uDC00"`,
		"high surrogate at end":          `"end\uD83D"`,
		"two high surrogates":            `"\uD800\uD800"`,
		"high then non-surrogate escape": `"\uD800A"`,
		"high then simple escape":        `"\uD800\n"`,
		"low then high (reversed pair)":  `"\uDC00\uD800"`,
		"low surrogate after valid pair": `"😀\uDE00"`,
		"escaped lowercase":              `"a\ud800"`,
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
				t.Fatalf("error does not name the job record and the id field: %v", err)
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

// The id encoding rule applies to every spelling the ordinary decode fills ID
// from — an ASCII case variant ("ID") and a JSON-\u-escaped spelling of the
// key — and a damaged id that is the only job in the file is located by its
// record position. Byte-bearing values are built by concatenation so the JSON
// text keeps the \uXXXX key escapes while the value carries raw bad bytes.
func TestCorruptJobIDSpellingVariantsRefused(t *testing.T) {
	jobTail := `,"circuit":"c","version":1,"kind":"prove","attempt":1}`
	cases := []struct {
		label  string
		record string
	}{
		{"case variant ID with invalid bytes", `{"ID":"bad` + "\xff" + `"` + jobTail},
		{"case variant ID with lone surrogate", `{"ID":"bad\uD800"` + jobTail},
		{`\u-escaped "id" key with invalid bytes`, "{\"\\u0069\\u0064\":\"bad" + "\xff" + "\"" + jobTail},
		{`\u-escaped "ID" key with lone surrogate`, "{\"\\u0049\\u0044\":\"bad\\uD800\"" + jobTail},
		{"only job with invalid bytes", `{"id":"only` + "\xff" + `"` + jobTail},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			raw := []byte(`{"format":1,"circuits":[` +
				`{"name":"c","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
				`],"setups":[{"name":"c","version":1}],"jobs":[` + tc.record + `]}`)
			dir := plantRawEnvelope(t, raw)

			_, err := Open(dir)
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), "job record #1") ||
				!strings.Contains(err.Error(), `"id"`) {
				t.Fatalf("error does not name the positional record and the id field: %v", err)
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

// Legal id encodings read back under their exact value: a surrogate pair
// decodes to its astral character, a committed "�" (direct or U+FFFD-escaped)
// is an ordinary id, and the literal text \uD800 carries no escape at all.
func TestLegalJobIDEncodingsReadBackExactly(t *testing.T) {
	cases := map[string]struct {
		idJSON string
		want   string
	}{
		"surrogate pair decodes to its character": {`"job-😀"`, "job-😀"},
		"escaped Chinese":                         {`"job-作业"`, "job-作业"},
		"escaped backslash keeps literal text":    {`"x\\uD800"`, `x\uD800`},
		"committed replacement char direct":       {"\"has�mark\"", "has�mark"},
		"committed replacement char escaped":      {`"has�mark"`, "has�mark"},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			raw := []byte(`{"format":1,"circuits":[` +
				`{"name":"c","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
				`],"setups":[{"name":"c","version":1}],"jobs":[` +
				`{"id":` + tc.idJSON + `,"circuit":"c","version":1,"kind":"prove","attempt":1}]}`)
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
		})
	}
}

// Once a committed id is corrupt, every operation refuses at open: reads
// return nothing partial, submissions and unrelated mutations commit
// nothing, and the file stays byte-for-byte in place.
func TestCorruptJobIDDirectoryFullyRefused(t *testing.T) {
	raw := rawJobEnvelope(`"作业\uD800"`)
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
