package zkcircuit

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// Regression tests for the identity field on a committed job record. The
// "id" is a job's sole, deterministic identity — the key every query uses —
// so it may not be a function of which field happens to appear last in the
// file: encoding/json keeps the last value, which let swapping "id" and "ID"
// change the id the same record was queried under. Any spelling the ordinary
// read would fill ID from names that one field: the canonical "id", the
// ASCII letter-case variants "ID", "Id" and "iD", or a JSON-escaped spelling
// that unescapes onto one of them ("id" has no non-ASCII case fold). Two
// such spellings on one record are refused as data corruption — equal or
// differing values, both present, non-empty and legal, even with unrelated
// fields (circuit name, version, attempt count, …) interleaved, and even with
// no lowercase "id" among the variants. Two byte-identical spellings stay
// refused as well.
//
// Because the record then has no settled identity, the error locates it by
// its 1-based position in the jobs array ("job record #n") instead of
// quoting either candidate value, and names the field by its canonical
// spelling "id". A single recognized spelling keeps reading as it does
// today, and its value keeps matching exactly (no case folding, trimming or
// U+FFFD repair).

// idLine is the single-line form of the job's own id member for the named
// job, as written by the deterministic encoder.
func idLine(id string) string { return `"id": "` + id + `"` }

// assertJobIDAmbiguous opens dir, requires ErrDataCorrupt naming the
// 1-based record position and the id field, requires that no candidate id is
// quoted as the record's settled identity, and checks the file is still
// byte-for-byte want.
func assertJobIDAmbiguous(t *testing.T, dir string, pos int, candidates []string, want []byte) {
	t.Helper()
	_, err := Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	loc := "job record #" + strconv.Itoa(pos)
	if !strings.Contains(err.Error(), loc) {
		t.Fatalf("error does not locate the record as %q: %v", loc, err)
	}
	if !strings.Contains(err.Error(), `field "id"`) {
		t.Fatalf("error does not name the id field: %v", err)
	}
	for _, c := range candidates {
		if strings.Contains(err.Error(), strconv.Quote(c)) {
			t.Fatalf("error treats the candidate id %q as the record's settled identity: %v", c, err)
		}
	}
	got, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(want) {
		t.Fatalf("open modified the damaged file")
	}
}

// A job carrying two recognized id spellings is refused: canonical plus any
// ASCII case variant or a JSON-escaped spelling, equal or differing values,
// both present, non-empty and legal (Chinese, emoji, "�"), in either key
// order. The error locates the record positionally and names the id field,
// and the file is left byte-for-byte in place.
func TestJobIDDuplicateIsCorrupt(t *testing.T) {
	escIDSans := jsonBackslash + "u0069d"                           // "id"
	escIDUpper := jsonBackslash + "u0049" + jsonBackslash + "u0044" // "ID"
	cases := []struct {
		name string
		frag string // replaces the single "id": "j-bound" line
	}{
		{"identical values, ID variant", `"id": "j-bound", "ID": "j-bound"`},
		{"differing values, ID variant", `"id": "j-bound", "ID": "j-other"`},
		{"upper then canonical, differing", `"ID": "j-other", "id": "j-bound"`},
		{"Id variant, same value", `"id": "j-bound", "Id": "j-bound"`},
		{"iD variant, differing value", `"id": "j-bound", "iD": "j-other"`},
		{"escaped canonical key, same value", `"id": "j-bound", "` + escIDSans + `": "j-bound"`},
		{"escaped ID key, differing value", `"id": "j-bound", "` + escIDUpper + `": "j-other"`},
		{"both legal Unicode values", `"id": "job-作业", "ID": "job-😀"`},
		{"legal values containing U+FFFD", `"id": "a�b", "ID": "c�d"`},
		{"surrogate-pair emoji value", `"id": "face-😀", "ID": "face-😀-2"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, h := seedBoundJobStore(t)
			bad := replaceInJob(t, dir, "j-bound", idLine("j-bound"), tc.frag)

			assertJobIDAmbiguous(t, dir, 1, []string{"j-bound", "j-other", "job-作业", "job-😀", "a�b", "c�d", "face-😀", "face-😀-2"}, bad)
			// The committed compiled hash survives untouched: the reader
			// neither picks an id nor rewrites (and drops) the binding.
			if !strings.Contains(string(bad), `"compiled_hash": "`+h+`"`) {
				t.Fatalf("test setup lost the bound hash")
			}
			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("reopen on ambiguous dir: want corruption, got %v", err)
			}
		})
	}
}

// A duplicate made only of case variants, with no lowercase "id", is just as
// ambiguous: encoding/json folds all of them onto ID and keeps the last, so
// "ID" beside "Id" must not bypass the rule.
func TestJobIDVariantOnlyDuplicateIsCorrupt(t *testing.T) {
	cases := []struct {
		name string
		frag string
	}{
		{"ID and Id, same value", `"ID": "j-bound", "Id": "j-bound"`},
		{"Id and iD, differing", `"Id": "j-bound", "iD": "j-other"`},
		{"iD then ID, differing", `"iD": "j-other", "ID": "j-bound"`},
		{"all three variants", `"ID": "a", "Id": "b", "iD": "c"`},
		{"ID and escaped lowercase, same value", `"ID": "j-bound", "` + jsonBackslash + `u0069d": "j-bound"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := seedBoundJobStore(t)
			bad := replaceInJob(t, dir, "j-bound", idLine("j-bound"), tc.frag)
			assertJobIDAmbiguous(t, dir, 1, []string{"j-bound", "j-other", "a", "b", "c"}, bad)
		})
	}
}

