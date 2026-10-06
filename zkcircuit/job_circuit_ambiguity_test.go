package zkcircuit

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// Regression tests for the owning circuit on a committed job record. The
// "circuit" a job writes is the sole authority for which circuit — and
// thereby which frozen version, trusted setup and compiled artifact — the
// job belongs to. Any spelling the ordinary read would fill Circuit from
// names that one field: the canonical "circuit", an ASCII letter-case
// variant such as "CIRCUIT" or "CiRcUiT", or a JSON-escaped spelling of
// either. Two such spellings on one record make the owning circuit a
// function of key order (encoding/json keeps the last value) and are refused
// as data corruption — equal or differing values, both circuits legal, and
// even with unrelated fields interleaved. A single recognized spelling keeps
// reading as it does today, and the value keeps being matched against
// circuit names exactly as written (no case folding, no trimming).

// seedTwoCircuitJobStore commits two frozen, set-up circuits alpha@1 and
// beta@1 (definitions/compile are unnecessary for an unbound job) and one
// unbound job owned by alpha. It reproduces the ambiguity that matters: a
// job spelling its circuit both as alpha and as beta has a valid owner under
// either value.
func seedTwoCircuitJobStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedFrozenSource(t, s, "alpha", 1, 1, 1, 1, "")
	seedFrozenSource(t, s, "beta", 1, 1, 1, 1, "")
	if _, err := s.SubmitJob(boundProveJob("jca", "alpha", 1, "")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// circuitLine is the single-line form of the job's own circuit member, as
// written by the deterministic encoder.
const circuitLine = `"circuit": "alpha"`

// assertJobCircuitCorrupt opens dir, requires ErrDataCorrupt naming id and
// the circuit field, and checks the file is still byte-for-byte want.
func assertJobCircuitCorrupt(t *testing.T, dir, id string, want []byte) {
	t.Helper()
	_, err := Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	if !strings.Contains(err.Error(), strconv.Quote(id)) {
		t.Fatalf("error does not name the ambiguous job %q: %v", id, err)
	}
	if !strings.Contains(err.Error(), "circuit") {
		t.Fatalf("error does not name the circuit field: %v", err)
	}
	got, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(want) {
		t.Fatalf("open modified the damaged file")
	}
}

// A job carrying two recognized circuit spellings is refused: canonical plus
// an ASCII case variant or a JSON-escaped spelling, equal or differing
// values, both circuits legal, in either key order. The error names the job
// and the circuit field, and the file is left byte-for-byte in place.
func TestJobCircuitDuplicateIsCorrupt(t *testing.T) {
	escC := jsonBackslash + "u0063ircuit" // "circuit" as raw JSON text
	escI := "cir" + jsonBackslash + "u0063uit"
	cases := []struct {
		name string
		frag string // replaces the single "circuit": "alpha" member
	}{
		{"identical values, ASCII variant", `"circuit": "alpha", "CIRCUIT": "alpha"`},
		{"differing values, ASCII variant", `"circuit": "alpha", "CIRCUIT": "beta"`},
		{"upper then canonical, differing", `"CIRCUIT": "beta", "circuit": "alpha"`},
		{"mixed case variant, same value", `"circuit": "alpha", "CiRcUiT": "alpha"`},
		{"escaped leading letter, same value", `"circuit": "alpha", "` + escC + `": "alpha"`},
		{"escaped inner letter, differing", `"circuit": "beta", "` + escI + `": "alpha"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedTwoCircuitJobStore(t)
			bad := replaceInJob(t, dir, "jca", circuitLine, tc.frag)

			assertJobCircuitCorrupt(t, dir, "jca", bad)
			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("reopen on ambiguous dir: want corruption, got %v", err)
			}
		})
	}
}

// The motivating ambiguity: an unbound job whose circuit is spelled both
// alpha and beta, with both circuits frozen and set up at the pinned
// version. Without the uniqueness rule encoding/json keeps the last value
// and the job reads as belonging to the other circuit; the read must instead
// be refused.
func TestJobCircuitAmbiguousBetweenTwoCircuits(t *testing.T) {
	dir := seedTwoCircuitJobStore(t)
	bad := replaceInJob(t, dir, "jca", circuitLine, `"circuit": "alpha", "CIRCUIT": "beta"`)
	assertJobCircuitCorrupt(t, dir, "jca", bad)
}

// The two circuit spellings need not be adjacent: a second spelling later in
// the record, separated by other fields, is still a duplicate and is
// rejected.
func TestJobCircuitDuplicateInterspersedIsCorrupt(t *testing.T) {
	dir := seedTwoCircuitJobStore(t)
	old := `"circuit": "alpha",` + jobFieldIndent + `"version": 1,` + jobFieldIndent + `"kind": "prove"`
	repl := `"circuit": "alpha",` + jobFieldIndent + `"version": 1,` + jobFieldIndent +
		`"kind": "prove",` + jobFieldIndent + `"CIRCUIT": "beta"`
	bad := replaceInJob(t, dir, "jca", old, repl)
	assertJobCircuitCorrupt(t, dir, "jca", bad)
}

// A single recognized circuit spelling keeps the current reading, including
// its ASCII case compatibility: the job loads against the circuit named,
// direct or escaped, and keeps its version and attempt count.
func TestJobCircuitSingleSpellingStillReads(t *testing.T) {
	escC := jsonBackslash + "u0063ircuit"
	cases := []struct {
		name string
		frag string
	}{
		{"canonical", `"circuit": "alpha"`},
		{"upper case", `"CIRCUIT": "alpha"`},
		{"mixed case", `"CiRcUiT": "alpha"`},
		{"escaped letter", `"` + escC + `": "alpha"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedTwoCircuitJobStore(t)
			replaceInJob(t, dir, "jca", circuitLine, tc.frag)

			s, err := Open(dir)
			if err != nil {
				t.Fatalf("single spelling %q must load: %v", tc.name, err)
			}
			defer s.Close()
			got, err := s.GetJob("jca")
			if err != nil {
				t.Fatal(err)
			}
			if got.Circuit != "alpha" || got.Version != 1 || got.Attempt != 1 {
				t.Fatalf("single spelling %q read %+v, want circuit alpha v1 attempt 1", tc.name, got)
			}
		})
	}
}

