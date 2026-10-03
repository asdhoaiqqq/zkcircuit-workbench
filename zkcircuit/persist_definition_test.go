package zkcircuit

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// These tests pin the read-time contract for a constraint definition that is
// already committed in data.json: its required fields and their types must
// match what the importer writes. A missing field or a null must never be
// read as a legitimate zero value (an empty side array, wire 0), because the
// two cases are indistinguishable after a plain struct decode even though
// only the first is a real definition.

// readGenericEnv loads data.json as a generic JSON value so a test can
// damage individual fields without relying on byte offsets.
func readGenericEnv(t *testing.T, dir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func writeGenericEnv(t *testing.T, dir string, env map[string]any) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(dir, dirDataFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return raw
}

// firstDefinition walks to the first circuit's definition object.
func firstDefinition(t *testing.T, env map[string]any) map[string]any {
	t.Helper()
	circuit := env["circuits"].([]any)[0].(map[string]any)
	def, ok := circuit["definition"].(map[string]any)
	if !ok {
		t.Fatalf("seeded circuit has no definition object: %v", circuit["definition"])
	}
	return def
}

// TestCorruptPersistedDefinitionRefused damages each required field in turn
// and requires Open to refuse the whole directory. The judgment must not
// depend on the version being frozen or already compiled, so every shape is
// damaged against both a draft (never compiled) and a frozen+compiled store.
func TestCorruptPersistedDefinitionRefused(t *testing.T) {
	const goodDef = `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`

	tamperDef := func(mut func(def map[string]any)) func(map[string]any) {
		return func(env map[string]any) { mut(firstDefinition(t, env)) }
	}
	setSideTerm := func(side string, mut func(term map[string]any)) func(map[string]any) {
		return tamperDef(func(def map[string]any) {
			con := def["constraints"].([]any)[0].(map[string]any)
			term := con[side].([]any)[0].(map[string]any)
			mut(term)
		})
	}
	setConstraint := func(mut func(con map[string]any)) func(map[string]any) {
		return tamperDef(func(def map[string]any) {
			mut(def["constraints"].([]any)[0].(map[string]any))
		})
	}
	setCircuitField := func(field string, value any) func(map[string]any) {
		return func(env map[string]any) {
			circuit := env["circuits"].([]any)[0].(map[string]any)
			circuit[field] = value
		}
	}

	damage := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing modulus", tamperDef(func(d map[string]any) { delete(d, "modulus") })},
		{"null modulus", tamperDef(func(d map[string]any) { d["modulus"] = nil })},
		{"non-string modulus", tamperDef(func(d map[string]any) { d["modulus"] = 7 })},
		{"missing constraints", tamperDef(func(d map[string]any) { delete(d, "constraints") })},
		{"null constraints", tamperDef(func(d map[string]any) { d["constraints"] = nil })},
		{"non-array constraints", tamperDef(func(d map[string]any) { d["constraints"] = map[string]any{} })},
		{"null constraint element", tamperDef(func(d map[string]any) { d["constraints"].([]any)[0] = nil })},
		{"missing side a", setConstraint(func(c map[string]any) { delete(c, "a") })},
		{"missing side b", setConstraint(func(c map[string]any) { delete(c, "b") })},
		{"missing side c", setConstraint(func(c map[string]any) { delete(c, "c") })},
		{"null side", setConstraint(func(c map[string]any) { c["a"] = nil })},
		{"side not array", setConstraint(func(c map[string]any) { c["a"] = map[string]any{} })},
		{"null term element", setConstraint(func(c map[string]any) { c["a"].([]any)[0] = nil })},
		{"term not object", setConstraint(func(c map[string]any) { c["a"].([]any)[0] = 5 })},
		{"term missing wire", setSideTerm("a", func(term map[string]any) { delete(term, "wire") })},
		{"term null wire", setSideTerm("a", func(term map[string]any) { term["wire"] = nil })},
		{"term string wire", setSideTerm("a", func(term map[string]any) { term["wire"] = "0" })},
		{"term float wire", setSideTerm("a", func(term map[string]any) { term["wire"] = 1.5 })},
		{"term missing coeff", setSideTerm("a", func(term map[string]any) { delete(term, "coeff") })},
		{"term null coeff", setSideTerm("a", func(term map[string]any) { term["coeff"] = nil })},
		{"term numeric coeff", setSideTerm("a", func(term map[string]any) { term["coeff"] = 1 })},
		{"unknown term field", setSideTerm("a", func(term map[string]any) { term["q"] = 1 })},
		{"definition not object", setCircuitField("definition", 5)},
	}

	for _, frozen := range []bool{false, true} {
		state := "draft"
		if frozen {
			state = "frozen-compiled"
		}
		t.Run(state, func(t *testing.T) {
			for _, tc := range damage {
				t.Run(tc.name, func(t *testing.T) {
					dir := filepath.Join(t.TempDir(), "bench")
					s, err := Open(dir)
					if err != nil {
						t.Fatal(err)
					}
					seedDraftWithDef(t, s, "c", 1, 1, 1, goodDef)
					if frozen {
						if _, err := s.FreezeCircuit("c", 1); err != nil {
							t.Fatal(err)
						}
						if _, err := s.CompileCircuit("c", 1); err != nil {
							t.Fatal(err)
						}
					}
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}

					env := readGenericEnv(t, dir)
					tc.mutate(env)
					bad := writeGenericEnv(t, dir, env)

					got, err := Open(dir)
					if err == nil {
						got.Close()
						t.Fatalf("damaged definition was accepted")
					}
					if !errors.Is(err, ErrDataCorrupt) {
						t.Fatalf("want ErrDataCorrupt, got %v", err)
					}
					// Read-only and mutating commands all start by re-reading;
					// none of them may compile, check inputs or succeed.
					left, rerr := os.ReadFile(filepath.Join(dir, dirDataFile))
					if rerr != nil {
						t.Fatal(rerr)
					}
					if string(left) != string(bad) {
						t.Fatal("refused read modified data.json")
					}
					if s2, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
						if s2 != nil {
							s2.Close()
						}
						t.Fatalf("reopen accepted the damage: %v", err)
					}
				})
			}
		})
	}
}

