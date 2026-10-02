package zkcircuit

import (
	"errors"
	"path/filepath"
	"testing"
)

// seedFrozenSource creates a frozen circuit version with the given counts and
// description, optionally with validDef imported.
func seedFrozenSource(t *testing.T, s *Store, name string, v, pub, priv int, withDef bool) {
	t.Helper()
	if _, err := s.CreateCircuit(Circuit{Name: name, Version: v, Constraints: 1,
		PublicInputs: pub, PrivateInputs: priv, Description: "source-desc"}); err != nil {
		t.Fatal(err)
	}
	if withDef {
		if _, err := s.ImportConstraints(name, v, writeTempJSON(t, validDef)); err != nil {
			t.Fatalf("import: %v", err)
		}
	}
	if _, err := s.FreezeCircuit(name, v); err != nil {
		t.Fatal(err)
	}
}

func TestCopyCircuitValidation(t *testing.T) {
	s := openTestStore(t)
	seedFrozenSource(t, s, "c", 1, 1, 1, false)

	if _, err := s.CopyCircuit("  ", 1, 2); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank name: %v", err)
	}
	if _, err := s.CopyCircuit("c", 0, 2); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero source version: %v", err)
	}
	if _, err := s.CopyCircuit("c", 1, 0); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero target version: %v", err)
	}
	if _, err := s.CopyCircuit("c", 1, -3); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative target version: %v", err)
	}
	if _, err := s.CopyCircuit("c", 1, 1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("same version: %v", err)
	}
	// Rejected attempts left nothing behind.
	list, err := s.ListCircuits()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("rejected copies left records: %+v", list)
	}
}

