package zkcircuit

import (
	"errors"
	"strings"
	"testing"
)

// Regression tests for the version field on a committed trusted-setup record.
// A trusted setup belongs only to the circuit version named at registration,
// and the record's version is the sole authority for which frozen version it
// covers. Any spelling the ordinary read would fill Version from names that
// one field: the canonical "version", an ASCII letter-case variant, a
// JSON-escaped spelling, or the Unicode-folded long-s "verſion" (U+017F).
// Two such spellings on one record make the covered version a function of key
// order (encoding/json keeps the last value) — the v1 setup becomes
// unfindable while v2 prove jobs pass the setup check — and are refused as
// data corruption: equal or differing values, both legal frozen versions with
// no other conflict, and even with unrelated fields interleaved. A single
// recognized spelling, a lone long-s spelling included, keeps reading as it
// does today.

// setupFieldIndent is the indentation of a setup record's own members as
// written by json.MarshalIndent with two-space indents (envelope → setups →
// record).
const setupFieldIndent = "\n      "

// seedTwoVersionSetupStore commits two frozen, set-up versions c@1 and c@2
// and returns the data directory. It reproduces the ambiguity that matters:
// a setup record spelling version both as 1 and 2 has a legal frozen target
// under either value and no other conflict.
func seedTwoVersionSetupStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedFrozenSource(t, s, "c", 1, 1, 1, 1, "")
	seedFrozenSource(t, s, "c", 2, 1, 1, 1, "")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// replaceInSetup scopes a literal replacement to the first setup record (the
// setups array is committed sorted by name and version, so c@1 leads): it
// finds the "setups" marker and replaces the first occurrence of old after
// it, so a "version": 1 shared by the circuit and job records is never
// mistaken for the setup's own field. It returns the bytes now on disk.
func replaceInSetup(t *testing.T, dir, old, repl string) []byte {
	t.Helper()
	raw, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	marker := `"setups": [`
	at := strings.Index(string(raw), marker)
	if at < 0 {
		t.Fatalf("committed file does not carry the setups marker %q", marker)
	}
	rel := strings.Index(string(raw[at:]), old)
	if rel < 0 {
		t.Fatalf("first setup record does not carry %q after the setups marker", old)
	}
	pos := at + rel
	bad := string(raw[:pos]) + repl + string(raw[pos+len(old):])
	writeDataFile(t, dir, []byte(bad))
	return []byte(bad)
}

// setupVersionLine is the single-line form of the first setup record's
// version member, as written by the deterministic encoder.
func setupVersionLine(v int) string { return `"version": ` + itoa(v) }

