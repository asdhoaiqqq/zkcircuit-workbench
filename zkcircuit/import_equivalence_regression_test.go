package zkcircuit

import (
	"reflect"
	"testing"
)

// This file pins the equivalence half of the import contract. On legal wires
// only, a zero coefficient, a coefficient that is a whole multiple of the
// modulus, or duplicate terms that merge to zero are all accepted and carry no
// information: a definition written with them imports to the same constraint
// content as one with those terms deleted, freezes and compiles to the same
// artifact hash, and — for one fixed public/private assignment — yields the
// same satisfied/unsatisfied verdict and the same first-failure position.
//
// A second, orthogonal rule is pinned here too: dropping a zero TERM never
// drops the CONSTRAINT that contained it, and a side that reduces to zero is
// never treated as automatically satisfied. Constraint order and count are
// preserved verbatim; a constraint with a genuine zero side is still
// evaluated as a·b = c.

// equivBase is the undecorated document: modulus 7, one public wire w1 and one
// private wire w2, four constraints in fixed order:
//
//	C1: w1·w2 = 6                 (holds for w1=2,w2=3; fails for 3,3)
//	C2: 0·w1 = 0                  (a genuine zero side; always holds)
//	C3: (w1-w1)·1 = 0             (cancelling left side; always holds)
//	C4: 0·1 = 1                   (zero side a, nonzero right; always fails)
const equivBase = `{"modulus":"7","constraints":[
	{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]},
	{"a":[],"b":[{"wire":1,"coeff":"1"}],"c":[]},
	{"a":[{"wire":1,"coeff":"1"},{"wire":1,"coeff":"-1"}],"b":[{"wire":0,"coeff":"1"}],"c":[]},
	{"a":[],"b":[{"wire":0,"coeff":"1"}],"c":[{"wire":0,"coeff":"1"}]}]}`

// equivVariants all canonicalize to equivBase. Every added term sits on a
// legal wire and vanishes or merges away: an explicit "0", a coefficient a
// whole multiple of 7, a congruent coefficient (8≡1, -6≡1, -1≡6, 15≡1), or a
// duplicate pair that sums to 0 or to the base coefficient.
var equivVariants = map[string]string{
	"explicit zero terms": `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"1"},{"wire":2,"coeff":"0"}],"b":[{"wire":2,"coeff":"1"},{"wire":0,"coeff":"0"}],"c":[{"wire":0,"coeff":"6"},{"wire":1,"coeff":"0"}]},
		{"a":[{"wire":1,"coeff":"0"}],"b":[{"wire":1,"coeff":"1"},{"wire":2,"coeff":"0"}],"c":[{"wire":0,"coeff":"0"}]},
		{"a":[{"wire":1,"coeff":"1"},{"wire":1,"coeff":"-1"},{"wire":0,"coeff":"0"}],"b":[{"wire":0,"coeff":"1"},{"wire":2,"coeff":"0"}],"c":[{"wire":1,"coeff":"0"}]},
		{"a":[{"wire":2,"coeff":"0"}],"b":[{"wire":0,"coeff":"1"}],"c":[{"wire":0,"coeff":"1"},{"wire":0,"coeff":"0"}]}]}`,

	"modulus multiple and congruent coefficients": `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"8"}],"b":[{"wire":2,"coeff":"-6"}],"c":[{"wire":0,"coeff":"-1"}]},
		{"a":[{"wire":2,"coeff":"14"}],"b":[{"wire":1,"coeff":"8"}],"c":[{"wire":1,"coeff":"-7"}]},
		{"a":[{"wire":1,"coeff":"8"},{"wire":1,"coeff":"-1"},{"wire":1,"coeff":"14"}],"b":[{"wire":0,"coeff":"15"}],"c":[{"wire":2,"coeff":"-28"}]},
		{"a":[{"wire":1,"coeff":"0"}],"b":[{"wire":0,"coeff":"-6"}],"c":[{"wire":0,"coeff":"8"}]}]}`,

	"duplicate terms merging into the base": `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"4"},{"wire":1,"coeff":"-3"}],"b":[{"wire":2,"coeff":"3"},{"wire":2,"coeff":"-2"}],"c":[{"wire":0,"coeff":"2"},{"wire":0,"coeff":"4"}]},
		{"a":[{"wire":2,"coeff":"7"}],"b":[{"wire":1,"coeff":"2"},{"wire":1,"coeff":"6"}],"c":[{"wire":0,"coeff":"7"}]},
		{"a":[{"wire":1,"coeff":"3"},{"wire":1,"coeff":"4"}],"b":[{"wire":0,"coeff":"1"},{"wire":0,"coeff":"7"}],"c":[{"wire":2,"coeff":"5"},{"wire":2,"coeff":"2"}]},
		{"a":[{"wire":1,"coeff":"0"}],"b":[{"wire":0,"coeff":"3"},{"wire":0,"coeff":"-2"}],"c":[{"wire":0,"coeff":"1"},{"wire":0,"coeff":"7"}]}]}`,

	"all decorations at once": `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"8"},{"wire":1,"coeff":"-7"},{"wire":2,"coeff":"0"}],"b":[{"wire":2,"coeff":"3"},{"wire":2,"coeff":"5"},{"wire":0,"coeff":"14"}],"c":[{"wire":0,"coeff":"-1"},{"wire":1,"coeff":"0"}]},
		{"a":[{"wire":2,"coeff":"-14"}],"b":[{"wire":1,"coeff":"15"},{"wire":1,"coeff":"-7"},{"wire":2,"coeff":"0"}],"c":[{"wire":0,"coeff":"21"}]},
		{"a":[{"wire":1,"coeff":"10"},{"wire":1,"coeff":"-3"},{"wire":0,"coeff":"0"}],"b":[{"wire":0,"coeff":"-6"},{"wire":2,"coeff":"7"}],"c":[{"wire":2,"coeff":"9"},{"wire":2,"coeff":"-2"}]},
		{"a":[{"wire":1,"coeff":"7"},{"wire":2,"coeff":"0"}],"b":[{"wire":0,"coeff":"1"},{"wire":0,"coeff":"14"}],"c":[{"wire":0,"coeff":"8"},{"wire":0,"coeff":"0"}]}]}`,
}

