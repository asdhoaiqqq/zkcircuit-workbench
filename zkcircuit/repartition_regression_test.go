package zkcircuit

import (
	"errors"
	"path/filepath"
	"testing"
)

// Regression coverage for changing the public/private boundary of a draft
// that already carries an imported constraint definition.
//
// The fixture starts as 1 public + 2 private (wires 1 / 2,3) and is
// repartitioned to 2 public + 1 private (wires 1,2 / 3): the constraint
// count and every referenced wire stay valid, only the ownership of wires 2
// and 3 changes. The tests pin that:
//
//   - the update succeeds as one atomic change and the modulus, constraint
//     order and wire numbering (wire 0 = constant 1) stay exactly as imported,
//   - freezing, compiling and input checking then follow the NEW partition,
//     with no re-import, trusted setup or proof job,
//   - the artifact hash reflects the partition: circuits in independent data
//     directories identical in name, version, modulus, constraints and total
//     input count but split differently compile to different hashes, and a
//     foreign partition's hash is an explicit artifact mismatch,
//   - arrays still laid out at the old boundary (right total, wrong group
//     lengths) are an input format error rather than silently concatenated,
//   - a group moved all the way to one end (count 0) still checks by wire
//     number, and a frozen version stays unmodifiable.
//
// The two constraints deliberately give wires 2 and 3 different roles so
// that placing a value on the wrong wire changes the check conclusion:
//
//	#1  wire2 · wire1 = 6 (mod 13)
//	#2  wire3 · 1     = 4 (mod 13)   (wire 0 is the constant one)
//
// The satisfying numbered values are wire1=2, wire2=3, wire3=4. Swapping the
// values of wires 2 and 3 makes constraint #1 fail (4·2 = 8 ≠ 6); a wrong
// wire3 alone makes constraint #2 fail first.
const repartitionDef = `{"modulus":"13","constraints":[
	{"a":[{"wire":2,"coeff":"1"}],"b":[{"wire":1,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]},
	{"a":[{"wire":3,"coeff":"1"}],"b":[{"wire":0,"coeff":"1"}],"c":[{"wire":0,"coeff":"4"}]}]}`

// seedRepartitionDraft creates a draft with 2 constraints, 1 public and 2
// private inputs and imports repartitionDef (whose highest wire is 3 = the
// 1+2 layout bound).
func seedRepartitionDraft(t *testing.T, s *Store, name string) {
	t.Helper()
	if _, err := s.CreateCircuit(Circuit{Name: name, Version: 1, Constraints: 2,
		PublicInputs: 1, PrivateInputs: 2, Description: "split"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints(name, 1, writeTempJSON(t, repartitionDef)); err != nil {
		t.Fatalf("import repartition definition: %v", err)
	}
}

// TestRepartitionDraftPreservesDefinitionAndCounts covers the headline
// success path: moving the boundary from 1/2 to 2/1 keeps the constraint
// count and every wire in range, so the update succeeds; the stored circuit
// then reports the two new group sizes while the modulus, constraint order
// and wire numbering of the already-imported definition stay byte-for-byte
// as imported (no re-import needed).
func TestRepartitionDraftPreservesDefinitionAndCounts(t *testing.T) {
	s := openTestStore(t)
	seedRepartitionDraft(t, s, "c")

	before, err := s.GetDefinition("c", 1)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PublicInputs:  intPtr(2),
		PrivateInputs: intPtr(1),
	})
	if err != nil {
		t.Fatalf("moving the boundary with the definition still in range must succeed: %v", err)
	}
	if got.Constraints != 2 || got.PublicInputs != 2 || got.PrivateInputs != 1 {
		t.Fatalf("updated counts wrong: %+v", got)
	}
	stored, _ := s.GetCircuit("c", 1)
	if stored.Constraints != 2 || stored.PublicInputs != 2 || stored.PrivateInputs != 1 {
		t.Fatalf("stored counts wrong: %+v", stored)
	}

	after, err := s.GetDefinition("c", 1)
	if err != nil {
		t.Fatalf("definition must survive the boundary change without re-import: %v", err)
	}
	if after.Modulus != "13" || len(after.Constraints) != 2 {
		t.Fatalf("modulus/count changed: %+v", after)
	}
	// Constraint order and exact wire/coefficient numbering are unchanged.
	wantSides := []struct {
		aWire, bWire, cWire int
		cCoeff              string
	}{
		{2, 1, 0, "6"}, // #1 wire2·wire1 = 6
		{3, 0, 0, "4"}, // #2 wire3·wire0 = 4 — wire 0 keeps numbering
	}
	for i, con := range after.Constraints {
		w := wantSides[i]
		if len(con.A) != 1 || con.A[0].Wire != w.aWire || con.A[0].Coeff != "1" ||
			len(con.B) != 1 || con.B[0].Wire != w.bWire || con.B[0].Coeff != "1" ||
			len(con.C) != 1 || con.C[0].Wire != w.cWire || con.C[0].Coeff != w.cCoeff {
			t.Fatalf("constraint #%d changed after repartition: %+v", i+1, con)
		}
	}
	if before.Modulus != after.Modulus || len(before.Constraints) != len(after.Constraints) {
		t.Fatalf("definition drifted:\n before %+v\n after  %+v", before, after)
	}
}

