package zkcircuit

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// Regression tests for the owning circuit name on a committed trusted-setup
// record. A setup belongs exclusively to the frozen circuit named in the
// record; any spelling the ordinary read would fill Name from names that one
// field: the canonical "name", an ASCII letter-case variant such as "NAME" or
// "NaMe", or a JSON-escaped spelling of either. Two such spellings on one
// record make the owning circuit a function of key order (encoding/json keeps
// the last value) — with frozen a@1 and b@1 and a setup registered only for a,
// a record carrying "name":"a" and "NAME":"b" would read as b's setup, so b
// would query as having a setup it never registered and a proof job for b
// would pass the setup gate. Such a record is refused as data corruption
// instead: equal or differing values, whether the named circuit exists or is
// frozen, even with the version or another member interleaved, in either key
// order, and with only case variants present. A single recognized spelling
// keeps reading as it does today, and the value matches circuit names exactly
// (no case folding, no trimming).

// seedTwoFrozenNamedSetupStore commits frozen a@1 and b@1 with a trusted setup
// recorded on a only (b deliberately has none). It reproduces the ambiguity
// that matters: the one setup record spelling its circuit both as a and as b
// has a valid owner under either value, and the last spelling would hand b a
// setup it never registered.
func seedTwoFrozenNamedSetupStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if _, err := s.CreateCircuit(Circuit{Name: name, Version: 1, Constraints: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FreezeCircuit(name, 1); err != nil {
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
func setupNameLine(name string) string { return `"name": "` + name + `"` }

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
	if !strings.Contains(msg, "more than once") {
		t.Fatalf("error does not state the name field is repeated: %v", err)
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
// values, both circuits legal, in either key order, including two variants
// with no canonical spelling and two byte-identical keys. The error locates
// the setup record and names the circuit name field, and the file is left
// byte-for-byte in place.
func TestSetupNameDuplicateIsCorrupt(t *testing.T) {
	escN := jsonBackslash + "u006eame" // "name" as raw JSON text
	escM := "na" + jsonBackslash + "u006de"
	escUpperN := jsonBackslash + "u004eAME" // "NAME" as raw JSON text
	cases := []struct {
		name string
		frag string // replaces the single "name": "a" line in setup #1
	}{
		{"identical values, ASCII variant", `"name": "a", "NAME": "a"`},
		{"differing values, ASCII variant", `"name": "a", "NAME": "b"`},
		{"upper then canonical, differing", `"NAME": "b", "name": "a"`},
		{"two non-canonical variants, same value", `"NAME": "a", "Name": "a"`},
		{"two non-canonical variants, differing", `"NAME": "b", "NaMe": "a"`},
		{"mixed case variant, differing value", `"name": "a", "NaMe": "b"`},
		{"escaped leading letter, same value", `"name": "a", "` + escN + `": "a"`},
		{"escaped inner letter, differing", `"name": "a", "` + escM + `": "b"`},
		{"escaped uppercase variant", `"name": "a", "` + escUpperN + `": "b"`},
		{"byte-identical duplicate", `"name": "a", "name": "a"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedTwoFrozenNamedSetupStore(t)
			bad := replaceInSetup(t, dir, setupNameLine("a"), tc.frag)
			assertSetupNameCorrupt(t, dir, bad)
			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("reopen on ambiguous dir: want corruption, got %v", err)
			}
		})
	}
}

// The motivating ambiguity: a and b both frozen at v1, only a registered a
// setup, yet the setup record spells its name both ways. Without the
// uniqueness rule encoding/json keeps the last value and b reads as having
// a setup; the read must instead be refused.
func TestSetupNameAmbiguousBetweenTwoCircuits(t *testing.T) {
	dir := seedTwoFrozenNamedSetupStore(t)
	bad := replaceInSetup(t, dir, setupNameLine("a"), `"name": "a", "NAME": "b"`)
	assertSetupNameCorrupt(t, dir, bad)
}

// The two name spellings need not be adjacent: a second spelling later in the
// record, with the version (or another member) sitting between them, is still
// a duplicate and is rejected.
func TestSetupNameDuplicateInterspersedIsCorrupt(t *testing.T) {
	dir := seedTwoFrozenNamedSetupStore(t)
	old := setupNameLine("a") + "," + setupFieldIndent + `"version": 1`
	repl := setupNameLine("a") + "," + setupFieldIndent + `"version": 1,` +
		setupFieldIndent + `"NAME": "b"`
	bad := replaceInSetup(t, dir, old, repl)
	assertSetupNameCorrupt(t, dir, bad)
}

// The duplicate-name finding is independent of whether the second value names
// an existing or frozen circuit: it is key ambiguity, so it outranks every
// domain check. A circuit that is missing entirely and one that is still a
// draft both yield the same refusal.
func TestSetupNameDuplicateIndependentOfTargetCircuit(t *testing.T) {
	t.Run("second name names no circuit", func(t *testing.T) {
		dir := t.TempDir()
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateCircuit(Circuit{Name: "a", Version: 1, Constraints: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FreezeCircuit("a", 1); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RecordSetup("a", 1); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		bad := replaceInSetup(t, dir, setupNameLine("a"), `"name": "a", "NAME": "ghost"`)
		assertSetupNameCorrupt(t, dir, bad)
	})

	t.Run("second name names a non-frozen circuit", func(t *testing.T) {
		dir := t.TempDir()
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateCircuit(Circuit{Name: "a", Version: 1, Constraints: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateCircuit(Circuit{Name: "b", Version: 1, Constraints: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FreezeCircuit("a", 1); err != nil {
			t.Fatal(err)
		}
		// b stays a draft on purpose.
		if _, err := s.RecordSetup("a", 1); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		bad := replaceInSetup(t, dir, setupNameLine("a"), `"NAME": "b", "name": "a"`)
		assertSetupNameCorrupt(t, dir, bad)
	})
}

// When the record duplicates both owning fields, the name ambiguity surfaces
// first: the owning circuit must be settled before its version can matter.
func TestSetupNameDuplicateOutranksVersionDuplicate(t *testing.T) {
	dir := seedTwoFrozenNamedSetupStore(t)
	old := setupNameLine("a") + "," + setupFieldIndent + `"version": 1`
	repl := `"name": "a", "NAME": "b",` + setupFieldIndent + `"version": 1, "VERSION": 1`
	bad := replaceInSetup(t, dir, old, repl)

	_, err := Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	if !strings.Contains(err.Error(), "name") || strings.Contains(err.Error(), "version field") {
		t.Fatalf("want the name finding to surface first, got %v", err)
	}
	got, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(bad) {
		t.Fatalf("open modified the damaged file")
	}
}

// A single recognized name spelling keeps the current reading, including its
// ASCII case compatibility: the setup loads against the circuit named, direct
// or escaped, stays exclusive to that name+version, and gates the other
// circuit's jobs.
func TestSetupNameSingleSpellingStillReads(t *testing.T) {
	escN := jsonBackslash + "u006eame"
	escA := jsonBackslash + "u0041" // "A" inside the value string
	cases := []struct {
		name string
		frag string
	}{
		{"canonical", setupNameLine("a")},
		{"upper case", `"NAME": "a"`},
		{"mixed case", `"NaMe": "a"`},
		{"escaped letter", `"` + escN + `": "a"`},
		{"escaped uppercase key", `"` + jsonBackslash + `u004eAME": "a"`},
		// The value is matched exactly too: an escaped uppercase value decodes
		// to "A", which names no circuit.
		{"escaped uppercase value stays exact", `"name": "` + escA + `"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := seedTwoFrozenNamedSetupStore(t)
			replaceInSetup(t, dir, setupNameLine("a"), tc.frag)

			s, err := Open(dir)
			if tc.name == "escaped uppercase value stays exact" {
				// "A" is not "a": no case folding on the value, so the setup
				// belongs to no committed circuit and the read is refused,
				// exactly as a directly-written "A" would be.
				if !errors.Is(err, ErrDataCorrupt) {
					if s != nil {
						s.Close()
					}
					t.Fatalf("value %q must not fold onto \"a\": got %v", "A", err)
				}
				return
			}
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
			// An a@1 job passes the setup gate; b@1 still reports the setup
			// missing.
			if _, err := s.SubmitJob(boundProveJob("j-a", "a", 1, "")); err != nil {
				t.Fatalf("a job after single spelling %q: %v", tc.name, err)
			}
			if _, err := s.SubmitJob(boundProveJob("j-b", "b", 1, "")); !errors.Is(err, ErrSetupMissing) {
				t.Fatalf("b job after single spelling %q: want ErrSetupMissing, got %v", tc.name, err)
			}
		})
	}
}

// The name value keeps matching exactly as written for a lone spelling: a
// different letter case or surrounding spaces names no committed circuit and
// fails the integrity validation, exactly as before — "a" and "A" are never
// merged and no whitespace is trimmed.
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
			dir := seedTwoFrozenNamedSetupStore(t)
			replaceInSetup(t, dir, setupNameLine("a"), tc.frag)
			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("%s: want ErrDataCorrupt, got %v", tc.name, err)
			}
		})
	}
}