// assertSetupVersionCorrupt opens dir, requires ErrDataCorrupt naming the
// setup record's position and the version field, and checks the file is
// still byte-for-byte want.
func assertSetupVersionCorrupt(t *testing.T, dir string, want []byte) {
	t.Helper()
	_, err := Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	if !strings.Contains(err.Error(), "setup record #1") {
		t.Fatalf("error does not locate the damaged setup record: %v", err)
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

// A setup record carrying two recognized version spellings is refused:
// canonical plus an ASCII case variant or a JSON-escaped spelling, two
// byte-identical spellings, equal or differing values, both values legal.
// The error locates the record and names the version field, and the file is
// left byte-for-byte in place.
func TestSetupVersionDuplicateIsCorrupt(t *testing.T) {
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
		{"byte-identical spelling, same value", `"version": 1, "version": 1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedTwoVersionSetupStore(t)
			bad := replaceInSetup(t, dir, setupVersionLine(1), tc.frag)

			assertSetupVersionCorrupt(t, dir, bad)
			// The reader must neither pick a version nor rewrite the file.
			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("reopen on ambiguous dir: want corruption, got %v", err)
			}
		})
	}
}

// The motivating ambiguity: with v1 and v2 both frozen and set up, a setup
// record spelling version both 1 and 2 would read as the v2 setup under
// last-value-wins — the v1 setup query misses and a v2 prove job passes the
// setup check. The read must instead be refused as corruption.
func TestSetupVersionAmbiguousBetweenTwoFrozenVersions(t *testing.T) {
	dir := seedTwoVersionSetupStore(t)
	bad := replaceInSetup(t, dir, setupVersionLine(1), `"version": 1, "VERSION": 2`)
	assertSetupVersionCorrupt(t, dir, bad)
}

// The two version spellings need not be adjacent: a second spelling later in
// the record, with the name field between them, is still a duplicate and is
// rejected.
func TestSetupVersionDuplicateInterspersedIsCorrupt(t *testing.T) {
	dir := seedTwoVersionSetupStore(t)
	old := `"name": "c",` + setupFieldIndent + `"version": 1`
	repl := `"version": 1,` + setupFieldIndent + `"name": "c",` + setupFieldIndent + `"VERSION": 2`
	bad := replaceInSetup(t, dir, old, repl)
	assertSetupVersionCorrupt(t, dir, bad)
}

// A long-s "verſion" beside a canonical or ASCII spelling is a duplicate
// version field, direct or u017f-escaped, in either order and whether the two
// values agree. It must not fold its way into one silently-chosen version.
func TestSetupVersionLongSBesideRecognizedIsCorrupt(t *testing.T) {
	direct := "verſion"
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
			dir := seedTwoVersionSetupStore(t)
			bad := replaceInSetup(t, dir, setupVersionLine(1), tc.frag)
			assertSetupVersionCorrupt(t, dir, bad)
		})
	}
}

// A single recognized version spelling keeps the current reading, including
// the ASCII case and long-s compatibility of the ordinary decode: the setup
// loads against the version named, direct or escaped.
func TestSetupVersionSingleSpellingStillReads(t *testing.T) {
	directLongS := "verſion"
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
			dir := seedTwoVersionSetupStore(t)
			replaceInSetup(t, dir, setupVersionLine(1), tc.frag)

			s, err := Open(dir)
			if err != nil {
				t.Fatalf("single spelling %q must load: %v", tc.name, err)
			}
			defer s.Close()
			if _, found, err := s.GetSetup("c", 1); err != nil || !found {
				t.Fatalf("single spelling %q: v1 setup must be found (found=%t, err=%v)", tc.name, found, err)
			}
			if _, found, err := s.GetSetup("c", 2); err != nil || !found {
				t.Fatalf("single spelling %q: v2 setup must be found (found=%t, err=%v)", tc.name, found, err)
			}
		})
	}
}

// An illegal single version value is still rejected, exactly as before: a
// non-positive version names no circuit and fails the integrity validation,
// and a non-integer value fails the ordinary decode.
func TestSetupVersionSingleIllegalValueStillRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		frag string
	}{
		{"zero", `"version": 0`},
		{"negative", `"version": -1`},
		{"string", `"version": "1"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedTwoVersionSetupStore(t)
			replaceInSetup(t, dir, setupVersionLine(1), tc.frag)
			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("%s: want ErrDataCorrupt, got %v", tc.name, err)
			}
		})
	}
}

// Damage appearing after the directory is open is caught by the very next
// operation, whether a query or a write: queries return no partial records,
// neither registering a setup nor submitting a job reports success or saves
// anything, and the file is never rewritten.
func TestOpenStoreRefusesAfterSetupVersionAmbiguity(t *testing.T) {
	dir := seedTwoVersionSetupStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bad := replaceInSetup(t, dir, setupVersionLine(1), `"version": 1, "VERSION": 2`)

	if _, _, err := s.GetSetup("c", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetSetup after ambiguity: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.RecordSetup("c", 2); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("RecordSetup after ambiguity: want ErrDataCorrupt, got %v", err)
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
		t.Fatalf("refused operations rewrote the damaged file")
	}
}

// Legal setup records keep their exact behavior: a registered setup is found
// only for its own name and version, re-registering returns the stored record
// without adding a duplicate, and a prove job pinned to a frozen version
// with no setup still reports trusted setup missing.
func TestSetupRecordsUnaffectedByVersionRule(t *testing.T) {
	dir := seedTwoVersionSetupStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, found, err := s.GetSetup("c", 1); err != nil || !found {
		t.Fatalf("v1 setup must be found (found=%t, err=%v)", found, err)
	}
	if _, found, err := s.GetSetup("c", 3); err != nil || found {
		t.Fatalf("v3 setup must not exist (found=%t, err=%v)", found, err)
	}
	setup, err := s.RecordSetup("c", 1)
	if err != nil {
		t.Fatalf("re-registering v1 setup must stay idempotent: %v", err)
	}
	if setup.Name != "c" || setup.Version != 1 {
		t.Fatalf("re-registration returned %+v, want c v1", setup)
	}
	if n := countSetups(t, s); n != 2 {
		t.Fatalf("re-registration added a record: want 2 setups, got %d", n)
	}

	// A frozen version with no setup still refuses prove jobs as before.
	seedFrozen := Circuit{Name: "c", Version: 3, Constraints: 1, PublicInputs: 1, PrivateInputs: 1}
	if _, err := s.CreateCircuit(seedFrozen); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit("c", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitJob(boundProveJob("j-nosetup", "c", 3, "")); !errors.Is(err, ErrSetupMissing) {
		t.Fatalf("job on setup-less version: want ErrSetupMissing, got %v", err)
	}
}
