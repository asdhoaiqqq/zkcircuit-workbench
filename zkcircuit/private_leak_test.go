package zkcircuit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the privacy contract of input checking: when a private
// input value (or the private section of an input file) is malformed, the
// returned error must keep an actionable, specific reason — the 1-based
// private-input position, the public/private group and the required format —
// without ever quoting a private value, a character or fragment taken from
// one, an object key inside a private element, or raw document text.

// assertNoLeak fails if msg carries any of the secret tokens planted in the
// private section.
func assertNoLeak(t *testing.T, msg string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(msg, secret) {
			t.Fatalf("diagnostic leaked private token %q: %s", secret, msg)
		}
	}
}

func TestParseWitnessPrivateDecimalDiagnostics(t *testing.T) {
	const secret = "ZXQWH"
	cases := []struct {
		name string
		doc  string
		want string // required diagnostic substring
	}{
		{"illegal character", `{"public":[],"private":["123` + secret + `"]}`, "private input #1 is not a decimal integer"},
		{"empty string", `{"public":[],"private":[""]}`, "the string is empty"},
		{"minus only", `{"public":[],"private":["-"]}`, "minus sign without digits"},
		{"leading plus", `{"public":[],"private":["+` + secret + `"]}`, "private input #1 is not a decimal integer"},
		{"decimal dot", `{"public":[],"private":["1.` + secret + `"]}`, "private input #1 is not a decimal integer"},
		{"position is 1-based", `{"public":[],"private":["9","-` + secret + `"]}`, "private input #2 is not a decimal integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			priv := 1
			if tc.name == "position is 1-based" {
				priv = 2
			}
			_, err := parseWitness([]byte(tc.doc), 0, priv)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("want input format error, got %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("diagnostic %q must say %q", msg, tc.want)
			}
			assertNoLeak(t, msg, secret, "123", "1.")
		})
	}
}

func TestParseWitnessPrivateElementType(t *testing.T) {
	cases := []struct {
		name   string
		doc    string
		secret string
		want   string
	}{
		{"number", `{"public":[],"private":[424242]}`, "424242", "not a number"},
		{"object", `{"public":[],"private":[{"KXQWH":1}]}`, "KXQWH", "not an object"},
		{"object duplicate key", `{"public":[],"private":[{"KXQWH":1,"KXQWH":2}]}`, "KXQWH", "duplicate key"},
		{"nested object duplicate", `{"public":[],"private":[["X",[{"KXQWH":1,"KXQWH":2}]]]}`, "KXQWH", "duplicate key"},
		{"boolean", `{"public":[],"private":[true]}`, "", "not a boolean"},
		{"null", `{"public":[],"private":[null]}`, "", "not null"},
		{"nested array", `{"public":[],"private":[["VXQWH"]]}`, "VXQWH", "not an array"},
		{"second element", `{"public":[],"private":["1",424242]}`, "424242", "private input #2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			priv := 1
			if tc.name == "second element" {
				priv = 2
			}
			_, err := parseWitness([]byte(tc.doc), 0, priv)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("want input format error, got %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "private input #") || !strings.Contains(msg, tc.want) {
				t.Fatalf("diagnostic %q must name private position and reason %q", msg, tc.want)
			}
			if tc.secret != "" {
				assertNoLeak(t, msg, tc.secret)
			}
		})
	}
}