// equivCircuit imports, freezes and compiles one variant in its own fresh
// directory, returning the store, artifact and exported definition. Every
// variant lives under the SAME circuit name/version — each gets an isolated
// store — so name/version fold identically into the artifact hash and any
// hash difference can only come from the constraint content.
func equivCircuit(t *testing.T, doc string) (*Store, Artifact, Definition) {
	t.Helper()
	s, artifact := regressionCircuit(t, "equiv", 1, 1, 1, 4, doc)
	def, err := s.GetDefinition("equiv", 1)
	if err != nil {
		t.Fatal(err)
	}
	return s, artifact, def
}

// TestEquivalentWriteupsImportIdentically proves the imported constraint
// content is definition-equal across every decoration, while the four
// constraints survive in their original order — zero sides included, no
// constraint deleted or auto-dropped.
func TestEquivalentWriteupsImportIdentically(t *testing.T) {
	_, _, want := equivCircuit(t, equivBase)

	if len(want.Constraints) != 4 {
		t.Fatalf("base should keep four constraints, got %d", len(want.Constraints))
	}
	// Zero sides stay empty slices; the constraints that own them remain.
	if len(want.Constraints[1].A) != 0 || len(want.Constraints[1].C) != 0 {
		t.Fatalf("C2 zero sides must be empty, not deleted-with-constraint: %+v", want.Constraints[1])
	}
	if len(want.Constraints[3].A) != 0 {
		t.Fatalf("C4 side a must be the genuine zero combination: %+v", want.Constraints[3].A)
	}
	// C4's nonzero b/c prove the constraint is present despite its zero a.
	if len(want.Constraints[3].B) != 1 || want.Constraints[3].B[0] != (Term{Wire: 0, Coeff: "1"}) {
		t.Fatalf("C4 side b must retain its constant term: %+v", want.Constraints[3].B)
	}
	if len(want.Constraints[3].C) != 1 || want.Constraints[3].C[0] != (Term{Wire: 0, Coeff: "1"}) {
		t.Fatalf("C4 side c must retain its constant term: %+v", want.Constraints[3].C)
	}

	for name, doc := range equivVariants {
		_, _, got := equivCircuit(t, doc)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: imported content differs from the stripped base\n got %+v\nwant %+v", name, got, want)
		}
	}
}

// TestEquivalentWriteupsCompileToSameHash proves the frozen artifact is one
// hash for the whole equivalence class, and that recompiling is idempotent
// after the decorated imports.
func TestEquivalentWriteupsCompileToSameHash(t *testing.T) {
	_, baseArtifact, _ := equivCircuit(t, equivBase)
	for name, doc := range equivVariants {
		s, artifact, _ := equivCircuit(t, doc)
		if artifact.Hash != baseArtifact.Hash {
			t.Fatalf("%s: hash %q != base hash %q", name, artifact.Hash, baseArtifact.Hash)
		}
		// A second compile of the frozen, decorated-import version returns the
		// very same artifact.
		again, err := s.CompileCircuit("equiv", 1)
		if err != nil {
			t.Fatalf("%s: recompile: %v", name, err)
		}
		if again.Hash != artifact.Hash {
			t.Fatalf("%s: recompile hash drifted: %q vs %q", name, again.Hash, artifact.Hash)
		}
	}
}

// equivCase pairs one compiled writing with the circuit name it lives under.
type equivCase struct {
	circuit string
	store   *Store
	hash    string
}

