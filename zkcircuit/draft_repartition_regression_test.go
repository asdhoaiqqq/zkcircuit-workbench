package zkcircuit

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

// This file is the end-to-end regression guard for moving the public/private
// boundary of a DRAFT that already carries an imported constraint definition.
// The definition references wires by number only; the circuit record owns the
// partition, so moving the boundary must not require re-importing anything and
// must re-attribute the same wire numbers to their new groups everywhere
// downstream: the version query, the compiled artifact hash and input
// checking.
//
// Shared fixture: modulus 101, three constraints over wires 1..3, wire 0 the
// constant-one wire. Every referenced wire (max 3) stays inside every layout
// with three total inputs, which is exactly the condition under which a
// boundary move is legal without touching the constraint count.
//
//	C1: w1 · w2 = w3                         (uses all three input wires)
//	C2: w2 · w3 = 100·w0                     (constant on the right)
//	C3: (w0 + w1) · (w0 + w3) = 4·w0         (constant 1 on both sides)
//
// With wire values w1=4, w2=5, w3=20 all three hold: 4·5=20, 5·20=100 and
// 5·21 = 105 ≡ 4 (mod 101). C2/C3 can only hold while wire 0 is the constant
// 1: with wire 0 treated as 0 their right-hand sides vanish. The three values
// are distinct, so reading an input at the wire position it does not belong
// to changes the verdict: values (4,20,5) make C1 read 4·20 = 80 ≠ 5.
const repartitionDef = `{"modulus":"101","constraints":[
	{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":3,"coeff":"1"}]},
	{"a":[{"wire":2,"coeff":"1"}],"b":[{"wire":3,"coeff":"1"}],"c":[{"wire":0,"coeff":"100"}]},
	{"a":[{"wire":0,"coeff":"1"},{"wire":1,"coeff":"1"}],"b":[{"wire":0,"coeff":"1"},{"wire":3,"coeff":"1"}],"c":[{"wire":0,"coeff":"4"}]}]}`

// repartitionWireValues are the satisfying wire values keyed by wire number:
// w1=4, w2=5, w3=20.
var repartitionWireValues = []string{"4", "5", "20"}

// seedRepartitionDraft creates a draft with three declared constraints, the
// given public/private split and repartitionDef imported once.
func seedRepartitionDraft(t *testing.T, s *Store, name string, version, pub, priv int) {
	t.Helper()
	if _, err := s.CreateCircuit(Circuit{Name: name, Version: version, Constraints: 3,
		PublicInputs: pub, PrivateInputs: priv, Description: name}); err != nil {
		t.Fatalf("create %s v%d: %v", name, version, err)
	}
	if _, err := s.ImportConstraints(name, version, writeTempJSON(t, repartitionDef)); err != nil {
		t.Fatalf("import %s v%d: %v", name, version, err)
	}
}

func freezeCompile(t *testing.T, s *Store, name string, version int) Artifact {
	t.Helper()
	if _, err := s.FreezeCircuit(name, version); err != nil {
		t.Fatalf("freeze %s v%d: %v", name, version, err)
	}
	a, err := s.CompileCircuit(name, version)
	if err != nil {
		t.Fatalf("compile %s v%d: %v", name, version, err)
	}
	return a
}

// assertVerdictExposesOnlyVerdict pins the result contract: a check verdict
// carries exactly the satisfaction flag, the bound hash and the 1-based
// first-failure index — no field in which a private input value could ride
// along.
func assertVerdictExposesOnlyVerdict(t *testing.T, res CheckResult) {
	t.Helper()
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"Satisfied": true, "Hash": true, "FirstFailure": true}
	if len(fields) != len(want) {
		t.Fatalf("verdict carries %d fields %v, want only satisfaction/hash/first-failure", len(fields), fields)
	}
	for k := range fields {
		if !want[k] {
			t.Fatalf("verdict carries unexpected field %q in %s", k, raw)
		}
	}
}

