package zkcircuit

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the input-layout ceiling: wire 0 is always the constant,
// so public+private+1 slots must fit this platform's int. Declaring a layout
// beyond that is refused at declaration time (ErrInvalidArgument) and a
// committed record carrying such a layout fails the read as ErrDataCorrupt.

func TestCreateRejectsUnrepresentableInputLayout(t *testing.T) {
	s := openTestStore(t)

	cases := []struct {
		name    string
		public  int
		private int
	}{
		{"max-plus-one", math.MaxInt, 1},
		{"max-alone", math.MaxInt, 0},   // constant wire 0 needs one more slot
		{"private-max", 0, math.MaxInt}, // the constant slot overflows here too
		{"sum-overflow", math.MaxInt - 1, 1},
		{"both-large", math.MaxInt / 2, math.MaxInt/2 + 1},
	}
	for i, tc := range cases {
		_, err := s.CreateCircuit(Circuit{
			Name: fmt.Sprintf("c%d", i), Version: 1, Constraints: 1,
			PublicInputs: tc.public, PrivateInputs: tc.private,
		})
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s: want invalid argument, got %v", tc.name, err)
		}
		if !strings.Contains(err.Error(), "input layout") {
			t.Fatalf("%s: error should name the input layout, got %q", tc.name, err)
		}
		// Nothing was saved: the version stays unknown, not silently
		// accepted with a wrapped-around smaller layout.
		if _, err := s.GetCircuit(fmt.Sprintf("c%d", i), 1); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: rejected create left a record behind: %v", tc.name, err)
		}
	}
}

