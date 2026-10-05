package zkcircuit

import (
	"errors"
	"strings"
	"testing"
)

// Regression tests for an ambiguous pinned-circuit version on a committed
// proof job. The single "version" member a job record writes is the only
// authority for the frozen version the job is bound to; it must never be
// decided by object member order. encoding/json keeps the last value of a
// repeated key and matches struct tags with Unicode case folding, so a record
// carrying both "version":1 and "VERSION":2 silently read as version 2 — and
// when both versions are frozen with their own trusted setups an unbound job
// could be queried as version 2.
//
// Every spelling the existing reading behavior recognizes as the version
// field — canonical, an ASCII letter-case variant, a JSON-unescaped spelling,
// or "verſion" with U+017F long s (direct or ſ-escaped) — counts as the
// same field, which may appear at most once. Two of them refuse the whole
// read as ErrDataCorrupt even when the values agree or both versions are
// legal, and even when interleaved with the record's other members. A single
// spelling keeps the old value and spelling compatibility, and an
// already-rejected illegal version value stays rejected.

// spliceJobObjectRaw replaces the whole committed JSON object of the job
// carrying id with objectText, returning the bytes now on disk. Job objects
// hold only scalar members, so the object spans the nearest '{' before the id
// marker to the first '}' after it. Everything else in the envelope (circuits,
// setups, artifacts) stays byte-for-byte as committed.
func spliceJobObjectRaw(t *testing.T, dir, id, objectText string) []byte {
	t.Helper()
	raw, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	str := string(raw)
	marker := `"id": "` + id + `"`
	at := strings.Index(str, marker)
	if at < 0 {
		t.Fatalf("committed file does not carry job %q", id)
	}
	open := strings.LastIndex(str[:at], "{")
	closeRel := strings.Index(str[at:], "}")
	if open < 0 || closeRel < 0 {
		t.Fatalf("cannot locate object braces for job %q", id)
	}
	close := at + closeRel
	bad := str[:open] + objectText + str[close+1:]
	writeDataFile(t, dir, []byte(bad))
	return []byte(bad)
}