// TestDraftRepartitionReattachesImportedWiresToNewGroups is the headline
// regression: a draft imported under 1 public + 2 private inputs is moved to
// 2 public + 1 private with the constraint count unchanged. Wire 2 used to be
// the first private input and becomes the second public; wire 3 used to be
// the second private and becomes the first (and only) private. Nothing is
// re-imported; the modulus, constraint order and wire numbering stay as they
// were, wire 0 stays the constant 1, and every downstream verdict is computed
// from the new attribution rather than from the old boundary.
func TestDraftRepartitionReattachesImportedWiresToNewGroups(t *testing.T) {
	s := openTestStore(t)
	seedRepartitionDraft(t, s, "split", 1, 1, 2)

	defBefore, err := s.GetDefinition("split", 1)
	if err != nil {
		t.Fatal(err)
	}

	// Move the boundary via the partial entry point: only the two input counts
	// are supplied, so the constraint count (and the imported definition) are
	// retained. The same draft-modification rule through the whole-record
	// entry point is exercised in TestRepartitionToZeroGroupBoundaryStillChecks.
	got, err := s.UpdateCircuitPartial("split", 1, PartialCircuit{
		PublicInputs: intPtr(2), PrivateInputs: intPtr(1),
	})
	if err != nil {
		t.Fatalf("moving the boundary inside the referenced wire range must succeed: %v", err)
	}
	if got.Constraints != 3 || got.PublicInputs != 2 || got.PrivateInputs != 1 {
		t.Fatalf("updated query must show the new group counts: %+v", got)
	}
	if got.Frozen {
		t.Fatalf("boundary move must not freeze the draft: %+v", got)
	}
	stored, err := s.GetCircuit("split", 1)
	if err != nil {
		t.Fatal(err)
	}
	if stored != got {
		t.Fatalf("stored record %+v != returned %+v", stored, got)
	}

	// The imported definition is byte-for-byte the same logical document:
	// same modulus, same constraint order, same wire numbers (including the
	// wire-0 constant terms). The user never re-imported it.
	defAfter, err := s.GetDefinition("split", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(defBefore, defAfter) {
		t.Fatalf("definition changed despite no re-import:\nbefore %+v\nafter  %+v", defBefore, defAfter)
	}
	if defAfter.Modulus != "101" || len(defAfter.Constraints) != 3 {
		t.Fatalf("modulus/constraint count drifted: %+v", defAfter)
	}
	if c2rhs := defAfter.Constraints[1].C; !reflect.DeepEqual(c2rhs, []Term{{Wire: 0, Coeff: "100"}}) {
		t.Fatalf("C2 constant term on wire 0 changed: %+v", c2rhs)
	}

	artifact := freezeCompile(t, s, "split", 1)

	// Input checking needs neither a trusted setup nor a proof job.
	if setup, found, err := s.GetSetup("split", 1); err != nil || found {
		t.Fatalf("input check must not depend on a trusted setup: %+v found=%v err=%v", setup, found, err)
	}
	if jobs, err := s.ListJobs(); err != nil || len(jobs) != 0 {
		t.Fatalf("input check must not need a proof job: %+v err=%v", jobs, err)
	}

	// Snapshot the committed file: every check below is read-only, including
	// the unsatisfied and rejected ones.
	dataPath := filepath.Join(s.Dir(), dirDataFile)
	committed, err := readDataFile(t, s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		left, rerr := readDataFile(t, s.Dir())
		if rerr != nil {
			t.Fatal(rerr)
		}
		if string(left) != string(committed) {
			t.Fatalf("input checking rewrote %s", dataPath)
		}
	})

	// The same wire values, supplied under the NEW attribution: w1=4,w2=5 are
	// the two public inputs; w3=20 is the one private input.
	good := Witness{Public: []string{"4", "5"}, Private: []string{"20"}}
	res, err := s.CheckInput("split", 1, artifact.Hash, good)
	if err != nil {
		t.Fatalf("new-attribution satisfying witness: %v", err)
	}
	if !res.Satisfied || res.FirstFailure != 0 || res.Hash != artifact.Hash {
		t.Fatalf("want satisfied with the artifact's own hash, got %+v", res)
	}
	assertVerdictExposesOnlyVerdict(t, res)

	// Congruent decimal representations of the same residues satisfy too.
	congruent := Witness{Public: []string{"-97", "5"}, Private: []string{"-81"}} // -97≡4, -81≡20 mod 101
	res, err = s.CheckInput("split", 1, artifact.Hash, congruent)
	if err != nil || !res.Satisfied || res.FirstFailure != 0 {
		t.Fatalf("congruent new-attribution witness: %+v %v", res, err)
	}

	// The same three NUMBERS handed over in the OLD one-public/two-private
	// layout must be an input format error even though the total count (3) is
	// right: the boundary moved, so the group lengths no longer match. It must
	// never be evaluated with values shifted to the wrong wires.
	for _, old := range []Witness{
		{Public: []string{"4"}, Private: []string{"5", "20"}},
		{Public: []string{"4", "5", "20"}, Private: nil}, // 3/0 grouping
		{Public: nil, Private: []string{"4", "5", "20"}}, // 0/3 grouping
	} {
		if _, err := s.CheckInput("split", 1, artifact.Hash, old); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("old-boundary witness %+v against a 2-public/1-private version: want input format error, got %v", old, err)
		}
	}
	// The file entry point enforces the same boundary.
	oldDoc := writeTempJSON(t, `{"public":["4"],"private":["5","20"]}`)
	if _, err := s.CheckInputFile("split", 1, artifact.Hash, oldDoc); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("file entry with old layout: want input format error, got %v", err)
	}

	// Distinct wire values make misplacement observable: putting w3's value in
	// public slot 2 and w2's value in the private slot gives w1=4,w2=20,w3=5,
	// so C1 reads 4·20 = 80 ≠ 5. The first failure is genuinely C1 rather than
	// a length complaint — the arrays ARE 2/1, the values are at wrong wires.
	swapped := Witness{Public: []string{"4", "20"}, Private: []string{"5"}}
	res, err = s.CheckInput("split", 1, artifact.Hash, swapped)
	if err != nil {
		t.Fatalf("misplaced-values witness should complete, not fail the call: %v", err)
	}
	if res.Satisfied || res.FirstFailure != 1 || res.Hash != artifact.Hash {
		t.Fatalf("wrong wire positions must change the verdict: got %+v", res)
	}
	assertVerdictExposesOnlyVerdict(t, res)

	// Unsatisfied witnesses under the correct new attribution locate the
	// constraint that actually fails first (1-based), proving evaluation reads
	// each value at its post-repartition wire number.
	badCases := []struct {
		name        string
		w           Witness
		wantFailure int
	}{
		{"C1 fails: w3=19 breaks 4·5=w3", Witness{Public: []string{"4", "5"}, Private: []string{"19"}}, 1},
		{"C2 fails: 2·20=40≠100 while C1 holds", Witness{Public: []string{"10", "2"}, Private: []string{"20"}}, 2},
		{"C3 fails: 83·26≡37≠4 while C1,C2 hold", Witness{Public: []string{"82", "4"}, Private: []string{"25"}}, 3},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := s.CheckInput("split", 1, artifact.Hash, tc.w)
			if err != nil {
				t.Fatalf("unsatisfied is a completed check: %v", err)
			}
			if res.Satisfied || res.FirstFailure != tc.wantFailure || res.Hash != artifact.Hash {
				t.Fatalf("want first_failure=%d with bound hash, got %+v", tc.wantFailure, res)
			}
		})
	}

	// The frozen version stays unmodifiable after the boundary move.
	if _, err := s.UpdateCircuit(Circuit{Name: "split", Version: 1, Constraints: 3,
		PublicInputs: 2, PrivateInputs: 1, Description: "later"}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("whole update after freeze: want frozen, got %v", err)
	}
	if _, err := s.UpdateCircuitPartial("split", 1, PartialCircuit{
		Description: stringPtr("later"),
	}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("partial update after freeze: want frozen, got %v", err)
	}
	final, _ := s.GetCircuit("split", 1)
	if final.PublicInputs != 2 || final.PrivateInputs != 1 || final.Constraints != 3 || final.Description != "split" {
		t.Fatalf("frozen repartitioned version changed after a refused edit: %+v", final)
	}
}

