package zkcircuit

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func pint(v int) *int          { return &v }
func pstring(v string) *string { return &v }

// seed a draft with 2 constraints, 1 public input, 3 private inputs and the
// description "初稿", matching the scenario in the requirements.
func seedPatchDraft(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.CreateCircuit(Circuit{
		Name: "c", Version: 1, Constraints: 2,
		PublicInputs: 1, PrivateInputs: 3, Description: "初稿",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPatchOmitsUnsuppliedFields(t *testing.T) {
	s := openTestStore(t)
	seedPatchDraft(t, s)

	// Changing only the description leaves every count untouched.
	got, err := s.PatchCircuit(CircuitPatch{Name: "c", Version: 1, Description: pstring("定稿")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Constraints != 2 || got.PublicInputs != 1 || got.PrivateInputs != 3 || got.Description != "定稿" {
		t.Fatalf("description-only patch changed counts: %+v", got)
	}

	// Changing only the constraint count keeps description and input counts.
	got, err = s.PatchCircuit(CircuitPatch{Name: "c", Version: 1, Constraints: pint(4)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Constraints != 4 || got.PublicInputs != 1 || got.PrivateInputs != 3 || got.Description != "定稿" {
		t.Fatalf("constraints-only patch changed other fields: %+v", got)
	}

	// Changing only one input count keeps the rest.
	got, err = s.PatchCircuit(CircuitPatch{Name: "c", Version: 1, PublicInputs: pint(2)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Constraints != 4 || got.PublicInputs != 2 || got.PrivateInputs != 3 || got.Description != "定稿" {
		t.Fatalf("public-only patch changed other fields: %+v", got)
	}

	got, err = s.PatchCircuit(CircuitPatch{Name: "c", Version: 1, PrivateInputs: pint(5)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Constraints != 4 || got.PublicInputs != 2 || got.PrivateInputs != 5 || got.Description != "定稿" {
		t.Fatalf("private-only patch changed other fields: %+v", got)
	}

	stored, _ := s.GetCircuit("c", 1)
	if stored != got {
		t.Fatalf("patch result disagrees with stored record: %+v vs %+v", got, stored)
	}
}

func TestPatchExplicitZeroAndEmptyAreApplied(t *testing.T) {
	s := openTestStore(t)
	seedPatchDraft(t, s)

	// Explicit zero clears the input counts; nil would have kept them.
	got, err := s.PatchCircuit(CircuitPatch{
		Name: "c", Version: 1, PublicInputs: pint(0), PrivateInputs: pint(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.PublicInputs != 0 || got.PrivateInputs != 0 {
		t.Fatalf("explicit zero inputs were not applied: %+v", got)
	}
	if got.Constraints != 2 || got.Description != "初稿" {
		t.Fatalf("omitted fields changed: %+v", got)
	}

	// Explicit empty description clears it.
	got, err = s.PatchCircuit(CircuitPatch{Name: "c", Version: 1, Description: pstring("")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "" {
		t.Fatalf("explicit empty description was not applied: %+v", got)
	}
	if got.Constraints != 2 || got.PublicInputs != 0 || got.PrivateInputs != 0 {
		t.Fatalf("omitted fields changed: %+v", got)
	}
}

func TestPatchNoFieldsReturnsExistingWithoutCommit(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedPatchDraft(t, s)

	before, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.PatchCircuit(CircuitPatch{Name: "c", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got.Constraints != 2 || got.PublicInputs != 1 || got.PrivateInputs != 3 || got.Description != "初稿" {
		t.Fatalf("empty patch did not return the existing record: %+v", got)
	}
	after, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("empty patch wrote a new version of the data file")
	}
}

func TestPatchGating(t *testing.T) {
	s := openTestStore(t)

	// Unknown version -> not found, even with no modifiable fields.
	if _, err := s.PatchCircuit(CircuitPatch{Name: "ghost", Version: 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	seedPatchDraft(t, s)
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	// Frozen version -> frozen, even when nothing is asked to change.
	if _, err := s.PatchCircuit(CircuitPatch{Name: "c", Version: 1}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("want frozen, got %v", err)
	}
	if _, err := s.PatchCircuit(CircuitPatch{Name: "c", Version: 1, Description: pstring("x")}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("want frozen for description patch, got %v", err)
	}
}

func TestPatchValidatesResultingRecordAtomically(t *testing.T) {
	s := openTestStore(t)
	seedPatchDraft(t, s)

	cases := []struct {
		name  string
		patch CircuitPatch
	}{
		{"zero constraints", CircuitPatch{Name: "c", Version: 1, Constraints: pint(0)}},
		{"negative public", CircuitPatch{Name: "c", Version: 1, PublicInputs: pint(-1)}},
		{"negative private", CircuitPatch{Name: "c", Version: 1, PrivateInputs: pint(-1)}},
		{
			"bad count bundled with good description",
			CircuitPatch{Name: "c", Version: 1, Constraints: pint(-3), Description: pstring("should not land")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.PatchCircuit(tc.patch); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("want invalid argument, got %v", err)
			}
		})
	}
	// Every field keeps its original value, including the description from the
	// bundled-but-rejected request.
	got, _ := s.GetCircuit("c", 1)
	if got.Constraints != 2 || got.PublicInputs != 1 || got.PrivateInputs != 3 || got.Description != "初稿" {
		t.Fatalf("a rejected patch leaked: %+v", got)
	}
}

func TestPatchWithDefinitionAllowsCompatibleAndRejectsIncompatible(t *testing.T) {
	s := openTestStore(t)
	// validDef uses wires 1 and 2: needs public+private >= 2 and exactly 1
	// constraint. Start at 1 public, 1 private.
	seedDraftWithDef(t, s, "d", 1, 1, 1, validDef)

	// A description-only change is accepted even though a definition exists.
	got, err := s.PatchCircuit(CircuitPatch{Name: "d", Version: 1, Description: pstring("notes")})
	if err != nil {
		t.Fatalf("description-only patch with definition: %v", err)
	}
	if got.Description != "notes" {
		t.Fatalf("description not applied: %+v", got)
	}

	// Widening the input layout keeps the definition legal and is accepted.
	got, err = s.PatchCircuit(CircuitPatch{Name: "d", Version: 1, PublicInputs: pint(2), PrivateInputs: pint(3)})
	if err != nil {
		t.Fatalf("compatible layout widening should be allowed: %v", err)
	}
	if got.PublicInputs != 2 || got.PrivateInputs != 3 {
		t.Fatalf("widening not applied: %+v", got)
	}

	// Changing the constraint count away from the definition length fails.
	if _, err := s.PatchCircuit(CircuitPatch{Name: "d", Version: 1, Constraints: pint(2)}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("constraint count mismatch should be refused: %v", err)
	}
	// Shrinking the layout below a referenced wire fails — even when bundled
	// with an otherwise-valid description, which must not land either.
	_, err = s.PatchCircuit(CircuitPatch{
		Name: "d", Version: 1, PublicInputs: pint(1), PrivateInputs: pint(0),
		Description: pstring("should not land"),
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("layout shrink should be refused: %v", err)
	}

	// Nothing leaked: counts, description and definition are all unchanged.
	got, _ = s.GetCircuit("d", 1)
	if got.Constraints != 1 || got.PublicInputs != 2 || got.PrivateInputs != 3 || got.Description != "notes" {
		t.Fatalf("refused patch with definition leaked: %+v", got)
	}
	def, err := s.GetDefinition("d", 1)
	if err != nil || def.Modulus != "7" || len(def.Constraints) != 1 {
		t.Fatalf("definition changed after refused patch: %+v %v", def, err)
	}
}

func TestPatchSequentialPartialUpdatesAccumulate(t *testing.T) {
	s := openTestStore(t)
	seedPatchDraft(t, s)

	// Two clients modify different fields in separate operations: both
	// results survive; a later write to the same field wins.
	if _, err := s.PatchCircuit(CircuitPatch{Name: "c", Version: 1, Description: pstring("A")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PatchCircuit(CircuitPatch{Name: "c", Version: 1, PublicInputs: pint(9)}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetCircuit("c", 1)
	if got.Description != "A" || got.PublicInputs != 9 ||
		got.Constraints != 2 || got.PrivateInputs != 3 {
		t.Fatalf("separate partial updates did not accumulate: %+v", got)
	}

	if _, err := s.PatchCircuit(CircuitPatch{Name: "c", Version: 1, Description: pstring("B")}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetCircuit("c", 1)
	if got.Description != "B" || got.PublicInputs != 9 {
		t.Fatalf("later same-field write did not win: %+v", got)
	}
}

func TestPatchPersistsAcrossReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedPatchDraft(t, s)
	if _, err := s.PatchCircuit(CircuitPatch{Name: "c", Version: 1, Description: pstring("重开")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PatchCircuit(CircuitPatch{Name: "c", Version: 1, PrivateInputs: pint(7)}); err != nil {
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
	got, err := s2.GetCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Constraints != 2 || got.PublicInputs != 1 || got.PrivateInputs != 7 || got.Description != "重开" {
		t.Fatalf("partial update lost after reopen: %+v", got)
	}
}

// UpdateCircuit retains its original full-replace semantics for Go API
// callers, including the meaning of zero input counts and empty descriptions.
func TestUpdateCircuitStillReplacesWholeRecord(t *testing.T) {
	s := openTestStore(t)
	seedPatchDraft(t, s)

	got, err := s.UpdateCircuit(Circuit{
		Name: "c", Version: 1, Constraints: 8,
		PublicInputs: 0, PrivateInputs: 0, Description: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Constraints != 8 || got.PublicInputs != 0 || got.PrivateInputs != 0 || got.Description != "" {
		t.Fatalf("UpdateCircuit no longer replaces the whole record: %+v", got)
	}
}

// Under concurrent patches against one draft, each successful commit is built
// on the latest committed state: a freeze that lands first makes every later
// patch fail with ErrFrozen, and two stores on the same directory never lose
// each other's committed field edits.
func TestPatchConcurrentStoreHandlesObserveCommitsAndFreeze(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	defer s2.Close()

	if _, err := s1.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 5,
		PublicInputs: 1, PrivateInputs: 1, Description: "start"}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 200)
	// Client A repeatedly bumps the description only.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if _, e := s1.PatchCircuit(CircuitPatch{
				Name: "c", Version: 1, Description: pstring("desc"),
			}); e != nil {
				if !errors.Is(e, ErrFrozen) {
					errs <- e
				}
				return
			}
		}
	}()
	// Client B repeatedly bumps both input counts (never above 1000 so the
	// record stays legal).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			v := 2 + i
			if _, e := s2.PatchCircuit(CircuitPatch{
				Name: "c", Version: 1, PublicInputs: pint(v), PrivateInputs: pint(v),
			}); e != nil {
				if !errors.Is(e, ErrFrozen) {
					errs <- e
				}
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("unexpected patch error: %v", e)
	}

	got, err := s1.GetCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Constraints != 5 || got.Description != "desc" ||
		got.PublicInputs != got.PrivateInputs || got.PublicInputs < 2 {
		t.Fatalf("concurrent partial updates did not both survive: %+v", got)
	}

	// Once frozen, even a field-less patch is refused.
	if _, err := s2.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.PatchCircuit(CircuitPatch{Name: "c", Version: 1}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("patch after freeze should be frozen, got %v", err)
	}
}