// Two byte-identical spellings keep being refused, now through the identity
// uniqueness rule rather than a key-order-dependent last value.
func TestJobIDExactDuplicateIsCorrupt(t *testing.T) {
	dir, _ := seedBoundJobStore(t)
	bad := replaceInJob(t, dir, "j-bound", idLine("j-bound"),
		`"id": "j-bound", "id": "j-bound"`)
	assertJobIDAmbiguous(t, dir, 1, []string{"j-bound"}, bad)
}

// The two id spellings need not be adjacent: a second spelling later in the
// record, separated by the circuit name, pinned version, kind and attempt, is
// still a duplicate and is rejected.
func TestJobIDDuplicateInterspersedIsCorrupt(t *testing.T) {
	dir, _ := seedBoundJobStore(t)
	old := `"id": "j-bound",` + jobFieldIndent + `"circuit": "c",` + jobFieldIndent +
		`"version": 1,` + jobFieldIndent + `"kind": "prove",` + jobFieldIndent + `"attempt": 1`
	repl := old + `,` + jobFieldIndent + `"ID": "j-other"`
	bad := replaceInJob(t, dir, "j-bound", old, repl)
	assertJobIDAmbiguous(t, dir, 1, []string{"j-bound", "j-other"}, bad)
}

// The positional attribution points at the record that carries the two ids:
// ambiguity on the second job leaves the first job out of the message and
// still does not print either record's data.
func TestJobIDDuplicateNamesTheDamagedPosition(t *testing.T) {
	dir, _ := seedBoundJobStore(t) // j-bound sorts first, j-plain second
	bad := replaceInJob(t, dir, "j-plain", idLine("j-plain"),
		`"id": "j-plain", "ID": "j-other"`)

	_, err := Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	if !strings.Contains(err.Error(), "job record #2") {
		t.Fatalf("error does not locate the second record: %v", err)
	}
	if strings.Contains(err.Error(), "job record #1") {
		t.Fatalf("error implicates the first, intact record: %v", err)
	}
	if strings.Contains(err.Error(), strconv.Quote("j-bound")) {
		t.Fatalf("error leaks the intact job's id: %v", err)
	}
	got, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(bad) {
		t.Fatalf("open modified the damaged file")
	}
}