// TestRepartitionedArtifactHashIsPartitionBoundAcrossDirectories compiles the
// same name/version/modulus/constraint/total-input circuit in two independent
// data directories that differ ONLY in the public/private split. The compiled
// hashes must differ, each hash must check inputs grouped for its own
// partition, and the other partition's hash must fail as an artifact mismatch
// — before any satisfaction conclusion and, by the binding order, even before
// a malformed witness is noticed.
func TestRepartitionedArtifactHashIsPartitionBoundAcrossDirectories(t *testing.T) {
	dirA := filepath.Join(t.TempDir(), "a")
	dirB := filepath.Join(t.TempDir(), "b")
	sA, err := Open(dirA)
	if err != nil {
		t.Fatal(err)
	}
	defer sA.Close()
	sB, err := Open(dirB)
	if err != nil {
		t.Fatal(err)
	}
	defer sB.Close()

	seedRepartitionDraft(t, sA, "split", 1, 1, 2) // wire 2 private #1, wire 3 private #2
	seedRepartitionDraft(t, sB, "split", 1, 2, 1) // wire 2 public #2,    wire 3 private #1
	hA := freezeCompile(t, sA, "split", 1).Hash
	hB := freezeCompile(t, sB, "split", 1).Hash
	if hA == hB {
		t.Fatalf("partitions 1/2 and 2/1 compiled to the same hash %q", hA)
	}

	// Each artifact checks its own partition's attribution of the same wire
	// values: w1=4, w2=5, w3=20.
	res, err := sA.CheckInput("split", 1, hA, Witness{Public: []string{"4"}, Private: []string{"5", "20"}})
	if err != nil || !res.Satisfied || res.Hash != hA {
		t.Fatalf("1/2 artifact with 1/2 attribution: %+v %v", res, err)
	}
	res, err = sB.CheckInput("split", 1, hB, Witness{Public: []string{"4", "5"}, Private: []string{"20"}})
	if err != nil || !res.Satisfied || res.Hash != hB {
		t.Fatalf("2/1 artifact with 2/1 attribution: %+v %v", res, err)
	}

	// The other partition's hash is a plain artifact mismatch: no verdict of
	// any kind (not even an "unsatisfied" zero value) is returned.
	goodB := Witness{Public: []string{"4", "5"}, Private: []string{"20"}}
	res, err = sB.CheckInput("split", 1, hA, goodB)
	if !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("checking the 2/1 circuit with the 1/2 hash: want artifact mismatch, got %v", err)
	}
	if res != (CheckResult{}) {
		t.Fatalf("artifact mismatch must not carry a satisfaction conclusion, got %+v", res)
	}
	// The file entry point reports the same mismatch.
	path := writeTempJSON(t, `{"public":["4","5"],"private":["20"]}`)
	if _, err := sB.CheckInputFile("split", 1, hA, path); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("file entry foreign hash: want artifact mismatch, got %v", err)
	}
	// Binding is settled before witness validation: a foreign hash beats even
	// an old-layout (total-correct, group-wrong) witness.
	oldLayout := Witness{Public: []string{"4"}, Private: []string{"5", "20"}}
	if _, err := sB.CheckInput("split", 1, hA, oldLayout); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("foreign hash must beat the old-layout witness error, got %v", err)
	}

	// The partition-bound hash survives reopening the data directory and keeps
	// checking inputs the same way.
	if err := sB.Close(); err != nil {
		t.Fatal(err)
	}
	sB2, err := Open(dirB)
	if err != nil {
		t.Fatal(err)
	}
	defer sB2.Close()
	reloaded, err := sB2.GetArtifact("split", 1)
	if err != nil || reloaded.Hash != hB {
		t.Fatalf("artifact after reopen: %+v %v", reloaded, err)
	}
	res, err = sB2.CheckInput("split", 1, hB, goodB)
	if err != nil || !res.Satisfied || res.FirstFailure != 0 || res.Hash != hB {
		t.Fatalf("check after reopen changed: %+v %v", res, err)
	}
	// The other directory's hash still mismatches after the reload.
	if _, err := sB2.CheckInput("split", 1, hA, goodB); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("foreign hash after reopen: want artifact mismatch, got %v", err)
	}
}

