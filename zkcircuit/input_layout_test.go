package zkcircuit

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// These tests pin the input-layout representability rule. Wire 0 always names
// the constant-one wire, so a version declares 1 + public + private wires; the
// sum must fit in the running platform's int (M = math.MaxInt). The rule is
// enforced with overflow-safe comparisons on every write path — create, whole
// replacement and partial update — and again when committed data is read.

const emptySidesDef = `{"modulus":"7","constraints":[{"a":[],"b":[],"c":[]}]}`

// TestCreateInputLayoutBoundaries drives create across the representable
// boundary in both directions.
func TestCreateInputLayoutBoundaries(t *testing.T) {
	M := math.MaxInt
	cases := []struct {
		name    string
		public  int
		private int
		wantErr bool
	}{
		{"zero zero keeps the constant wire", 0, 0, false},
		{"max-1 public, zero private", M - 1, 0, false},
		{"zero public, max-1 private", 0, M - 1, false},
		{"max public, one private wraps", M, 1, true},
		{"one public, max private wraps", 1, M, true},
		{"max public, zero private already overflows the constant slot", M, 0, true},
		{"zero public, max private plus constant overflows", 0, M, true},
		{"max both", M, M, true},
		{"splitting the one spare slot exactly", 1, M - 2, false}, // 1+1+M-2 = M
		{"one past the split spare slot", 1, M - 1, true},         // 1+1+M-1 = M+1
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			_, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1,
				PublicInputs: tc.public, PrivateInputs: tc.private})
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidArgument) {
					t.Fatalf("want ErrInvalidArgument, got %v", err)
				}
				if !strings.Contains(err.Error(), "not representable") {
					t.Fatalf("error must explain the layout is not representable: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("legal layout rejected: %v", err)
			}
		})
	}
}

// TestCreateOverflowWritesNothing ensures a rejected create leaves no record
// and no committed file change behind.
func TestCreateOverflowWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "keep", Version: 1, Constraints: 1,
		Description: "stay"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.CreateCircuit(Circuit{Name: "bad", Version: 1, Constraints: 1,
		PublicInputs: math.MaxInt, PrivateInputs: 1}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("rejected overflow create rewrote data.json")
	}
}

// TestFullUpdateOverflowRejected: the whole-record replacement judges the
// supplied counts before looking the version up, so an unrepresentable
// replacement is invalid even for an unknown or a frozen version, and on a
// live draft the whole change (counts and description) is refused together.
func TestFullUpdateOverflowRejected(t *testing.T) {
	s := openTestStore(t)
	seedPartialDraft(t, s, "c") // 2 constraints, 1 public, 3 private, "初稿"

	overflow := Circuit{Name: "c", Version: 1, Constraints: 2,
		PublicInputs: math.MaxInt, PrivateInputs: 1, Description: "new"}
	if _, err := s.UpdateCircuit(overflow); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("overflow full update on a draft: want invalid, got %v", err)
	}
	got, _ := s.GetCircuit("c", 1)
	if got.Constraints != 2 || got.PublicInputs != 1 || got.PrivateInputs != 3 || got.Description != "初稿" {
		t.Fatalf("rejected full update leaked: %+v", got)
	}

	// Counts judged before existence: unknown version still reports invalid.
	unknown := overflow
	unknown.Version = 9
	if _, err := s.UpdateCircuit(unknown); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("overflow full update on unknown version: want invalid, got %v", err)
	}

	// Counts judged before frozen state: a frozen target still reports invalid.
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateCircuit(overflow); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("overflow full update on frozen version: want invalid, got %v", err)
	}
}

