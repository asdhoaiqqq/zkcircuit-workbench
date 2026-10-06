package zkcircuit

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

// This file is the regression guard for the GetDefinition copy contract.
//
// GetDefinition hands a caller the canonical definition as a value the caller
// is free to treat as its own editable draft: changing the modulus, a term's
// wire or coefficient, any of the a/b/c term arrays (including appending,
// truncating or nil-ing them) or the order and length of the constraint array
// must never reach the registered version. A second copy taken earlier, a
// fresh GetDefinition after the mutation, the frozen version's definition and
// its already-saved artifact hash, and the verdicts of input checks all stay
// bound to the original content. A legal re-import in the draft stage exposes
// the new definition while every previously taken copy keeps its own snapshot.
//
// The definition below is deliberately built from non-canonical input so the
// tests also pin what a query must return after import normalization:
// duplicate wire references merged, coefficients reduced modulo the field,
// zero terms dropped, the constraint order preserved and explicit empty side
// arrays returned as empty (never null) slices.
//
// Layout: wire 0 is the constant 1, wire 1 is the one public input, wires 2
// and 3 are the two private inputs. Four constraints:
//
//	C1: w1 · w2 = 6
//	C2: (2·w3 - 1·w3) · 1 = 11        (canonical: w3 · 1 = 4, so w3 = 4)
//	C3: 0 · (w1 + w2) = 0
//	C4: ((-1 + 8)·1) · 0 = 0          (the two wire-0 terms merge to 0 mod 7
//	                                   and are dropped: side a stays empty)
const isolatedDefDoc = `{"modulus":"7","constraints":[
	{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]},
	{"a":[{"wire":3,"coeff":"2"},{"wire":3,"coeff":"-1"}],"b":[{"wire":0,"coeff":"1"}],"c":[{"wire":0,"coeff":"11"}]},
	{"a":[],"b":[{"wire":1,"coeff":"1"},{"wire":2,"coeff":"1"}],"c":[]},
	{"a":[{"wire":0,"coeff":"-1"},{"wire":0,"coeff":"8"}],"b":[],"c":[]}]}`

// Witnesses against the frozen canonical definition.
var (
	isolatedSatisfied = Witness{Public: []string{"2"}, Private: []string{"3", "4"}} // 2·3=6; w3=4
	isolatedFailFirst = Witness{Public: []string{"2"}, Private: []string{"4", "4"}} // C1: 2·4≡1≠6
	isolatedFailC2    = Witness{Public: []string{"2"}, Private: []string{"3", "5"}} // C1 holds; C2: 5≠4
)

// isolatedCanonical is exactly what GetDefinition must return after the
// importer normalizes isolatedDefDoc. Empty sides are non-nil empty slices:
// the zero linear combination reads back as an explicit array, never as null.
func isolatedCanonical() Definition {
	return Definition{
		Modulus: "7",
		Constraints: []Constraint{
			{
				A: []Term{{Wire: 1, Coeff: "1"}},
				B: []Term{{Wire: 2, Coeff: "1"}},
				C: []Term{{Wire: 0, Coeff: "6"}},
			},
			{
				A: []Term{{Wire: 3, Coeff: "1"}}, // 2 + (-1) merged
				B: []Term{{Wire: 0, Coeff: "1"}},
				C: []Term{{Wire: 0, Coeff: "4"}}, // 11 reduced mod 7
			},
			{
				A: []Term{},
				B: []Term{{Wire: 1, Coeff: "1"}, {Wire: 2, Coeff: "1"}},
				C: []Term{},
			},
			{
				A: []Term{}, // -1 + 8 = 7 ≡ 0, both terms dropped
				B: []Term{},
				C: []Term{},
			},
		},
	}
}

func mustGetDefinition(t *testing.T, s *Store, name string, version int) Definition {
	t.Helper()
	def, err := s.GetDefinition(name, version)
	if err != nil {
		t.Fatalf("GetDefinition %q v%d: %v", name, version, err)
	}
	return def
}

