package zkcircuit

import (
	"errors"
	"reflect"
	"testing"
)

// This file is the regression guard for the constraint-definition query
// contract: GetDefinition hands the caller a normalized snapshot that is
// fully detached from the stored definition. A caller may treat the result
// as an editable draft copy — change the modulus, rewrite wire numbers or
// coefficients, reorder, truncate or extend the constraint and term arrays —
// and neither another copy obtained earlier or later, nor the stored
// definition behind queries, freezing, compilation and input checking, may
// observe any of those edits.

// isolationDef is the import document every scenario here starts from. It is
// deliberately not in canonical form: constraint 2 carries duplicate wires
// that merge to zero (and are dropped), a coefficient above the modulus and
// a negative coefficient, so the stored form only exists after the import
// normalization this guard also pins. Constraint 3 keeps all three sides
// empty (the zero linear combination).
//
//	C1: w1·w1 = 4            -> holds iff w1 = 2 or 5 (mod 7)
//	C2: (3·w2 + 4·w2)·9 = -w2 -> 0 = 6·w2, holds iff w2 = 0
//	C3: 0·0 = 0              -> always holds
const isolationDef = `{"modulus":"7","constraints":[
	{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":1,"coeff":"1"}],"c":[{"wire":0,"coeff":"4"}]},
	{"a":[{"wire":2,"coeff":"3"},{"wire":2,"coeff":"4"}],"b":[{"wire":0,"coeff":"9"}],"c":[{"wire":2,"coeff":"-1"}]},
	{"a":[],"b":[],"c":[]}]}`

// isolationWant is the normalized content every fresh query of isolationDef
// must reproduce: duplicate wires merged, coefficients reduced modulo 7,
// zero terms dropped, constraint order preserved, empty sides kept empty.
func isolationWant() Definition {
	return Definition{
		Modulus: "7",
		Constraints: []Constraint{
			{A: []Term{{Wire: 1, Coeff: "1"}}, B: []Term{{Wire: 1, Coeff: "1"}}, C: []Term{{Wire: 0, Coeff: "4"}}},
			{A: []Term{}, B: []Term{{Wire: 0, Coeff: "2"}}, C: []Term{{Wire: 2, Coeff: "6"}}},
			{A: []Term{}, B: []Term{}, C: []Term{}},
		},
	}
}

// mangleDefinition rewrites every aspect of a queried copy that could change
// its mathematical meaning or its shape: the modulus, a term's wire and
// coefficient, the contents and length of side arrays, the order of the
// constraints and the length of the constraint array itself.
func mangleDefinition(d *Definition) {
	d.Modulus = "13"
	d.Constraints[0].A[0].Wire = 2
	d.Constraints[0].A[0].Coeff = "6"
	d.Constraints[1].B = nil
	d.Constraints[1].C = append(d.Constraints[1].C, Term{Wire: 1, Coeff: "1"})
	d.Constraints[0], d.Constraints[2] = d.Constraints[2], d.Constraints[0]
	d.Constraints = d.Constraints[:2]
}

func assertDefinitionEqual(t *testing.T, got, want Definition) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("definition mismatch:\n got %+v\nwant %+v", got, want)
	}
}

// seedIsolationDraft creates the 3-constraint, 1-public/1-private draft and
// imports isolationDef into it.
func seedIsolationDraft(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 3,
		PublicInputs: 1, PrivateInputs: 1, Description: "c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints("c", 1, writeTempJSON(t, isolationDef)); err != nil {
		t.Fatalf("import: %v", err)
	}
}