// The same name key appearing once on each of two different records is not a
// duplicate: setups for a@1 and b@1 each carry their own "name", and both
// read.
func TestSetupNameOncePerDifferentRecordsStillReads(t *testing.T) {
	dir := seedTwoFrozenNamedSetupStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSetup("b", 1); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatalf("two records each naming their own circuit must load: %v", err)
	}
	defer s.Close()
	if _, found, err := s.GetSetup("a", 1); err != nil || !found {
		t.Fatalf("a setup lost: found=%v err=%v", found, err)
	}
	if _, found, err := s.GetSetup("b", 1); err != nil || !found {
		t.Fatalf("b setup lost: found=%v err=%v", found, err)
	}
}

// A damaged record later in the setups array is located by its own 1-based
// position even when the records ahead of it are legal, and the legal records
// never produce partial results.
func TestSetupNameDuplicateOnSecondRecordNamesPosition(t *testing.T) {
	dir := seedTwoFrozenNamedSetupStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Add a second setup (b@1) through the normal API; the file then carries
	// two legal records.
	if _, err := s.RecordSetup("b", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Duplicate the name only on the second setup record ("b").
	raw, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	marker := `"setups": [`
	at := strings.Index(string(raw), marker)
	if at < 0 {
		t.Fatal("no setups marker")
	}
	tail := string(raw[at:])
	first := strings.Index(tail, setupNameLine("b"))
	if first < 0 {
		t.Fatal("second record name not found")
	}
	pos := at + first
	bad := string(raw[:pos]) + `"name": "b", "NAME": "b"` + string(raw[pos+len(setupNameLine("b")):])
	writeDataFile(t, dir, []byte(bad))

	_, err = Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	if !strings.Contains(err.Error(), "setup record #2") {
		t.Fatalf("error does not locate the second setup record: %v", err)
	}
	got, _ := readDataFile(t, dir)
	if string(got) != bad {
		t.Fatalf("open modified the damaged file")
	}
}

// Damage appearing after the directory is open is caught by the very next
// operation, whether a read or a write: neither circuit's setup query returns
// a result, neither a setup registration nor a job submission succeeds or
// commits, and the file is never rewritten — in particular b must neither
// query as having a setup nor accept a proof job.
func TestOpenStoreRefusesAfterSetupNameAmbiguity(t *testing.T) {
	dir := seedTwoFrozenNamedSetupStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bad := replaceInSetup(t, dir, setupNameLine("a"), `"name": "a", "NAME": "b"`)

	if _, found, err := s.GetSetup("a", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetSetup a after ambiguity: found=%v want ErrDataCorrupt, got %v", found, err)
	}
	if _, found, err := s.GetSetup("b", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetSetup b after ambiguity: found=%v want ErrDataCorrupt, got %v", found, err)
	}
	if _, err := s.RecordSetup("b", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("RecordSetup b after ambiguity: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.SubmitJob(boundProveJob("j-b", "b", 1, "")); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("SubmitJob b after ambiguity: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.SubmitJob(boundProveJob("j-a", "a", 1, "")); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("SubmitJob a after ambiguity: want ErrDataCorrupt, got %v", err)
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

// The typed decode of one setup record enforces the same rule on its own, the
// way it does during a directory open: a record with two name spellings is a
// StoreError of kind data corrupt whether or not the values agree, while a
// single canonical or case-variant spelling decodes to that exact name.
func TestPersistSetupDecodeRejectsDuplicateName(t *testing.T) {
	corruptCases := []string{
		`{"name":"a","NAME":"b","version":1}`,
		`{"NAME":"b","name":"a","version":1}`,
		`{"NAME":"a","NaMe":"a","version":1}`,
		`{"name":"a","name":"a","version":1}`,
	}
	for _, raw := range corruptCases {
		var p persistSetup
		if err := json.Unmarshal([]byte(raw), &p); !errors.Is(err, ErrDataCorrupt) {
			t.Fatalf("%s: want ErrDataCorrupt, got %v (p=%+v)", raw, err, p)
		}
	}

	for _, tc := range []struct {
		raw  string
		want string
	}{
		{`{"name":"a","version":1}`, "a"},
		{`{"NAME":"a","version":1}`, "a"},
		{`{"NaMe":"a","version":1}`, "a"},
	} {
		var p persistSetup
		if err := json.Unmarshal([]byte(tc.raw), &p); err != nil {
			t.Fatalf("%s: single spelling must decode: %v", tc.raw, err)
		}
		if p.Name != tc.want || p.Version != 1 {
			t.Fatalf("%s: decoded %+v, want name %q version 1", tc.raw, p, tc.want)
		}
	}
}

// Guard the message shape callers and the CLI match on: it names the record's
// array position, the canonical "name" field, and that it is a duplicate.
func TestSetupNameDuplicateErrorMessage(t *testing.T) {
	dir := seedTwoFrozenNamedSetupStore(t)
	replaceInSetup(t, dir, setupNameLine("a"), `"name": "a", "NAME": "b"`)
	_, err := Open(dir)
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{
		"setup record #1",
		"circuit name field " + strconv.Quote("name"),
		"more than once",
		"NAME",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err.Error(), want)
		}
	}
}