// The circuit value keeps being matched exactly as written: a single
// spelling whose value differs only by letter case or surrounding spaces
// names no committed circuit and fails the integrity validation, exactly as
// before — "alpha" and "ALPHA" are never merged and no whitespace is
// trimmed.
func TestJobCircuitValueMatchingStillExact(t *testing.T) {
	cases := []struct {
		name string
		frag string
	}{
		{"case differs", `"circuit": "ALPHA"`},
		{"trailing space", `"circuit": "alpha "`},
		{"leading space", `"circuit": " alpha"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedTwoCircuitJobStore(t)
			replaceInJob(t, dir, "jca", circuitLine, tc.frag)
			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("%s: want ErrDataCorrupt, got %v", tc.name, err)
			}
		})
	}
}

// Damage appearing after the directory is open is caught by the very next
// operation, whether a read or a write: reads return no partial job list,
// mutations are not committed, and the file is never rewritten.
func TestOpenStoreRefusesAfterJobCircuitAmbiguity(t *testing.T) {
	dir := seedTwoCircuitJobStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bad := replaceInJob(t, dir, "jca", circuitLine, `"circuit": "alpha", "CIRCUIT": "beta"`)

	if _, err := s.GetJob("jca"); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetJob after ambiguity: want ErrDataCorrupt, got %v", err)
	}
	if jobs, err := s.ListJobs(); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("ListJobs after ambiguity: want ErrDataCorrupt, got %d jobs err=%v", len(jobs), err)
	}
	if _, err := s.SubmitJob(boundProveJob("j-new", "alpha", 1, "")); !errors.Is(err, ErrDataCorrupt) {
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
