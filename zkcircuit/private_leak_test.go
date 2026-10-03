package zkcircuit

import (
	"errors"
	"strings"
	"testing"
)

// These tests pin the privacy contract of input checking: when a check is
// rejected because of malformed PRIVATE input, the error must still say what
// is wrong and where (private group, 1-based index, required shape) but must
// never quote a private value, a character or fragment taken from one, an
// object key found inside the private array, or the raw bytes the parser met.
// Public input keeps its detailed diagnostics. Every such rejection stays in
// the existing input-format-error category (never "constraint unsatisfied").

// setupCheckStore seeds a frozen, compiled version with the given input
// counts and returns the store and its bound artifact hash.
func setupCheckStore(t *testing.T, name string, pub, priv int) (*Store, string) {
	t.Helper()
	s := openTestStore(t)
	seedDraftWithDef(t, s, name, 1, pub, priv, validDef)
	if _, err := s.FreezeCircuit(name, 1); err != nil {
		t.Fatal(err)
	}
	a, err := s.CompileCircuit(name, 1)
	if err != nil {
		t.Fatal(err)
	}
	return s, a.Hash
}

func TestPrivateFileErrorsNeverLeakValues(t *testing.T) {
	s, hash := setupCheckStore(t, "c", 1, 1)

	cases := []struct {
		name    string
		doc     string
		marker  string // distinctive private bytes that must never appear
		wantSub string // required, non-secret diagnostic content
	}{
		{"illegal character", `{"public":["2"],"private":["123ZZSECRETQ"]}`, "ZZSECRETQ",
			"private input #1 is not a decimal integer"},
		{"empty string", `{"public":["2"],"private":[""]}`, "",
			"private input #1 is not a decimal integer: empty string"},
		{"sign only", `{"public":["2"],"private":["-"]}`, "",
			"private input #1 is not a decimal integer: sign without digits"},
		{"object element with duplicate key", `{"public":["2"],"private":[{"ZZDUPKEY":1,"ZZDUPKEY":2}]}`, "ZZDUPKEY",
			"private input #1 must be a decimal string, not an object"},
		{"number element", `{"public":["2"],"private":[999983]}`, "999983",
			"private input #1 must be a decimal string, not a number"},
		{"float number element", `{"public":["2"],"private":[2.5]}`, "2.5",
			"private input #1 must be a decimal string, not a number"},
		{"negative number element", `{"public":["2"],"private":[-7]}`, "-7",
			"private input #1 must be a decimal string, not a number"},
		{"boolean element", `{"public":["2"],"private":[true]}`, "",
			"private input #1 must be a decimal string, not a boolean"},
		{"null element", `{"public":["2"],"private":[null]}`, "",
			"private input #1 must be a decimal string, not null"},
		{"nested array element", `{"public":["2"],"private":[["ZZFRAG"]]}`, "ZZFRAG",
			"private input #1 must be a decimal string, not an array"},
		{"syntax error in private array", `{"public":["2"],"private":["ZZSYNTAX" "x"]}`, "ZZSYNTAX",
			`input field "private" is not valid JSON`},
		{"truncated private section", `{"public":["2"],"private":["ZZTRUNC"`, "ZZTRUNC",
			`input field "private" is not valid JSON`},
		{"private group is null", `{"public":["2"],"private":null}`, "",
			`input field "private" must be an array of decimal strings`},
		{"private group is an object", `{"public":["2"],"private":{}}`, "",
			`input field "private" must be an array of decimal strings`},
		{"private group is a number", `{"public":["2"],"private":7}`, "",
			`input field "private" must be an array of decimal strings`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempJSON(t, tc.doc)
			_, err := s.CheckInputFile("c", 1, hash, path)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("want ErrInvalidInput, got %v", err)
			}
			msg := err.Error()
			if tc.marker != "" && strings.Contains(msg, tc.marker) {
				t.Errorf("diagnostic leaks private bytes %q:\n %s", tc.marker, msg)
			}
			if !strings.Contains(msg, tc.wantSub) {
				t.Errorf("diagnostic %q\n must contain %q", msg, tc.wantSub)
			}
			// Must never be mislabeled as a constraint failure.
			if strings.Contains(msg, "constraint") {
				t.Errorf("input-format failure must not be reported as a constraint failure: %s", msg)
			}
		})
	}
}

func TestPrivateErrorsAreIndexedFromOne(t *testing.T) {
	// Two private inputs so a failure in the second is located as #2.
	s, hash := setupCheckStore(t, "c", 1, 2)

	// File entry point.
	path := writeTempJSON(t, `{"public":["2"],"private":["3","9ZZIDXQ"]}`)
	_, err := s.CheckInputFile("c", 1, hash, path)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want input format error, got %v", err)
	}
	if !strings.Contains(err.Error(), "private input #2") || strings.Contains(err.Error(), "ZZIDXQ") {
		t.Fatalf("want indexed, non-leaking private error, got %v", err)
	}

	// In-API entry point gives the same indexed, non-leaking diagnostic.
	_, err = s.CheckInput("c", 1, hash, Witness{Public: []string{"2"}, Private: []string{"3", "9ZZAPIQ"}})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want input format error, got %v", err)
	}
	if !strings.Contains(err.Error(), "private input #2") || strings.Contains(err.Error(), "ZZAPIQ") {
		t.Fatalf("want indexed, non-leaking private error, got %v", err)
	}
}