func assertDefinitionEqual(t *testing.T, got, want Definition, context string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s:\n got %#v\nwant %#v", context, got, want)
	}
}

func seedIsolatedDraft(t *testing.T, s *Store, name string, version int) {
	t.Helper()
	if _, err := s.CreateCircuit(Circuit{
		Name: name, Version: version, Constraints: 4,
		PublicInputs: 1, PrivateInputs: 2, Description: "isolation",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints(name, version, writeTempJSON(t, isolatedDefDoc)); err != nil {
		t.Fatalf("import: %v", err)
	}
}

// TestGetDefinitionReturnsCanonicalNormalization pins the query result after
// an import: duplicate wire numbers merged, coefficients reduced under the
// modulus, merged-to-zero terms dropped, constraint order preserved, and empty
// sides kept as explicit empty arrays.
func TestGetDefinitionReturnsCanonicalNormalization(t *testing.T) {
	s := openTestStore(t)
	seedIsolatedDraft(t, s, "c", 1)

	got := mustGetDefinition(t, s, "c", 1)
	assertDefinitionEqual(t, got, isolatedCanonical(), "query must return the imported, normalized definition")
}

// TestGetDefinitionCopiesAreIndependent is the central use case: two
// definitions fetched from the same version must not share any mutable
// backing. Every shape of edit a caller can make on one fetched copy — the
// modulus, a term wire or coefficient, the term arrays, the constraint array
// — leaves the earlier copy and the next query on the original content.
func TestGetDefinitionCopiesAreIndependent(t *testing.T) {
	mutations := []struct {
		name string
		edit func(*Definition)
	}{
		{"change modulus", func(d *Definition) { d.Modulus = "11" }},
		{"change a term wire", func(d *Definition) { d.Constraints[0].A[0].Wire = 99 }},
		{"change a term coefficient", func(d *Definition) { d.Constraints[0].A[0].Coeff = "5" }},
		{"mutate deep term element", func(d *Definition) { d.Constraints[1].A[0].Wire = 7 }},
		{"append term to a side", func(d *Definition) {
			d.Constraints[1].B = append(d.Constraints[1].B, Term{Wire: 99, Coeff: "9"})
		}},
		{"nil a populated side", func(d *Definition) { d.Constraints[2].B = nil }},
		{"overwrite an empty side with a term", func(d *Definition) {
			d.Constraints[3].A = []Term{{Wire: 1, Coeff: "1"}}
		}},
		{"replace a side wholesale", func(d *Definition) {
			d.Constraints[0].C = []Term{{Wire: 2, Coeff: "2"}}
		}},
		{"reorder constraints", func(d *Definition) {
			d.Constraints[0], d.Constraints[2] = d.Constraints[2], d.Constraints[0]
		}},
		{"truncate the constraint array", func(d *Definition) {
			d.Constraints = d.Constraints[:1]
		}},
		{"append a constraint", func(d *Definition) {
			d.Constraints = append(d.Constraints, Constraint{
				A: []Term{{Wire: 1, Coeff: "1"}}, B: []Term{}, C: []Term{},
			})
		}},
		{"replace the whole constraint array", func(d *Definition) {
			d.Constraints = []Constraint{{A: []Term{}, B: []Term{}, C: []Term{}}}
		}},
	}

	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			seedIsolatedDraft(t, s, "c", 1)

			want := isolatedCanonical()
			first := mustGetDefinition(t, s, "c", 1)
			second := mustGetDefinition(t, s, "c", 1)
			assertDefinitionEqual(t, first, want, "the two fetched copies start equal")
			assertDefinitionEqual(t, second, want, "the two fetched copies start equal")

			tc.edit(&second)

			// The earlier fetched copy keeps its snapshot, including which
			// sides were empty versus populated.
			assertDefinitionEqual(t, first, want, "editing one copy changed the earlier copy")
			// The registered definition is untouched: a fresh query still
			// answers with the original normalized content.
			assertDefinitionEqual(t, mustGetDefinition(t, s, "c", 1), want,
				"editing a fetched copy changed the registered definition")
		})
	}
}