// TestPartialUpdateOverflowMergesRetainedCounts covers the headline partial
// rule: changing only one input count still judges it together with the
// retained other count, and any description carried in the same change is
// refused with it.
func TestPartialUpdateOverflowMergesRetainedCounts(t *testing.T) {
	s := openTestStore(t)
	seedPartialDraft(t, s, "c") // 1 public, 3 private
	M := math.MaxInt

	// Only the public count is supplied; the retained 3 private inputs push it
	// over the bound.
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PublicInputs: intPtr(M), Description: stringPtr("x"),
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("public=M with retained private=3: want invalid, got %v", err)
	}
	// Only the private count is supplied; the retained 1 public input pushes.
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PrivateInputs: intPtr(M),
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("private=M with retained public=1: want invalid, got %v", err)
	}
	// Counts and description all stay as they were; nothing committed.
	got, _ := s.GetCircuit("c", 1)
	if got.Constraints != 2 || got.PublicInputs != 1 || got.PrivateInputs != 3 || got.Description != "初稿" {
		t.Fatalf("rejected partial update leaked: %+v", got)
	}

	// The exact boundary through a partial edit is still reachable: zero the
	// retained private inputs first, then take public to M-1.
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PrivateInputs: intPtr(0),
	}); err != nil {
		t.Fatal(err)
	}
	edge, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PublicInputs: intPtr(M - 1),
	})
	if err != nil {
		t.Fatalf("public=M-1 with private=0 must be legal: %v", err)
	}
	if edge.PublicInputs != M-1 || edge.PrivateInputs != 0 {
		t.Fatalf("boundary partial update wrong: %+v", edge)
	}

	// Explicit zero supplied on the other side while at M-1 stays zero and is
	// not conflated with "omitted".
	again, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PrivateInputs: intPtr(0),
	})
	if err != nil {
		t.Fatalf("explicit zero private at the boundary: %v", err)
	}
	if again.PublicInputs != M-1 || again.PrivateInputs != 0 {
		t.Fatalf("explicit zero leaked: %+v", again)
	}
}

// TestPartialUpdateGatingOrderWithOverflow preserves the existing judgment
// order: unknown versions report not found and frozen versions frozen even
// when the merged counts would be unrepresentable.
func TestPartialUpdateGatingOrderWithOverflow(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.UpdateCircuitPartial("ghost", 1, PartialCircuit{
		PublicInputs: intPtr(math.MaxInt),
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown version: want not found, got %v", err)
	}

	seedPartialDraft(t, s, "c") // 1 public, 3 private
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	// public=M merged with the frozen record's private=3 would overflow, but
	// the frozen state is settled first.
	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PublicInputs: intPtr(math.MaxInt),
	}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("frozen version: want frozen, got %v", err)
	}
}