// TestGetDefinitionCopiesAreIndependent pins the draft-stage rule: two
// queries of the same version return two detached snapshots of the
// normalized definition, and mangling one leaves the other — and every later
// query — with the original content.
func TestGetDefinitionCopiesAreIndependent(t *testing.T) {
	s := openTestStore(t)
	seedIsolationDraft(t, s)

	first, err := s.GetDefinition("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.GetDefinition("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	assertDefinitionEqual(t, first, isolationWant())
	assertDefinitionEqual(t, second, isolationWant())

	mangleDefinition(&first)

	// The other copy obtained before the edits is untouched.
	assertDefinitionEqual(t, second, isolationWant())
	// A fresh query after the edits still returns the stored content.
	again, err := s.GetDefinition("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	assertDefinitionEqual(t, again, isolationWant())
}

// TestFrozenQueryCopyMutationKeepsArtifactAndVerdicts pins the same
// isolation after freeze and compile: editing a queried copy must not drift
// the frozen definition, the compiled artifact hash, or the verdicts of
// input checks bound to that hash.
func TestFrozenQueryCopyMutationKeepsArtifactAndVerdicts(t *testing.T) {
	s := openTestStore(t)
	seedIsolationDraft(t, s)
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	artifact, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// Verdicts before any copy is mangled: w1 = 2, w2 = 0 satisfies every
	// constraint; w1 = 2, w2 = 1 fails C2 (0 ≠ 6) after C1 held; w1 = 1
	// fails C1 immediately. Both outcomes are completed checks, not errors.
	satisfying := Witness{Public: []string{"2"}, Private: []string{"0"}}
	failingSecond := Witness{Public: []string{"2"}, Private: []string{"1"}}
	failingFirst := Witness{Public: []string{"1"}, Private: []string{"0"}}
	assertSatisfied(t, s, "c", 1, artifact.Hash, satisfying)
	assertUnsatisfied(t, s, "c", 1, artifact.Hash, failingSecond, 2)
	assertUnsatisfied(t, s, "c", 1, artifact.Hash, failingFirst, 1)

	// A queried copy of the frozen definition is mangled in every way that
	// would change its meaning.
	frozen, err := s.GetDefinition("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	assertDefinitionEqual(t, frozen, isolationWant())
	mangleDefinition(&frozen)

	// The stored frozen definition and the artifact hash did not drift.
	after, err := s.GetDefinition("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	assertDefinitionEqual(t, after, isolationWant())
	recompiled, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if recompiled.Hash != artifact.Hash {
		t.Fatalf("artifact hash drifted after query-copy mutation: %q -> %q", artifact.Hash, recompiled.Hash)
	}

	// The same witnesses keep their original conclusions, still bound to the
	// original artifact hash.
	assertSatisfied(t, s, "c", 1, artifact.Hash, satisfying)
	assertUnsatisfied(t, s, "c", 1, artifact.Hash, failingSecond, 2)
	assertUnsatisfied(t, s, "c", 1, artifact.Hash, failingFirst, 1)

	// Satisfied and unsatisfied are the two normal outcomes of a completed
	// check; a malformed witness stays a distinct input format error.
	if _, err := s.CheckInput("c", 1, artifact.Hash, Witness{Public: []string{"2"}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("malformed witness: want ErrInvalidInput, got %v", err)
	}
}

// TestDraftReimportLeavesEarlierQueryCopiesBehind pins the re-import rule: a
// legal re-import replaces the stored definition for later queries, but
// copies obtained before the re-import keep their then-current content, and
// editing such a stale copy never reaches the new definition.
func TestDraftReimportLeavesEarlierQueryCopiesBehind(t *testing.T) {
	s := openTestStore(t)
	seedIsolationDraft(t, s)

	before, err := s.GetDefinition("c", 1)
	if err != nil {
		t.Fatal(err)
	}

	const replacementDef = `{"modulus":"11","constraints":[
		{"a":[{"wire":1,"coeff":"2"}],"b":[{"wire":1,"coeff":"2"}],"c":[{"wire":0,"coeff":"4"}]},
		{"a":[],"b":[],"c":[]},
		{"a":[{"wire":2,"coeff":"1"}],"b":[{"wire":0,"coeff":"1"}],"c":[{"wire":2,"coeff":"1"}]}]}`
	replacementWant := Definition{
		Modulus: "11",
		Constraints: []Constraint{
			{A: []Term{{Wire: 1, Coeff: "2"}}, B: []Term{{Wire: 1, Coeff: "2"}}, C: []Term{{Wire: 0, Coeff: "4"}}},
			{A: []Term{}, B: []Term{}, C: []Term{}},
			{A: []Term{{Wire: 2, Coeff: "1"}}, B: []Term{{Wire: 0, Coeff: "1"}}, C: []Term{{Wire: 2, Coeff: "1"}}},
		},
	}
	if _, err := s.ImportConstraints("c", 1, writeTempJSON(t, replacementDef)); err != nil {
		t.Fatalf("re-import: %v", err)
	}

	// New queries reflect the new definition.
	now, err := s.GetDefinition("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	assertDefinitionEqual(t, now, replacementWant)

	// The copy obtained before the re-import still holds the old content.
	assertDefinitionEqual(t, before, isolationWant())

	// Mangling that stale copy cannot overwrite the new definition.
	mangleDefinition(&before)
	after, err := s.GetDefinition("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	assertDefinitionEqual(t, after, replacementWant)
}

// TestGetDefinitionMissingAndUnknownVersion pins the negative boundary: a
// version without an imported definition reports constraint definition
// missing and an unknown version reports not found. Neither case hands back
// an editable empty definition, and the failed queries create nothing.
func TestGetDefinitionMissingAndUnknownVersion(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(Circuit{Name: "bare", Version: 1, Constraints: 1,
		PublicInputs: 0, PrivateInputs: 0, Description: "d"}); err != nil {
		t.Fatal(err)
	}

	for _, state := range []string{"draft", "frozen"} {
		if state == "frozen" {
			if _, err := s.FreezeCircuit("bare", 1); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.GetDefinition("bare", 1)
		if !errors.Is(err, ErrDefinitionMissing) {
			t.Fatalf("%s counts-only version: want ErrDefinitionMissing, got %v", state, err)
		}
		if !reflect.DeepEqual(got, Definition{}) {
			t.Fatalf("%s counts-only version returned an editable definition: %+v", state, got)
		}
	}

	got, err := s.GetDefinition("ghost", 9)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown version: want ErrNotFound, got %v", err)
	}
	if !reflect.DeepEqual(got, Definition{}) {
		t.Fatalf("unknown version returned an editable definition: %+v", got)
	}

	// The failed queries committed nothing: still exactly one circuit, still
	// without a definition.
	list, err := s.ListCircuits()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("failed queries created records: %+v", list)
	}
	if _, err := s.GetDefinition("bare", 1); !errors.Is(err, ErrDefinitionMissing) {
		t.Fatalf("definition still missing after failed queries: %v", err)
	}
}