// TestImportResultIsAnIndependentCopy covers the value ImportConstraints hands
// back: it is the same editable-copy contract, so mutating it must not change
// what the store later returns.
func TestImportResultIsAnIndependentCopy(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(Circuit{
		Name: "c", Version: 1, Constraints: 4,
		PublicInputs: 1, PrivateInputs: 2, Description: "isolation",
	}); err != nil {
		t.Fatal(err)
	}
	imported, err := s.ImportConstraints("c", 1, writeTempJSON(t, isolatedDefDoc))
	if err != nil {
		t.Fatal(err)
	}
	want := isolatedCanonical()
	assertDefinitionEqual(t, imported, want, "import result is the normalized definition")

	imported.Modulus = "13"
	imported.Constraints[0].A[0].Coeff = "99"
	imported.Constraints = imported.Constraints[:1]

	assertDefinitionEqual(t, mustGetDefinition(t, s, "c", 1), want,
		"mutating the import result changed the registered definition")
}

// freezeIsolated seeds, freezes and compiles the isolated definition,
// returning the artifact. The caller owns the store's lifecycle.
func freezeIsolated(t *testing.T, s *Store, name string, version int) Artifact {
	t.Helper()
	seedIsolatedDraft(t, s, name, version)
	if _, err := s.FreezeCircuit(name, version); err != nil {
		t.Fatal(err)
	}
	artifact, err := s.CompileCircuit(name, version)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return artifact
}

func assertVerdict(t *testing.T, s *Store, name string, version int, hash string,
	w Witness, satisfied bool, firstFailure int) {
	t.Helper()
	res, err := s.CheckInput(name, version, hash, w)
	if err != nil {
		t.Fatalf("a completed check (satisfied or not) must not be a call error: %v", err)
	}
	if res.Satisfied != satisfied || res.FirstFailure != firstFailure {
		t.Fatalf("verdict drifted: got satisfied=%v first_failure=%d, want satisfied=%v first_failure=%d",
			res.Satisfied, res.FirstFailure, satisfied, firstFailure)
	}
	if res.Hash != hash {
		t.Fatalf("verdict must stay bound to the original artifact hash %q, got %q", hash, res.Hash)
	}
}

// TestFrozenCompiledDefinitionCannotDriftFromCopies takes the copy contract
// through freezing and compilation. Copies taken before and after the freeze
// can be edited freely; the frozen definition must not drift, recompilation
// must stay idempotent on the saved hash, and input checks must keep their
// original conclusions (including the first failing constraint's number) on
// inputs that satisfied and inputs that did not.
func TestFrozenCompiledDefinitionCannotDriftFromCopies(t *testing.T) {
	s := openTestStore(t)

	seedIsolatedDraft(t, s, "c", 1)
	before1 := mustGetDefinition(t, s, "c", 1)
	before2 := mustGetDefinition(t, s, "c", 1)
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	artifact, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	after1 := mustGetDefinition(t, s, "c", 1)
	after2 := mustGetDefinition(t, s, "c", 1)
	want := isolatedCanonical()

	// Edit copies from both sides of the freeze, in every structural way.
	before1.Modulus = "997"
	before1.Constraints[0].A[0].Wire = 42
	before2.Constraints[0], before2.Constraints[3] = before2.Constraints[3], before2.Constraints[0]
	before2.Constraints = before2.Constraints[:2]
	after1.Constraints[1].A[0].Coeff = "100000"
	after1.Constraints[2].B = nil
	after2.Constraints = append(after2.Constraints, Constraint{})
	after2.Constraints[3].A = []Term{{Wire: 99, Coeff: "-7"}}

	assertDefinitionEqual(t, mustGetDefinition(t, s, "c", 1), want,
		"frozen definition drifted after its fetched copies were edited")

	// The saved artifact hash is unchanged and recompilation stays idempotent.
	again, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if again != artifact {
		t.Fatalf("recompilation after copy edits changed the artifact: %+v vs %+v", again, artifact)
	}
	if got, err := s.GetArtifact("c", 1); err != nil || got.Hash != artifact.Hash {
		t.Fatalf("saved artifact hash changed: %+v %v", got, err)
	}

	// Original check conclusions survive: satisfaction, failure at C1 and
	// failure at C2 (the first failing constraint number is preserved).
	assertVerdict(t, s, "c", 1, artifact.Hash, isolatedSatisfied, true, 0)
	assertVerdict(t, s, "c", 1, artifact.Hash, isolatedFailFirst, false, 1)
	assertVerdict(t, s, "c", 1, artifact.Hash, isolatedFailC2, false, 2)
}

