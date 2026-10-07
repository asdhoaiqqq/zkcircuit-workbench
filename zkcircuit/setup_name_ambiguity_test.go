package zkcircuit

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// Regression tests for the owning circuit name on a committed trusted-setup
// record. The "name" a setup carries is the sole authority for which circuit
// the setup belongs to. With a and b both frozen at version 1 and a setup
// registered only for a, a record carrying both "name":"a" and "NAME":"b"
// would otherwise let encoding/json keep the last value and confirm the setup
// against b: b would read as having a setup and even accept a proof job. Any
// spelling the ordinary read would fill Name from names that one field: the
// canonical "name", an ASCII letter-case variant such as "NAME" or "NaMe", or
// a JSON-escaped spelling of either. Two such spellings on one record make the
// owning circuit a function of key order and are refused as data corruption —
// equal or differing values, whether either named circuit exists or is frozen,
// whether the keys are adjacent or the version sits between them, in either
// order, and even when neither spelling is the lowercase "name". A single
// recognized spelling keeps reading as it does today, and the value keeps
// being matched against circuit names exactly as written (no case folding, no
// trimming).

// seedTwoNamedFrozenSetupStore commits frozen a@1 and b@1 with a trusted setup
// recorded on a only. It reproduces the ambiguity that matters: a setup for a
// whose record also names b reads as b's setup, so a's setup vanishes while a
// b proof job would pass the setup gate.
func seedTwoNamedFrozenSetupStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b"} {
		if _, err := s.CreateCircuit(Circuit{Name: n, Version: 1, Constraints: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FreezeCircuit(n, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RecordSetup("a", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// setupNameLine is the single-line form of a setup's own name member, as
// written by the deterministic encoder.
func setupNameLine(n string) string { return `"name": ` + strconv.Quote(n) }

// assertSetupNameCorrupt opens dir, requires ErrDataCorrupt naming the setup
// record positionally and the name field, and checks the file is still
// byte-for-byte want.
func assertSetupNameCorrupt(t *testing.T, dir string, want []byte) {
	t.Helper()
	_, err := Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "setup record #1") {
		t.Fatalf("error does not locate the damaged setup record: %v", err)
	}
	if !strings.Contains(msg, "name") {
		t.Fatalf("error does not name the circuit name field: %v", err)
	}
	got, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(want) {
		t.Fatalf("open modified the damaged file")
	}
}

// A setup record carrying two recognized name spellings is refused: canonical
// plus an ASCII case variant or a JSON-escaped spelling, equal or differing
// values, both circuits frozen, in either key order. The error locates the
// setup record and names the name field, and the file is left byte-for-byte in
// place.
func TestSetupNameDuplicateIsCorrupt(t *testing.T) {
	escN := jsonBackslash + "u006eame" // "name" as raw JSON text
	escE := "nam" + jsonBackslash + "u0065"
	escUpperN := jsonBackslash + "u004e" + "AME" // "NAME" as raw JSON text
	cases := []struct {
		name string
		frag string // replaces the single "name": "a" line in setup #1
	}{
		{"identical values, ASCII variant", `"name": "a", "NAME": "a"`},
		{"differing values, ASCII variant", `"name": "a", "NAME": "b"`},
		{"upper then canonical, differing", `"NAME": "b", "name": "a"`},
		{"two non-canonical variants, same value", `"NAME": "a", "Name": "a"`},
		{"two non-canonical variants, differing", `"NAME": "b", "Name": "a"`},
		{"mixed case variant, same value", `"name": "a", "NaMe": "a"`},
		{"escaped leading letter, same value", `"name": "a", "` + escN + `": "a"`},
		{"escaped inner letter, differing", `"name": "b", "` + escE + `": "a"`},
		{"escaped uppercase variant", `"name": "a", "` + escUpperN + `": "b"`},
		{"byte-identical duplicate", `"name": "a", "name": "a"`},
		{"second value names a circuit that does not exist", `"name": "a", "NAME": "ghost"`},
		{"first value names a circuit that does not exist", `"NAME": "ghost", "name": "a"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedTwoNamedFrozenSetupStore(t)
			bad := replaceInSetup(t, dir, setupNameLine("a"), tc.frag)
			assertSetupNameCorrupt(t, dir, bad)
			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("reopen on ambiguous dir: want corruption, got %v", err)
			}
		})
	}
}

// The motivating ambiguity: a and b both frozen at v1, the a setup record
// spells its owner both a and b. Without the uniqueness rule encoding/json
// keeps the last value and the setup reads against b — a's setup disappears
// while a b job passes the setup gate. The read must instead be refused.
func TestSetupNameAmbiguousBetweenTwoFrozenCircuits(t *testing.T) {
	dir := seedTwoNamedFrozenSetupStore(t)
	bad := replaceInSetup(t, dir, setupNameLine("a"), `"name": "a", "NAME": "b"`)
	assertSetupNameCorrupt(t, dir, bad)
}

// The two name spellings need not be adjacent: a second spelling later in the
// record, with the version (or another member) sitting between them, is still
// a duplicate and is rejected.
func TestSetupNameDuplicateInterspersedIsCorrupt(t *testing.T) {
	dir := seedTwoNamedFrozenSetupStore(t)
	old := `"name": "a",` + setupFieldIndent + `"version": 1`
	repl := `"name": "a",` + setupFieldIndent + `"version": 1,` + setupFieldIndent + `"NAME": "b"`
	bad := replaceInSetup(t, dir, old, repl)
	assertSetupNameCorrupt(t, dir, bad)
}

// A single recognized name spelling keeps the current reading, including its
// ASCII case compatibility: the setup loads against the circuit named, direct
// or escaped, and stays exclusive to that name at v1.
func TestSetupNameSingleSpellingStillReads(t *testing.T) {
	escN := jsonBackslash + "u006eame"
	cases := []struct {
		name string
		frag string
	}{
		{"canonical", `"name": "a"`},
		{"upper case", `"NAME": "a"`},
		{"mixed case", `"NaMe": "a"`},
		{"escaped letter", `"` + escN + `": "a"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedTwoNamedFrozenSetupStore(t)
			replaceInSetup(t, dir, setupNameLine("a"), tc.frag)

			s, err := Open(dir)
			if err != nil {
				t.Fatalf("single spelling %q must load: %v", tc.name, err)
			}
			defer s.Close()
			if _, found, err := s.GetSetup("a", 1); err != nil || !found {
				t.Fatalf("single spelling %q lost a's setup: found=%v err=%v", tc.name, found, err)
			}
			if _, found, err := s.GetSetup("b", 1); err != nil || found {
				t.Fatalf("single spelling %q must not provide b a setup: found=%v err=%v", tc.name, found, err)
			}
			// An a job passes the setup gate; b still reports the setup
			// missing.
			if _, err := s.SubmitJob(boundProveJob("ja", "a", 1, "")); err != nil {
				t.Fatalf("a job after single spelling %q: %v", tc.name, err)
			}
			if _, err := s.SubmitJob(boundProveJob("jb", "b", 1, "")); !errors.Is(err, ErrSetupMissing) {
				t.Fatalf("b job after single spelling %q: want ErrSetupMissing, got %v", tc.name, err)
			}
		})
	}
}

// The name value keeps being matched exactly as written: a single spelling
// whose value differs only by letter case or surrounding spaces names no
// committed circuit and fails the integrity validation, exactly as before —
// "a" and "A" are never merged and no whitespace is trimmed.
func TestSetupNameValueMatchingStillExact(t *testing.T) {
	cases := []struct {
		name string
		frag string
	}{
		{"case differs", `"name": "A"`},
		{"trailing space", `"name": "a "`},
		{"leading space", `"name": " a"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedTwoNamedFrozenSetupStore(t)
			replaceInSetup(t, dir, setupNameLine("a"), tc.frag)
			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("%s: want ErrDataCorrupt, got %v", tc.name, err)
			}
		})
	}
}

// The same name used by separate setup records is not a duplicate: the rule is
// per record. a@1 and b@1 each carry their own single "name" and both load.
func TestSetupNameRepeatedAcrossRecordsIsFine(t *testing.T) {
	dir := seedTwoNamedFrozenSetupStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.RecordSetup("b", 1); err != nil {
		t.Fatalf("recording b's setup: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("two setups each naming their own circuit must load: %v", err)
	}
	defer s2.Close()
	if _, found, err := s2.GetSetup("a", 1); err != nil || !found {
		t.Fatalf("a setup: found=%v err=%v", found, err)
	}
	if _, found, err := s2.GetSetup("b", 1); err != nil || !found {
		t.Fatalf("b setup: found=%v err=%v", found, err)
	}
}

// A damaged record refuses the whole read even when another record in the same
// setups array is perfectly legal: there is no partial success. The second
// record is the one carrying two names, so its 1-based position is reported.
func TestSetupNameDuplicateOnSecondRecordNamesPosition(t *testing.T) {
	dir := seedTwoNamedFrozenSetupStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSetup("b", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Records are sorted by (name, version) on commit, so b@1 is record #2.
	// Scope the replacement to after the setups marker so the shared circuit
	// "name" fields are never touched.
	raw, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	text := string(raw)
	marker := `"setups": [`
	at := strings.Index(text, marker)
	if at < 0 {
		t.Fatalf("committed file does not carry the setups array")
	}
	rel := strings.Index(text[at:], setupNameLine("b"))
	if rel < 0 {
		t.Fatalf("setups array does not carry the b record name line")
	}
	pos := at + rel
	bad := text[:pos] + `"name": "b", "NAME": "a"` + text[pos+len(setupNameLine("b")):]
	want := []byte(bad)
	writeDataFile(t, dir, want)

	_, err = Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	if !strings.Contains(err.Error(), "setup record #2") {
		t.Fatalf("error does not locate setup record #2: %v", err)
	}
	got, rerr2 := readDataFile(t, dir)
	if rerr2 != nil {
		t.Fatal(rerr2)
	}
	if string(got) != string(want) {
		t.Fatalf("open modified the damaged file")
	}
}

// Damage appearing after the directory is open is caught by the very next
// operation, whether a read or a write: queries return no setup, neither a
// setup registration nor a job submission succeeds or commits, and the file is
// never rewritten.
func TestOpenStoreRefusesAfterSetupNameAmbiguity(t *testing.T) {
	dir := seedTwoNamedFrozenSetupStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bad := replaceInSetup(t, dir, setupNameLine("a"), `"name": "a", "NAME": "b"`)

	if _, found, err := s.GetSetup("a", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetSetup(a) after ambiguity: found=%v want ErrDataCorrupt, got %v", found, err)
	}
	if _, found, err := s.GetSetup("b", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetSetup(b) after ambiguity must not partially succeed: found=%v err=%v", found, err)
	}
	if _, err := s.RecordSetup("b", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("RecordSetup after ambiguity: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.SubmitJob(boundProveJob("j-b", "b", 1, "")); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("SubmitJob for b after ambiguity: want ErrDataCorrupt, got %v", err)
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
