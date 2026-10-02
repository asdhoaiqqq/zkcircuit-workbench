package zkcircuit

import (
	"errors"
	"path/filepath"
	"testing"
)

// copyDef is a 2-constraint definition matching counts 2/1/1.
const copyDef = `{"modulus":"7","constraints":[
	{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]},
	{"a":[{"wire":1,"coeff":"2"}],"b":[],"c":[]}]}`

// seedFrozenSource creates a frozen source version with the given counts and
// optional definition, plus a setup and a compiled artifact when def is set.
func seedFrozenSource(t *testing.T, s *Store, name string, version, constraints, pub, priv int, def string) {
	t.Helper()
	if _, err := s.CreateCircuit(Circuit{
		Name: name, Version: version,
		Constraints: constraints, PublicInputs: pub, PrivateInputs: priv,
		Description: "source-desc",
	}); err != nil {
		t.Fatal(err)
	}
	if def != "" {
		if _, err := s.ImportConstraints(name, version, writeTempJSON(t, def)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.FreezeCircuit(name, version); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSetup(name, version); err != nil {
		t.Fatal(err)
	}
	if def != "" {
		if _, err := s.CompileCircuit(name, version); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCopyCircuitWithDefinition(t *testing.T) {
	s := openTestStore(t)
	seedFrozenSource(t, s, "c", 1, 2, 1, 1, copyDef)

	got, err := s.CopyCircuit("c", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "c" || got.Version != 2 || got.Constraints != 2 ||
		got.PublicInputs != 1 || got.PrivateInputs != 1 ||
		got.Description != "source-desc" || got.Frozen {
		t.Fatalf("copy returned wrong record: %+v", got)
	}

	// The definition must have been copied.
	def, err := s.GetDefinition("c", 2)
	if err != nil {
		t.Fatalf("target definition missing: %v", err)
	}
	if def.Modulus != "7" || len(def.Constraints) != 2 {
		t.Fatalf("target definition wrong: %+v", def)
	}

	// The source is untouched.
	src, _ := s.GetCircuit("c", 1)
	if src.Constraints != 2 || !src.Frozen || src.Description != "source-desc" {
		t.Fatalf("source changed after copy: %+v", src)
	}
}

func TestCopyCircuitWithoutDefinition(t *testing.T) {
	s := openTestStore(t)
	// Counts-only frozen source (no definition, no artifact).
	seedFrozenSource(t, s, "c", 1, 3, 2, 4, "")

	got, err := s.CopyCircuit("c", 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.Constraints != 3 || got.PublicInputs != 2 || got.PrivateInputs != 4 ||
		got.Description != "source-desc" || got.Frozen {
		t.Fatalf("copy returned wrong record: %+v", got)
	}

	// Target must have no definition.
	if _, err := s.GetDefinition("c", 5); !errors.Is(err, ErrDefinitionMissing) {
		t.Fatalf("counts-only source should produce counts-only target, got %v", err)
	}

	// Target can import a definition afterwards.
	if _, err := s.ImportConstraints("c", 5, writeTempJSON(t, copyDef)); !errors.Is(err, ErrInvalidArgument) {
		// copyDef has 2 constraints but target declares 3 -> mismatch expected
		t.Fatalf("expected count mismatch, got %v", err)
	}
}

func TestCopyCircuitTargetIndependent(t *testing.T) {
	s := openTestStore(t)
	seedFrozenSource(t, s, "c", 1, 2, 1, 1, copyDef)

	if _, err := s.CopyCircuit("c", 1, 2); err != nil {
		t.Fatal(err)
	}

	// Modifying the target must not affect the source.
	if _, err := s.UpdateCircuit(Circuit{
		Name: "c", Version: 2, Constraints: 2,
		PublicInputs: 2, PrivateInputs: 2, Description: "target-changed",
	}); err != nil {
		t.Fatal(err)
	}
	src, _ := s.GetCircuit("c", 1)
	if src.PublicInputs != 1 || src.PrivateInputs != 1 || src.Description != "source-desc" {
		t.Fatalf("source leaked from target update: %+v", src)
	}

	// Re-importing the target definition must not affect the source.
	alt := `{"modulus":"11","constraints":[
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"1"}]},
		{"a":[],"b":[],"c":[]}]}`
	if _, err := s.ImportConstraints("c", 2, writeTempJSON(t, alt)); err != nil {
		t.Fatal(err)
	}
	srcDef, _ := s.GetDefinition("c", 1)
	if srcDef.Modulus != "7" {
		t.Fatalf("source definition leaked from target re-import: %+v", srcDef)
	}

	// Freezing the target must not affect the source's frozen status.
	if _, err := s.FreezeCircuit("c", 2); err != nil {
		t.Fatal(err)
	}
	src, _ = s.GetCircuit("c", 1)
	if !src.Frozen {
		t.Fatal("source became unfrozen")
	}

	// The target compiles to a different hash than the source even with the
	// same definition, because the version identity is folded in.
	srcArt, _ := s.GetArtifact("c", 1)
	tgtArt, err := s.CompileCircuit("c", 2)
	if err != nil {
		t.Fatal(err)
	}
	if srcArt.Hash == tgtArt.Hash {
		t.Fatalf("source and target artifacts must have different hashes, both %s", srcArt.Hash)
	}

	// The source hash cannot satisfy the target's input check.
	witness := Witness{Public: []string{"2"}, Private: []string{"3"}}
	if _, err := s.CheckInput("c", 2, srcArt.Hash, witness); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("source hash must not work for target input check, got %v", err)
	}

	// The target must register its own setup before submitting a job.
	if _, err := s.SubmitJob(Job{ID: "j1", Circuit: "c", Version: 2, Kind: "prove", Attempt: 1}); !errors.Is(err, ErrSetupMissing) {
		t.Fatalf("target job without setup: want setup missing, got %v", err)
	}
	if _, err := s.RecordSetup("c", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitJob(Job{ID: "j1", Circuit: "c", Version: 2, Kind: "prove", Attempt: 1, CompiledHash: tgtArt.Hash}); err != nil {
		t.Fatalf("target job with own setup and hash: %v", err)
	}
}

func TestCopyCircuitIdempotent(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedFrozenSource(t, s, "c", 1, 2, 1, 1, copyDef)

	first, err := s.CopyCircuit("c", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	dataBefore, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}

	// Repeating the same copy returns the existing target and writes nothing.
	second, err := s.CopyCircuit("c", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("idempotent copy returned different record: %+v vs %+v", second, first)
	}
	dataAfter, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(dataBefore) != string(dataAfter) {
		t.Fatal("idempotent copy wrote to the data file")
	}

	// Definition is also unchanged.
	def1, _ := s.GetDefinition("c", 2)
	def2, _ := s.GetDefinition("c", 2)
	if def1.Modulus != def2.Modulus || len(def1.Constraints) != len(def2.Constraints) {
		t.Fatal("definition changed on idempotent copy")
	}
}

func TestCopyCircuitIdempotentWithoutDefinition(t *testing.T) {
	s := openTestStore(t)
	seedFrozenSource(t, s, "c", 1, 2, 1, 1, "")

	first, err := s.CopyCircuit("c", 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CopyCircuit("c", 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("idempotent counts-only copy returned different record: %+v vs %+v", second, first)
	}
}

func TestCopyCircuitConflictFrozenTarget(t *testing.T) {
	s := openTestStore(t)
	seedFrozenSource(t, s, "c", 1, 2, 1, 1, copyDef)

	if _, err := s.CopyCircuit("c", 1, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit("c", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyCircuit("c", 1, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("frozen target: want conflict, got %v", err)
	}
}

func TestCopyCircuitConflictChangedTarget(t *testing.T) {
	s := openTestStore(t)
	seedFrozenSource(t, s, "c", 1, 2, 1, 1, copyDef)

	if _, err := s.CopyCircuit("c", 1, 2); err != nil {
		t.Fatal(err)
	}

	// Change the description -> conflict.
	if _, err := s.UpdateCircuit(Circuit{Name: "c", Version: 2, Constraints: 2,
		PublicInputs: 1, PrivateInputs: 1, Description: "changed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyCircuit("c", 1, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed description: want conflict, got %v", err)
	}

	// Change the counts -> conflict (public/private change is definition-compatible).
	if _, err := s.UpdateCircuit(Circuit{Name: "c", Version: 2, Constraints: 2,
		PublicInputs: 2, PrivateInputs: 2, Description: "changed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyCircuit("c", 1, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed counts: want conflict, got %v", err)
	}

	// Change the definition -> conflict.
	alt := `{"modulus":"11","constraints":[
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"1"}]},
		{"a":[],"b":[],"c":[]}]}`
	if _, err := s.ImportConstraints("c", 2, writeTempJSON(t, alt)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyCircuit("c", 1, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed definition: want conflict, got %v", err)
	}
}

func TestCopyCircuitConflictDefinitionStateMismatch(t *testing.T) {
	s := openTestStore(t)
	// Source has a definition.
	seedFrozenSource(t, s, "c", 1, 2, 1, 1, copyDef)

	// Pre-create the target as a draft without a definition.
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 2, Constraints: 2,
		PublicInputs: 1, PrivateInputs: 1, Description: "source-desc"}); err != nil {
		t.Fatal(err)
	}
	// Source has a definition, target does not -> conflict.
	if _, err := s.CopyCircuit("c", 1, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("definition state mismatch: want conflict, got %v", err)
	}

	// Now the reverse: source without definition, target with definition.
	s2 := openTestStore(t)
	seedFrozenSource(t, s2, "d", 1, 1, 1, 1, "")
	if _, err := s2.CreateCircuit(Circuit{Name: "d", Version: 2, Constraints: 1,
		PublicInputs: 1, PrivateInputs: 1, Description: "source-desc"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.ImportConstraints("d", 2, writeTempJSON(t,
		`{"modulus":"7","constraints":[{"a":[],"b":[],"c":[]}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.CopyCircuit("d", 1, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("definition state mismatch (source no def, target has def): want conflict, got %v", err)
	}
}

func TestCopyCircuitGating(t *testing.T) {
	s := openTestStore(t)

	// Source not found.
	if _, err := s.CopyCircuit("ghost", 1, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown source: want not found, got %v", err)
	}

	// Source exists but not frozen.
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1, Description: "d"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyCircuit("c", 1, 2); !errors.Is(err, ErrNotFrozen) {
		t.Fatalf("unfrozen source: want not frozen, got %v", err)
	}

	// Same version -> invalid argument.
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopyCircuit("c", 1, 1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("same version: want invalid argument, got %v", err)
	}

	// Invalid target version.
	if _, err := s.CopyCircuit("c", 1, 0); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero target version: want invalid argument, got %v", err)
	}
	if _, err := s.CopyCircuit("c", 1, -1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative target version: want invalid argument, got %v", err)
	}

	// Blank name.
	if _, err := s.CopyCircuit("  ", 1, 2); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank name: want invalid argument, got %v", err)
	}
}

func TestCopyCircuitSourceNotCompiledOrNoSetup(t *testing.T) {
	s := openTestStore(t)
	// Frozen source with a definition but no setup and no artifact.
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1,
		PublicInputs: 1, PrivateInputs: 1, Description: "d"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints("c", 1, writeTempJSON(t,
		`{"modulus":"7","constraints":[{"a":[],"b":[],"c":[]}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	// No setup recorded, no artifact compiled. Copy must still succeed.
	if _, err := s.CopyCircuit("c", 1, 2); err != nil {
		t.Fatalf("copy should succeed without source setup/artifact: %v", err)
	}
	got, _ := s.GetCircuit("c", 2)
	if got.Frozen {
		t.Fatal("target should be a draft")
	}
}

func TestCopyCircuitPreservesConstraintOrder(t *testing.T) {
	s := openTestStore(t)
	// Definition with a distinctive constraint order.
	ordered := `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"3"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"2"}]},
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"2"}],"c":[{"wire":0,"coeff":"5"}]},
		{"a":[],"b":[],"c":[]}]}`
	seedFrozenSource(t, s, "c", 1, 3, 1, 1, ordered)

	if _, err := s.CopyCircuit("c", 1, 2); err != nil {
		t.Fatal(err)
	}
	def, err := s.GetDefinition("c", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Constraints) != 3 {
		t.Fatalf("expected 3 constraints, got %d", len(def.Constraints))
	}
	// Verify the order is preserved: first constraint's a-term coeff is "3".
	if len(def.Constraints[0].A) != 1 || def.Constraints[0].A[0].Coeff != "3" {
		t.Fatalf("constraint order not preserved: %+v", def.Constraints[0])
	}
	if len(def.Constraints[1].A) != 1 || def.Constraints[1].A[0].Coeff != "1" {
		t.Fatalf("constraint order not preserved at index 1: %+v", def.Constraints[1])
	}
}

func TestCopyCircuitSurvivesReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedFrozenSource(t, s, "c", 1, 2, 1, 1, copyDef)

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

	// The copied draft and its definition must survive.
	got, err := s2.GetCircuit("c", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Constraints != 2 || got.PublicInputs != 1 || got.PrivateInputs != 1 ||
		got.Description != "source-desc" || got.Frozen {
		t.Fatalf("copied circuit lost across reopen: %+v", got)
	}
	def, err := s2.GetDefinition("c", 2)
	if err != nil {
		t.Fatalf("copied definition lost across reopen: %v", err)
	}
	if def.Modulus != "7" || len(def.Constraints) != 2 {
		t.Fatalf("copied definition wrong across reopen: %+v", def)
	}

	// The source is intact.
	src, _ := s2.GetCircuit("c", 1)
	if !src.Frozen {
		t.Fatal("source lost frozen status across reopen")
	}
	srcDef, _ := s2.GetDefinition("c", 1)
	if srcDef.Modulus != "7" {
		t.Fatal("source definition changed across reopen")
	}

	// The target can be frozen and compiled after reopen.
	if _, err := s2.FreezeCircuit("c", 2); err != nil {
		t.Fatal(err)
	}
	art, err := s2.CompileCircuit("c", 2)
	if err != nil {
		t.Fatal(err)
	}
	if art.Hash == "" {
		t.Fatal("compiled artifact has empty hash")
	}
}

func TestCopyCircuitTargetCreatedByOtherOperation(t *testing.T) {
	s := openTestStore(t)
	seedFrozenSource(t, s, "c", 1, 2, 1, 1, copyDef)

	// A different operation (CreateCircuit) pre-creates the target as a draft
	// with the same content. Copy should treat it as idempotent.
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 2, Constraints: 2,
		PublicInputs: 1, PrivateInputs: 1, Description: "source-desc"}); err != nil {
		t.Fatal(err)
	}
	// But without a definition -> conflict.
	if _, err := s.CopyCircuit("c", 1, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("target created by other op without definition: want conflict, got %v", err)
	}

	// Now create it with a definition that matches.
	s2 := openTestStore(t)
	seedFrozenSource(t, s2, "c", 1, 2, 1, 1, copyDef)
	if _, err := s2.CreateCircuit(Circuit{Name: "c", Version: 2, Constraints: 2,
		PublicInputs: 1, PrivateInputs: 1, Description: "source-desc"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.ImportConstraints("c", 2, writeTempJSON(t, copyDef)); err != nil {
		t.Fatal(err)
	}
	// This time it should be idempotent.
	got, err := s2.CopyCircuit("c", 1, 2)
	if err != nil {
		t.Fatalf("target created by other op with matching content: %v", err)
	}
	if got.Version != 2 || got.Frozen {
		t.Fatalf("idempotent return wrong: %+v", got)
	}
}

func TestCopyCircuitNonConsecutiveTarget(t *testing.T) {
	s := openTestStore(t)
	seedFrozenSource(t, s, "c", 5, 1, 1, 1, "")

	// Target version need not be consecutive or greater.
	for _, tv := range []int{1, 3, 10, 100} {
		got, err := s.CopyCircuit("c", 5, tv)
		if err != nil {
			t.Fatalf("copy to v%d: %v", tv, err)
		}
		if got.Version != tv || got.Frozen {
			t.Fatalf("copy to v%d returned wrong record: %+v", tv, got)
		}
	}
}