func TestCopyCircuitSourceGating(t *testing.T) {
	s := openTestStore(t)

	if _, err := s.CopyCircuit("ghost", 1, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing source: want not found, got %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyCircuit("c", 1, 2); !errors.Is(err, ErrNotFrozen) {
		t.Fatalf("draft source: want not frozen, got %v", err)
	}
	// Source checks run before the existing-target rule: even with a
	// conflicting target present, a missing source reports not found.
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 2, Constraints: 9, Description: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyCircuit("c", 9, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing source with existing target: want not found, got %v", err)
	}
	if _, err := s.CopyCircuit("c", 1, 2); !errors.Is(err, ErrNotFrozen) {
		t.Fatalf("draft source with existing target: want not frozen, got %v", err)
	}
}

func TestCopyCircuitWithDefinition(t *testing.T) {
	s := openTestStore(t)
	seedFrozenSource(t, s, "c", 1, 1, 1, true)

	got, err := s.CopyCircuit("c", 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "c" || got.Version != 5 || got.Constraints != 1 ||
		got.PublicInputs != 1 || got.PrivateInputs != 1 ||
		got.Description != "source-desc" || got.Frozen {
		t.Fatalf("bad copy: %+v", got)
	}
	// The definition came along in canonical form.
	def, err := s.GetDefinition("c", 5)
	if err != nil {
		t.Fatal(err)
	}
	srcDef, err := s.GetDefinition("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if def.Modulus != srcDef.Modulus || len(def.Constraints) != len(srcDef.Constraints) {
		t.Fatalf("definition not carried over: %+v vs %+v", def, srcDef)
	}
	// Queries and listings see the new draft.
	stored, err := s.GetCircuit("c", 5)
	if err != nil || stored.Frozen {
		t.Fatalf("get copied draft: %+v %v", stored, err)
	}
	list, err := s.ListCircuits()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Version != 1 || list[1].Version != 5 {
		t.Fatalf("list after copy: %+v", list)
	}
	// No setup, artifact or job was created for the target.
	if _, found, err := s.GetSetup("c", 5); err != nil || found {
		t.Fatalf("copy must not register a setup: found=%t err=%v", found, err)
	}
	if _, err := s.GetArtifact("c", 5); !errors.Is(err, ErrArtifactMissing) {
		t.Fatalf("copy must not compile an artifact: %v", err)
	}
	jobs, err := s.ListJobs()
	if err != nil || len(jobs) != 0 {
		t.Fatalf("copy must not create jobs: %+v %v", jobs, err)
	}
}

func TestCopyCircuitCountsOnlySource(t *testing.T) {
	s := openTestStore(t)
	seedFrozenSource(t, s, "c", 1, 2, 0, false)

	got, err := s.CopyCircuit("c", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Frozen || got.PublicInputs != 2 || got.PrivateInputs != 0 {
		t.Fatalf("bad counts-only copy: %+v", got)
	}
	// The target has no definition either, and one can be imported later.
	if _, err := s.GetDefinition("c", 2); !errors.Is(err, ErrDefinitionMissing) {
		t.Fatalf("counts-only copy must have no definition: %v", err)
	}
	if _, err := s.ImportConstraints("c", 2, writeTempJSON(t, validDef)); err != nil {
		t.Fatalf("import into copied draft: %v", err)
	}
}

func TestCopyCircuitTargetIsIndependentDraft(t *testing.T) {
	s := openTestStore(t)
	seedFrozenSource(t, s, "c", 1, 1, 1, true)
	if _, err := s.CopyCircuit("c", 1, 2); err != nil {
		t.Fatal(err)
	}

	// The copied draft supports the ordinary draft operations.
	if _, err := s.UpdateCircuitPartial("c", 2, PartialCircuit{Description: stringPtr("edited")}); err != nil {
		t.Fatalf("partial update on copy: %v", err)
	}
	// A count change that breaks the imported definition is refused as a whole.
	if _, err := s.UpdateCircuitPartial("c", 2, PartialCircuit{PrivateInputs: intPtr(0)}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("illegal count change on copy must be refused: %v", err)
	}
	if _, err := s.ImportConstraints("c", 2, writeTempJSON(t, validDef)); err != nil {
		t.Fatalf("re-import on copy: %v", err)
	}
	if _, err := s.FreezeCircuit("c", 2); err != nil {
		t.Fatalf("freeze copy: %v", err)
	}

	// The source is untouched: same definition, still frozen, no new records.
	src, err := s.GetCircuit("c", 1)
	if err != nil || !src.Frozen || src.Description != "source-desc" {
		t.Fatalf("source changed: %+v %v", src, err)
	}
	srcDef, err := s.GetDefinition("c", 1)
	if err != nil || srcDef.Modulus != "7" {
		t.Fatalf("source definition changed: %+v %v", srcDef, err)
	}
}

func TestCopyCircuitArtifactIdentityAndSetupIsolation(t *testing.T) {
	s := openTestStore(t)
	seedFrozenSource(t, s, "c", 1, 1, 1, true)
	if _, err := s.RecordSetup("c", 1); err != nil {
		t.Fatal(err)
	}
	srcArt, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitJob(Job{ID: "j1", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: srcArt.Hash}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CopyCircuit("c", 1, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit("c", 2); err != nil {
		t.Fatal(err)
	}
	// The target cannot prove before its own setup is registered, and the
	// source's artifact hash does not bind to it.
	if _, err := s.SubmitJob(Job{ID: "j2", Circuit: "c", Version: 2, Kind: "prove", Attempt: 1}); !errors.Is(err, ErrSetupMissing) {
		t.Fatalf("target must need its own setup: %v", err)
	}
	tgtArt, err := s.CompileCircuit("c", 2)
	if err != nil {
		t.Fatal(err)
	}
	// Identical definition, but the hash carries the target's own identity.
	if tgtArt.Hash == srcArt.Hash {
		t.Fatalf("target artifact hash must differ from source: %q", tgtArt.Hash)
	}
	// The source hash fails the target's input check and job binding.
	witnessPath := writeTempJSON(t, `{"public":["2"],"private":["3"]}`)
	if _, err := s.CheckInputFile("c", 2, srcArt.Hash, witnessPath); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("source hash on target input check: %v", err)
	}
	if _, err := s.RecordSetup("c", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitJob(Job{ID: "j3", Circuit: "c", Version: 2, Kind: "prove", Attempt: 1, CompiledHash: srcArt.Hash}); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("source hash on target job: %v", err)
	}
	// The target's own hash works for both.
	res, err := s.CheckInputFile("c", 2, tgtArt.Hash, witnessPath)
	if err != nil || !res.Satisfied {
		t.Fatalf("target input check: %+v %v", res, err)
	}
	if _, err := s.SubmitJob(Job{ID: "j4", Circuit: "c", Version: 2, Kind: "prove", Attempt: 1, CompiledHash: tgtArt.Hash}); err != nil {
		t.Fatalf("target job with own hash: %v", err)
	}
	// The source's job and artifact are unchanged.
	j1, err := s.GetJob("j1")
	if err != nil || j1.CompiledHash != srcArt.Hash {
		t.Fatalf("source job changed: %+v %v", j1, err)
	}
}

func TestCopyCircuitIdempotenceAndConflict(t *testing.T) {
	s := openTestStore(t)
	seedFrozenSource(t, s, "c", 1, 1, 1, true)

	first, err := s.CopyCircuit("c", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.CopyCircuit("c", 1, 2)
	if err != nil {
		t.Fatalf("idempotent re-copy: %v", err)
	}
	if again != first {
		t.Fatalf("re-copy returned a different record: %+v vs %+v", again, first)
	}
	list, err := s.ListCircuits()
	if err != nil || len(list) != 2 {
		t.Fatalf("re-copy added records: %+v", list)
	}

	// A draft target whose description changed conflicts.
	if _, err := s.UpdateCircuitPartial("c", 2, PartialCircuit{Description: stringPtr("edited")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyCircuit("c", 1, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed description: want conflict, got %v", err)
	}

	// A draft target whose definition was replaced conflicts, even when the
	// counts still match.
	if _, err := s.UpdateCircuitPartial("c", 2, PartialCircuit{Description: stringPtr("source-desc")}); err != nil {
		t.Fatal(err)
	}
	otherDef := `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"2"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"5"}]}]}`
	if _, err := s.ImportConstraints("c", 2, writeTempJSON(t, otherDef)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyCircuit("c", 1, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed definition: want conflict, got %v", err)
	}

	// Restoring the canonical-equal definition makes the copy idempotent
	// again: equivalence follows the canonical semantics, not the source JSON.
	rewritten := `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"8"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`
	if _, err := s.ImportConstraints("c", 2, writeTempJSON(t, rewritten)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyCircuit("c", 1, 2); err != nil {
		t.Fatalf("canonically equal target must be returned, got %v", err)
	}

	// A frozen target always conflicts, even with identical content.
	if _, err := s.FreezeCircuit("c", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyCircuit("c", 1, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("frozen target: want conflict, got %v", err)
	}

	// A target created by another operation with different counts conflicts.
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 3, Constraints: 7, Description: "source-desc"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyCircuit("c", 1, 3); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign target: want conflict, got %v", err)
	}
}

func TestCopyCircuitDefinitionPresenceMismatch(t *testing.T) {
	s := openTestStore(t)
	// Source without a definition; target created by another operation that
	// has one — no definition and some definition are different states.
	seedFrozenSource(t, s, "c", 1, 1, 1, false)
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 2, Constraints: 1,
		PublicInputs: 1, PrivateInputs: 1, Description: "source-desc"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints("c", 2, writeTempJSON(t, validDef)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyCircuit("c", 1, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("definition presence mismatch: want conflict, got %v", err)
	}
	// And the mirror image: source with a definition, target without.
	if _, err := s.CreateCircuit(Circuit{Name: "d", Version: 1, Constraints: 1,
		PublicInputs: 1, PrivateInputs: 1, Description: "with-def"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints("d", 1, writeTempJSON(t, validDef)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit("d", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "d", Version: 2, Constraints: 1,
		PublicInputs: 1, PrivateInputs: 1, Description: "with-def"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyCircuit("d", 1, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing target definition: want conflict, got %v", err)
	}
}

func TestCopyCircuitSurvivesReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedFrozenSource(t, s, "c", 1, 1, 1, true)
	if _, err := s.CopyCircuit("c", 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.GetCircuit("c", 2)
	if err != nil || got.Frozen || got.Description != "source-desc" {
		t.Fatalf("copied draft after reopen: %+v %v", got, err)
	}
	def, err := s2.GetDefinition("c", 2)
	if err != nil || def.Modulus != "7" || len(def.Constraints) != 1 {
		t.Fatalf("copied definition after reopen: %+v %v", def, err)
	}
	// The reopened copy is fully usable: freeze, compile, check.
	if _, err := s2.FreezeCircuit("c", 2); err != nil {
		t.Fatal(err)
	}
	art, err := s2.CompileCircuit("c", 2)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s2.CheckInputFile("c", 2, art.Hash, writeTempJSON(t, `{"public":["2"],"private":["3"]}`))
	if err != nil || !res.Satisfied {
		t.Fatalf("check on reopened copy: %+v %v", res, err)
	}
}

func TestCopyCircuitAnyTargetVersion(t *testing.T) {
	s := openTestStore(t)
	seedFrozenSource(t, s, "c", 4, 1, 1, false)
	// The target need not be consecutive or larger than the source.
	for _, v := range []int{1, 2, 100} {
		got, err := s.CopyCircuit("c", 4, v)
		if err != nil {
			t.Fatalf("copy to %d: %v", v, err)
		}
		if got.Version != v || got.Frozen {
			t.Fatalf("copy to %d: %+v", v, got)
		}
	}
}