// TestCopyIsolationSurvivesReopen repeats the frozen-stage mutation against a
// store that is closed and reopened: the on-disk definition and the saved
// artifact must still answer with the original content, hash and verdicts.
func TestCopyIsolationSurvivesReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	artifact := freezeIsolated(t, s, "c", 1)
	fetched := mustGetDefinition(t, s, "c", 1)

	fetched.Modulus = "11"
	fetched.Constraints[0].A[0].Coeff = "99"
	fetched.Constraints[0], fetched.Constraints[1] = fetched.Constraints[1], fetched.Constraints[0]
	fetched.Constraints[2].B = nil
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	assertDefinitionEqual(t, mustGetDefinition(t, s2, "c", 1), isolatedCanonical(),
		"edited copy reached the persisted definition across reopen")
	if got, err := s2.GetArtifact("c", 1); err != nil || got.Hash != artifact.Hash {
		t.Fatalf("artifact hash changed across reopen: %+v %v", got, err)
	}
	assertVerdict(t, s2, "c", 1, artifact.Hash, isolatedSatisfied, true, 0)
	assertVerdict(t, s2, "c", 1, artifact.Hash, isolatedFailC2, false, 2)
}

// TestReimportExposesNewDefinitionWhileOldCopiesStaySnapshots covers the draft
// re-import flow: after a legal re-import a new query reflects the new
// definition, but copies taken under the old definition keep their own content
// and editing them (or the new copy) cannot overwrite either definition.
func TestReimportExposesNewDefinitionWhileOldCopiesStaySnapshots(t *testing.T) {
	s := openTestStore(t)
	seedIsolatedDraft(t, s, "c", 1)

	oldWant := isolatedCanonical()
	old1 := mustGetDefinition(t, s, "c", 1)
	old2 := mustGetDefinition(t, s, "c", 1)

	const replacementDoc = `{"modulus":"11","constraints":[
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":0,"coeff":"1"}],"c":[{"wire":1,"coeff":"1"}]},
		{"a":[],"b":[],"c":[]},
		{"a":[],"b":[],"c":[]},
		{"a":[],"b":[],"c":[]}]}`
	reimported, err := s.ImportConstraints("c", 1, writeTempJSON(t, replacementDoc))
	if err != nil {
		t.Fatalf("legal re-import: %v", err)
	}
	newWant := Definition{
		Modulus: "11",
		Constraints: []Constraint{
			{A: []Term{{Wire: 1, Coeff: "1"}}, B: []Term{{Wire: 0, Coeff: "1"}}, C: []Term{{Wire: 1, Coeff: "1"}}},
			{A: []Term{}, B: []Term{}, C: []Term{}},
			{A: []Term{}, B: []Term{}, C: []Term{}},
			{A: []Term{}, B: []Term{}, C: []Term{}},
		},
	}
	assertDefinitionEqual(t, reimported, newWant, "re-import result is the new normalized definition")
	assertDefinitionEqual(t, mustGetDefinition(t, s, "c", 1), newWant,
		"a query after re-import must reflect the new definition")

	// Old snapshots keep the content they had when they were fetched.
	assertDefinitionEqual(t, old1, oldWant, "old copy changed after re-import")
	assertDefinitionEqual(t, old2, oldWant, "old copy changed after re-import")

	// Editing an old snapshot cannot overwrite the newly imported definition.
	old1.Modulus = "13"
	old1.Constraints[0].A[0] = Term{Wire: 99, Coeff: "99"}
	old1.Constraints = nil
	assertDefinitionEqual(t, mustGetDefinition(t, s, "c", 1), newWant,
		"editing an old copy overwrote the re-imported definition")
	assertDefinitionEqual(t, old2, oldWant, "editing one old copy changed the other old copy")

	// Editing a snapshot of the new definition cannot reach the store either.
	new1 := mustGetDefinition(t, s, "c", 1)
	new1.Modulus = "3"
	new1.Constraints[0], new1.Constraints[3] = new1.Constraints[3], new1.Constraints[0]
	assertDefinitionEqual(t, mustGetDefinition(t, s, "c", 1), newWant,
		"editing the new copy overwrote the new definition")
	assertDefinitionEqual(t, old2, oldWant, "editing the new copy reached an old copy")
}