// TestRepartitionUpdateRejectsLayoutOrCountThatBreaksDefinition: the same
// successful move has a hard boundary — a new partition whose bound drops
// below a referenced wire, or a new constraint count, is refused as one
// atomic change, leaving counts and definition untouched.
func TestRepartitionUpdateRejectsLayoutOrCountThatBreaksDefinition(t *testing.T) {
	s := openTestStore(t)
	seedRepartitionDraft(t, s, "c")

	// 2 public + 0 private leaves highest wire 2, but the definition uses
	// wire 3: total still 2 < 3, so it must be refused.
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PublicInputs: intPtr(2), PrivateInputs: intPtr(0),
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("shrinking past wire 3: want invalid, got %v", err)
	}
	// Changing the constraint count away from the definition's is refused too.
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		Constraints: intPtr(3), PublicInputs: intPtr(2), PrivateInputs: intPtr(1),
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("constraint count drift: want invalid, got %v", err)
	}
	got, _ := s.GetCircuit("c", 1)
	if got.Constraints != 2 || got.PublicInputs != 1 || got.PrivateInputs != 2 {
		t.Fatalf("rejected repartition leaked: %+v", got)
	}
	if def, err := s.GetDefinition("c", 1); err != nil || len(def.Constraints) != 2 || def.Modulus != "13" {
		t.Fatalf("definition damaged by refused update: %+v %v", def, err)
	}
}

