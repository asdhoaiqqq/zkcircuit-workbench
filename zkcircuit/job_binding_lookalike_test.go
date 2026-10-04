package zkcircuit

import (
	"errors"
	"strings"
	"testing"
)

// Regression tests for a non-ASCII spelling of the compiled-artifact binding
// field. encoding/json folds object keys onto struct tags with Unicode case
// folding, so "compiled_haſh" (long s, U+017F) lands on compiled_hash even
// though it is neither the standard spelling nor an ASCII case variant; a
// null there used to read as an unbound job and disappeared on the next
// commit. Such a spelling is damage to a binding the record claims to carry
// and is refused whatever its value and whether or not the standard spelling
// is also present.

// longS is U+017F LATIN SMALL LETTER LONG S as the literal rune;
// escapedLongS is the same letter written as a JSON string escape, so a key
// built from it carries the six ASCII bytes backslash-u-0-1-7-f on disk
// rather than the rune.
const (
	longS        = "ſ"
	escapedLongS = "\\u017f"
)

// rewriteBoundHashLookalike replaces j-bound's committed binding field with
// the given raw JSON member text (which names the field with a lookalike
// key), returning the damaged file content.
func rewriteBoundHashLookalike(t *testing.T, dir, hash, member string) string {
	t.Helper()
	valid, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	field := `"compiled_hash": "` + hash + `"`
	if !strings.Contains(string(valid), field) {
		t.Fatalf("committed file does not carry the bound field %q", field)
	}
	bad := strings.Replace(string(valid), field, member, 1)
	writeDataFile(t, dir, []byte(bad))
	return bad
}

