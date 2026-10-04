package zkcircuit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Regression tests for a non-ASCII lookalike spelling of one of data.json's
// top-level fields. encoding/json matches struct tags with Unicode case
// folding, under which "circuitſ", "setupſ", "jobſ" and "artifactſ"
// (U+017F long s, written directly or as the JSON escape u017f) name the
// canonical fields exactly like their standard spellings. Only the standard
// spelling and ASCII letter-case variants are accepted, so such a lookalike
// is data corruption even when it appears alone: a later "circuitſ": []
// would otherwise override a populated circuits array through
// last-value-wins and the records would vanish on the next commit. The rule
// holds whatever the value is (legal records, an empty array or null), in
// either key order beside the standard spelling or an ASCII case variant,
// and whether the two values agree.

// envLongS is U+017F (Latin small letter long s). Each of the four
// array-valued envelope fields ends in an ASCII 's' it can stand in for.
const envLongS = "ſ"

// envJSONEscape is one literal backslash, so an escaped spelling written in a
// test keeps its backslash in the source.
const envJSONEscape = `\`

// longSSpelling is the long-s lookalike of an s-ending field: "circuits" →
// "circuitſ".
func longSSpelling(field string) string {
	return field[:len(field)-1] + envLongS
}

// longSKeyToken is the full JSON key token (quotes included) naming field
// under its long-s spelling, written directly or as the u017f escape. The
// escaped token is assembled literally (a backslash between the quotes)
// rather than passed through strconv.Quote, which would double the backslash
// and make JSON read it as a literal "ſ" instead of the long-s rune.
func longSKeyToken(field string, escaped bool) string {
	if escaped {
		return `"` + field[:len(field)-1] + envJSONEscape + "u017f" + `"`
	}
	return strconv.Quote(longSSpelling(field))
}

// requireLookalikeCorrupt opens dir, requires an ErrDataCorrupt failure that
// names both the canonical field and the actual (unescaped) long-s spelling,
// requires data.json to stay byte-for-byte, and requires a reopen to fail the
// same way.
func requireLookalikeCorrupt(t *testing.T, dir string, bad []byte, canonical string) {
	t.Helper()
	actual := longSSpelling(canonical)
	s, err := Open(dir)
	if err == nil {
		s.Close()
		t.Fatalf("long-s spelling of %q was accepted", canonical)
	}
	var se StoreError
	if !errors.As(err, &se) || se.Kind != ErrDataCorrupt.Kind {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"non-ASCII spelling", strconv.Quote(canonical), strconv.Quote(actual)} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not contain %q", msg, want)
		}
	}
	left, rerr := os.ReadFile(filepath.Join(dir, dirDataFile))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(left) != string(bad) {
		t.Fatalf("refused read modified data.json\nwant: %q\n got: %q", bad, left)
	}
	if s2, err := Open(dir); err == nil {
		s2.Close()
		t.Fatalf("reopen accepted the long-s spelling of %q", canonical)
	} else if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("reopen: want ErrDataCorrupt, got %v", err)
	}
}

// renameTopKey returns kvs with the member whose decoded key is field renamed
// to token and, when val is non-nil, given that value.
func renameTopKey(kvs []envKv, field, token string, val json.RawMessage) []envKv {
	out := make([]envKv, 0, len(kvs))
	for _, kv := range kvs {
		var k string
		if err := json.Unmarshal([]byte(kv.keyToken), &k); err != nil {
			panic(err)
		}
		if k == field {
			kv.keyToken = token
			if val != nil {
				kv.val = val
			}
		}
		out = append(out, kv)
	}
	return out
}

// TestTopLevelLongSLookalikeAloneIsCorrupt is the headline matrix: the
// canonical member removed and replaced by a lone long-s spelling — direct
// or u017f-escaped — is refused for every one of the four fields whether its
// value is the field's real records, an empty array or null. A lone
// lookalike carrying legal records must not be ignored as an unknown member
// (which would read every list as empty), and []/null must not read as
// "no records".
func TestTopLevelLongSLookalikeAloneIsCorrupt(t *testing.T) {
	fields := []string{"circuits", "setups", "jobs", "artifacts"}
	shapes := []struct {
		name string
		val  func(seeded json.RawMessage) json.RawMessage
	}{
		{"legal records", func(v json.RawMessage) json.RawMessage { return v }},
		{"empty array", func(json.RawMessage) json.RawMessage { return json.RawMessage("[]") }},
		{"null", func(json.RawMessage) json.RawMessage { return json.RawMessage("null") }},
	}
	for _, field := range fields {
		for _, escaped := range []bool{false, true} {
			for _, sh := range shapes {
				t.Run(fmt.Sprintf("%s/%s/escaped=%v", field, sh.name, escaped), func(t *testing.T) {
					dir, _ := seedBoundStore(t)
					vals := envelopeValues(readOrderedEnvelope(t, dir))
					kvs := renameTopKey(readOrderedEnvelope(t, dir), field,
						longSKeyToken(field, escaped), sh.val(vals[field]))
					bad := writeEnvelope(t, dir, kvs)
					requireLookalikeCorrupt(t, dir, bad, field)
				})
			}
		}
	}
}

// TestTopLevelLongSLookalikeBesideAcceptedSpellingIsCorrupt: a long-s
// spelling next to the standard key or the upper-case ASCII variant is
// refused in either position, whether the lookalike carries the same real
// value, an empty array or null. The read may never choose one value.
func TestTopLevelLongSLookalikeBesideAcceptedSpellingIsCorrupt(t *testing.T) {
	fields := []string{"circuits", "setups", "jobs", "artifacts"}
	values := []struct {
		name string
		val  func(seeded json.RawMessage) json.RawMessage
	}{
		{"same value", func(v json.RawMessage) json.RawMessage { return v }},
		{"empty array", func(json.RawMessage) json.RawMessage { return json.RawMessage("[]") }},
		{"null", func(json.RawMessage) json.RawMessage { return json.RawMessage("null") }},
	}
	for _, field := range fields {
		for _, accepted := range []string{"canonical", "upper"} {
			for _, escaped := range []bool{false, true} {
				for _, before := range []bool{false, true} {
					for _, vsh := range values {
						name := fmt.Sprintf("%s/%s/escaped=%v/before=%v/%s",
							field, accepted, escaped, before, vsh.name)
						t.Run(name, func(t *testing.T) {
							dir, _ := seedBoundStore(t)
							vals := envelopeValues(readOrderedEnvelope(t, dir))
							acceptedToken := strconv.Quote(field)
							if accepted == "upper" {
								acceptedToken = strconv.Quote(strings.ToUpper(field))
							}
							base := renameTopKey(readOrderedEnvelope(t, dir), field,
								acceptedToken, nil)
							lookalike := envKv{keyToken: longSKeyToken(field, escaped),
								val: vsh.val(vals[field])}
							var kvs []envKv
							if before {
								kvs = append([]envKv{lookalike}, base...)
							} else {
								kvs = append(append([]envKv{}, base...), lookalike)
							}
							bad := writeEnvelope(t, dir, kvs)
							requireLookalikeCorrupt(t, dir, bad, field)
						})
					}
				}
			}
		}
	}
}

// TestTopLevelLongSEmptyArrayCannotHideRecords pins the exact reported
// failure: legal circuits committed first, a trailing "circuitſ": [] added
// afterward must not read the circuits as empty, and a subsequent
// CreateCircuit must not commit the records away.
func TestTopLevelLongSEmptyArrayCannotHideRecords(t *testing.T) {
	dir, _ := seedBoundStore(t)
	kvs := append(readOrderedEnvelope(t, dir),
		envKv{keyToken: longSKeyToken("circuits", false), val: json.RawMessage("[]")})
	bad := writeEnvelope(t, dir, kvs)

	s, err := Open(dir)
	if err == nil {
		s.Close()
		t.Fatalf("trailing circuitſ empty array was accepted")
	}
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}

	// The same tamper discovered on an already-open store fails closed: no
	// partial listing, and no mutation commits.
	dir2, h := seedBoundStore(t)
	open, err := Open(dir2)
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	writeEnvelope(t, dir2, append(readOrderedEnvelope(t, dir2),
		envKv{keyToken: longSKeyToken("circuits", true), val: json.RawMessage("[]")}))

	if list, err := open.ListCircuits(); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("ListCircuits after tamper: want ErrDataCorrupt, got %d records err=%v",
			len(list), err)
	}
	if _, err := open.GetCircuit("mul", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetCircuit after tamper: want ErrDataCorrupt, got %v", err)
	}
	if _, err := open.CreateCircuit(Circuit{Name: "new", Version: 2, Constraints: 1,
		Description: "d"}); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("CreateCircuit after tamper: want ErrDataCorrupt, got %v", err)
	}
	if _, err := open.FreezeCircuit("mul", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("FreezeCircuit after tamper: want ErrDataCorrupt, got %v", err)
	}
	if _, err := open.SubmitJob(boundProveJob("j2", "mul", 1, h)); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("SubmitJob after tamper: want ErrDataCorrupt, got %v", err)
	}
	left, err := os.ReadFile(filepath.Join(dir2, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(left), longSSpelling("circuits")) {
		t.Fatalf("expected the pristine seed file, found the long-s tamper on disk")
	}
	if _, err := os.Stat(filepath.Join(dir2, dirTmpFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused mutation left a staging file behind: %v", err)
	}

	// dir's damaged file is exactly the bytes we wrote.
	left, err = os.ReadFile(filepath.Join(dir, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(bad) {
		t.Fatalf("damaged data.json was rewritten")
	}
}

// TestTopLevelLongSLeavesGenuinelyUnknownFieldsAlone documents the boundary:
// "cİrcuits" (U+0130 dotted capital I) and "note" do not fold onto a known
// field and stay ordinary unknown members, while "circuitſ" (U+017F) folds
// and is refused. The two must not be conflated.
func TestTopLevelLongSLeavesGenuinelyUnknownFieldsAlone(t *testing.T) {
	dir, _ := seedBoundStore(t)
	kvs := append(readOrderedEnvelope(t, dir),
		envKv{keyToken: strconv.Quote("note"), val: json.RawMessage("1")},
		envKv{keyToken: strconv.Quote("cİrcuits"), val: json.RawMessage("[]")})
	writeEnvelope(t, dir, kvs)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("genuinely unknown fields rejected: %v", err)
	}
	if list, err := s.ListCircuits(); err != nil || len(list) != 1 {
		t.Fatalf("real circuits swallowed by unknown keys: %+v %v", list, err)
	}
	s.Close()

	// The same envelope with the dotted-I key replaced by long s is corrupt,
	// proving only the folding lookalike is caught.
	dir2, _ := seedBoundStore(t)
	bad := writeEnvelope(t, dir2, append(readOrderedEnvelope(t, dir2),
		envKv{keyToken: strconv.Quote("note"), val: json.RawMessage("1")},
		envKv{keyToken: longSKeyToken("circuits", false), val: json.RawMessage("[]")}))
	if _, err := Open(dir2); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("long-s spelling beside note: want ErrDataCorrupt, got %v", err)
	}
	left, _ := os.ReadFile(filepath.Join(dir2, dirDataFile))
	if string(left) != string(bad) {
		t.Fatalf("data.json modified after refused read")
	}
}

// TestEnvelopeFieldLookalikeBoundary is the focused unit check: ASCII case
// variants and canonical names are accepted spellings (never lookalikes),
// long-s spellings map to their canonical field, and non-folding names map to
// nothing.
func TestEnvelopeFieldLookalikeBoundary(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want string
	}{
		{"circuits", ""}, {"CIRCUITS", ""}, {"CiRcUiTs", ""},
		{"setups", ""}, {"JOBS", ""}, {"Artifacts", ""},
		{longSSpelling("circuits"), "circuits"},
		{longSSpelling("setups"), "setups"},
		{longSSpelling("jobs"), "jobs"},
		{longSSpelling("artifacts"), "artifacts"},
		{"cİrcuits", ""}, // U+0130, does not fold
		{"note", ""}, {"CIRCUIT", ""}, {"", ""},
	} {
		if got := envelopeFieldLookalike(tc.key); got != tc.want {
			t.Errorf("envelopeFieldLookalike(%q) = %q, want %q", tc.key, got, tc.want)
		}
		if canonicalEnvelopeField(tc.key) != "" && envelopeFieldLookalike(tc.key) != "" {
			t.Errorf("accepted ASCII spelling %q reported as a lookalike", tc.key)
		}
	}
}
