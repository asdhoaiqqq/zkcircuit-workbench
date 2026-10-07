package zkcircuit

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Regression coverage for constraint import wire bounds: every term in the
// submitted file must reference a wire inside the draft's declared input
// layout, no matter what its coefficient reduces to. A zero coefficient, a
// coefficient that is a multiple of the modulus, or a pair of terms that
// cancel each other must never launder an out-of-range or negative wire into
// an accepted import. Conversely, on legal wires those same rewrites are
// semantically inert: they must import to the same constraints, compile to
// the same artifact hash and yield the same check verdicts as the definition
// with the terms removed.

// boundsBaseDef is the control definition used by the equivalence tests:
// modulus 7, one public and one private input (legal wires 0, 1, 2), two
// constraints — pub·priv = 6 and priv = pub + 1.
const boundsBaseDef = `{"modulus":"7","constraints":[
	{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]},
	{"a":[{"wire":2,"coeff":"1"}],"b":[{"wire":0,"coeff":"1"}],"c":[{"wire":1,"coeff":"1"},{"wire":0,"coeff":"1"}]}]}`

// boundsBaseVariants rewrites boundsBaseDef with zero coefficients,
// modulus-multiple coefficients and cancelling duplicate terms on legal
// wires only. Every variant must be accepted and must be semantically
// identical to the control.
var boundsBaseVariants = map[string]string{
	"zero coefficients on legal wires": `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"1"},{"wire":0,"coeff":"0"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"},{"wire":2,"coeff":"0"}]},
		{"a":[{"wire":2,"coeff":"1"}],"b":[{"wire":0,"coeff":"1"},{"wire":1,"coeff":"0"}],"c":[{"wire":1,"coeff":"1"},{"wire":0,"coeff":"1"},{"wire":2,"coeff":"0"}]}]}`,
	"modulus-multiple coefficients": `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"8"}],"b":[{"wire":2,"coeff":"1"},{"wire":0,"coeff":"7"}],"c":[{"wire":0,"coeff":"6"},{"wire":1,"coeff":"-14"}]},
		{"a":[{"wire":2,"coeff":"-6"}],"b":[{"wire":0,"coeff":"1"}],"c":[{"wire":1,"coeff":"1"},{"wire":0,"coeff":"8"},{"wire":2,"coeff":"21"}]}]}`,
	"cancelling duplicate terms": `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"1"},{"wire":2,"coeff":"3"},{"wire":2,"coeff":"4"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"},{"wire":0,"coeff":"7"},{"wire":0,"coeff":"-7"}]},
		{"a":[{"wire":2,"coeff":"1"}],"b":[{"wire":0,"coeff":"2"},{"wire":0,"coeff":"-1"}],"c":[{"wire":1,"coeff":"1"},{"wire":0,"coeff":"1"},{"wire":1,"coeff":"5"},{"wire":1,"coeff":"-5"}]}]}`,
	"huge modulus multiples": `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"7000000000000000000000001"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"},{"wire":2,"coeff":"-7000000000000000000000000"}]},
		{"a":[{"wire":2,"coeff":"1"}],"b":[{"wire":0,"coeff":"1"}],"c":[{"wire":1,"coeff":"1"},{"wire":0,"coeff":"7000000000000000000000001"}]}]}`,
}

// importInFreshStore creates the standard draft (name "c", version 1, two
// constraints, one public and one private input) in a fresh store and imports
// doc into it.
func importInFreshStore(t *testing.T, doc string) *Store {
	t.Helper()
	s := openTestStore(t)
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 2,
		PublicInputs: 1, PrivateInputs: 1, Description: "bounds"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints("c", 1, writeTempJSON(t, doc)); err != nil {
		t.Fatalf("import: %v", err)
	}
	return s
}