// A non-ASCII spelling of compiled_hash is refused however it is written
// (literal rune or JSON escape) and whatever value it carries: null, a legal
// hash or an empty string must never be read as an unbound or bound job.
func TestJobCompiledHashLookalikeKeyIsCorrupt(t *testing.T) {
	cases := []struct {
		name   string
		member func(h string) string
	}{
		{"long s null", func(string) string { return `"compiled_ha` + longS + `h": null` }},
		{"escaped long s null", func(string) string { return `"compiled_ha` + escapedLongS + `h": null` }},
		{"long s legal hash", func(h string) string { return `"compiled_ha` + longS + `h": "` + h + `"` }},
		{"escaped long s legal hash", func(h string) string {
			return `"compiled_ha` + escapedLongS + `h": "` + h + `"`
		}},
		{"long s empty string", func(string) string { return `"compiled_ha` + longS + `h": ""` }},
		{"escaped long s empty string", func(string) string {
			return `"compiled_ha` + escapedLongS + `h": ""`
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, hash := seedBoundJobStore(t)
			bad := rewriteBoundHashLookalike(t, dir, hash, tc.member(hash))

			_, err := Open(dir)
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), `"j-bound"`) {
				t.Fatalf("error does not name the damaged job: %v", err)
			}
			if !strings.Contains(err.Error(), "compiled_hash") {
				t.Fatalf("error does not name the binding field: %v", err)
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

// When the lookalike spelling and the standard spelling coexist, the whole
// record is refused in whichever key order they appear and whether or not
// the two values agree; the decoder never picks one value and continues.
func TestJobCompiledHashLookalikeCoexistenceIsCorrupt(t *testing.T) {
	_, h := seedBoundJobStore(t)
	standard := func(v string) string { return `"compiled_hash": ` + v }
	literal := func(v string) string { return `"compiled_ha` + longS + `h": ` + v }
	escaped := func(v string) string { return `"compiled_ha` + escapedLongS + `h": ` + v }
	hashV := `"` + h + `"`
	otherV := `"deadbeef"`
	nullV := `null`
	cases := []struct {
		name   string
		member string
	}{
		{"standard then literal, equal hashes", standard(hashV) + ", " + literal(hashV)},
		{"literal then standard, equal hashes", literal(hashV) + ", " + standard(hashV)},
		{"standard then literal, differing values", standard(hashV) + ", " + literal(otherV)},
		{"literal then standard, differing values", literal(otherV) + ", " + standard(hashV)},
		{"standard then literal null", standard(hashV) + ", " + literal(nullV)},
		{"literal null then standard", literal(nullV) + ", " + standard(hashV)},
		// The JSON-escape spelling behaves identically to the literal rune.
		{"escaped null then standard", escaped(nullV) + ", " + standard(hashV)},
		{"standard then escaped null", standard(hashV) + ", " + escaped(nullV)},
		{"escaped hash then standard, equal", escaped(hashV) + ", " + standard(hashV)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, hash := seedBoundJobStore(t)
			bad := rewriteBoundHashLookalike(t, dir, hash, tc.member)

			s, err := Open(dir)
			if err == nil {
				s.Close()
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), `"j-bound"`) || !strings.Contains(err.Error(), "compiled_hash") {
				t.Fatalf("error does not name the damaged job and binding field: %v", err)
			}
			got, rerr := readDataFile(t, dir)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(got) != bad {
				t.Fatalf("open modified the damaged file")
			}
		})
	}
}

// ASCII letter-case spellings — including one written through a JSON escape
// of an ASCII letter or underscore — remain the accepted variant spellings
// and read the binding exactly.
func TestJobCompiledHashASCIIVariantsStillRead(t *testing.T) {
	cases := []struct {
		name   string
		member func(h string) string
	}{
		{"upper case", func(h string) string { return `"COMPILED_HASH": "` + h + `"` }},
		{"mixed case", func(h string) string { return `"CoMpIlEd_HaSh": "` + h + `"` }},
		{"escaped underscore", func(h string) string {
			return "\"compiled\\u005fhash\": \"" + h + `"`
		}},
		{"escaped ascii letter in upper case", func(h string) string {
			return "\"\\u0043OMPILED_HASH\": \"" + h + `"`
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, h := seedBoundJobStore(t)
			rewriteBoundHashLookalike(t, dir, h, tc.member(h))
			s, err := Open(dir)
			if err != nil {
				t.Fatalf("ASCII case spelling must still load: %v", err)
			}
			defer s.Close()
			got, err := s.GetJob("j-bound")
			if err != nil {
				t.Fatal(err)
			}
			if got.CompiledHash != h {
				t.Fatalf("binding lost through ASCII case spelling: got %q want %q", got.CompiledHash, h)
			}
			jobs, err := s.ListJobs()
			if err != nil || len(jobs) != 2 {
				t.Fatalf("list after ASCII spelling: %+v err=%v", jobs, err)
			}
		})
	}
}

// A non-ASCII key that does NOT fold onto compiled_hash keeps its ordinary
// compatibility: it is merely an unknown member, not the binding field. The
// fix tightens only the binding field's spellings.
func TestJobUnrelatedNonASCIIKeyStaysCompatible(t *testing.T) {
	dir, h := seedBoundJobStore(t)
	valid, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Add an unknown member carrying a Kelvin sign (U+212A) to the bound job
	// object; it folds onto nothing the job struct declares.
	needle := `"compiled_hash": "` + h + `"`
	withUnknown := strings.Replace(string(valid), needle,
		`"compiled_hash": "`+h+`", "extra_K": "x"`, 1)
	writeDataFile(t, dir, []byte(withUnknown))

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("unrelated non-ASCII unknown member must keep loading: %v", err)
	}
	defer s.Close()
	got, err := s.GetJob("j-bound")
	if err != nil || got.CompiledHash != h {
		t.Fatalf("bound job wrong after unrelated key: %+v err=%v", got, err)
	}
}

// Damage appearing after the directory was opened is caught by the very next
// read or modification: no partial job view, no committed replacement, and an
// unrelated circuit change must not rewrite the damaged file.
func TestOpenStoreRefusesAfterLookalikeCorruption(t *testing.T) {
	dir, h := seedBoundJobStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bad := rewriteBoundHashLookalike(t, dir, h, `"compiled_ha`+longS+`h": null`)

	if _, err := s.GetJob("j-bound"); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetJob after corruption: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.ListJobs(); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("ListJobs after corruption: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.SubmitJob(boundProveJob("j-new", "c", 1, h)); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("SubmitJob after corruption: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "other", Version: 1, Constraints: 1}); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("unrelated CreateCircuit after corruption: want ErrDataCorrupt, got %v", err)
	}
	got, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != bad {
		t.Fatalf("damaged file was rewritten by a refused operation")
	}
}