// seedTwoVersionJobStore commits two frozen versions of "c", each with its
// own trusted setup, and one unbound job "jx" pinned to v1. This is exactly
// the situation in which an ambiguous version used to let the job be queried
// as v2.
func seedTwoVersionJobStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	readyCircuit(t, s, "c", 1)
	readyCircuit(t, s, "c", 2)
	if _, err := s.SubmitJob(boundProveJob("jx", "c", 1, "")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// escapedVersionV spells "version" with the leading 'v' JSON-escaped; after
// unescaping it is the canonical spelling byte-for-byte, so a canonical key
// beside it is an exact duplicate caught by the envelope-wide scanner.
var escapedVersionV = jsonEscape + "u0076" + "ersion"

// escapedVersionUpper spells the ASCII variant "VERSION" with its capital V
// JSON-escaped. After unescaping it is the variant "VERSION", not the
// canonical key, so it reaches the job-level folded-spelling check.
var escapedVersionUpper = jsonEscape + "u0056" + "ERSION"

// longVersion is "verſion" with the long s written directly.
var longVersion = "ver" + longS + "ion"

// escapedLongVersion is the same key written with the JSON escape u017f.
var escapedLongVersion = "ver" + jsonEscape + "u017f" + "ion"

// TestJobVersionFieldAmbiguousIsCorrupt drives every shape of ambiguity
// through a fresh Open: two recognized spellings of the version field refuse
// the read, name the damaged job and the version field, and leave the file
// byte-for-byte in place. Two byte-identical canonical keys are caught a
// little earlier by the envelope-wide duplicate scanner and are located by
// their jobs-array index instead of the id; that still names the record and
// the field.
func TestJobVersionFieldAmbiguousIsCorrupt(t *testing.T) {
	job := func(members string) string { return `{` + members + `}` }
	cases := []struct {
		name   string
		object string
		wantID bool // message is expected to name the job id "jx"
	}{
		{"canonical then upper, differing values",
			job(`"id": "jx", "circuit": "c", "version": 1, "VERSION": 2, "kind": "prove", "attempt": 1`), true},
		{"canonical then upper, identical values",
			job(`"id": "jx", "circuit": "c", "version": 1, "VERSION": 1, "kind": "prove", "attempt": 1`), true},
		{"upper then canonical (last-value would pick 1)",
			job(`"id": "jx", "circuit": "c", "VERSION": 2, "version": 1, "kind": "prove", "attempt": 1`), true},
		{"mixed-case variant",
			job(`"id": "jx", "circuit": "c", "version": 1, "Version": 2, "kind": "prove", "attempt": 1`), true},
		{"duplicate spread among other fields",
			job(`"VERSION": 2, "id": "jx", "kind": "prove", "circuit": "c", "attempt": 1, "version": 1`), true},
		{"long-s spelling beside canonical, same value",
			job(`"id": "jx", "circuit": "c", "version": 1, "` + longVersion + `": 1, "kind": "prove", "attempt": 1`), true},
		{"long-s spelling before canonical, differing value",
			job(`"id": "jx", "` + longVersion + `": 1, "circuit": "c", "version": 2, "kind": "prove", "attempt": 1`), true},
		{"escaped long-s beside canonical",
			job(`"id": "jx", "circuit": "c", "version": 1, "` + escapedLongVersion + `": 2, "kind": "prove", "attempt": 1`), true},
		{"long-s spelling beside an ASCII variant (neither canonical)",
			job(`"id": "jx", "circuit": "c", "VERSION": 1, "` + longVersion + `": 2, "kind": "prove", "attempt": 1`), true},
		{"escaped ASCII variant beside canonical",
			job(`"id": "jx", "circuit": "c", "version": 1, "` + escapedVersionUpper + `": 2, "kind": "prove", "attempt": 1`), true},
		{"escaped canonical letter beside canonical (exact after unescape)",
			job(`"id": "jx", "circuit": "c", "version": 1, "` + escapedVersionV + `": 2, "kind": "prove", "attempt": 1`), false},
		{"two byte-identical canonical keys",
			job(`"id": "jx", "circuit": "c", "version": 1, "version": 2, "kind": "prove", "attempt": 1`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedTwoVersionJobStore(t)
			bad := spliceJobObjectRaw(t, dir, "jx", tc.object)

			s, err := Open(dir)
			if err == nil {
				s.Close()
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), "version") {
				t.Fatalf("error does not name the version field: %v", err)
			}
			if tc.wantID && !strings.Contains(err.Error(), `"jx"`) {
				t.Fatalf("error does not name the damaged job: %v", err)
			}
			if !tc.wantID && !strings.Contains(strings.ToLower(err.Error()), "job") {
				t.Fatalf("error does not locate the damaged job record: %v", err)
			}
			got, rerr := readDataFile(t, dir)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(got) != string(bad) {
				t.Fatalf("open modified the damaged file")
			}
			// A second open still refuses; nothing picks a version or drops the key.
			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("reopen on ambiguous dir: want corruption, got %v", err)
			}
		})
	}
}

