package zkcircuit

import (
	"errors"
	"path/filepath"
	"testing"
)

func intPtr(v int) *int       { return &v }
func stringPtr(v string) *string { return &v }

// seedPartialDraft creates a draft with 2 constraints, 1 public, 3 private
// inputs and description "初稿" — the exact fixture from the task.
func seedPartialDraft(t *testing.T, s *Store, name string) {
	t.Helper()
	if _, err := s.CreateCircuit(Circuit{
		Name: name, Version: 1,
		Constraints: 2, PublicInputs: 1, PrivateInputs: 3,
		Description: "初稿",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUpdatePartialDescriptionOnlyKeepsCounts(t *testing.T) {
	s := openTestStore(t)
	seedPartialDraft(t, s, "c")

	got, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{Description: stringPtr("改后")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Constraints != 2 || got.PublicInputs != 1 || got.PrivateInputs != 3 {
		t.Fatalf("description-only update changed counts: %+v", got)
	}
	if got.Description != "改后" {
		t.Fatalf("description not updated: %+v", got)
	}
	// The stored record matches.
	stored, _ := s.GetCircuit("c", 1)
	if stored != got {
		t.Fatalf("stored record %+v != returned %+v", stored, got)
	}
}

func TestUpdatePartialExplicitZeroAndEmpty(t *testing.T) {
	s := openTestStore(t)
	seedPartialDraft(t, s, "c")

	// Explicit zero input counts must zero the fields, not be treated as
	// "not provided".
	got, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PublicInputs:  intPtr(0),
		PrivateInputs: intPtr(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.PublicInputs != 0 || got.PrivateInputs != 0 {
		t.Fatalf("explicit zero counts not applied: %+v", got)
	}
	if got.Constraints != 2 || got.Description != "初稿" {
		t.Fatalf("zero update leaked into other fields: %+v", got)
	}

	// Explicit empty description must clear the description.
	got, err = s.UpdateCircuitPartial("c", 1, PartialCircuit{Description: stringPtr("")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "" {
		t.Fatalf("explicit empty description not applied: %+v", got)
	}
	if got.Constraints != 2 || got.PublicInputs != 0 || got.PrivateInputs != 0 {
		t.Fatalf("empty description update leaked: %+v", got)
	}
}

func TestUpdatePartialNoOpReturnsRecordAndCommitsNothing(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedPartialDraft(t, s, "c")
	dataBefore, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Constraints != 2 || got.PublicInputs != 1 || got.PrivateInputs != 3 || got.Description != "初稿" {
		t.Fatalf("no-op returned wrong record: %+v", got)
	}
	dataAfter, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(dataBefore) != string(dataAfter) {
		t.Fatal("no-op update wrote to the data file")
	}
}

func TestUpdatePartialGatingNotFoundAndFrozen(t *testing.T) {
	s := openTestStore(t)

	if _, err := s.UpdateCircuitPartial("ghost", 1, PartialCircuit{Description: stringPtr("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown version: want not found, got %v", err)
	}
	// No fields at all still reports not found.
	if _, err := s.UpdateCircuitPartial("ghost", 1, PartialCircuit{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no-op on unknown version: want not found, got %v", err)
	}

	seedPartialDraft(t, s, "c")
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{Description: stringPtr("x")}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("frozen version: want frozen, got %v", err)
	}
	// No fields at all still reports frozen.
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("no-op on frozen version: want frozen, got %v", err)
	}
}

func TestUpdatePartialInvalidFieldRejectsWholeChange(t *testing.T) {
	s := openTestStore(t)
	seedPartialDraft(t, s, "c")

	cases := []struct {
		name  string
		patch PartialCircuit
	}{
		{"zero constraints", PartialCircuit{Constraints: intPtr(0)}},
		{"negative constraints", PartialCircuit{Constraints: intPtr(-1)}},
		{"negative public", PartialCircuit{PublicInputs: intPtr(-1)}},
		{"negative private", PartialCircuit{PrivateInputs: intPtr(-2)}},
		{"bad field with good field", PartialCircuit{Constraints: intPtr(0), Description: stringPtr("x")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.UpdateCircuitPartial("c", 1, tc.patch); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("want invalid argument, got %v", err)
			}
		})
	}
	// Everything preserved.
	got, _ := s.GetCircuit("c", 1)
	if got.Constraints != 2 || got.PublicInputs != 1 || got.PrivateInputs != 3 || got.Description != "初稿" {
		t.Fatalf("rejected update leaked: %+v", got)
	}
}

func TestUpdatePartialMultipleFieldsValidatedTogether(t *testing.T) {
	s := openTestStore(t)
	seedPartialDraft(t, s, "c")

	// Both fields valid individually and combined: applied atomically.
	got, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		Constraints: intPtr(5), PublicInputs: intPtr(2), Description: stringPtr("both"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Constraints != 5 || got.PublicInputs != 2 || got.Description != "both" {
		t.Fatalf("combined update wrong: %+v", got)
	}
	if got.PrivateInputs != 3 {
		t.Fatalf("combined update leaked into private inputs: %+v", got)
	}
}

func TestUpdatePartialWithDefinition(t *testing.T) {
	s := openTestStore(t)
	// Definition has 1 constraint referencing wire 2, with 1 public + 1
	// private input (wire 2 = first private).
	seedDraftWithDef(t, s, "c", 1, 1, 1, validDef)

	// A compatible adjustment is allowed even though a definition exists.
	got, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PublicInputs: intPtr(2), PrivateInputs: intPtr(3),
	})
	if err != nil {
		t.Fatalf("compatible adjustment should be allowed: %v", err)
	}
	if got.PublicInputs != 2 || got.PrivateInputs != 3 || got.Constraints != 1 {
		t.Fatalf("compatible adjustment wrong: %+v", got)
	}

	// Shrinking the layout below a referenced wire is rejected wholesale.
	// After the compatible adjustment the layout is 2 public + 3 private
	// (wires 1..5); zeroing both inputs leaves only the constant wire, so
	// wire 2 is out of range.
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PublicInputs: intPtr(0), PrivateInputs: intPtr(0),
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("shrinking past a referenced wire: want invalid, got %v", err)
	}
	// Changing the constraint count away from the definition's is rejected.
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{Constraints: intPtr(2)}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("constraint count drift: want invalid, got %v", err)
	}
	// A good field alongside a bad field refuses the whole change.
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		Description: stringPtr("good"), PublicInputs: intPtr(0), PrivateInputs: intPtr(0),
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("good field with bad field: want invalid, got %v", err)
	}

	// Counts and description preserved; definition untouched.
	stored, _ := s.GetCircuit("c", 1)
	if stored.PublicInputs != 2 || stored.PrivateInputs != 3 || stored.Constraints != 1 {
		t.Fatalf("rejected update leaked: %+v", stored)
	}
	if stored.Description != "c" {
		t.Fatalf("rejected update changed description: %+v", stored)
	}
	def, err := s.GetDefinition("c", 1)
	if err != nil || len(def.Constraints) != 1 {
		t.Fatalf("definition changed after rejected update: %+v %v", def, err)
	}
}

func TestUpdatePartialPreservesPriorCommittedFields(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedPartialDraft(t, s, "c")

	// Client A changes the description.
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{Description: stringPtr("A")}); err != nil {
		t.Fatal(err)
	}
	// Client B changes the input counts.
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{PublicInputs: intPtr(4), PrivateInputs: intPtr(5)}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetCircuit("c", 1)
	if got.Description != "A" || got.PublicInputs != 4 || got.PrivateInputs != 5 || got.Constraints != 2 {
		t.Fatalf("prior fields not preserved: %+v", got)
	}

	// Same field: the later write wins.
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{Description: stringPtr("B")}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetCircuit("c", 1)
	if got.Description != "B" || got.PublicInputs != 4 || got.PrivateInputs != 5 {
		t.Fatalf("later field write did not win / leaked: %+v", got)
	}

	// Reopen: the merged result is the committed state.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	disk, err := s2.GetCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if disk != got {
		t.Fatalf("disk state %+v != memory state %+v", disk, got)
	}
}
