package zkcircuit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the owning-circuit exact-readback rule on a committed job
// record, the circuit-field counterpart of the job-id rule in
// job_id_encoding_test.go: the "circuit" a job writes is the sole authority
// for which circuit — and thereby which frozen version, trusted setup and
// compiled artifact — the job belongs to, so it must read back as the exact
// value the bytes on disk carry.
//
// A committed job record whose raw circuit token carries invalid UTF-8
// bytes, or \uXXXX escapes forming an unpaired surrogate — under the
// canonical "circuit", an ASCII case variant, or a JSON-escaped spelling of
// either — is data corruption: the whole directory read fails with
// ErrDataCorrupt naming the job (by its id, which is not the damaged field)
// and the circuit field, no partial results are returned, no job is dropped
// or renamed, and the file stays byte-for-byte in place. The refusal holds
// even when the U+FFFD-repaired name exactly matches a circuit that
// genuinely exists — frozen, set up, even compiled — because the job's
// owner must come from what was saved, never from what the damage happens
// to decode to. Legal Chinese, emoji, an actually committed "�", a legal
// surrogate pair and the literal six characters \uD800 are all ordinary
// circuit names and keep their exact value.

// rawJobCircuitEnvelope builds a complete committed envelope around one
// frozen, set-up circuit per name in circuits plus one legal job j0 owned
// by the first circuit, and one job j1 whose circuit member is the given
// raw JSON token (already quoted/escaped by the caller). The records are
// semantically legal apart from the damaged circuit value.
func rawJobCircuitEnvelope(circuits []string, circuitJSON string) []byte {
	var b strings.Builder
	b.WriteString(`{"format":1,"circuits":[`)
	for i, name := range circuits {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"name":`)
		b.WriteString(strconvQuote(name))
		b.WriteString(`,"version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}`)
	}
	b.WriteString(`],"setups":[`)
	for i, name := range circuits {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"name":`)
		b.WriteString(strconvQuote(name))
		b.WriteString(`,"version":1}`)
	}
	b.WriteString(`],"jobs":[`)
	b.WriteString(`{"id":"j0","circuit":`)
	b.WriteString(strconvQuote(circuits[0]))
	b.WriteString(`,"version":1,"kind":"prove","attempt":1},`)
	b.WriteString(`{"id":"j1","circuit":`)
	b.WriteString(circuitJSON)
	b.WriteString(`,"version":1,"kind":"prove","attempt":1}]}`)
	return []byte(b.String())
}

// strconvQuote quotes a circuit name as a JSON string without importing
// strconv under an alias; names in these tests carry no characters JSON
// escaping would alter beyond the standard set.
func strconvQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// assertJobCircuitEncodingCorrupt opens dir, requires ErrDataCorrupt naming
// the job j1 and the circuit field (never a "circuit not found" or "setup
// missing" misdiagnosis), and checks the file is still byte-for-byte want.
func assertJobCircuitEncodingCorrupt(t *testing.T, dir string, want []byte) {
	t.Helper()
	s, err := Open(dir)
	if err == nil {
		s.Close()
		t.Fatalf("directory with a damaged job circuit was accepted")
	}
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	if !strings.Contains(err.Error(), `"j1"`) || !strings.Contains(err.Error(), `"circuit"`) {
		t.Fatalf("error does not name the damaged job and the circuit field: %v", err)
	}
	if strings.Contains(err.Error(), "unknown circuit") || strings.Contains(err.Error(), "setup") {
		t.Fatalf("encoding damage misreported as a missing circuit or setup: %v", err)
	}
	left, rerr := os.ReadFile(filepath.Join(dir, dirDataFile))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(left) != string(want) {
		t.Fatalf("refused read modified data.json\nwant: %q\n got: %q", want, left)
	}
}

// A committed circuit value with invalid UTF-8 bytes is data corruption even
// when every other record is legal: the read fails outright, names the job
// and the circuit field, and leaves the file byte-for-byte in place.
func TestCorruptJobCircuitInvalidUTF8Refused(t *testing.T) {
	bad := map[string]string{
		"lone continuation byte":   "\"alp\x80ha\"",
		"invalid byte 0xff":        "\"alp\xffha\"",
		"truncated multi-byte":     "\"al\xe4\xb8\"",
		"overlong encoding":        "\"al\xc0\xafpha\"",
		"encoded surrogate half":   "\"al\xed\xa0\x80pha\"",
		"valid prefix then damage": "\"电路\xff\"",
	}
	for label, circuitJSON := range bad {
		t.Run(label, func(t *testing.T) {
			raw := rawJobCircuitEnvelope([]string{"alpha"}, circuitJSON)
			dir := plantRawEnvelope(t, raw)
			assertJobCircuitEncodingCorrupt(t, dir, raw)
		})
	}
}

// A committed circuit value whose JSON escapes form an unpaired surrogate is
// data corruption under the same rule as the job id and the circuit name.
func TestCorruptJobCircuitUnpairedSurrogateRefused(t *testing.T) {
	bad := map[string]string{
		"lone high surrogate":            `"电路\uD800"`,
		"lone low surrogate":             `"\uDC00lpha"`,
		"high surrogate at end":          `"alpha\uD83D"`,
		"two high surrogates":            `"\uD800\uD800"`,
		"high then non-surrogate escape": `"\uD800A"`,
		"high then simple escape":        `"alp\uD800\n"`,
		"low then high (reversed pair)":  `"\uDC00\uD800"`,
		"low surrogate after valid pair": `"😀\uDE00"`,
		"escaped lowercase":              `"al\ud800pha"`,
	}
	for label, circuitJSON := range bad {
		t.Run(label, func(t *testing.T) {
			raw := rawJobCircuitEnvelope([]string{"alpha"}, circuitJSON)
			dir := plantRawEnvelope(t, raw)
			assertJobCircuitEncodingCorrupt(t, dir, raw)
		})
	}
}

// The motivating ambiguity: a circuit genuinely named "电路�" exists —
// frozen and with a trusted setup — and a job's circuit token decodes onto
// that very name only because encoding/json repairs the damage to U+FFFD.
// The job must not borrow that circuit's version and setup: the read is
// refused as corruption, not accepted and not misreported as a missing
// circuit.
func TestCorruptJobCircuitReplacementNameExistsRefused(t *testing.T) {
	cases := map[string]string{
		"invalid UTF-8 byte repairs to U+FFFD":  "\"电路\xff\"",
		"unpaired high surrogate decodes to it": `"电路\uD800"`,
		"unpaired low surrogate decodes to it":  `"电路\uDFFF"`,
	}
	for label, circuitJSON := range cases {
		t.Run(label, func(t *testing.T) {
			raw := rawJobCircuitEnvelope([]string{"电路�"}, circuitJSON)
			dir := plantRawEnvelope(t, raw)
			assertJobCircuitEncodingCorrupt(t, dir, raw)
		})
	}
}

// The encoding rule applies to every spelling the ordinary decode fills
// Circuit from — an ASCII case variant and a JSON-\u-escaped spelling of the
// key — exactly as for the canonical "circuit".
func TestCorruptJobCircuitSpellingVariantsRefused(t *testing.T) {
	jobTail := `,"version":1,"kind":"prove","attempt":1}`
	cases := []struct {
		label  string
		record string
	}{
		{"case variant CIRCUIT with invalid bytes", `{"id":"j1","CIRCUIT":"alp` + "\xff" + `ha"` + jobTail},
		{"mixed case CiRcUiT with lone surrogate", `{"id":"j1","CiRcUiT":"alpha\uD800"` + jobTail},
		{`\u-escaped "circuit" key with invalid bytes`, `{"id":"j1","\u0063ircuit":"alp` + "\xff" + `ha"` + jobTail},
		{`\u-escaped "CIRCUIT" key with lone surrogate`, `{"id":"j1","\u0043IRCUIT":"alpha\uD800"` + jobTail},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			raw := []byte(`{"format":1,"circuits":[` +
				`{"name":"alpha","version":1,"constraints":1,"public_inputs":1,"private_inputs":1,"frozen":true,"description":""}` +
				`],"setups":[{"name":"alpha","version":1}],"jobs":[` + tc.record + `]}`)
			dir := plantRawEnvelope(t, raw)
			assertJobCircuitEncodingCorrupt(t, dir, raw)
		})
	}
}

// Legal circuit names read back under their exact value: Chinese, emoji, a
// surrogate pair decoding to its astral character, a committed "�" (direct
// or U+FFFD-escaped) and the literal text \uD800 all stay ordinary owners.
func TestLegalJobCircuitEncodingsReadBackExactly(t *testing.T) {
	cases := map[string]struct {
		circuitJSON string
		want        string
	}{
		"Chinese direct": {`"电路"`, "电路"},
		"emoji direct":   {`"emoji-😀"`, "emoji-😀"},
		"surrogate pair decodes to its character": {`"pair-\ud83d\ude00"`, "pair-😀"},
		"committed replacement char direct":       {"\"has�mark\"", "has�mark"},
		"committed replacement char escaped":      {`"has\ufffdmark"`, "has�mark"},
		"escaped backslash keeps literal text":    {`"x\\uD800"`, `x\uD800`},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			raw := rawJobCircuitEnvelope([]string{tc.want}, tc.circuitJSON)
			dir := plantRawEnvelope(t, raw)

			s, err := Open(dir)
			if err != nil {
				t.Fatalf("legal circuit name refused: %v", err)
			}
			defer s.Close()
			got, err := s.GetJob("j1")
			if err != nil {
				t.Fatalf("job owned by %q did not read back: %v", tc.want, err)
			}
			if got.Circuit != tc.want {
				t.Fatalf("owning circuit changed on read: want %q, got %q", tc.want, got.Circuit)
			}
		})
	}
}

// One damaged circuit value poisons the whole directory: no partial results
// are returned for the legal records, the damaged job is neither dropped nor
// renamed, mutations commit nothing, and the file stays byte-for-byte in
// place — including when the damage appears after the directory was opened.
func TestCorruptJobCircuitDirectoryFullyRefused(t *testing.T) {
	// Seed a healthy directory through the public API: circuit alpha frozen
	// with a setup, jobs j1 and j2 owned by it.
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedFrozenSource(t, s, "alpha", 1, 1, 1, 1, "")
	if _, err := s.RecordSetup("alpha", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitJob(boundProveJob("j1", "alpha", 1, "")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitJob(boundProveJob("j2", "alpha", 1, "")); err != nil {
		t.Fatal(err)
	}

	// Damage j2's committed circuit value on disk while the store is open:
	// the next read or write must refuse, and nothing may be rewritten.
	dataPath := filepath.Join(dir, dirDataFile)
	healthy, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	marker := `"id": "j2"`
	at := strings.Index(string(healthy), marker)
	if at < 0 {
		t.Fatalf("committed file does not carry the j2 marker %q", marker)
	}
	rel := strings.Index(string(healthy[at:]), `"circuit": "alpha"`)
	if rel < 0 {
		t.Fatalf("j2 job does not carry its circuit member after the id marker")
	}
	pos := at + rel
	bad := []byte(string(healthy[:pos]) + `"circuit": "alp` + "\xff" + `ha"` + string(healthy[pos+len(`"circuit": "alpha"`):]))
	if err := os.WriteFile(dataPath, bad, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetJob("j1"); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetJob on the legal job after corruption: want ErrDataCorrupt, got %v", err)
	}
	if jobs, err := s.ListJobs(); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("ListJobs after corruption: want ErrDataCorrupt, got %d jobs err=%v", len(jobs), err)
	}
	if _, err := s.SubmitJob(boundProveJob("j3", "alpha", 1, "")); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("SubmitJob after corruption: want ErrDataCorrupt, got %v", err)
	}
	left, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(bad) {
		t.Fatalf("refused operations modified data.json")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// A fresh open of the damaged directory refuses the same way, still
	// without touching the file.
	if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("reopen of damaged directory: want ErrDataCorrupt, got %v", err)
	}
	left, err = os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(bad) {
		t.Fatalf("refused open modified data.json")
	}
}
