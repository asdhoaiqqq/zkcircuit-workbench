package zkcircuit

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// Regression tests for the pinned circuit version on a committed job record.
// The "version" a job writes is the sole authority for which frozen circuit —
// and thereby which trusted setup and compiled artifact — the job belongs to.
// Any spelling the ordinary read would fill Version from names that one
// field: the canonical "version", an ASCII letter-case variant, a JSON-escaped
// spelling, or the Unicode-folded long-s "verſion" (U+017F). Two such
// spellings on one record make the version a function of key order
// (encoding/json keeps the last value) and are refused as data corruption —
// equal or differing values, both legal, and even with unrelated fields
// interleaved. A long-s spelling standing alone is still the single version
// field and keeps reading as it does today.

// jsonBackslash is one literal backslash, written as a raw string so the
// uXXXX escape sequences built below keep their backslash in this source.
const jsonBackslash = `\`

// jobFieldIndent is the indentation of a job record's own members as written
// by json.MarshalIndent with two-space indents (envelope → jobs → record).
const jobFieldIndent = "\n      "

// seedTwoVersionJobStore commits two frozen, set-up versions c@1 and c@2
// (definitions/compile are unnecessary for an unbound job) and one unbound
// job pinned to v1. It reproduces the ambiguity that matters: a job spelling
// version both as 1 and 2 has a valid target under either value.
func seedTwoVersionJobStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedFrozenSource(t, s, "c", 1, 1, 1, 1, "")
	seedFrozenSource(t, s, "c", 2, 1, 1, 1, "")
	if _, err := s.SubmitJob(boundProveJob("jmv", "c", 1, "")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// replaceInJob scopes a literal replacement to the job object carrying id:
// it finds the id marker and replaces the first occurrence of old after it,
// so a "version": 1 shared by the circuit/setup/artifact records is never
// mistaken for the job's own field.
func replaceInJob(t *testing.T, dir, id, old, repl string) []byte {
	t.Helper()
	raw, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	marker := `"id": "` + id + `"`
	at := strings.Index(string(raw), marker)
	if at < 0 {
		t.Fatalf("committed file does not carry job marker %q", marker)
	}
	rel := strings.Index(string(raw[at:]), old)
	if rel < 0 {
		t.Fatalf("job %q does not carry %q after its id marker", id, old)
	}
	pos := at + rel
	bad := string(raw[:pos]) + repl + string(raw[pos+len(old):])
	writeDataFile(t, dir, []byte(bad))
	return []byte(bad)
}

// versionLine is the single-line form of the job's own version member, as
// written by the deterministic encoder.
func versionLine(v int) string { return `"version": ` + strconv.Itoa(v) }

// assertJobVersionCorrupt opens dir, requires ErrDataCorrupt naming id and
// the version field, and checks the file is still byte-for-byte want.
func assertJobVersionCorrupt(t *testing.T, dir, id string, want []byte) {
	t.Helper()
	_, err := Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	if !strings.Contains(err.Error(), strconv.Quote(id)) {
		t.Fatalf("error does not name the ambiguous job %q: %v", id, err)
	}
	if !strings.Contains(err.Error(), "version") {
		t.Fatalf("error does not name the version field: %v", err)
	}
	got, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(want) {
		t.Fatalf("open modified the damaged file")
	}
}

// A job carrying two recognized version spellings is refused: canonical plus
// an ASCII case variant or a JSON-escaped spelling, equal or differing values,
// both values legal. The error names the job and the version field, and the
// file is left byte-for-byte in place.
func TestJobVersionDuplicateIsCorrupt(t *testing.T) {
	escV := jsonBackslash + "u0076ersion" // "version" as raw JSON text
	escI := "vers" + jsonBackslash + "u0069on"
	cases := []struct {
		name string
		frag string // replaces the single "version": 1 line
	}{
		{"identical values, ASCII variant", `"version": 1, "VERSION": 1`},
		{"differing values, ASCII variant", `"version": 1, "VERSION": 2`},
		{"upper then canonical, differing", `"VERSION": 2, "version": 1`},
		{"mixed case variant, same value", `"version": 1, "VeRsIoN": 1`},
		{"escaped leading letter, same value", `"version": 1, "` + escV + `": 1`},
		{"escaped inner letter, differing", `"version": 1, "` + escI + `": 2`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, h := seedBoundJobStore(t)
			bad := replaceInJob(t, dir, "j-bound", versionLine(1), tc.frag)

			assertJobVersionCorrupt(t, dir, "j-bound", bad)
			// The committed compiled hash survives untouched: the reader must
			// neither pick a version nor rewrite (and drop) the binding.
			if !strings.Contains(string(bad), `"compiled_hash": "`+h+`"`) {
				t.Fatalf("test setup lost the bound hash")
			}
			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("reopen on ambiguous dir: want corruption, got %v", err)
			}
		})
	}
}

// The motivating ambiguity: an unbound job whose version is spelled both 1 and
// 2, with both versions frozen and set up. Without the uniqueness rule
// encoding/json keeps the last value and the job reads against v2; the read
// must instead be refused.
func TestJobVersionAmbiguousBetweenTwoFrozenVersions(t *testing.T) {
	dir := seedTwoVersionJobStore(t)
	bad := replaceInJob(t, dir, "jmv", versionLine(1), `"version": 1, "VERSION": 2`)
	assertJobVersionCorrupt(t, dir, "jmv", bad)
}

// The two version spellings need not be adjacent: a second spelling later in
// the record, separated by other fields, is still a duplicate and is rejected.
func TestJobVersionDuplicateInterspersedIsCorrupt(t *testing.T) {
	dir, _ := seedBoundJobStore(t)
	old := `"version": 1,` + jobFieldIndent + `"kind": "prove",` + jobFieldIndent + `"attempt": 1`
	repl := `"version": 1,` + jobFieldIndent + `"kind": "prove",` + jobFieldIndent +
		`"attempt": 1,` + jobFieldIndent + `"VERSION": 2`
	bad := replaceInJob(t, dir, "j-bound", old, repl)
	assertJobVersionCorrupt(t, dir, "j-bound", bad)
}

// A long-s "verſion" beside a canonical or ASCII spelling is a duplicate
// version field, direct or u017f-escaped, in either order and whether the two
// values agree. It must not fold its way into one silently-chosen version.
func TestJobVersionLongSLookalikeBesideCanonicalIsCorrupt(t *testing.T) {
	direct := "ver" + "ſ" + "ion"
	escaped := "ver" + jsonBackslash + "u017f" + "ion"
	cases := []struct {
		name string
		frag string
	}{
		{"canonical then direct, same value", `"version": 1, "` + direct + `": 1`},
		{"direct then canonical, differing", `"` + direct + `": 2, "version": 1`},
		{"canonical then escaped, same value", `"version": 1, "` + escaped + `": 1`},
		{"escaped then ASCII variant, differing", `"` + escaped + `": 2, "VERSION": 1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := seedBoundJobStore(t)
			bad := replaceInJob(t, dir, "j-bound", versionLine(1), tc.frag)
			assertJobVersionCorrupt(t, dir, "j-bound", bad)
		})
	}
}