// TestRepartitionFreezeCompileCheckFollowsNewBoundary drives the complete
// lifecycle after the move: freeze, compile and input checking all use the
// new ownership of wires 2 and 3. It requires neither a re-import nor a
// trusted setup or proof job.
func TestRepartitionFreezeCompileCheckFollowsNewBoundary(t *testing.T) {
	s := openTestStore(t)
	seedRepartitionDraft(t, s, "c")
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PublicInputs: intPtr(2), PrivateInputs: intPtr(1),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	artifact, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatalf("compile repartitioned draft without re-import: %v", err)
	}
	if artifact.Modulus != 13 || artifact.Constraints != 2 || len(artifact.Hash) != 64 {
		t.Fatalf("bad artifact: %+v", artifact)
	}

	// No trusted setup and no proof job exist or are required for checking.
	if _, found, err := s.GetSetup("c", 1); err != nil || found {
		t.Fatalf("input check must not need a trusted setup: found=%v err=%v", found, err)
	}

	// New ownership: wire1=2, wire2=3 are public; wire3=4 is private.
	// #1 3·2=6 holds; #2 4·1=4 holds (wire 0 is the constant one).
	sat := Witness{Public: []string{"2", "3"}, Private: []string{"4"}}
	res, err := s.CheckInput("c", 1, artifact.Hash, sat)
	if err != nil {
		t.Fatalf("satisfied check under new partition: %v", err)
	}
	if !res.Satisfied || res.FirstFailure != 0 || res.Hash != artifact.Hash {
		t.Fatalf("want satisfied with the bound hash, got %+v", res)
	}

	// Same numbered values with wires 2 and 3 swapped (a value taken from the
	// wrong moved position) changes the conclusion: #1 4·2=8 ≠ 6 fails first.
	swapped := Witness{Public: []string{"2", "4"}, Private: []string{"3"}}
	res, err = s.CheckInput("c", 1, artifact.Hash, swapped)
	if err != nil {
		t.Fatal(err)
	}
	if res.Satisfied || res.FirstFailure != 1 || res.Hash != artifact.Hash {
		t.Fatalf("swapped position must fail at constraint #1, got %+v", res)
	}
	// Keeping wire1/wire2 right but giving wire3 a wrong value reaches #2,
	// proving wire 3 is evaluated on its own as the (single) private input.
	badWire3 := Witness{Public: []string{"2", "3"}, Private: []string{"5"}}
	res, err = s.CheckInput("c", 1, artifact.Hash, badWire3)
	if err != nil || res.Satisfied || res.FirstFailure != 2 {
		t.Fatalf("wrong wire3 must fail first at constraint #2, got %+v %v", res, err)
	}

	// The verdict never carries a private value and checking created no job.
	jobs, _ := s.ListJobs()
	if len(jobs) != 0 {
		t.Fatalf("read-only input check must not create proof jobs: %+v", jobs)
	}
}

// TestRepartitionOldLayoutArraysAreFormatError pins the rule against keeping
// the old boundary: once the hash binds the 2-public/1-private artifact, a
// request whose arrays are still split 1/2 (the old layout) has the correct
// grand total (3) but must be an input format error, never be evaluated.
func TestRepartitionOldLayoutArraysAreFormatError(t *testing.T) {
	s := openTestStore(t)
	seedRepartitionDraft(t, s, "c")
	s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PublicInputs: intPtr(2), PrivateInputs: intPtr(1),
	})
	s.FreezeCircuit("c", 1)
	artifact, _ := s.CompileCircuit("c", 1)

	oldLayout := []Witness{
		{Public: []string{"2"}, Private: []string{"3", "4"}}, // old 1/2 split
		{Public: []string{"2", "3", "4"}, Private: nil},      // everything public
		{Public: nil, Private: []string{"2", "3", "4"}},      // everything private
	}
	for _, w := range oldLayout {
		if _, err := s.CheckInput("c", 1, artifact.Hash, w); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("old-boundary witness %+v must be an input format error, got %v", w, err)
		}
	}

	// The same rejection through the JSON file entry point, even though the
	// document contains the right three values in total.
	doc := writeTempJSON(t, `{"public":["2"],"private":["3","4"]}`)
	if _, err := s.CheckInputFile("c", 1, artifact.Hash, doc); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("old-layout JSON must be an input format error, got %v", err)
	}
	// A correctly grouped document checks normally.
	good := writeTempJSON(t, `{"public":["2","3"],"private":["4"]}`)
	res, err := s.CheckInputFile("c", 1, artifact.Hash, good)
	if err != nil || !res.Satisfied || res.Hash != artifact.Hash {
		t.Fatalf("new-layout JSON check: %+v %v", res, err)
	}
}

