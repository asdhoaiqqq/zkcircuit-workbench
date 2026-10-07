package zkcircuit

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// Regression tests for the circuit version on a committed trusted-setup
// record. A setup belongs exclusively to the frozen circuit version named in
// the record; any spelling the ordinary read would fill Version from names
// that one field: the canonical "version", an ASCII letter-case variant, a
// JSON-escaped spelling, or the Unicode-folded long-s "verſion" (U+017F). Two
// such spellings on one record make the version a function of key order
// (encoding/json keeps the last value) — with both versions of one circuit
// frozen, a record carrying 1 and 2 would read as v2's setup, hiding v1's
// setup while letting a v2 proof job pass the setup check. Such a record is
// refused as data corruption instead: equal or differing values, both legal,
// even with the name or another member interleaved, in either key order. A
// long-s spelling standing alone is still the single version field and keeps
// reading as it does today.

// setupFieldIndent is the indentation of a setup record's own members as
// written by json.MarshalIndent with two-space indents (envelope → setups →
// record).
const setupFieldIndent = "\n      "

// seedTwoFrozenSetupStore commits frozen c@1 and c@2 with a trusted setup on
// v1 only. It reproduces the ambiguity that matters: a setup for v1 whose
// record also spells version 2 reads as v2's setup, so v1's setup vanishes
// while a v2 job would pass the setup gate.
func seedTwoFrozenSetupStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 2, Constraints: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit("c", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSetup("c", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// replaceInSetup scopes a literal replacement to the first record of the
// committed setups array: it finds the array marker and replaces the first
// occurrence of old after it, so the shared circuit "version" fields are
// never mistaken for a setup's own field.
func replaceInSetup(t *testing.T, dir, old, repl string) []byte {
	t.Helper()
	raw, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	marker := `"setups": [`
	at := strings.Index(string(raw), marker)
	if at < 0 {
		t.Fatalf("committed file does not carry the setups array")
	}
	rel := strings.Index(string(raw[at:]), old)
	if rel < 0 {
		t.Fatalf("setups array does not carry %q after its marker", old)
	}
	pos := at + rel
	bad := string(raw[:pos]) + repl + string(raw[pos+len(old):])
	writeDataFile(t, dir, []byte(bad))
	return []byte(bad)
}

// setupVersionLine is the single-line form of a setup's own version member,
// as written by the deterministic encoder.
func setupVersionLine(v int) string { return `"version": ` + strconv.Itoa(v) }

// assertSetupVersionCorrupt opens dir, requires ErrDataCorrupt naming the
// setup record positionally and the version field, and checks the file is
// still byte-for-byte want.
func assertSetupVersionCorrupt(t *testing.T, dir string, want []byte) {
	t.Helper()
	_, err := Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "setup record #1") {
		t.Fatalf("error does not locate the damaged setup record: %v", err)
	}
	if !strings.Contains(msg, "version") {
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

// A setup record carrying two recognized version spellings is refused:
// canonical plus an ASCII case variant or a JSON-escaped spelling, equal or
// differing values, both values legal, in either key order. The error locates
// the setup record and names the version field, and the file is left
// byte-for-byte in place.
func TestSetupVersionDuplicateIsCorrupt(t *testing.T) {
	escV := jsonBackslash + "u0076ersion" // "version" as raw JSON text
	escI := "vers" + jsonBackslash + "u0069on"
	escUpperV := jsonBackslash + "u0056ERSION" // "VERSION" as raw JSON text
	cases := []struct {
		name string
		frag string // replaces the single "version": 1 line in setup #1
	}{
		{"identical values, ASCII variant", `"version": 1, "VERSION": 1`},
		{"differing values, ASCII variant", `"version": 1, "VERSION": 2`},
		{"upper then canonical, differing", `"VERSION": 2, "version": 1`},
		{"two non-canonical variants, same value", `"VERSION": 1, "Version": 1`},
		{"two non-canonical variants, differing", `"VERSION": 2, "Version": 1`},
		{"mixed case variant, same value", `"version": 1, "VeRsIoN": 1`},
		{"escaped leading letter, same value", `"version": 1, "` + escV + `": 1`},
		{"escaped inner letter, differing", `"version": 1, "` + escI + `": 2`},
		{"escaped uppercase variant", `"version": 1, "` + escUpperV + `": 2`},
		{"byte-identical duplicate", `"version": 1, "version": 1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedTwoFrozenSetupStore(t)
			bad := replaceInSetup(t, dir, setupVersionLine(1), tc.frag)
			assertSetupVersionCorrupt(t, dir, bad)
			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("reopen on ambiguous dir: want corruption, got %v", err)
			}
		})
	}
}

// The motivating ambiguity: both versions frozen and the v1 setup record
// spells version both 1 and 2. Without the uniqueness rule encoding/json keeps
// the last value and the setup reads against v2 — v1's setup disappears while
// a v2 job passes the setup gate. The read must instead be refused.
func TestSetupVersionAmbiguousBetweenTwoFrozenVersions(t *testing.T) {
	dir := seedTwoFrozenSetupStore(t)
	bad := replaceInSetup(t, dir, setupVersionLine(1), `"version": 1, "VERSION": 2`)
	assertSetupVersionCorrupt(t, dir, bad)
}

// The two version spellings need not be adjacent: a second spelling later in
// the record, with the name (or another member) sitting between them, is still
// a duplicate and is rejected.
func TestSetupVersionDuplicateInterspersedIsCorrupt(t *testing.T) {
	dir := seedTwoFrozenSetupStore(t)
	old := `"name": "c",` + setupFieldIndent + `"version": 1`
	repl := `"version": 1,` + setupFieldIndent + `"name": "c",` + setupFieldIndent + `"VERSION": 2`
	bad := replaceInSetup(t, dir, old, repl)
	assertSetupVersionCorrupt(t, dir, bad)
}

// A long-s "verſion" beside a canonical or ASCII spelling is a duplicate
// version field, direct or u017f-escaped, in either order and whether the two
// values agree. It must not fold its way into one silently-chosen version.
func TestSetupVersionLongSLookalikeBesideCanonicalIsCorrupt(t *testing.T) {
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
		{"direct beside ASCII variant", `"VERSION": 1, "` + direct + `": 2`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedTwoFrozenSetupStore(t)
			bad := replaceInSetup(t, dir, setupVersionLine(1), tc.frag)
			assertSetupVersionCorrupt(t, dir, bad)
		})
	}
}

// A single recognized version spelling keeps the current reading, including
// its ASCII case and long-s compatibility: the setup loads against the
// version named, direct or escaped, and stays exclusive to that name+version.
func TestSetupVersionSingleSpellingStillReads(t *testing.T) {
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
			dir := seedTwoFrozenSetupStore(t)
			replaceInSetup(t, dir, setupVersionLine(1), tc.frag)

			s, err := Open(dir)
			if err != nil {
				t.Fatalf("single spelling %q must load: %v", tc.name, err)
			}
			defer s.Close()
			if _, found, err := s.GetSetup("c", 1); err != nil || !found {
				t.Fatalf("single spelling %q lost v1 setup: found=%v err=%v", tc.name, found, err)
			}
			if _, found, err := s.GetSetup("c", 2); err != nil || found {
				t.Fatalf("single spelling %q must not provide a v2 setup: found=%v err=%v", tc.name, found, err)
			}
			// A v1 job passes the setup gate; v2 still reports the setup
			// missing.
			if _, err := s.SubmitJob(boundProveJob("j1", "c", 1, "")); err != nil {
				t.Fatalf("v1 job after single spelling %q: %v", tc.name, err)
			}
			if _, err := s.SubmitJob(boundProveJob("j2", "c", 2, "")); !errors.Is(err, ErrSetupMissing) {
				t.Fatalf("v2 job after single spelling %q: want ErrSetupMissing, got %v", tc.name, err)
			}
		})
	}
}

// An illegal single version value is still rejected, exactly as before: a
// non-positive version names no circuit and fails the integrity validation.
func TestSetupVersionSingleIllegalValueStillRejected(t *testing.T) {
	dir := seedTwoFrozenSetupStore(t)
	replaceInSetup(t, dir, setupVersionLine(1), `"version": 0`)
	if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("version 0: want ErrDataCorrupt, got %v", err)
	}
}

// Re-registering a setup for the same name+version stays idempotent under the
// unchanged read, and returns the existing setup without a duplicate record.
func TestSetupReregisterStillIdempotent(t *testing.T) {
	dir := seedTwoFrozenSetupStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before := countSetups(t, s)
	got, err := s.RecordSetup("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "c" || got.Version != 1 {
		t.Fatalf("re-register returned %+v", got)
	}
	if after := countSetups(t, s); after != before {
		t.Fatalf("re-register added a record: %d -> %d", before, after)
	}
}

// Damage appearing after the directory is open is caught by the very next
// operation, whether a read or a write: queries return no setup, neither a
// setup registration nor a job submission succeeds or commits, and the file
// is never rewritten.
func TestOpenStoreRefusesAfterSetupVersionAmbiguity(t *testing.T) {
	dir := seedTwoFrozenSetupStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bad := replaceInSetup(t, dir, setupVersionLine(1), `"version": 1, "VERSION": 2`)

	if _, found, err := s.GetSetup("c", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetSetup after ambiguity: found=%v want ErrDataCorrupt, got %v", found, err)
	}
	if _, err := s.RecordSetup("c", 2); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("RecordSetup after ambiguity: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.SubmitJob(boundProveJob("j-new", "c", 2, "")); !errors.Is(err, ErrDataCorrupt) {
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