func TestPrivateInAPIErrorsNeverLeakValues(t *testing.T) {
	s, hash := setupCheckStore(t, "c", 1, 1)
	for _, tc := range []struct {
		name   string
		value  string
		reason string
		marker string
	}{
		{"illegal character", "123ZZINAPIQ", "illegal character", "ZZINAPIQ"},
		{"empty", "", "empty string", ""},
		{"sign only", "-", "sign without digits", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CheckInput("c", 1, hash, Witness{Public: []string{"2"}, Private: []string{tc.value}})
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("want input format error, got %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "private input #1 is not a decimal integer: "+tc.reason) {
				t.Fatalf("want private #1 %s reason, got %v", tc.reason, msg)
			}
			if tc.marker != "" && strings.Contains(msg, tc.marker) {
				t.Fatalf("diagnostic leaks private value: %v", msg)
			}
		})
	}
}

func TestPublicDiagnosticsKeepDetail(t *testing.T) {
	s, hash := setupCheckStore(t, "c", 1, 1)

	// A bad PUBLIC decimal string is still quoted with its detailed reason.
	_, err := s.CheckInput("c", 1, hash, Witness{Public: []string{"1ZZPUBQ"}, Private: []string{"3"}})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want input format error, got %v", err)
	}
	if !strings.Contains(err.Error(), `public input #1 value "1ZZPUBQ"`) {
		t.Fatalf("public value should stay visible in the diagnostic: %v", err)
	}

	// File entry point retains detail for the public group: a well-formed
	// array holding a bad decimal string is quoted verbatim.
	path := writeTempJSON(t, `{"public":["1ZZPUBQ"],"private":["3"]}`)
	_, err = s.CheckInputFile("c", 1, hash, path)
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "ZZPUBQ") {
		t.Fatalf("public file diagnostic should keep detail, got %v", err)
	}
}

func TestWitnessArrayLayoutMessagesNameGroupAndShape(t *testing.T) {
	// Version declares 1 public and 1 private.
	s, hash := setupCheckStore(t, "c", 1, 1)

	lengthCases := []struct {
		name    string
		doc     string
		wantSub string
	}{
		{"private too many", `{"public":["2"],"private":["3","4"]}`,
			"private input array has 2 values but the version requires 1 decimal strings"},
		{"private too few", `{"public":["2"],"private":[]}`,
			"private input array has 0 values but the version requires 1 decimal strings"},
		{"public too many", `{"public":["2","8"],"private":["3"]}`,
			"public input array has 2 values but the version requires 1 decimal strings"},
		{"public too few", `{"public":[],"private":["3"]}`,
			"public input array has 0 values but the version requires 1 decimal strings"},
		{"missing private", `{"public":["2"]}`,
			`input is missing required field "private": the private group must be an array of decimal strings`},
		{"missing public", `{"private":["3"]}`,
			`input is missing required field "public": the public group must be an array of decimal strings`},
	}
	for _, tc := range lengthCases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempJSON(t, tc.doc)
			_, err := s.CheckInputFile("c", 1, hash, path)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("want input format error, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("diagnostic %q\n must contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestUnattributableDamageIsGeneric(t *testing.T) {
	s, hash := setupCheckStore(t, "c", 1, 1)
	for _, doc := range []string{
		`{ZZGARBAGE`,
		`{"public":["2"],"private":["3"]} ZZGARBAGE`,
		`{"public":["2", "private":["3"]}`,
	} {
		path := writeTempJSON(t, doc)
		_, err := s.CheckInputFile("c", 1, hash, path)
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("doc %q: want input format error, got %v", doc, err)
		}
		if strings.Contains(err.Error(), "ZZGARBAGE") {
			t.Fatalf("damaged document bytes must not be quoted: %v", err)
		}
	}
}

func TestLegalWitnessAfterChangesUnchanged(t *testing.T) {
	s, hash := setupCheckStore(t, "c", 1, 1)

	// Satisfying assignment (2*3 = 6 mod 7).
	res, err := s.CheckInput("c", 1, hash, Witness{Public: []string{"2"}, Private: []string{"3"}})
	if err != nil || !res.Satisfied || res.FirstFailure != 0 || res.Hash != hash {
		t.Fatalf("satisfied verdict changed: %+v %v", res, err)
	}
	// Unsatisfying assignment reports the first failing constraint.
	res, err = s.CheckInput("c", 1, hash, Witness{Public: []string{"2"}, Private: []string{"4"}})
	if err != nil || res.Satisfied || res.FirstFailure != 1 || res.Hash != hash {
		t.Fatalf("unsatisfied verdict changed: %+v %v", res, err)
	}
}