// TestImportZeroCoeffVariantsEquivalent pins that the legal rewrites —
// zero coefficients, modulus-multiple coefficients and cancelling duplicate
// terms on in-range wires — import to the identical constraint content,
// compile to the identical artifact hash and produce identical check
// verdicts (including the first-failure position) as the control definition.
func TestImportZeroCoeffVariantsEquivalent(t *testing.T) {
	control := importInFreshStore(t, boundsBaseDef)
	controlDef, err := control.GetDefinition("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	controlArtifact, err := control.CompileCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}

	// Satisfying, failing at constraint 1 and failing at constraint 2.
	witnesses := []struct {
		name         string
		w            Witness
		satisfied    bool
		firstFailure int
	}{
		{"satisfying", Witness{Public: []string{"2"}, Private: []string{"3"}}, true, 0},
		{"fails constraint 1", Witness{Public: []string{"2"}, Private: []string{"4"}}, false, 1},
		{"fails constraint 2", Witness{Public: []string{"3"}, Private: []string{"2"}}, false, 2},
	}
	controlVerdicts := make([]CheckResult, len(witnesses))
	for i, tc := range witnesses {
		res, err := control.CheckInput("c", 1, controlArtifact.Hash, tc.w)
		if err != nil {
			t.Fatalf("control check %q: %v", tc.name, err)
		}
		if res.Satisfied != tc.satisfied || res.FirstFailure != tc.firstFailure {
			t.Fatalf("control check %q: want satisfied=%t first_failure=%d, got %+v",
				tc.name, tc.satisfied, tc.firstFailure, res)
		}
		controlVerdicts[i] = res
	}

	for name, doc := range boundsBaseVariants {
		t.Run(name, func(t *testing.T) {
			s := importInFreshStore(t, doc)

			// The imported constraint content must equal the control's:
			// zeros dropped, duplicates merged, coefficients reduced.
			got, err := s.GetDefinition("c", 1)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, controlDef) {
				t.Fatalf("imported definition differs from control:\n got %+v\nwant %+v", got, controlDef)
			}

			// Freeze and compile: the artifact hash must be identical.
			if _, err := s.FreezeCircuit("c", 1); err != nil {
				t.Fatal(err)
			}
			artifact, err := s.CompileCircuit("c", 1)
			if err != nil {
				t.Fatal(err)
			}
			if artifact.Hash != controlArtifact.Hash {
				t.Fatalf("artifact hash differs from control:\n got %s\nwant %s", artifact.Hash, controlArtifact.Hash)
			}

			// The same inputs must give the same verdicts at the same
			// first-failure positions.
			for i, tc := range witnesses {
				res, err := s.CheckInput("c", 1, artifact.Hash, tc.w)
				if err != nil {
					t.Fatalf("check %q: %v", tc.name, err)
				}
				if res.Satisfied != controlVerdicts[i].Satisfied ||
					res.FirstFailure != controlVerdicts[i].FirstFailure {
					t.Fatalf("check %q: got %+v, control gave %+v", tc.name, res, controlVerdicts[i])
				}
			}
		})
	}
}

