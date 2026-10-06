package zkcircuit

import (
	"errors"
	"strings"
	"testing"
)

// Regression tests for the identity id on a committed job record. The "id" a
// job writes is the sole authority for the job's identity — the key every
// query, resubmit-conflict check and list ordering resolves the record by.
// Any spelling the ordinary read would fill ID from names that one field:
// the canonical "id", an ASCII letter-case variant ("ID", "Id", "iD"), or a
// JSON-escaped spelling of either. Two such spellings on one record make the
// identity a function of key order (encoding/json keeps the last value) and
// are refused as data corruption — equal or differing values, both ids legal,
// even with unrelated fields interleaved, and even when no canonical "id" is
// among them. Because the record's identity is itself in doubt, the error
// locates the record by its 1-based position in the jobs array and never
// quotes a candidate id as the established identity. A single recognized
// spelling keeps reading as it does today, with the value matched exactly as
// written.

// seedJobIDStore commits one frozen, set-up circuit c@1 (a definition is
// unnecessary for an unbound job) and one unbound job jia. It reproduces the
// ambiguity that matters: a record spelling its id both as jia and as jib
// has a legible, legal identity under either value.
func seedJobIDStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedFrozenSource(t, s, "c", 1, 1, 1, 1, "")
	if _, err := s.SubmitJob(boundProveJob("jia", "c", 1, "")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// idLine is the single-line form of the job's own id member, as written by
// the deterministic encoder.
const idLine = `"id": "jia"`

// assertJobIDCorrupt opens dir, requires ErrDataCorrupt locating the record
// by its 1-based position pos and naming the id field — never quoting either
// candidate id as the job's identity — and checks the file is still
// byte-for-byte want.
func assertJobIDCorrupt(t *testing.T, dir, pos string, want []byte) {
	t.Helper()
	_, err := Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	if !strings.Contains(err.Error(), "job record #"+pos) {
		t.Fatalf("error does not locate the record at position #%s: %v", pos, err)
	}
	if !strings.Contains(err.Error(), `"id"`) {
		t.Fatalf("error does not name the id field: %v", err)
	}
	if strings.Contains(err.Error(), "jia") || strings.Contains(err.Error(), "jib") {
		t.Fatalf("error quotes a candidate id as the job's identity: %v", err)
	}
	got, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(want) {
		t.Fatalf("open modified the damaged file")
	}
}

// A job carrying two recognized id spellings is refused: canonical plus an
// ASCII case variant or a JSON-escaped spelling, equal or differing values,
// both ids legal, in either key order, and case variants with no canonical
// "id" among them. The error locates the record positionally and names the
// id field, and the file is left byte-for-byte in place.
func TestJobIDDuplicateIsCorrupt(t *testing.T) {
	escID := jsonBackslash + `u0049` + jsonBackslash + `u0044` // "ID" as raw JSON text
	escId := jsonBackslash + `u0069` + jsonBackslash + `u0064` // "id" as raw JSON text
	cases := []struct {
		name string
		frag string // replaces the single "id": "jia" member
	}{
		{"identical values, ASCII variant", `"id": "jia", "ID": "jia"`},
		{"differing values, ASCII variant", `"id": "jia", "ID": "jib"`},
		{"upper then canonical, differing", `"ID": "jib", "id": "jia"`},
		{"mixed case variant Id, same value", `"id": "jia", "Id": "jia"`},
		{"mixed case variant iD, differing", `"id": "jia", "iD": "jib"`},
		{"case variants only, no canonical", `"ID": "jia", "Id": "jia"`},
		{"case variants only, differing", `"Id": "jia", "iD": "jib"`},
		{"escaped upper, same value", `"id": "jia", "` + escID + `": "jia"`},
		{"escaped canonical plus upper, differing", `"` + escId + `": "jia", "ID": "jib"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedJobIDStore(t)
			bad := replaceInJob(t, dir, "jia", idLine, tc.frag)

			assertJobIDCorrupt(t, dir, "1", bad)
			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("reopen on ambiguous dir: want corruption, got %v", err)
			}
		})
	}
}

// The two id spellings need not be adjacent: a second spelling later in the
// record, separated by the circuit, version, kind and attempt members, is
// still a duplicate identity and is rejected.
func TestJobIDDuplicateInterspersedIsCorrupt(t *testing.T) {
	dir := seedJobIDStore(t)
	old := `"id": "jia",` + jobFieldIndent + `"circuit": "c",` + jobFieldIndent +
		`"version": 1,` + jobFieldIndent + `"kind": "prove",` + jobFieldIndent + `"attempt": 1`
	repl := old + `,` + jobFieldIndent + `"ID": "jib"`
	bad := replaceInJob(t, dir, "jia", old, repl)
	assertJobIDCorrupt(t, dir, "1", bad)
}

// The record is located by its 1-based position in the jobs array: with two
// jobs committed, the ambiguous second record is named "job record #2".
func TestJobIDDuplicateLocatedByPosition(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedFrozenSource(t, s, "c", 1, 1, 1, 1, "")
	if _, err := s.SubmitJob(boundProveJob("j1", "c", 1, "")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitJob(boundProveJob("j2", "c", 1, "")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	bad := replaceInJob(t, dir, "j2", `"id": "j2"`, `"id": "j2", "ID": "j9"`)

	_, err = Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	if !strings.Contains(err.Error(), "job record #2") {
		t.Fatalf("error does not locate the ambiguous record at position #2: %v", err)
	}
	if strings.Contains(err.Error(), `"j2"`) || strings.Contains(err.Error(), `"j9"`) {
		t.Fatalf("error quotes a candidate id as the job's identity: %v", err)
	}
	got, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(bad) {
		t.Fatalf("open modified the damaged file")
	}
}

// An exact repeated "id" key stays rejected as before — the pre-existing
// duplicate-field rule, not a reading that picks one of the values.
func TestJobIDExactDuplicateStillCorrupt(t *testing.T) {
	dir := seedJobIDStore(t)
	bad := replaceInJob(t, dir, "jia", idLine, `"id": "jia", "id": "jia"`)

	_, err := Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("exact duplicate id not reported as a duplicate: %v", err)
	}
	got, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(bad) {
		t.Fatalf("open modified the damaged file")
	}
}

// A single recognized id spelling keeps the current reading, including its
// ASCII case compatibility and JSON-escaped forms: the job loads under its
// one id and keeps its circuit, version and attempt count.
func TestJobIDSingleSpellingStillReads(t *testing.T) {
	escID := jsonBackslash + `u0049` + jsonBackslash + `u0044`
	escId := jsonBackslash + `u0069` + jsonBackslash + `u0064`
	cases := []struct {
		name string
		frag string
	}{
		{"canonical", `"id": "jia"`},
		{"upper case", `"ID": "jia"`},
		{"mixed case Id", `"Id": "jia"`},
		{"mixed case iD", `"iD": "jia"`},
		{"escaped upper", `"` + escID + `": "jia"`},
		{"escaped canonical", `"` + escId + `": "jia"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedJobIDStore(t)
			replaceInJob(t, dir, "jia", idLine, tc.frag)

			s, err := Open(dir)
			if err != nil {
				t.Fatalf("single spelling %q must load: %v", tc.name, err)
			}
			defer s.Close()
			got, err := s.GetJob("jia")
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != "jia" || got.Circuit != "c" || got.Version != 1 || got.Attempt != 1 {
				t.Fatalf("single spelling %q read %+v, want id jia circuit c v1 attempt 1", tc.name, got)
			}
		})
	}
}

// Damage appearing after the directory is open is caught by the very next
// operation, whether a read or a write: reads return no partial job list,
// mutations are not committed, and the file is never rewritten.
func TestOpenStoreRefusesAfterJobIDAmbiguity(t *testing.T) {
	dir := seedJobIDStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bad := replaceInJob(t, dir, "jia", idLine, `"id": "jia", "ID": "jib"`)

	if _, err := s.GetJob("jia"); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetJob after ambiguity: want ErrDataCorrupt, got %v", err)
	}
	if jobs, err := s.ListJobs(); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("ListJobs after ambiguity: want ErrDataCorrupt, got %d jobs err=%v", len(jobs), err)
	}
	if _, err := s.SubmitJob(boundProveJob("j-new", "c", 1, "")); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("SubmitJob after ambiguity: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "other", Version: 1, Constraints: 1}); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("unrelated CreateCircuit after ambiguity: want ErrDataCorrupt, got %v", err)
	}
	got, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(bad) {
		t.Fatalf("ambiguous file was rewritten by a refused operation")
	}
}