// The identity ambiguity outranks the other per-record findings: a record
// carrying two ids and two version spellings is reported as an id problem,
// located positionally.
func TestJobIDAmbiguityOutranksOtherRecordFindings(t *testing.T) {
	dir, _ := seedBoundJobStore(t)
	bad := replaceInJob(t, dir, "j-bound", idLine("j-bound"),
		`"id": "j-bound", "ID": "j-other"`)
	// A second recognized version spelling on the same record must not be
	// the finding that surfaces first.
	bad = replaceInJob(t, dir, "j-bound", versionLine(1), `"version": 1, "VERSION": 2`)

	_, err := Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	if !strings.Contains(err.Error(), "job record #1") || !strings.Contains(err.Error(), `field "id"`) {
		t.Fatalf("want the id ambiguity reported first, got %v", err)
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
// ASCII case-variant compatibility: the job loads and is queried under the
// exact value written, with its binding intact.
func TestJobIDSingleSpellingStillReads(t *testing.T) {
	escID := jsonBackslash + "u0069d"
	cases := []struct {
		name string
		frag string
	}{
		{"canonical", `"id": "j-bound"`},
		{"upper case", `"ID": "j-bound"`},
		{"capitalized", `"Id": "j-bound"`},
		{"inverted case", `"iD": "j-bound"`},
		{"escaped letters", `"` + escID + `": "j-bound"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, h := seedBoundJobStore(t)
			replaceInJob(t, dir, "j-bound", idLine("j-bound"), tc.frag)

			s, err := Open(dir)
			if err != nil {
				t.Fatalf("single spelling %q must load: %v", tc.name, err)
			}
			defer s.Close()
			got, err := s.GetJob("j-bound")
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != "j-bound" {
				t.Fatalf("single spelling %q read id %q, want j-bound", tc.name, got.ID)
			}
			if got.CompiledHash != h {
				t.Fatalf("binding lost under single spelling %q: %q", tc.name, got.CompiledHash)
			}
		})
	}
}

// The id value keeps matching exactly as written under a lone variant key: a
// differently cased value is not folded onto the lowercase id, and no
// surrounding whitespace is trimmed.
func TestJobIDValueMatchingStillExact(t *testing.T) {
	cases := []struct {
		name string
		frag string
		want string
		miss string
	}{
		{"mixed-case value under variant key", `"ID": "J-Bound"`, "J-Bound", "j-bound"},
		{"canonical key, uppercase value", `"id": "J-Bound"`, "J-Bound", "j-bound"},
		{"trailing space kept", `"id": "j-bound "`, "j-bound ", "j-bound"},
		{"leading space kept", `"ID": " j-bound"`, " j-bound", "j-bound"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := seedBoundJobStore(t)
			replaceInJob(t, dir, "j-bound", idLine("j-bound"), tc.frag)

			s, err := Open(dir)
			if err != nil {
				t.Fatalf("single spelling %q must load: %v", tc.name, err)
			}
			defer s.Close()
			if got, err := s.GetJob(tc.want); err != nil || got.ID != tc.want {
				t.Fatalf("job not found under its exact value: %+v %v", got, err)
			}
			if _, err := s.GetJob(tc.miss); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s: the id %q must not match the exact value %q", tc.name, tc.miss, tc.want)
			}
		})
	}
}

// Damage appearing after the directory is open is caught by the very next
// operation, whether a read or a write: reads return no partial job list
// (the intact job is not produced ahead of the failure), mutations are not
// committed, and the file (hash included) is never rewritten.
func TestOpenStoreRefusesAfterJobIDAmbiguity(t *testing.T) {
	dir, h := seedBoundJobStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bad := replaceInJob(t, dir, "j-bound", idLine("j-bound"),
		`"id": "j-bound", "ID": "j-other"`)

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

// The ambiguity must not masquerade as a missing id or a submit conflict:
// the error is exactly the data-corrupt kind, not ErrNotFound or ErrConflict.
func TestJobIDAmbiguityIsNotNotFoundOrConflict(t *testing.T) {
	dir, _ := seedBoundJobStore(t)
	replaceInJob(t, dir, "j-bound", idLine("j-bound"),
		`"id": "j-bound", "ID": "j-other"`)

	_, err := Open(dir)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
		t.Fatalf("ambiguous id reported as not-found/conflict: %v", err)
	}
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
}