// TestPersistedDefinitionEmptyArraysAndWireZeroLegal pins the other half:
// explicit empty side arrays are the zero linear combination and an explicit
// wire:0 names the constant wire. Both are ordinary definitions and must
// survive a reload, unlike a missing field.
func TestPersistedDefinitionEmptyArraysAndWireZeroLegal(t *testing.T) {
	const def = `{"modulus":"7","constraints":[
		{"a":[],"b":[],"c":[]},
		{"a":[{"wire":0,"coeff":"1"}],"b":[{"wire":0,"coeff":"1"}],"c":[{"wire":0,"coeff":"1"}]}]}`
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	createAndImport := func() {
		if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 2,
			PublicInputs: 0, PrivateInputs: 0, Description: "c"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ImportConstraints("c", 1, writeTempJSON(t, def)); err != nil {
			t.Fatal(err)
		}
	}
	createAndImport()
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	want, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen reads the canonical form, which keeps the explicit empty arrays
	// and the wire-0 constant terms; the artifact hash is unchanged.
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("explicit empty arrays / wire 0 rejected on reload: %v", err)
	}
	defer s2.Close()
	got, err := s2.GetDefinition("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Constraints) != 2 || len(got.Constraints[0].A) != 0 {
		t.Fatalf("empty side array did not survive reload: %+v", got.Constraints[0])
	}
	if len(got.Constraints[1].A) != 1 || got.Constraints[1].A[0].Wire != 0 {
		t.Fatalf("wire-0 term did not survive reload: %+v", got.Constraints[1].A)
	}
	if art, err := s2.CompileCircuit("c", 1); err != nil || art.Hash != want.Hash {
		t.Fatalf("artifact hash changed across reload: %+v %v", art, err)
	}
}

// TestCountsOnlyAndNullOrMissingDefinitionLoad verifies the legacy
// compatibility boundary: a version whose definition is absent or explicitly
// null is still the counts-only state, loads cleanly and later reports a
// missing definition when compiled.
func TestCountsOnlyAndNullOrMissingDefinitionLoad(t *testing.T) {
	for _, mode := range []string{"absent", "null"} {
		t.Run(mode, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateCircuit(Circuit{Name: "bare", Version: 1, Constraints: 1,
				Description: "d"}); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			if mode == "null" {
				env := readGenericEnv(t, dir)
				env["circuits"].([]any)[0].(map[string]any)["definition"] = nil
				writeGenericEnv(t, dir, env)
			}

			s2, err := Open(dir)
			if err != nil {
				t.Fatalf("counts-only record (%s definition) refused: %v", mode, err)
			}
			if _, err := s2.GetDefinition("bare", 1); !errors.Is(err, ErrDefinitionMissing) {
				t.Fatalf("want ErrDefinitionMissing, got %v", err)
			}
			if _, err := s2.FreezeCircuit("bare", 1); err != nil {
				t.Fatal(err)
			}
			if _, err := s2.CompileCircuit("bare", 1); !errors.Is(err, ErrDefinitionMissing) {
				t.Fatalf("counts-only compile: want ErrDefinitionMissing, got %v", err)
			}
			s2.Close()
		})
	}
}

// TestDeletedFieldRejectedEvenWhenEmptyArrayWouldHashSame covers the scenario
// from the repair rule: a legal side that was an empty array is saved, the
// field is then deleted, and restoring [] would reproduce the identical
// artifact hash. The damaged file must still be refused.
func TestDeletedFieldRejectedEvenWhenEmptyArrayWouldHashSame(t *testing.T) {
	const zeroSideDef = `{"modulus":"7","constraints":[
		{"a":[],"b":[{"wire":1,"coeff":"1"}],"c":[]}]}`
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedDraftWithDef(t, s, "c", 1, 1, 0, zeroSideDef)
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	want, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	env := readGenericEnv(t, dir)
	con := firstDefinition(t, env)["constraints"].([]any)[0].(map[string]any)
	delete(con, "a") // "a" was []; restoring [] would keep the same hash
	bad := writeGenericEnv(t, dir, env)

	if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("deleted (empty) side accepted: %v", err)
	}
	left, _ := os.ReadFile(filepath.Join(dir, dirDataFile))
	if string(left) != string(bad) {
		t.Fatal("refused read modified data.json")
	}

	// Sanity: restoring the explicit empty array reproduces the same hash.
	con2 := readGenericEnv(t, dir)
	con2["circuits"].([]any)[0].(map[string]any)["definition"].(map[string]any)["constraints"].([]any)[0].(map[string]any)["a"] = []any{}
	writeGenericEnv(t, dir, con2)
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("restored empty array should load: %v", err)
	}
	defer s2.Close()
	if art, err := s2.CompileCircuit("c", 1); err != nil || art.Hash != want.Hash {
		t.Fatalf("restored [] must hash identically: %+v %v", art, err)
	}
}