// TestRepartitionToZeroGroupBoundaryStillChecksByWireNumber covers moving the
// boundary all the way to one end: a group whose declared count is zero is
// empty, and checking still wires the other group's values by number
// (starting at wire 1; wire 0 remains the constant 1).
func TestRepartitionToZeroGroupBoundaryStillChecksByWireNumber(t *testing.T) {
	s := openTestStore(t)

	// Move 1/2 -> 0/3 on the partial path: the explicit zero PUBLIC count
	// must be applied, not read as "field omitted".
	seedRepartitionDraft(t, s, "privonly", 1, 1, 2)
	got, err := s.UpdateCircuitPartial("privonly", 1, PartialCircuit{
		PublicInputs: intPtr(0), PrivateInputs: intPtr(3),
	})
	if err != nil {
		t.Fatalf("moving the boundary to 0/3 must be legal: %v", err)
	}
	if got.PublicInputs != 0 || got.PrivateInputs != 3 || got.Constraints != 3 {
		t.Fatalf("0/3 update wrong: %+v", got)
	}
	a03 := freezeCompile(t, s, "privonly", 1)

	// w1=4,w2=5,w3=20 all arrive through the private group; public is empty.
	for _, w := range []Witness{
		{Public: nil, Private: repartitionWireValues},
		{Public: []string{}, Private: repartitionWireValues},
	} {
		res, err := s.CheckInput("privonly", 1, a03.Hash, w)
		if err != nil || !res.Satisfied || res.FirstFailure != 0 || res.Hash != a03.Hash {
			t.Fatalf("0/3 witness %+v: %+v %v", w, res, err)
		}
	}
	doc := writeTempJSON(t, `{"public":[],"private":["4","5","20"]}`)
	if res, err := s.CheckInputFile("privonly", 1, a03.Hash, doc); err != nil || !res.Satisfied {
		t.Fatalf("0/3 file witness: %+v %v", res, err)
	}
	// Lengths are still enforced per group at the zero boundary.
	for _, w := range []Witness{
		{Public: []string{"4"}, Private: []string{"5", "20"}}, // a value in the empty group
		{Public: nil, Private: []string{"4", "5"}},            // non-empty group too short
	} {
		if _, err := s.CheckInput("privonly", 1, a03.Hash, w); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("0/3 wrong lengths %+v: want input format error, got %v", w, err)
		}
	}

	// Move 1/2 -> 3/0 through the whole-record replacement entry point.
	seedRepartitionDraft(t, s, "pubonly", 1, 1, 2)
	got, err = s.UpdateCircuit(Circuit{Name: "pubonly", Version: 1, Constraints: 3,
		PublicInputs: 3, PrivateInputs: 0, Description: "pubonly"})
	if err != nil {
		t.Fatalf("moving the boundary to 3/0 must be legal: %v", err)
	}
	if got.PublicInputs != 3 || got.PrivateInputs != 0 {
		t.Fatalf("3/0 update wrong: %+v", got)
	}
	a30 := freezeCompile(t, s, "pubonly", 1)
	for _, w := range []Witness{
		{Public: repartitionWireValues, Private: nil},
		{Public: repartitionWireValues, Private: []string{}},
	} {
		res, err := s.CheckInput("pubonly", 1, a30.Hash, w)
		if err != nil || !res.Satisfied || res.FirstFailure != 0 || res.Hash != a30.Hash {
			t.Fatalf("3/0 witness %+v: %+v %v", w, res, err)
		}
	}
	doc = writeTempJSON(t, `{"public":["4","5","20"],"private":[]}`)
	if res, err := s.CheckInputFile("pubonly", 1, a30.Hash, doc); err != nil || !res.Satisfied {
		t.Fatalf("3/0 file witness: %+v %v", res, err)
	}
	// A value in the empty private group is a format error even with three
	// values already present publicly.
	if _, err := s.CheckInput("pubonly", 1, a30.Hash,
		Witness{Public: repartitionWireValues, Private: []string{"1"}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("3/0 non-empty private group: want input format error, got %v", err)
	}

	// Values still land on their numbered wires at the boundary: (4,20,5)
	// makes C1 4·20=80≠5 at either end.
	if res, err := s.CheckInput("pubonly", 1, a30.Hash,
		Witness{Public: []string{"4", "20", "5"}}); err != nil || res.Satisfied || res.FirstFailure != 1 {
		t.Fatalf("3/0 misplaced values must fail at C1: %+v %v", res, err)
	}
	if res, err := s.CheckInput("privonly", 1, a03.Hash,
		Witness{Private: []string{"4", "20", "5"}}); err != nil || res.Satisfied || res.FirstFailure != 1 {
		t.Fatalf("0/3 misplaced values must fail at C1: %+v %v", res, err)
	}
}

