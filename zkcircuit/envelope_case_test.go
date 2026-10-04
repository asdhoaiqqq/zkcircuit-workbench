package zkcircuit

import (
	"errors"
	"strings"
	"testing"
)

// Regression tests for case-variant duplicates of the envelope's top-level
// fields. format, circuits, setups, jobs and artifacts are recognized under
// any ASCII letter-case spelling (escaped or not), so the same field
// appearing twice — in any case combination, in any order, with equal or
// different values — is data corruption: the read must be refused rather
// than letting encoding/json's case-insensitive key matching merge the two
// values into whichever comes last.

// seedCaseDupStore commits one frozen circuit with a setup and returns the
// directory and its committed file contents.
func seedCaseDupStore(t *testing.T) (dir string, raw string) {
	t.Helper()
	dir = t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	readyCircuit(t, s, "c", 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, string(b)
}

// A top-level field repeated under a different letter-case spelling is one
// field carried twice: the directory read is refused as corruption, the
// error calls it a top-level duplicate and names the standard lowercase
// field, and the file is left byte-for-byte in place.
func TestTopLevelCaseVariantDuplicateIsCorrupt(t *testing.T) {
	cases := []struct {
		name    string
		replace func(raw string) string
		field   string
	}{
		{"circuits then CIRCUITS", func(raw string) string {
			return strings.Replace(raw, `"setups":`, `"CIRCUITS": [], "setups":`, 1)
		}, "circuits"},
		{"CIRCUITS then circuits", func(raw string) string {
			return strings.Replace(raw, `"circuits":`, `"CIRCUITS": [], "circuits":`, 1)
		}, "circuits"},
		{"circuits then escaped CIRCUITS", func(raw string) string {
			return strings.Replace(raw, `"setups":`, "\"\\u0043IRCUITS\": [], \"setups\":", 1)
		}, "circuits"},
		{"circuits then null Circuits", func(raw string) string {
			return strings.Replace(raw, `"setups":`, `"Circuits": null, "setups":`, 1)
		}, "circuits"},
		{"format then FORMAT", func(raw string) string {
			return strings.Replace(raw, `"format": 1`, `"format": 1, "FORMAT": 999`, 1)
		}, "format"},
		{"FORMAT then format", func(raw string) string {
			return strings.Replace(raw, `"format": 1`, `"FORMAT": 999, "format": 1`, 1)
		}, "format"},
		{"setups then SETUPS", func(raw string) string {
			return strings.Replace(raw, `"jobs":`, `"SETUPS": [], "jobs":`, 1)
		}, "setups"},
		{"jobs then Jobs", func(raw string) string {
			return strings.Replace(raw, `"jobs":`, `"jobs": [], "Jobs": [], "jobs":`, 1)
		}, "jobs"},
		{"artifacts then ARTIFACTS", func(raw string) string {
			return strings.Replace(raw, `"jobs":`, `"artifacts": [], "ARTIFACTS": [], "jobs":`, 1)
		}, "artifacts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, raw := seedCaseDupStore(t)
			bad := tc.replace(raw)
			if bad == raw {
				t.Fatalf("replacement did not apply to committed file:\n%s", raw)
			}
			writeDataFile(t, dir, []byte(bad))

			_, err := Open(dir)
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), "top-level") {
				t.Fatalf("error does not call out a top-level duplicate: %v", err)
			}
			if !strings.Contains(err.Error(), `"`+tc.field+`"`) {
				t.Fatalf("error does not name the standard field %q: %v", tc.field, err)
			}
			got, rerr := readDataFile(t, dir)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(got) != bad {
				t.Fatalf("open modified the damaged file")
			}
			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("reopen on damaged dir: want corruption, got %v", err)
			}
		})
	}
}

// A field repeated through a JSON-escaped spelling of a case variant is the
// same duplicate: keys are compared after unescaping and case folding.
func TestTopLevelEscapedCaseVariantDuplicateIsCorrupt(t *testing.T) {
	dir, raw := seedCaseDupStore(t)
	// CIRCUITS is the circuits field; "C" escapes to C.
	bad := strings.Replace(raw, `"setups":`, "\"\\u0043IRCUITS\": [], \"setups\":", 1)
	writeDataFile(t, dir, []byte(bad))

	_, err := Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	if !strings.Contains(err.Error(), `"circuits"`) {
		t.Fatalf("error does not name the standard field: %v", err)
	}
	got, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != bad {
		t.Fatalf("open modified the damaged file")
	}
}

