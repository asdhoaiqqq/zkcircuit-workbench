package zkcircuit

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// This file is the import-time regression guard for the wire-range rule. A
// wire index is checked on EVERY term as the document is read, BEFORE terms
// are merged, before coefficients are reduced modulo the field and before the
// zero terms are dropped from the canonical form. Consequently a coefficient
// that is zero, a whole multiple of the modulus, or two terms on the same
// illegal wire that cancel to zero can never launder an out-of-range wire
// into an accepted definition. The rule applies to all three sides a/b/c,
// including terms that would never survive into the compiled artifact.

// guardTerms is one way of writing the offending reference: each value is the
// raw JSON array placed on the side that carries the illegal wire.
var guardTerms = map[string]string{
	"zero coefficient":            `[{"wire":%d,"coeff":"0"}]`,
	"positive modulus multiple":   `[{"wire":%d,"coeff":"11"}]`,
	"negative modulus multiple":   `[{"wire":%d,"coeff":"-22"}]`,
	"cancelling pair":             `[{"wire":%d,"coeff":"4"},{"wire":%d,"coeff":"-4"}]`,
	"pair merging to a multiple":  `[{"wire":%d,"coeff":"5"},{"wire":%d,"coeff":"6"}]`,
	"huge coefficient congruent0": `[{"wire":%d,"coeff":"11000000000000000000000000"}]`,
}

// guardDoc builds a one-constraint modulus-11 definition whose offending side
// carries the illegal wire and whose other sides are empty (the zero linear
// combination), so the only possible reason for rejection is the wire.
func guardDoc(side string, wire int, terms string) string {
	fill := func(which string) string {
		if which == side {
			return fmt.Sprintf(terms, wireVals(terms, wire)...)
		}
		return "[]"
	}
	return `{"modulus":"11","constraints":[{"a":` + fill("a") + `,"b":` + fill("b") + `,"c":` + fill("c") + `}]}`
}

// wireVals repeats the wire value once per %d verb in the term template.
func wireVals(template string, wire int) []any {
	n := strings.Count(template, "%d")
	vals := make([]any, n)
	for i := range vals {
		vals[i] = wire
	}
	return vals
}

// TestIllegalWireRejectedDespiteVanishingCoefficient is the central guard: for
// a version with one public and one private input the legal range is [0,2].
// A wire just past the end (3) and a negative wire (-1) are both rejected on
// every side, under every coefficient write-up that would make the term
// vanish during canonicalization.
func TestIllegalWireRejectedDespiteVanishingCoefficient(t *testing.T) {
	const pub, priv, count = 1, 1, 1 // legal wires 0,1,2
	for _, wire := range []int{3, -1, 4, -100} {
		for _, side := range []string{"a", "b", "c"} {
			for name, tmpl := range guardTerms {
				doc := guardDoc(side, wire, tmpl)
				_, err := parseDefinitionJSON([]byte(doc), pub, priv, count)
				if err == nil {
					t.Fatalf("wire %d on side %s (%s) was accepted:\n%s", wire, side, name, doc)
				}
				if !errors.Is(err, ErrInvalidArgument) {
					t.Fatalf("wire %d on side %s (%s): want ErrInvalidArgument, got %v", wire, side, name, err)
				}
				if !strings.Contains(err.Error(), "outside") {
					t.Fatalf("wire %d on side %s (%s): rejection must name the allowed wire range, got %v",
						wire, side, name, err)
				}
			}
		}
	}
}

// TestVanishingCoefficientAcceptedOnLegalWire is the control for the test
// above: precisely the coefficient write-ups that are rejected on an illegal
// wire must be accepted on an in-range wire. The thing being policed is the
// index, never the (otherwise legal) coefficient form.
func TestVanishingCoefficientAcceptedOnLegalWire(t *testing.T) {
	const pub, priv, count = 1, 1, 1 // legal wires 0,1,2
	for _, wire := range []int{0, 1, 2} {
		for _, side := range []string{"a", "b", "c"} {
			for name, tmpl := range guardTerms {
				doc := guardDoc(side, wire, tmpl)
				d, err := parseDefinitionJSON([]byte(doc), pub, priv, count)
				if err != nil {
					t.Fatalf("legal wire %d on side %s (%s) rejected: %v\n%s", wire, side, name, err, doc)
				}
				// Every one of these write-ups vanishes (zero / multiple of 11 /
				// cancelling pair), so the offending side canonicalizes to the
				// empty zero combination — the term is legal but carries no data.
				var got []parsedTerm
				switch side {
				case "a":
					got = d.constraints[0].a
				case "b":
					got = d.constraints[0].b
				case "c":
					got = d.constraints[0].c
				}
				if len(got) != 0 {
					t.Fatalf("legal wire %d on side %s (%s): term should vanish, got %+v", wire, side, name, got)
				}
			}
		}
	}
}