// TestRepartitionCompileHashDistinguishesPartitions builds two independent
// data directories whose circuits are identical in name, version, modulus,
// constraint content and total input count, differing only in the
// public/private split. Their compile hashes must differ, each circuit's
// input check works only with its own hash, and presenting the other
// partition's hash is an explicit artifact mismatch with no verdict.
func TestRepartitionCompileHashDistinguishesPartitions(t *testing.T) {
	build := func(t *testing.T, pub, priv int) (*Store, Artifact) {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "bench")
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 2,
			PublicInputs: pub, PrivateInputs: priv}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ImportConstraints("c", 1, writeTempJSON(t, repartitionDef)); err != nil {
			t.Fatalf("import for %d/%d: %v", pub, priv, err)
		}
		if _, err := s.FreezeCircuit("c", 1); err != nil {
			t.Fatal(err)
		}
		a, err := s.CompileCircuit("c", 1)
		if err != nil {
			t.Fatal(err)
		}
		return s, a
	}

	sOld, artOld := build(t, 1, 2)
	sNew, artNew := build(t, 2, 1)
	if artOld.Hash == artNew.Hash {
		t.Fatal("compile hashes must differ when only the public/private partition differs")
	}
	if artOld.Modulus != artNew.Modulus || artOld.Constraints != artNew.Constraints {
		t.Fatalf("everything but the partition should match: %+v vs %+v", artOld, artNew)
	}

	// The 2/1 circuit checks only with its own hash.
	own, err := sNew.CheckInput("c", 1, artNew.Hash, Witness{Public: []string{"2", "3"}, Private: []string{"4"}})
	if err != nil || !own.Satisfied {
		t.Fatalf("own-hash check: %+v %v", own, err)
	}
	// The 1/2 partition's hash must be refused outright as a mismatch, with no
	// satisfaction conclusion even though name/version/total all match.
	res, err := sNew.CheckInput("c", 1, artOld.Hash, Witness{Public: []string{"2", "3"}, Private: []string{"4"}})
	if !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("foreign partition hash: want artifact mismatch, got %v (res=%+v)", err, res)
	}
	if res.Satisfied || res.FirstFailure != 0 || res.Hash != "" {
		t.Fatalf("a mismatched hash must return no verdict, got %+v", res)
	}
	// Symmetrically, the 1/2 circuit rejects the 2/1 hash but accepts its own
	// (there wire2 and wire3 are both private: public=[2], private=[3,4]).
	if _, err := sOld.CheckInput("c", 1, artNew.Hash, Witness{Public: []string{"2"}, Private: []string{"3", "4"}}); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("reverse direction must also mismatch, got %v", err)
	}
	oldOwn, err := sOld.CheckInput("c", 1, artOld.Hash, Witness{Public: []string{"2"}, Private: []string{"3", "4"}})
	if err != nil || !oldOwn.Satisfied {
		t.Fatalf("1/2 circuit own-hash check: %+v %v", oldOwn, err)
	}
}