// TestRepartitionRejectedWhenImportedWiresWouldFallOutside closes the other
// half of the rule: keeping the constraint count but shrinking the layout past
// a referenced wire refuses the whole edit (both update entry points), leaves
// the imported definition usable and keeps the draft compilable as it was.
func TestRepartitionRejectedWhenImportedWiresWouldFallOutside(t *testing.T) {
	s := openTestStore(t)
	seedRepartitionDraft(t, s, "c", 1, 2, 1) // references wire 3

	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PublicInputs: intPtr(2), PrivateInputs: intPtr(0), // wire 3 falls outside
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("shrinking past wire 3 (partial): want invalid argument, got %v", err)
	}
	if _, err := s.UpdateCircuit(Circuit{Name: "c", Version: 1, Constraints: 3,
		PublicInputs: 1, PrivateInputs: 0, // only wire 1 remains
		Description: "c"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("shrinking past wire 3 (full): want invalid argument, got %v", err)
	}
	// Drifting the declared constraint count away from the imported one is
	// refused just as before the boundary feature existed.
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		Constraints: intPtr(2),
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("constraint-count drift: want invalid argument, got %v", err)
	}

	// Nothing leaked: counts, definition and draft status stay intact.
	stored, _ := s.GetCircuit("c", 1)
	if stored.PublicInputs != 2 || stored.PrivateInputs != 1 || stored.Constraints != 3 || stored.Frozen {
		t.Fatalf("refused repartition leaked into the draft: %+v", stored)
	}
	def, err := s.GetDefinition("c", 1)
	if err != nil || def.Modulus != "101" || len(def.Constraints) != 3 {
		t.Fatalf("imported definition damaged by refused edits: %+v %v", def, err)
	}

	// The untouched draft still freezes and compiles under its original
	// partition, whose own artifact checks the values by wire number.
	artifact := freezeCompile(t, s, "c", 1)
	res, err := s.CheckInput("c", 1, artifact.Hash,
		Witness{Public: []string{"4", "5"}, Private: []string{"20"}})
	if err != nil || !res.Satisfied {
		t.Fatalf("original 2/1 layout still checks after refused edits: %+v %v", res, err)
	}
}