// TestConstantOnlyLayoutWireBoundaries pins the layout at the zero-input
// version. With zero public and zero private inputs wire 0 (the constant 1)
// is still the only legal wire: a legal reference to 0 is accepted while a
// reference to 1 is already out of range — even when that reference would
// vanish in canonicalization.
func TestConstantOnlyLayoutWireBoundaries(t *testing.T) {
	const pub, priv, count = 0, 0, 1 // legal wires: {0}

	// Legal references to the constant wire, including vanishing write-ups.
	for _, doc := range []string{
		`{"modulus":"11","constraints":[{"a":[{"wire":0,"coeff":"1"}],"b":[],"c":[]}]}`,
		`{"modulus":"11","constraints":[{"a":[{"wire":0,"coeff":"0"}],"b":[],"c":[]}]}`,
		`{"modulus":"11","constraints":[{"a":[],"b":[],"c":[{"wire":0,"coeff":"11"}]}]}`,
		`{"modulus":"11","constraints":[{"a":[],"b":[{"wire":0,"coeff":"5"},{"wire":0,"coeff":"6"}],"c":[]}]}`,
	} {
		if _, err := parseDefinitionJSON([]byte(doc), pub, priv, count); err != nil {
			t.Fatalf("legal constant-wire reference rejected: %v\n%s", err, doc)
		}
	}

	// Wire 1 is one past the end of a 0/0 layout and stays illegal under every
	// vanishing disguise, on every side.
	for _, side := range []string{"a", "b", "c"} {
		for name, tmpl := range guardTerms {
			doc := guardDoc(side, 1, tmpl)
			_, err := parseDefinitionJSON([]byte(doc), pub, priv, count)
			if err == nil || !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "outside [0,0]") {
				t.Fatalf("0/0 layout, side %s (%s): want invalid-argument outside [0,0], got %v\n%s",
					side, name, err, doc)
			}
		}
	}
}

// TestLastInputWireIsInRange guards the off-by-one at the top of the layout:
// with two public and one private input the last input is exactly wire 3 (=
// public+private), and it must be accepted; only wire 4 is out of range.
func TestLastInputWireIsInRange(t *testing.T) {
	const pub, priv, count = 2, 1, 1 // legal wires 0..3
	for _, doc := range []string{
		`{"modulus":"11","constraints":[{"a":[{"wire":3,"coeff":"1"}],"b":[],"c":[]}]}`,
		`{"modulus":"11","constraints":[{"a":[{"wire":3,"coeff":"0"}],"b":[],"c":[]}]}`,
		`{"modulus":"11","constraints":[{"a":[],"b":[{"wire":3,"coeff":"22"}],"c":[]}]}`,
		`{"modulus":"11","constraints":[{"a":[],"b":[],"c":[{"wire":3,"coeff":"7"},{"wire":3,"coeff":"-7"}]}]}`,
	} {
		if _, err := parseDefinitionJSON([]byte(doc), pub, priv, count); err != nil {
			t.Fatalf("last input wire 3 must be in range: %v\n%s", err, doc)
		}
	}
	for _, side := range []string{"a", "b", "c"} {
		for name, tmpl := range guardTerms {
			doc := guardDoc(side, 4, tmpl)
			_, err := parseDefinitionJSON([]byte(doc), pub, priv, count)
			if err == nil || !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "outside [0,3]") {
				t.Fatalf("2/1 layout, side %s (%s): want invalid-argument outside [0,3], got %v\n%s",
					side, name, err, doc)
			}
		}
	}
}