// TestRepartitionEmptyGroupAtBoundary covers moving the split all the way to
// one end: the empty group is legal, and checking still resolves values by
// wire number as long as the non-empty group's length matches. Both
// all-public (3/0) and all-private (0/3) are exercised through the API and
// the JSON file entry, including persistence across a directory reopen.
func TestRepartitionEmptyGroupAtBoundary(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedRepartitionDraft(t, s, "c")

	// 3 public, 0 private.
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PublicInputs: intPtr(3), PrivateInputs: intPtr(0),
	}); err != nil {
		t.Fatalf("3/0 repartition: %v", err)
	}
	if c, _ := s.GetCircuit("c", 1); c.PublicInputs != 3 || c.PrivateInputs != 0 {
		t.Fatalf("3/0 counts wrong: %+v", c)
	}
	s.FreezeCircuit("c", 1)
	allPub, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	// Empty private group via nil or empty slice; values are taken by wire.
	for _, priv := range [][]string{nil, {}} {
		res, err := s.CheckInput("c", 1, allPub.Hash, Witness{Public: []string{"2", "3", "4"}, Private: priv})
		if err != nil || !res.Satisfied {
			t.Fatalf("3/0 check (priv=%v): %+v %v", priv, res, err)
		}
	}
	doc := writeTempJSON(t, `{"public":["2","3"],"private":["4"]}`) // wrong: 2/1 against 3/0
	if _, err := s.CheckInputFile("c", 1, allPub.Hash, doc); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("2/1 document against 3/0 must be an input format error, got %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// A fresh draft in another directory for the 0 public / 3 private end.
	dir2 := filepath.Join(t.TempDir(), "bench")
	s2, err := Open(dir2)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	seedRepartitionDraft(t, s2, "p")
	if _, err := s2.UpdateCircuitPartial("p", 1, PartialCircuit{
		PublicInputs: intPtr(0), PrivateInputs: intPtr(3),
	}); err != nil {
		t.Fatalf("0/3 repartition: %v", err)
	}
	s2.FreezeCircuit("p", 1)
	allPriv, _ := s2.CompileCircuit("p", 1)
	// File entry must carry explicit (empty) arrays; wires 1..3 are private.
	path := writeTempJSON(t, `{"public":[],"private":["2","3","4"]}`)
	res, err := s2.CheckInputFile("p", 1, allPriv.Hash, path)
	if err != nil || !res.Satisfied {
		t.Fatalf("0/3 file check: %+v %v", res, err)
	}
	if _, err := s2.CheckInput("p", 1, allPriv.Hash, Witness{Public: []string{}, Private: []string{"2", "3", "4"}}); err != nil {
		t.Fatalf("0/3 API check: %v", err)
	}
}

// TestRepartitionedArtifactAndCheckSurviveReopen pins persistence: the new
// partition, its artifact hash and bound checks survive an atomic reload
// with no re-import, and the reloaded artifact hash is identical.
func TestRepartitionedArtifactAndCheckSurviveReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedRepartitionDraft(t, s, "c")
	s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PublicInputs: intPtr(2), PrivateInputs: intPtr(1),
	})
	s.FreezeCircuit("c", 1)
	first, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	c, err := s2.GetCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicInputs != 2 || c.PrivateInputs != 1 || !c.Frozen {
		t.Fatalf("reloaded partition wrong: %+v", c)
	}
	again, err := s2.CompileCircuit("c", 1)
	if err != nil || again.Hash != first.Hash {
		t.Fatalf("recompile across reload: %+v %v", again, err)
	}
	res, err := s2.CheckInput("c", 1, again.Hash, Witness{Public: []string{"2", "3"}, Private: []string{"4"}})
	if err != nil || !res.Satisfied {
		t.Fatalf("check after reopen must follow the new partition: %+v %v", res, err)
	}
	// Old-boundary arrays remain a format error after reload.
	if _, err := s2.CheckInput("c", 1, again.Hash, Witness{Public: []string{"2"}, Private: []string{"3", "4"}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("old layout after reload: want input format error, got %v", err)
	}
}

// TestRepartitionFrozenVersionStillImmutable confirms that moving the
// boundary is a draft-only operation: once frozen, the counts can never
// change again, including to an otherwise-valid partition.
func TestRepartitionFrozenVersionStillImmutable(t *testing.T) {
	s := openTestStore(t)
	seedRepartitionDraft(t, s, "c")
	s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PublicInputs: intPtr(2), PrivateInputs: intPtr(1),
	})
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}

	// 3/0 keeps every wire in range and would be legal on a draft, but the
	// version is frozen.
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PublicInputs: intPtr(3), PrivateInputs: intPtr(0),
	}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("frozen repartition: want frozen, got %v", err)
	}
	if _, err := s.UpdateCircuit(Circuit{Name: "c", Version: 1, Constraints: 2,
		PublicInputs: 1, PrivateInputs: 2, Description: "split"}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("frozen whole replacement: want frozen, got %v", err)
	}
	got, _ := s.GetCircuit("c", 1)
	if got.PublicInputs != 2 || got.PrivateInputs != 1 || !got.Frozen {
		t.Fatalf("frozen counts changed: %+v", got)
	}
}