// TestZeroSideConstraintsPreserved pins that canonicalization never deletes
// a constraint whose terms all reduce to zero and never treats a zero side
// as automatically satisfied: constraint order and count survive the import,
// an all-zero constraint evaluates as 0·0 = 0 (satisfied), and a constraint
// with zero a/b sides but a nonzero c side genuinely fails.
func TestZeroSideConstraintsPreserved(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(Circuit{Name: "z", Version: 1, Constraints: 3,
		PublicInputs: 1, PrivateInputs: 1, Description: "zeros"}); err != nil {
		t.Fatal(err)
	}
	// Constraint 1: pub·priv = 6 (real constraint).
	// Constraint 2: every term cancels or is zero -> 0·0 = 0, always true.
	// Constraint 3: zero a and b sides but c = 1 -> 0·0 = 1, never true.
	doc := `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]},
		{"a":[{"wire":1,"coeff":"7"},{"wire":2,"coeff":"0"}],"b":[{"wire":0,"coeff":"0"}],"c":[{"wire":1,"coeff":"3"},{"wire":1,"coeff":"4"}]},
		{"a":[],"b":[],"c":[{"wire":0,"coeff":"1"}]}]}`
	if _, err := s.ImportConstraints("z", 1, writeTempJSON(t, doc)); err != nil {
		t.Fatalf("import: %v", err)
	}

	// All three constraints survive the import in order; the zeroed
	// constraint is kept with empty sides, not deleted.
	def, err := s.GetDefinition("z", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Constraints) != 3 {
		t.Fatalf("constraint count changed by canonicalization: %+v", def.Constraints)
	}
	zeroed := def.Constraints[1]
	if len(zeroed.A) != 0 || len(zeroed.B) != 0 || len(zeroed.C) != 0 {
		t.Fatalf("zeroed constraint should have empty sides, got %+v", zeroed)
	}

	if _, err := s.FreezeCircuit("z", 1); err != nil {
		t.Fatal(err)
	}
	artifact, err := s.CompileCircuit("z", 1)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Constraints != 3 {
		t.Fatalf("compiled artifact lost constraints: %+v", artifact)
	}

	// The all-zero constraint 2 is satisfied, so the unsatisfiable
	// constraint 3 is the first failure even for a witness that satisfies
	// constraint 1.
	res, err := s.CheckInput("z", 1, artifact.Hash, Witness{Public: []string{"2"}, Private: []string{"3"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Satisfied || res.FirstFailure != 3 {
		t.Fatalf("zero c-side constraint must fail at position 3, got %+v", res)
	}
	// Constraint 1 still genuinely evaluates: a witness breaking it fails
	// there first, so the zero constraint was not skipped or auto-satisfied
	// out of order.
	res, err = s.CheckInput("z", 1, artifact.Hash, Witness{Public: []string{"2"}, Private: []string{"4"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Satisfied || res.FirstFailure != 1 {
		t.Fatalf("first constraint must fail first, got %+v", res)
	}
}

// TestImportRejectsInvalidWireRegardlessOfCoeff pins that an out-of-layout
// wire dooms the whole import no matter how its coefficient reads: zero, a
// modulus multiple, a huge modulus multiple, or one half of a cancelling
// pair. The check applies to all three sides of a constraint, not only to
// terms that would survive into the compiled artifact.
func TestImportRejectsInvalidWireRegardlessOfCoeff(t *testing.T) {
	// Legal wires for one public and one private input: 0, 1, 2.
	illegalTerms := map[string]string{
		"negative wire":                       `{"wire":-1,"coeff":"1"}`,
		"negative wire zero coeff":            `{"wire":-1,"coeff":"0"}`,
		"negative wire modulus multiple":      `{"wire":-1,"coeff":"7"}`,
		"negative wire cancelling pair":       `{"wire":-1,"coeff":"5"},{"wire":-1,"coeff":"2"}`,
		"wire above layout":                   `{"wire":3,"coeff":"1"}`,
		"wire above layout zero coeff":        `{"wire":3,"coeff":"0"}`,
		"wire above layout modulus multiple":  `{"wire":3,"coeff":"7"}`,
		"wire above layout negative multiple": `{"wire":3,"coeff":"-14"}`,
		"wire above layout cancelling pair":   `{"wire":3,"coeff":"5"},{"wire":3,"coeff":"2"}`,
		"wire above layout huge multiple":     `{"wire":3,"coeff":"7000000000000000000000000"}`,
	}
	sides := []string{"a", "b", "c"}

	for termName, terms := range illegalTerms {
		for _, side := range sides {
			t.Run(termName+" on side "+side, func(t *testing.T) {
				// The illegal terms sit in the second of two constraints,
				// behind a fully legal first constraint: a partial import
				// would be observable, so the whole file must be refused.
				sideJSON := func(s string) string {
					if s == side {
						return `[` + terms + `]`
					}
					return `[]`
				}
				doc := `{"modulus":"7","constraints":[
					{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]},
					{"a":` + sideJSON("a") + `,"b":` + sideJSON("b") + `,"c":` + sideJSON("c") + `}]}`

				s := openTestStore(t)
				if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 2,
					PublicInputs: 1, PrivateInputs: 1, Description: "bounds"}); err != nil {
					t.Fatal(err)
				}
				_, err := s.ImportConstraints("c", 1, writeTempJSON(t, doc))
				if !errors.Is(err, ErrInvalidArgument) {
					t.Fatalf("want ErrInvalidArgument, got %v", err)
				}
				if !strings.Contains(err.Error(), "outside [0,2]") {
					t.Fatalf("rejection must name the allowed wire range, got %v", err)
				}
				if errors.Is(err, ErrDataCorrupt) {
					t.Fatalf("a bad import file must not be reported as data corruption: %v", err)
				}
				// The whole import is refused: no definition is left behind,
				// not even the legal first constraint.
				if _, err := s.GetDefinition("c", 1); !errors.Is(err, ErrDefinitionMissing) {
					t.Fatalf("rejected import left a partial definition: %v", err)
				}
			})
		}
	}
}

// TestImportWireBoundsAtLayoutEdges pins the exact boundary of the legal
// wire range for every input-layout shape: wire 0 (the constant 1) is always
// legal even with zero declared inputs, the wire numbered exactly
// public+private (the last input) is legal, and one past it is out of range
// even with a zero or cancelling coefficient.
func TestImportWireBoundsAtLayoutEdges(t *testing.T) {
	one := func(wire int, coeff string) string {
		return `{"modulus":"7","constraints":[{"a":[{"wire":` +
			strconv.Itoa(wire) + `,"coeff":"` + coeff + `"}],"b":[],"c":[]}]}`
	}
	cancelling := func(wire int) string {
		return `{"modulus":"7","constraints":[{"a":[{"wire":` + strconv.Itoa(wire) +
			`,"coeff":"3"},{"wire":` + strconv.Itoa(wire) + `,"coeff":"4"}],"b":[],"c":[]}]}`
	}

	cases := []struct {
		name      string
		pub, priv int
		doc       string
		legal     bool
	}{
		{"no inputs: constant wire 0 legal", 0, 0, one(0, "1"), true},
		{"no inputs: wire 1 out of range", 0, 0, one(1, "1"), false},
		{"no inputs: wire 1 zero coeff still out of range", 0, 0, one(1, "0"), false},
		{"no inputs: wire 1 cancelling pair still out of range", 0, 0, cancelling(1), false},
		{"no inputs: negative wire out of range", 0, 0, one(-1, "0"), false},
		{"public only: last input wire legal", 1, 0, one(1, "1"), true},
		{"public only: wire past last input out of range", 1, 0, one(2, "0"), false},
		{"private only: last input wire legal", 0, 1, one(1, "1"), true},
		{"private only: wire past last input out of range", 0, 1, one(2, "7"), false}, // modulus multiple
		{"mixed: wire == public+private is the last input", 2, 1, one(3, "1"), true},
		{"mixed: one past the last input out of range", 2, 1, one(4, "0"), false},
		{"mixed: one past the last input cancelling pair", 2, 1, cancelling(4), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1,
				PublicInputs: tc.pub, PrivateInputs: tc.priv, Description: "edge"}); err != nil {
				t.Fatal(err)
			}
			_, err := s.ImportConstraints("c", 1, writeTempJSON(t, tc.doc))
			if tc.legal {
				if err != nil {
					t.Fatalf("legal wire rejected: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("want ErrInvalidArgument, got %v", err)
			}
			if _, gerr := s.GetDefinition("c", 1); !errors.Is(gerr, ErrDefinitionMissing) {
				t.Fatalf("rejected import left a definition: %v", gerr)
			}
		})
	}
}

// TestFailedImportLeavesPriorDefinitionAndState pins the atomicity of a
// refused re-import: a draft that already holds a legal definition keeps
// that definition, its counts and its description byte-for-byte when a
// later file is rejected for an out-of-range wire, and every subsequent
// operation — query, freeze, compile, input check — works from the original
// definition, never from the refused file.
func TestFailedImportLeavesPriorDefinitionAndState(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1,
		PublicInputs: 1, PrivateInputs: 1, Description: "original"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints("c", 1, writeTempJSON(t, validDef)); err != nil {
		t.Fatalf("seed import: %v", err)
	}
	wantDef, err := s.GetDefinition("c", 1)
	if err != nil {
		t.Fatal(err)
	}

	// Every refusal mode: zero coefficient, modulus multiple, cancelling
	// pair, negative wire — each on a version that already has a definition.
	bad := map[string]string{
		"zero coeff out of range": `{"modulus":"11","constraints":[
			{"a":[{"wire":9,"coeff":"0"}],"b":[],"c":[]}]}`,
		"modulus multiple out of range": `{"modulus":"11","constraints":[
			{"a":[],"b":[{"wire":3,"coeff":"11"}],"c":[]}]}`,
		"cancelling pair out of range": `{"modulus":"11","constraints":[
			{"a":[],"b":[],"c":[{"wire":4,"coeff":"6"},{"wire":4,"coeff":"5"}]}]}`,
		"negative wire zero coeff": `{"modulus":"11","constraints":[
			{"a":[{"wire":-2,"coeff":"0"}],"b":[],"c":[]}]}`,
	}
	for name, doc := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := s.ImportConstraints("c", 1, writeTempJSON(t, doc)); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("want ErrInvalidArgument, got %v", err)
			}
		})
	}

	// The stored definition, counts and description are exactly as before.
	gotDef, err := s.GetDefinition("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotDef, wantDef) {
		t.Fatalf("definition changed by refused imports:\n got %+v\nwant %+v", gotDef, wantDef)
	}
	c, err := s.GetCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if c.Constraints != 1 || c.PublicInputs != 1 || c.PrivateInputs != 1 || c.Description != "original" {
		t.Fatalf("circuit record changed by refused imports: %+v", c)
	}

	// Freeze and compile: the artifact must be the one the original
	// definition produces in a clean store, not anything derived from the
	// refused files (which all carried modulus 11).
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	artifact, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	control := openTestStore(t)
	seedDraftWithDef(t, control, "c", 1, 1, 1, validDef)
	if _, err := control.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	controlArtifact, err := control.CompileCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if artifact != controlArtifact {
		t.Fatalf("artifact after refused imports differs from control:\n got %+v\nwant %+v", artifact, controlArtifact)
	}

	// Input checks still run against the original definition: 2·3 = 6
	// satisfies, 2·4 = 1 ≠ 6 fails at constraint 1.
	res, err := s.CheckInput("c", 1, artifact.Hash, Witness{Public: []string{"2"}, Private: []string{"3"}})
	if err != nil || !res.Satisfied {
		t.Fatalf("check against the preserved definition: %+v %v", res, err)
	}
	res, err = s.CheckInput("c", 1, artifact.Hash, Witness{Public: []string{"2"}, Private: []string{"4"}})
	if err != nil || res.Satisfied || res.FirstFailure != 1 {
		t.Fatalf("unsatisfied verdict against the preserved definition: %+v %v", res, err)
	}
}