// A full circuits array must not be rescued or emptied by a second spelling
// of the field: circuits carrying the committed record plus CIRCUITS as an
// empty array (or the reverse) refuses the read in both orders, and no
// record list is produced from either value.
func TestTopLevelCircuitsDuplicateDoesNotPickASide(t *testing.T) {
	for _, emptyFirst := range []bool{false, true} {
		dir, raw := seedCaseDupStore(t)
		var bad string
		if emptyFirst {
			bad = strings.Replace(raw, `"circuits":`, `"CIRCUITS": [], "circuits":`, 1)
		} else {
			bad = strings.Replace(raw, `"setups":`, `"CIRCUITS": [], "setups":`, 1)
		}
		writeDataFile(t, dir, []byte(bad))

		s, err := Open(dir)
		if !errors.Is(err, ErrDataCorrupt) {
			t.Fatalf("emptyFirst=%v: want ErrDataCorrupt, got %v", emptyFirst, err)
		}
		if s != nil {
			t.Fatalf("emptyFirst=%v: corrupt directory must not open", emptyFirst)
		}
	}
}

// An unsupported format number must not be masked by a later case-variant
// spelling of format, nor may a supported one mask an earlier unsupported
// one: both orders are duplicates and corrupt the read.
func TestTopLevelFormatDuplicateCannotMaskVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"supported then unsupported", `{"format":1,"FORMAT":999,"circuits":[],"setups":[],"jobs":[]}`},
		{"unsupported then supported", `{"FORMAT":999,"format":1,"circuits":[],"setups":[],"jobs":[]}`},
		{"escaped duplicate", `{"format":1,"\u0046ORMAT":1,"circuits":[],"setups":[],"jobs":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeDataFile(t, dir, []byte(tc.content))
			_, err := Open(dir)
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), `"format"`) {
				t.Fatalf("error does not name the standard field: %v", err)
			}
		})
	}
}

// A single occurrence of each field keeps reading under any letter-case
// spelling — standard lowercase, all caps, mixed case and escaped forms —
// with the committed records, setups and jobs intact.
func TestTopLevelCaseVariantSingleFieldsStillLoad(t *testing.T) {
	dir, raw := seedCaseDupStore(t)
	for _, from := range []string{`"format"`, `"circuits"`, `"setups"`, `"jobs"`} {
		if strings.Count(raw, from) != 1 {
			t.Fatalf("committed file must carry %s exactly once:\n%s", from, raw)
		}
	}
	renamed := strings.Replace(raw, `"format"`, `"FORMAT"`, 1)
	renamed = strings.Replace(renamed, `"circuits"`, `"Circuits"`, 1)
	renamed = strings.Replace(renamed, `"setups"`, `"SETUPS"`, 1)
	renamed = strings.Replace(renamed, `"jobs"`, `"Jobs"`, 1)
	writeDataFile(t, dir, []byte(renamed))

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("case-variant fields must load: %v", err)
	}
	defer s.Close()
	list, err := s.ListCircuits()
	if err != nil || len(list) != 1 || list[0].Name != "c" || !list[0].Frozen {
		t.Fatalf("committed circuit not read back: %+v %v", list, err)
	}
	if n := countSetups(t, s); n != 1 {
		t.Fatalf("committed setup not read back: %d", n)
	}
}

// An escaped spelling of a case-variant field name is the same field on a
// single occurrence too.
func TestTopLevelEscapedFieldSpellingLoads(t *testing.T) {
	dir, raw := seedCaseDupStore(t)
	// "circuits" is circuits; "format" is format.
	renamed := strings.Replace(raw, `"circuits"`, `"\u0063ircuits"`, 1)
	renamed = strings.Replace(renamed, `"format"`, `"\u0066ormat"`, 1)
	writeDataFile(t, dir, []byte(renamed))

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("escaped field spellings must load: %v", err)
	}
	defer s.Close()
	if list, err := s.ListCircuits(); err != nil || len(list) != 1 {
		t.Fatalf("committed circuit not read back: %+v %v", list, err)
	}
}

// Once a directory is open, a case-variant duplicate appearing on disk is
// caught by the very next operation: reads return no records and mutations
// commit nothing, leaving the damaged file byte-for-byte in place.
func TestOpenStoreRefusesAfterTopLevelCaseDuplicate(t *testing.T) {
	dir, raw := seedCaseDupStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bad := strings.Replace(raw, `"setups":`, `"CIRCUITS": [], "setups":`, 1)
	writeDataFile(t, dir, []byte(bad))

	if _, err := s.ListCircuits(); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("ListCircuits after corruption: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.GetCircuit("c", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetCircuit after corruption: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.ListJobs(); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("ListJobs after corruption: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "other", Version: 1, Constraints: 1}); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("CreateCircuit after corruption: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.FreezeCircuit("c", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("FreezeCircuit after corruption: want ErrDataCorrupt, got %v", err)
	}
	got, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != bad {
		t.Fatalf("damaged file was rewritten by a refused operation")
	}
}