// A single version spelling keeps the existing value and spelling
// compatibility: canonical, ASCII case variants, an escaped canonical letter,
// and even a lone U+017F long-s spelling all read pinned to v1, and the other
// committed job data is untouched.
func TestJobVersionFieldLoneSpellingStillReads(t *testing.T) {
	job := func(key string) string {
		return `{"id": "jx", "circuit": "c", "` + key + `": 1, "kind": "prove", "attempt": 1}`
	}
	cases := []struct {
		name string
		key  string
	}{
		{"canonical", "version"},
		{"upper case", "VERSION"},
		{"mixed case", "Version"},
		{"escaped canonical letter", escapedVersionV},
		{"direct long s", longVersion},
		{"escaped long s", escapedLongVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedTwoVersionJobStore(t)
			spliceJobObjectRaw(t, dir, "jx", job(tc.key))

			s, err := Open(dir)
			if err != nil {
				t.Fatalf("lone spelling %q must load: %v", tc.key, err)
			}
			defer s.Close()
			got, err := s.GetJob("jx")
			if err != nil {
				t.Fatalf("GetJob under lone %q: %v", tc.key, err)
			}
			if got.Version != 1 || got.Circuit != "c" || got.Kind != "prove" || got.Attempt != 1 {
				t.Fatalf("lone %q read the job wrong: %+v", tc.key, got)
			}
			jobs, err := s.ListJobs()
			if err != nil || len(jobs) != 1 || jobs[0].Version != 1 {
				t.Fatalf("ListJobs under lone %q: %+v err=%v", tc.key, jobs, err)
			}
			// It must not have drifted onto v2.
			if _, err := s.GetJob("jx"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// An already-rejected illegal version value stays rejected under every
// accepted spelling: null and 0 fail to resolve a pinned version, and a
// string is not an integer. None reads as a valid bound job.
func TestJobVersionFieldIllegalValueStillRejected(t *testing.T) {
	for _, spelling := range []string{"version", "VERSION", longVersion} {
		for _, value := range []string{"null", "0", `"x"`} {
			key := spelling
			t.Run(spelling+"/"+value, func(t *testing.T) {
				dir := seedTwoVersionJobStore(t)
				object := `{"id": "jx", "circuit": "c", "` + key + `": ` + value + `, "kind": "prove", "attempt": 1}`
				spliceJobObjectRaw(t, dir, "jx", object)
				s, err := Open(dir)
				if err == nil {
					s.Close()
				}
				if !errors.Is(err, ErrDataCorrupt) {
					t.Fatalf("version=%s under %q: want ErrDataCorrupt, got %v", value, spelling, err)
				}
			})
		}
	}
}

// Once a healthy directory is open, introducing the ambiguity on disk is
// caught by the very next operation: reads return no partial job list, writes
// commit nothing, an unrelated circuit change cannot rewrite (and thereby
// clean up) the file, and the in-memory copy from the successful open is
// never served one more time.
func TestOpenStoreRefusesAfterVersionAmbiguity(t *testing.T) {
	dir := seedTwoVersionJobStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// The first open serves the clean state normally.
	if j, err := s.GetJob("jx"); err != nil || j.Version != 1 {
		t.Fatalf("clean state before tampering: %+v %v", j, err)
	}

	object := `{"id": "jx", "circuit": "c", "version": 1, "VERSION": 2, "kind": "prove", "attempt": 1}`
	bad := spliceJobObjectRaw(t, dir, "jx", object)

	if _, err := s.GetJob("jx"); !errors.Is(err, ErrDataCorrupt) {
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
		t.Fatalf("damaged file was rewritten by a refused operation")
	}
}

// An ambiguous version on a hash-bound job is refused just the same, and the
// pinned compiled hash survives byte-for-byte because nothing is rewritten.
func TestBoundJobVersionAmbiguityKeepsHashUntouched(t *testing.T) {
	dir, h := seedBoundJobStore(t) // "j-bound" on c@1, carries the artifact hash
	object := `{"id": "j-bound", "circuit": "c", "version": 1, "VERSION": 1, "kind": "prove", "attempt": 1, "compiled_hash": "` + h + `"}`
	bad := spliceJobObjectRaw(t, dir, "j-bound", object)

	if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("bound job with ambiguous version: want ErrDataCorrupt, got %v", err)
	}
	got, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(bad) || !strings.Contains(string(got), `"compiled_hash": "`+h+`"`) {
		t.Fatalf("bound hash was not preserved byte-for-byte")
	}
}