// TestRejectedImportNeverPartiallyReplaces proves the store-level guarantee:
// a draft already carrying a legal definition is left exactly as it was when a
// later import is refused for an out-of-range wire, regardless of the
// coefficient disguise. The saved definition, the frozen compile and the
// input verdict all continue to come from the earlier, legal document — never
// from a partial or failed replacement.
func TestRejectedImportNeverPartiallyReplaces(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1,
		PublicInputs: 1, PrivateInputs: 1, Description: "keep"}); err != nil {
		t.Fatal(err)
	}
	// validDef is modulus 7: w1·w2 = 6, satisfied by w1=2, w2=3 (6 ≡ 6).
	if _, err := s.ImportConstraints("c", 1, writeTempJSON(t, validDef)); err != nil {
		t.Fatalf("seed import: %v", err)
	}
	before, err := s.GetDefinition("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}

	// All bad documents keep modulus 7 and exactly one constraint, so the wire
	// is the sole defect; each tries a different vanishing disguise.
	badDocs := map[string]string{
		"zero past end": `{"modulus":"7","constraints":[
			{"a":[{"wire":3,"coeff":"0"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`,
		"multiple past end": `{"modulus":"7","constraints":[
			{"a":[{"wire":3,"coeff":"7"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`,
		"cancelling past end": `{"modulus":"7","constraints":[
			{"a":[{"wire":3,"coeff":"3"},{"wire":3,"coeff":"-3"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`,
		"negative wire": `{"modulus":"7","constraints":[
			{"a":[{"wire":-1,"coeff":"0"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`,
		"negative wire multiple": `{"modulus":"7","constraints":[
			{"a":[{"wire":-2,"coeff":"14"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`,
		"illegal wire on b": `{"modulus":"7","constraints":[
			{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":9,"coeff":"7"}],"c":[{"wire":0,"coeff":"6"}]}]}`,
		"illegal wire on c": `{"modulus":"7","constraints":[
			{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":-1,"coeff":"0"}]}]}`,
	}
	for name, bad := range badDocs {
		if _, err := s.ImportConstraints("c", 1, writeTempJSON(t, bad)); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s: want ErrInvalidArgument, got %v", name, err)
		}
		after, gerr := s.GetDefinition("c", 1)
		if gerr != nil {
			t.Fatalf("%s: definition unreadable after refused import: %v", name, gerr)
		}
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("%s: refused import changed the saved definition:\nbefore %+v\nafter  %+v", name, before, after)
		}
		onDisk, rerr := readDataFile(t, dir)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if string(onDisk) != string(committed) {
			t.Fatalf("%s: refused import rewrote data.json", name)
		}
	}

	// Freeze and compile the untouched definition; its artifact must equal one
	// built from the same legal document in a clean directory (proving the
	// failed files left no trace in the compiled output).
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	artifact, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatalf("compile after refused imports: %v", err)
	}
	clean := openTestStore(t)
	if _, err := clean.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1,
		PublicInputs: 1, PrivateInputs: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := clean.ImportConstraints("c", 1, writeTempJSON(t, validDef)); err != nil {
		t.Fatal(err)
	}
	if _, err := clean.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	cleanArtifact, err := clean.CompileCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Hash != cleanArtifact.Hash {
		t.Fatalf("artifact hash %q differs from the never-poisoned build %q", artifact.Hash, cleanArtifact.Hash)
	}

	// The surviving definition still evaluates on its own terms: the satisfying
	// assignment for w1·w2 = 6 (mod 7) is accepted, and a wrong one is reported
	// against constraint 1.
	if res, err := s.CheckInput("c", 1, artifact.Hash, Witness{Public: []string{"2"}, Private: []string{"3"}}); err != nil ||
		!res.Satisfied || res.FirstFailure != 0 {
		t.Fatalf("satisfying check against surviving definition: %+v %v", res, err)
	}
	if res, err := s.CheckInput("c", 1, artifact.Hash, Witness{Public: []string{"2"}, Private: []string{"2"}}); err != nil ||
		res.Satisfied || res.FirstFailure != 1 {
		t.Fatalf("unsatisfied check against surviving definition: %+v %v", res, err)
	}
}