func TestParseWitnessPrivateSyntaxAttribution(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"number with garbage", `{"public":[],"private":[123QWH]}`},
		{"bad escape", `{"public":[],"private":["a\qZWH"]}`},
		{"unterminated string", `{"public":[],"private":["ZWH`},
		{"bare word", `{"public":[],"private":ZWH}`},
		{"bad literal", `{"public":[],"private":[truZWH]}`},
		{"missing comma", `{"public":[],"private":[{"aZWH":1 "bZWH":2}]}`},
		{"leading zero number", `{"public":[],"private":[01]}`},
		{"double fraction", `{"public":[],"private":[1.2.3]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseWitness([]byte(tc.doc), 0, 1)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("want input format error, got %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "private input group") {
				t.Fatalf("syntax failure must be attributed to the private group, got: %s", msg)
			}
			assertNoLeak(t, msg, "ZWH", "qZ", "aZ", "bZ", "tru", "123")
		})
	}

	// Damage that cannot be reliably placed in one group reports a document
	// format error without quoting any of the bytes the parser met.
	for _, doc := range []string{
		`{QSYNTAX`,
		`{"public" QCOLON`,
		`{"public":[],"private":[]} QTRAIL`,
		`{"public":[] QPRIVATE`,
	} {
		_, err := parseWitness([]byte(doc), 0, 1)
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("corrupt doc %q: want input format error, got %v", doc, err)
		}
		msg := err.Error()
		if !strings.Contains(msg, "input document is malformed") {
			t.Fatalf("corrupt doc %q: want document-wide malformed reason, got %s", doc, msg)
		}
		assertNoLeak(t, msg, "QSYNTAX", "QCOLON", "QTRAIL", "QPRIVATE")
	}
}

func TestParseWitnessStructuralReasons(t *testing.T) {
	cases := []struct {
		name      string
		doc       string
		pub, priv int
		wantSub   string
		wantFmt   bool // diagnostic should state the required "decimal string" form
	}{
		{"missing public", `{"private":[]}`, 1, 1, "missing the \"public\" group", true},
		{"missing private", `{"public":[]}`, 1, 1, "missing the \"private\" group", true},
		{"public length", `{"public":["1"],"private":["1"]}`, 2, 1, "public input length mismatch", true},
		{"private length", `{"public":["1"],"private":["1"]}`, 1, 2, "private input length mismatch", true},
		{"public null", `{"public":null,"private":[]}`, 0, 0, "\"public\" must be an array", true},
		{"private null", `{"public":[],"private":null}`, 0, 0, "\"private\" must be an array", true},
		{"public not array", `{"public":1,"private":[]}`, 0, 0, "\"public\" must be an array", true},
		{"private not array", `{"public":[],"private":1}`, 0, 0, "\"private\" must be an array", true},
		{"duplicate private group", `{"public":[],"private":[],"private":[]}`, 0, 0, "\"private\" group more than once", false},
		{"unknown field", `{"public":[],"private":[],"extra":1}`, 0, 0, "unknown field", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseWitness([]byte(tc.doc), tc.pub, tc.priv)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("want input format error, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("diagnostic %q must contain %q", err.Error(), tc.wantSub)
			}
			if tc.wantFmt && !strings.Contains(err.Error(), "decimal string") {
				t.Fatalf("structural diagnostic must state the required format: %s", err.Error())
			}
		})
	}
}

func TestParseWitnessPublicKeepsDetailedDiagnostics(t *testing.T) {
	// Public diagnostics remain detailed: the value and the offending
	// character may be quoted; only the private section is sanitized.
	_, err := parseWitness([]byte(`{"public":["9Q"],"private":[]}`), 1, 0)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want input format error, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "public input #1") || !strings.Contains(msg, `"9Q"`) {
		t.Fatalf("public value detail should be retained: %s", msg)
	}
}

// TestCheckInputPrivateValueNeverLeakedThroughAPI covers the in-API entry
// point: a malformed private string in a Witness reaches the caller with a
// positional, sanitized ErrInvalidInput.
func TestCheckInputPrivateValueNeverLeakedThroughAPI(t *testing.T) {
	s := openTestStore(t)
	seedDraftWithDef(t, s, "c", 1, 1, 2, validDef)
	s.FreezeCircuit("c", 1)
	a, _ := s.CompileCircuit("c", 1)

	const secret = "SECRV"
	w := Witness{Public: []string{"2"}, Private: []string{"3", "123" + secret}}
	_, err := s.CheckInput("c", 1, a.Hash, w)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want input format error, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "private input #2") {
		t.Fatalf("must identify the 1-based private input: %s", msg)
	}
	assertNoLeak(t, msg, secret)
}

// TestCheckInputFilePrivateNeverLeakedThroughFile covers the file entry
// point end to end: secrets planted in the private section never appear in
// the error, while the reason stays specific.
func TestCheckInputFilePrivateNeverLeakedThroughFile(t *testing.T) {
	s := openTestStore(t)
	seedDraftWithDef(t, s, "c", 1, 1, 1, validDef)
	s.FreezeCircuit("c", 1)
	a, _ := s.CompileCircuit("c", 1)

	const secret = "SECRF"
	docs := map[string]string{
		"bad decimal":  `{"public":["2"],"private":["123` + secret + `"]}`,
		"number elem":  `{"public":["2"],"private":[728364]}`,
		"object elem":  `{"public":["2"],"private":[{"` + secret + `":1,"` + secret + `":2}]}`,
		"private junk": `{"public":["2"],"private":` + secret + `}`,
		"empty":        `{"public":["2"],"private":[""]}`,
		"minus":        `{"public":["2"],"private":["-"]}`,
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input.json")
			if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := s.CheckInputFile("c", 1, a.Hash, path)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("want input format error, got %v", err)
			}
			assertNoLeak(t, err.Error(), secret)
			if name == "number elem" {
				assertNoLeak(t, err.Error(), "728364")
			}
		})
	}
}

// TestCheckInputFileBindingPrecedenceUnchanged verifies that a readable but
// bad file is still gated by version/artifact checks: the binding reasons
// take precedence over file-content complaints, and an unreadable file stays
// a read-style input format error.
func TestCheckInputFilePrivateSanitizationPrecedence(t *testing.T) {
	s := openTestStore(t)
	seedDraftWithDef(t, s, "c", 1, 1, 1, validDef)
	s.FreezeCircuit("c", 1)
	a, _ := s.CompileCircuit("c", 1)
	bad := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(bad, []byte(`{"public":["2"],"private":["SECRP"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CheckInputFile("c", 1, a.Hash+"00", bad); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("artifact mismatch must beat malformed file: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "absent.json")
	_, err := s.CheckInputFile("c", 1, a.Hash, missing)
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "cannot read input file") {
		t.Fatalf("unreadable file must keep its read error, got %v", err)
	}
}