// TestEquivalentWriteupsShareVerdicts proves one fixed public/private
// assignment draws the same conclusion and the same 1-based first failure in
// every writing. It also fixes the zero-side semantics: C4 (0·1 = 1) really
// fails at position 4 rather than being auto-satisfied by its empty side a,
// and a failing C1 hides it at position 1.
func TestEquivalentWriteupsShareVerdicts(t *testing.T) {
	base, baseArtifact, _ := equivCircuit(t, equivBase)
	cases := []equivCase{{circuit: "equiv", store: base, hash: baseArtifact.Hash}}
	for _, doc := range equivVariants {
		s, artifact, _ := equivCircuit(t, doc)
		cases = append(cases, equivCase{circuit: "equiv", store: s, hash: artifact.Hash})
	}

	// C1..C3 hold, C4 (0·1 = 1) must fail: a zero side is not an automatic
	// pass, and the still-present fourth constraint is evaluated at position 4.
	holdThenC4 := Witness{Public: []string{"2"}, Private: []string{"3"}}
	// C1 fails (3·3 = 9 ≡ 2 ≠ 6), masking the also-failing C4.
	c1Fails := Witness{Public: []string{"3"}, Private: []string{"3"}}

	for _, tc := range cases {
		res, err := tc.store.CheckInput(tc.circuit, 1, tc.hash, holdThenC4)
		if err != nil {
			t.Fatalf("%s: check returned a call error: %v", tc.circuit, err)
		}
		if res.Satisfied || res.FirstFailure != 4 {
			t.Fatalf("%s: want unsatisfied first_failure=4 (zero side is not auto-satisfied), got %+v", tc.circuit, res)
		}

		res2, err2 := tc.store.CheckInput(tc.circuit, 1, tc.hash, c1Fails)
		if err2 != nil {
			t.Fatalf("%s: check returned a call error: %v", tc.circuit, err2)
		}
		if res2.Satisfied || res2.FirstFailure != 1 {
			t.Fatalf("%s: want unsatisfied first_failure=1, got %+v", tc.circuit, res2)
		}
	}
}

// TestEquivalentWriteupsSatisfiableCircuit closes the satisfied side of the
// contract: a genuinely satisfiable three-constraint family stays satisfied
// for one assignment when decorated with zero / modulus-multiple / merging
// terms, and the decoration leaves its definition and hash untouched.
func TestEquivalentWriteupsSatisfiableCircuit(t *testing.T) {
	satBase := `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]},
		{"a":[],"b":[{"wire":1,"coeff":"1"}],"c":[]},
		{"a":[{"wire":1,"coeff":"1"},{"wire":1,"coeff":"-1"}],"b":[{"wire":0,"coeff":"1"}],"c":[]}]}`
	satDecorated := `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"8"},{"wire":2,"coeff":"0"}],"b":[{"wire":2,"coeff":"3"},{"wire":2,"coeff":"-2"}],"c":[{"wire":0,"coeff":"-1"}]},
		{"a":[{"wire":2,"coeff":"7"}],"b":[{"wire":1,"coeff":"-6"},{"wire":2,"coeff":"0"}],"c":[{"wire":0,"coeff":"0"}]},
		{"a":[{"wire":1,"coeff":"3"},{"wire":1,"coeff":"4"}],"b":[{"wire":0,"coeff":"15"},{"wire":0,"coeff":"-14"}],"c":[{"wire":2,"coeff":"5"},{"wire":2,"coeff":"2"}]}]}`

	// Both circuits share one name/version in separate stores, so a hash
	// difference can only come from the constraint content, not the name fold.
	s0, a0 := regressionCircuit(t, "sat", 1, 1, 1, 3, satBase)
	s1, a1 := regressionCircuit(t, "sat", 1, 1, 1, 3, satDecorated)
	if a0.Hash != a1.Hash {
		t.Fatalf("satisfiable family hashes differ: %q vs %q", a0.Hash, a1.Hash)
	}
	d0, err := s0.GetDefinition("sat", 1)
	if err != nil {
		t.Fatal(err)
	}
	d1, err := s1.GetDefinition("sat", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d0, d1) {
		t.Fatalf("satisfiable family definitions differ:\n %+v\n %+v", d0, d1)
	}
	w := Witness{Public: []string{"2"}, Private: []string{"3"}}
	for _, tc := range []struct {
		name     string
		s        *Store
		artifact Artifact
	}{
		{"base", s0, a0}, {"decorated", s1, a1},
	} {
		res, err := tc.s.CheckInput("sat", 1, tc.artifact.Hash, w)
		if err != nil {
			t.Fatalf("%s: check error: %v", tc.name, err)
		}
		if !res.Satisfied || res.FirstFailure != 0 {
			t.Fatalf("%s: want satisfied first_failure=0, got %+v", tc.name, res)
		}
	}
}