// TestPartialOverflowWithImportedDefinition refuses the whole change on a
// version that already carries a definition, leaving the imported definition
// byte-for-byte usable.
func TestPartialOverflowWithImportedDefinition(t *testing.T) {
	s := openTestStore(t)
	// 1 constraint, 1 public + 1 private, a term-free definition.
	seedDraftWithDef(t, s, "c", 1, 1, 1, emptySidesDef)

	if _, err := s.UpdateCircuitPartial("c", 1, PartialCircuit{
		PublicInputs: intPtr(math.MaxInt),
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("overflow partial with definition: want invalid, got %v", err)
	}
	stored, _ := s.GetCircuit("c", 1)
	if stored.PublicInputs != 1 || stored.PrivateInputs != 1 {
		t.Fatalf("counts changed after refused update: %+v", stored)
	}
	def, err := s.GetDefinition("c", 1)
	if err != nil || len(def.Constraints) != 1 {
		t.Fatalf("imported definition damaged: %+v %v", def, err)
	}
}

// TestBoundaryLayoutPersistsAcrossReload: public M-1, private 0 creates,
// queries, survives a reload and stays editable, exercising the exact
// representable edge without allocating the (impossibly large) wire array.
func TestBoundaryLayoutPersistsAcrossReload(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateCircuit(Circuit{Name: "edge", Version: 1, Constraints: 1,
		PublicInputs: math.MaxInt - 1, PrivateInputs: 0, Description: "edge"})
	if err != nil {
		t.Fatalf("create M-1,0: %v", err)
	}
	if created.PublicInputs != math.MaxInt-1 {
		t.Fatalf("created count drifted: %d", created.PublicInputs)
	}
	if _, err := s.GetCircuit("edge", 1); err != nil {
		t.Fatalf("query boundary record: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reload boundary record: %v", err)
	}
	defer s2.Close()
	got, err := s2.GetCircuit("edge", 1)
	if err != nil {
		t.Fatalf("query after reload: %v", err)
	}
	if got.PublicInputs != math.MaxInt-1 || got.PrivateInputs != 0 {
		t.Fatalf("reloaded boundary record drifted: %+v", got)
	}
}

// TestZeroInputsEmptySidesDefinitionUnchanged pins the constant-wire layout
// and the no-regression path: zero public and zero private inputs still name
// wire 0, an all-empty a/b/c definition imports, compiles and re-hashes
// identically across a reload.
func TestZeroInputsEmptySidesDefinitionUnchanged(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "z", Version: 1, Constraints: 1,
		PublicInputs: 0, PrivateInputs: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints("z", 1, writeTempJSON(t, emptySidesDef)); err != nil {
		t.Fatalf("term-free definition on 0/0 inputs: %v", err)
	}
	if _, err := s.FreezeCircuit("z", 1); err != nil {
		t.Fatal(err)
	}
	first, err := s.CompileCircuit("z", 1)
	if err != nil {
		t.Fatalf("compile 0/0: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reload 0/0 record: %v", err)
	}
	defer s2.Close()
	again, err := s2.CompileCircuit("z", 1)
	if err != nil {
		t.Fatalf("recompile after reload: %v", err)
	}
	if again.Hash != first.Hash {
		t.Fatalf("compile hash changed across reload: %q vs %q", first.Hash, again.Hash)
	}
}

// TestOverflowRecordRefusedOnLoad: a committed record whose declared input
// layout cannot be represented (here M-1 public bumped to 1 private, summing
// past M once the constant wire is counted) makes the whole directory
// unreadable with ErrDataCorrupt; no query returns partial results and the
// original file is left byte-for-byte in place.
func TestOverflowRecordRefusedOnLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "edge", Version: 1, Constraints: 1,
		PublicInputs: math.MaxInt - 1, PrivateInputs: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "healthy", Version: 1, Constraints: 1,
		Description: "fine"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	env := readGenericEnv(t, dir)
	for _, raw := range env["circuits"].([]any) {
		rec := raw.(map[string]any)
		if rec["name"] == "edge" {
			// json.Number keeps the digits exact; a float64 round-trip would
			// rewrite M-1 as an imprecise value the strict int decoder rejects
			// before the layout rule is reached.
			rec["public_inputs"] = json.Number(strconv.Itoa(math.MaxInt - 1))
			rec["private_inputs"] = json.Number("1") // 1 + (M-1) + 1 = M+1
		}
	}
	bad := writeGenericEnv(t, dir, env)

	got, err := Open(dir)
	if err == nil {
		got.Close()
		t.Fatal("unrepresentable committed layout was accepted on load")
	}
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	if !strings.Contains(err.Error(), "not representable") {
		t.Fatalf("corruption error must name the layout limit: %v", err)
	}
	left, rerr := os.ReadFile(filepath.Join(dir, dirDataFile))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(left) != string(bad) {
		t.Fatal("refused read modified data.json")
	}
}

// TestOverflowRecordWithDefinitionRefusedOnLoad reproduces the original
// failure mode: a term-free definition committed against overflowing counts
// imported cleanly only because public+private wrapped negative, and the next
// read reported corruption via a definition mismatch. The explicit layout
// rule now names the same condition directly and still refuses the read.
func TestOverflowRecordWithDefinitionRefusedOnLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1,
		PublicInputs: math.MaxInt - 1, PrivateInputs: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints("c", 1, writeTempJSON(t, emptySidesDef)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	env := readGenericEnv(t, dir)
	rec := readFirstCircuit(env)
	// Keep both counts as exact digits: a float64 round-trip would fail the
	// strict int decoder instead of reaching the layout rule.
	rec["public_inputs"] = json.Number(strconv.Itoa(math.MaxInt - 1))
	rec["private_inputs"] = json.Number("1") // 1 + (M-1) + 1 = M+1
	writeGenericEnv(t, dir, env)

	if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("overflow layout with definition: want ErrDataCorrupt, got %v", err)
	}
}