// TestGetDefinitionMissingAndUnknownNeverYieldsEditableEmpty pins the
// not-found contract: a version without an imported definition reports
// "constraint definition missing", an unknown version "not found", and no
// editable empty definition is ever handed out for either case.
func TestGetDefinitionMissingAndUnknownNeverYieldsEditableEmpty(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(Circuit{Name: "bare", Version: 1, Constraints: 1,
		PublicInputs: 0, PrivateInputs: 0, Description: "d"}); err != nil {
		t.Fatal(err)
	}

	def, err := s.GetDefinition("bare", 1)
	if !errors.Is(err, ErrDefinitionMissing) {
		t.Fatalf("version without definition: want constraint definition missing, got %v", err)
	}
	if !reflect.DeepEqual(def, Definition{}) {
		t.Fatalf("no editable definition may be returned alongside the missing error, got %#v", def)
	}

	if _, err := s.GetDefinition("ghost", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown version: want not found, got %v", err)
	}

	// Importing on the bare version is still the only way to obtain a
	// definition; the missing result did not double as a starting point.
	if _, err := s.ImportConstraints("bare", 1, writeTempJSON(t,
		`{"modulus":"7","constraints":[{"a":[],"b":[],"c":[]}]}`)); err != nil {
		t.Fatalf("import after missing query: %v", err)
	}
	got := mustGetDefinition(t, s, "bare", 1)
	if got.Modulus != "7" || len(got.Constraints) != 1 {
		t.Fatalf("definition after import wrong: %#v", got)
	}
}

// TestCheckVerdictsStayDistinctFromInputFormatErrors locks the three-way
// distinction the copy edits must not blur: a satisfied and an unsatisfied
// input are both normal completed checks (no call error), while a malformed
// witness is the input-format error class. Mutating fetched copies before the
// checks changes none of this.
func TestCheckVerdictsStayDistinctFromInputFormatErrors(t *testing.T) {
	s := openTestStore(t)
	artifact := freezeIsolated(t, s, "c", 1)

	for _, d := range []Definition{
		mustGetDefinition(t, s, "c", 1),
		mustGetDefinition(t, s, "c", 1),
	} {
		d.Modulus = "2"
		d.Constraints[0].A[0].Coeff = "0"
		d.Constraints = d.Constraints[:1]
	}

	assertVerdict(t, s, "c", 1, artifact.Hash, isolatedSatisfied, true, 0)
	assertVerdict(t, s, "c", 1, artifact.Hash, isolatedFailFirst, false, 1)

	for _, bad := range []Witness{
		{Public: []string{"2"}, Private: []string{"3"}},      // wrong private count
		{Public: []string{"x"}, Private: []string{"3", "4"}}, // illegal decimal
		{Public: []string{"2"}, Private: []string{"3", ""}},  // empty value
		{Public: nil, Private: []string{"3", "4"}},           // missing public value
	} {
		if _, err := s.CheckInput("c", 1, artifact.Hash, bad); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("malformed witness %+v: want input format error, got %v", bad, err)
		}
	}
}