// A single recognized version spelling keeps the current reading, including
// its ASCII case and long-s compatibility: the job loads against the version
// named, direct or escaped.
func TestJobVersionSingleSpellingStillReads(t *testing.T) {
	directLongS := "ver" + "ſ" + "ion"
	escapedLongS := "ver" + jsonBackslash + "u017f" + "ion"
	escS := "ver" + jsonBackslash + "u0073" + "ion"
	cases := []struct {
		name string
		frag string
	}{
		{"canonical", `"version": 1`},
		{"upper case", `"VERSION": 1`},
		{"mixed case", `"VeRsIoN": 1`},
		{"escaped letter", `"` + escS + `": 1`},
		{"direct long s", `"` + directLongS + `": 1`},
		{"escaped long s", `"` + escapedLongS + `": 1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, h := seedBoundJobStore(t)
			replaceInJob(t, dir, "j-bound", versionLine(1), tc.frag)

			s, err := Open(dir)
			if err != nil {
				t.Fatalf("single spelling %q must load: %v", tc.name, err)
			}
			defer s.Close()
			got, err := s.GetJob("j-bound")
			if err != nil {
				t.Fatal(err)
			}
			if got.Version != 1 {
				t.Fatalf("single spelling %q read version %d, want 1", tc.name, got.Version)
			}
			if got.CompiledHash != h {
				t.Fatalf("binding lost under single spelling %q: %q", tc.name, got.CompiledHash)
			}
		})
	}
}

// An illegal single version value is still rejected, exactly as before: a
// non-positive version names no circuit and fails the integrity validation.
func TestJobVersionSingleIllegalValueStillRejected(t *testing.T) {
	dir, _ := seedBoundJobStore(t)
	replaceInJob(t, dir, "j-bound", versionLine(1), `"version": 0`)
	if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("version 0: want ErrDataCorrupt, got %v", err)
	}
}

// Damage appearing after the directory is open is caught by the very next
// operation, whether a read or a write: reads return no partial job list,
// mutations are not committed, and the file (hash included) is never rewritten.
func TestOpenStoreRefusesAfterJobVersionAmbiguity(t *testing.T) {
	dir, h := seedBoundJobStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bad := replaceInJob(t, dir, "j-bound", versionLine(1), `"version": 1, "VERSION": 2`)

	if _, err := s.GetJob("j-bound"); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetJob after ambiguity: want ErrDataCorrupt, got %v", err)
	}
	if jobs, err := s.ListJobs(); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("ListJobs after ambiguity: want ErrDataCorrupt, got %d jobs err=%v", len(jobs), err)
	}
	if _, err := s.SubmitJob(boundProveJob("j-new", "c", 1, h)); !errors.Is(err, ErrDataCorrupt) {
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
