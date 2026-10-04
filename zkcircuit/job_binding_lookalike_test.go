package zkcircuit

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// Regression tests for a non-ASCII lookalike spelling of the compiled-artifact
// binding field. encoding/json matches struct tags with Unicode case folding,
// under which "compiled_haſh" (U+017F long s, written directly or as the
// JSON escape u017f) fills CompiledHash exactly like "compiled_hash". The
// binding field only accepts the canonical spelling and its ASCII case
// variants, so such a lookalike is data corruption: it must not be ignored
// as an unknown member (which would read a bound job as unbound and drop the
// field on the next commit), whatever its value, even with a legal hash or
// an explicit "", and even beside a correctly spelled key in either order
// and with agreeing values.

// longS is U+017F (Latin small letter long s), the only non-ASCII rune whose
// fold set intersects compiled_hash.
const longS = "ſ"

// lookalikeKey is "compiled_haſh" with the long s.
var lookalikeKey = "compiled_ha" + longS + "h"

// jsonEscapePrefix is one literal backslash, used to spell uXXXX escapes in
// the raw files written below without the escapes being decoded in this
// source.
const jsonEscape = `\`

// escapedLookalikeKey is the same key written with the JSON escape u017f.
var escapedLookalikeKey = "compiled_ha" + jsonEscape + "u017f" + "h"

// escapedUnderKey is compiled_hash with the underscore escaped.
var escapedUnderKey = "compiled" + jsonEscape + "u005f" + "hash"

// rewriteBoundHashRaw replaces the committed j-bound binding field with an
// arbitrary raw JSON fragment, returning the damaged file content.
func rewriteBoundHashRaw(t *testing.T, dir, hash, fragment string) string {
	t.Helper()
	valid, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	field := `"compiled_hash": "` + hash + `"`
	if !strings.Contains(string(valid), field) {
		t.Fatalf("committed file does not carry the bound field %q", field)
	}
	bad := strings.Replace(string(valid), field, fragment, 1)
	writeDataFile(t, dir, []byte(bad))
	return bad
}

// assertRefusedAndUntouched opens the directory, requires the read to fail
// with ErrDataCorrupt naming j-bound and the binding field, and checks that
// the data file is still byte-for-byte bad.
func assertRefusedAndUntouched(t *testing.T, dir, bad string) {
	t.Helper()
	if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	got, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != bad {
		t.Fatalf("open modified the damaged file")
	}
}

// A lookalike spelling alone is refused for every value shape: null (the
// binding-damage case), a legal hash and an explicit empty string are all
// corruption, never an unbound job. Writing the long s directly or escaping
// it as u017f gives the same result.
func TestJobCompiledHashLookalikeSpellingIsCorrupt(t *testing.T) {
	literal := func(v string) func(string) string { return func(string) string { return v } }
	cases := []struct {
		name    string
		keyText string                // raw JSON key text as it appears between the quotes
		valueFn func(h string) string // raw JSON value text; h is the version's real artifact hash
	}{
		{"direct long s, null", lookalikeKey, literal("null")},
		{"direct long s, foreign hash", lookalikeKey, literal(`"deadbeef"`)},
		{"direct long s, real artifact hash", lookalikeKey, func(h string) string { return strconv.Quote(h) }},
		{"direct long s, empty string", lookalikeKey, literal(`""`)},
		{"escaped long s, null", escapedLookalikeKey, literal("null")},
		{"escaped long s, foreign hash", escapedLookalikeKey, literal(`"deadbeef"`)},
		{"escaped long s, real artifact hash", escapedLookalikeKey, func(h string) string { return strconv.Quote(h) }},
		{"escaped long s, empty string", escapedLookalikeKey, literal(`""`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, h := seedBoundJobStore(t)
			bad := rewriteBoundHashRaw(t, dir, h, `"`+tc.keyText+`": `+tc.valueFn(h))

			s, err := Open(dir)
			if err == nil {
				s.Close()
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), `"j-bound"`) {
				t.Fatalf("error does not name the damaged job: %v", err)
			}
			if !strings.Contains(err.Error(), "compiled_hash") {
				t.Fatalf("error does not name the binding field: %v", err)
			}
			assertRefusedAndUntouched(t, dir, bad)
		})
	}
}

// A lookalike beside the canonical spelling is always refused: in either key
// order and whether the two values agree — even when both are the version's
// real, valid artifact hash — the read never picks one value and carries on.
func TestJobCompiledHashLookalikeBesideCanonicalIsCorrupt(t *testing.T) {
	canon := func(value string) string { return `"compiled_hash": ` + value }
	alias := func(keyText, value string) string { return `"` + keyText + `": ` + value }

	type fragmentCase struct {
		name     string
		fragment func(h string) string
	}
	cases := []fragmentCase{
		{"canonical then direct lookalike, same value",
			func(h string) string { return canon(strconv.Quote(h)) + `, ` + alias(lookalikeKey, strconv.Quote(h)) }},
		{"canonical then direct lookalike, differing values",
			func(string) string { return canon(`"H"`) + `, ` + alias(lookalikeKey, "null") }},
		{"direct lookalike then canonical, same value",
			func(h string) string { return alias(lookalikeKey, strconv.Quote(h)) + `, ` + canon(strconv.Quote(h)) }},
		{"direct lookalike then canonical, differing values",
			func(string) string { return alias(lookalikeKey, "null") + `, ` + canon(`"H"`) }},
		{"canonical then escaped lookalike, same value",
			func(h string) string {
				return canon(strconv.Quote(h)) + `, ` + alias(escapedLookalikeKey, strconv.Quote(h))
			}},
		{"escaped lookalike then canonical, empty vs hash",
			func(h string) string { return alias(escapedLookalikeKey, `""`) + `, ` + canon(strconv.Quote(h)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, h := seedBoundJobStore(t)
			bad := rewriteBoundHashRaw(t, dir, h, tc.fragment(h))
			assertRefusedAndUntouched(t, dir, bad)
		})
	}
}

// A formerly unbound record that gains a lookalike key is just as corrupt:
// the unknown-looking key is not ignored into the normal unbound reading.
func TestJobCompiledHashLookalikeOnUnboundJobIsCorrupt(t *testing.T) {
	for _, keyText := range []string{lookalikeKey, escapedLookalikeKey} {
		dir, _ := seedBoundJobStore(t)
		valid, err := readDataFile(t, dir)
		if err != nil {
			t.Fatal(err)
		}
		marker := `"id": "j-plain"`
		if !strings.Contains(string(valid), marker) {
			t.Fatalf("committed file does not carry the unbound job marker %q", marker)
		}
		bad := strings.Replace(string(valid), marker,
			marker+`, "`+keyText+`": null`, 1)
		writeDataFile(t, dir, []byte(bad))

		s, err := Open(dir)
		if err == nil {
			s.Close()
		}
		if !errors.Is(err, ErrDataCorrupt) {
			t.Fatalf("lookalike %q on unbound job: want ErrDataCorrupt, got %v", keyText, err)
		}
		if !strings.Contains(err.Error(), `"j-plain"`) {
			t.Fatalf("error does not name the unbound job: %v", err)
		}
		got, rerr := readDataFile(t, dir)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if string(got) != bad {
			t.Fatalf("open modified the damaged file")
		}
	}
}

// Accepted ASCII spellings keep reading normally, including a JSON-escaped
// ASCII spelling: only non-ASCII lookalikes are refused.
func TestJobCompiledHashASCIICaseVariantsStillRead(t *testing.T) {
	cases := []struct {
		name    string
		keyText string
	}{
		{"upper case", "COMPILED_HASH"},
		{"mixed case", "CoMpIlEd_HaSh"},
		{"escaped underscore", escapedUnderKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, h := seedBoundJobStore(t)
			fragment := `"` + tc.keyText + `": "` + h + `"`
			rewriteBoundHashRaw(t, dir, h, fragment)

			s, err := Open(dir)
			if err != nil {
				t.Fatalf("ASCII spelling %q must load: %v", tc.keyText, err)
			}
			defer s.Close()
			got, err := s.GetJob("j-bound")
			if err != nil {
				t.Fatal(err)
			}
			if got.CompiledHash != h {
				t.Fatalf("binding under %q read as %q, want %q", tc.keyText, got.CompiledHash, h)
			}
		})
	}
}

// Damage appearing after the directory is open fails the next read and the
// next modification alike: no partial job list is shown, no new record is
// committed, and an unrelated circuit change cannot rewrite (and thereby
// silently clean up) the damaged file.
func TestOpenStoreRefusesAfterLookalikeBindingCorruption(t *testing.T) {
	dir, h := seedBoundJobStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bad := rewriteBoundHashRaw(t, dir, h, `"`+lookalikeKey+`": null`)

	if _, err := s.GetJob("j-bound"); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetJob after corruption: want ErrDataCorrupt, got %v", err)
	}
	if jobs, err := s.ListJobs(); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("ListJobs after corruption: want ErrDataCorrupt, got %d jobs err=%v", len(jobs), err)
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