func TestCreateAcceptsBoundaryInputLayout(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// public = MaxInt-1, private = 0: with the constant wire the layout is
	// exactly MaxInt slots — the largest representable one.
	if _, err := s.CreateCircuit(Circuit{
		Name: "edge", Version: 1, Constraints: 1,
		PublicInputs: math.MaxInt - 1, PrivateInputs: 0,
	}); err != nil {
		t.Fatalf("largest representable layout rejected: %v", err)
	}
	// Zero public and zero private inputs keep the constant wire 0.
	if _, err := s.CreateCircuit(Circuit{
		Name: "zero", Version: 1, Constraints: 1,
		PublicInputs: 0, PrivateInputs: 0,
	}); err != nil {
		t.Fatalf("zero-input layout rejected: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Both records survive a reload and read back exactly as declared.
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	edge, err := s2.GetCircuit("edge", 1)
	if err != nil {
		t.Fatal(err)
	}
	if edge.PublicInputs != math.MaxInt-1 || edge.PrivateInputs != 0 {
		t.Fatalf("boundary layout did not round-trip: %+v", edge)
	}
	zero, err := s2.GetCircuit("zero", 1)
	if err != nil {
		t.Fatal(err)
	}
	if zero.PublicInputs != 0 || zero.PrivateInputs != 0 {
		t.Fatalf("zero layout did not round-trip: %+v", zero)
	}
}

func TestUpdateCircuitRejectsOverflowLayout(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(Circuit{
		Name: "c", Version: 1, Constraints: 2,
		PublicInputs: 1, PrivateInputs: 3, Description: "初稿",
	}); err != nil {
		t.Fatal(err)
	}

	// Whole-record replacement carrying an unrepresentable layout.
	if _, err := s.UpdateCircuit(Circuit{
		Name: "c", Version: 1, Constraints: 2,
		PublicInputs: math.MaxInt, PrivateInputs: 1, Description: "爆",
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("whole replacement: want invalid argument, got %v", err)
	}
	stored, _ := s.GetCircuit("c", 1)
	if stored.PublicInputs != 1 || stored.PrivateInputs != 3 || stored.Description != "初稿" {
		t.Fatalf("rejected replacement leaked into the record: %+v", stored)
	}

	// The pre-existing judgement order is kept: counts are judged before
	// the target is looked up, so an unknown version with an overflowing
	// layout still reports invalid argument rather than not found.
	if _, err := s.UpdateCircuit(Circuit{
		Name: "ghost", Version: 1, Constraints: 1,
		PublicInputs: math.MaxInt, PrivateInputs: 1,
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unknown version with bad layout: want invalid argument, got %v", err)
	}
}

func TestUpdatePartialLayoutCheckMergesKeptCounts(t *testing.T) {
	s := openTestStore(t)

	// Only the public count is patched; the kept private count (1) must
	// still be part of the layout judgement.
	if _, err := s.CreateCircuit(Circuit{
		Name: "c", Version: 1, Constraints: 1,
		PublicInputs: 0, PrivateInputs: 1, Description: "d",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PublicInputs: intPtr(math.MaxInt),
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("public-only patch: want invalid argument, got %v", err)
	}
	stored, _ := s.GetCircuit("c", 1)
	if stored.PublicInputs != 0 || stored.PrivateInputs != 1 {
		t.Fatalf("rejected patch leaked: %+v", stored)
	}

	// And the mirror: only the private count is patched, the kept public
	// count (MaxInt-1) leaves no room for the constant wire any more.
	if _, err := s.CreateCircuit(Circuit{
		Name: "m", Version: 1, Constraints: 1,
		PublicInputs: math.MaxInt - 1, PrivateInputs: 0, Description: "d",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateCircuitPartial("m", 1, PartialCircuit{
		PrivateInputs: intPtr(1),
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("private-only patch: want invalid argument, got %v", err)
	}
	stored, _ = s.GetCircuit("m", 1)
	if stored.PublicInputs != math.MaxInt-1 || stored.PrivateInputs != 0 {
		t.Fatalf("rejected patch leaked: %+v", stored)
	}
}

func TestUpdatePartialOverflowRefusesWholeChange(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// A draft that already carries an imported constraint definition.
	seedDraftWithDef(t, s, "c", 1, 1, 1, validDef)
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{Description: stringPtr("初稿")}); err != nil {
		t.Fatal(err)
	}
	dataBefore, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}

	// The patch also carries a description and a constraint count; the
	// overflowing layout alone refuses the entire modification.
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PublicInputs: intPtr(math.MaxInt),
		Constraints:  intPtr(5),
		Description:  stringPtr("改后"),
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want invalid argument, got %v", err)
	}

	stored, _ := s.GetCircuit("c", 1)
	if stored.PublicInputs != 1 || stored.PrivateInputs != 1 ||
		stored.Constraints != 1 || stored.Description != "初稿" {
		t.Fatalf("rejected modification leaked: %+v", stored)
	}
	// The imported definition is still there and still intact.
	if _, err := s.GetDefinition("c", 1); err != nil {
		t.Fatalf("imported definition lost after rejected update: %v", err)
	}
	// The committed file is byte-for-byte untouched.
	dataAfter, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(dataBefore) != string(dataAfter) {
		t.Fatal("rejected modification rewrote the data file")
	}
}

func TestLoadRejectsUnrepresentableInputLayout(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{
		Name: "c", Version: 1, Constraints: 1,
		PublicInputs: 1, PrivateInputs: 1, Description: "d",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Damage the committed record into an unrepresentable layout.
	path := filepath.Join(dir, "data.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(raw), `"public_inputs": 1`,
		fmt.Sprintf(`"public_inputs": %d`, math.MaxInt), 1)
	if bad == string(raw) {
		t.Fatal("seed file did not contain the expected public_inputs field")
	}
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}

	// The directory no longer opens: the layout is reported as corruption,
	// never shrunk or skipped.
	_, err = Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want data corrupt, got %v", err)
	}
	if !strings.Contains(err.Error(), "input layout") {
		t.Fatalf("error should name the input layout, got %q", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != bad {
		t.Fatal("corrupt data file was modified")
	}
}
