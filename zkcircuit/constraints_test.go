package zkcircuit

import (
	"errors"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
)

// This file covers constraint definition import, compilation and witness
// checking: parsing rules, canonicalization/hash invariants, compile
// idempotence and isolation, check outcomes, reopen persistence and
// rejection of corrupt definition/artifact records.

// --- helpers ---------------------------------------------------------------

func term(wire int, coeff string) Term { return Term{Wire: wire, Coeff: coeff} }

func con(a, b, c []Term) Constraint { return Constraint{A: a, B: b, C: c} }

func setupCompiled(t *testing.T, s *Store, name string, v, pub, priv int, def ConstraintDefinition) Artifact {
	t.Helper()
	if _, err := s.CreateCircuit(Circuit{Name: name, Version: v,
		Constraints: len(def.Constraints), PublicInputs: pub, PrivateInputs: priv}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints(name, v, def); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit(name, v); err != nil {
		t.Fatal(err)
	}
	art, err := s.CompileCircuit(name, v)
	if err != nil {
		t.Fatal(err)
	}
	return art
}

// --- parsing ---------------------------------------------------------------

func TestParseConstraintDefinitionValid(t *testing.T) {
	raw := []byte(`{
		"modulus": "2147483647",
		"constraints": [
			{"a": [{"wire": 0, "coeff": "1"}], "b": [], "c": []}
		]
	}`)
	def, err := ParseConstraintDefinition(raw)
	if err != nil {
		t.Fatalf("valid definition rejected: %v", err)
	}
	if def.Modulus != "2147483647" || len(def.Constraints) != 1 {
		t.Fatalf("parsed definition wrong: %+v", def)
	}
	if len(def.Constraints[0].A) != 1 || len(def.Constraints[0].B) != 0 || len(def.Constraints[0].C) != 0 {
		t.Fatalf("parsed sides wrong: %+v", def.Constraints[0])
	}
}

func TestParseConstraintDefinitionRejections(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"invalid json", `{not json`},
		{"modulus missing", `{"constraints":[]}`},
		{"constraints missing", `{"modulus":"2"}`},
		{"modulus non-decimal", `{"modulus":"0x10","constraints":[]}`},
		{"modulus float", `{"modulus":"2.5","constraints":[]}`},
		{"modulus plus sign", `{"modulus":"+2","constraints":[]}`},
		{"modulus empty", `{"modulus":"","constraints":[]}`},
		{"modulus one", `{"modulus":"1","constraints":[]}`},
		{"modulus zero", `{"modulus":"0","constraints":[]}`},
		{"modulus negative", `{"modulus":"-2","constraints":[]}`},
		{"modulus composite", `{"modulus":"4","constraints":[]}`},
		{"modulus composite large", `{"modulus":"2147483646","constraints":[]}`},
		{"modulus too large", `{"modulus":"2147483648","constraints":[]}`},
		{"modulus huge composite", `{"modulus":"999999999999999999999999999999","constraints":[]}`},
		{"side a missing", `{"modulus":"2","constraints":[{"b":[],"c":[]}]}`},
		{"side b missing", `{"modulus":"2","constraints":[{"a":[],"c":[]}]}`},
		{"side c missing", `{"modulus":"2","constraints":[{"a":[],"b":[]}]}`},
		{"term wire missing", `{"modulus":"2","constraints":[{"a":[{"coeff":"1"}],"b":[],"c":[]}]}`},
		{"term coeff missing", `{"modulus":"2","constraints":[{"a":[{"wire":0}],"b":[],"c":[]}]}`},
		{"negative wire", `{"modulus":"2","constraints":[{"a":[{"wire":-1,"coeff":"1"}],"b":[],"c":[]}]}`},
		{"non-decimal coeff", `{"modulus":"2","constraints":[{"a":[{"wire":0,"coeff":"1.5"}],"b":[],"c":[]}]}`},
		{"coeff hex", `{"modulus":"2","constraints":[{"a":[{"wire":0,"coeff":"0xff"}],"b":[],"c":[]}]}`},
		{"coeff empty", `{"modulus":"2","constraints":[{"a":[{"wire":0,"coeff":""}],"b":[],"c":[]}]}`},
		{"wire float", `{"modulus":"2","constraints":[{"a":[{"wire":1.5,"coeff":"1"}],"b":[],"c":[]}]}`},
		{"modulus null", `{"modulus":null,"constraints":[]}`},
		{"constraints null", `{"modulus":"2","constraints":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseConstraintDefinition([]byte(tc.raw)); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("want ErrInvalidArgument, got %v", err)
			}
		})
	}
}

// --- import rules ----------------------------------------------------------

func TestImportConstraintsRules(t *testing.T) {
	s := openTestStore(t)
	def := ConstraintDefinition{
		Modulus: "2147483647",
		Constraints: []Constraint{
			con([]Term{term(0, "1")}, []Term{}, []Term{}),
		},
	}

	// Unknown version.
	if _, err := s.ImportConstraints("ghost", 1, def); !errors.Is(err, ErrNotFound) {
		t.Fatalf("import unknown: want not found, got %v", err)
	}
	// Frozen version.
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints("c", 1, def); !errors.Is(err, ErrFrozen) {
		t.Fatalf("import frozen: want frozen, got %v", err)
	}
	// Draft version with the declared constraint count.
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 2, Constraints: 1, PublicInputs: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints("c", 2, def); err != nil {
		t.Fatalf("import draft: %v", err)
	}
	got, err := s.GetCircuit("c", 2)
	if err != nil || got.Constraints != 1 {
		t.Fatalf("import changed counts: %+v %v", got, err)
	}
}

func TestImportConstraintsCountAndWireChecks(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 2, PublicInputs: 1, PrivateInputs: 1}); err != nil {
		t.Fatal(err)
	}

	badCount := ConstraintDefinition{Modulus: "2", Constraints: []Constraint{
		con(nil, nil, nil),
	}}
	if _, err := s.ImportConstraints("c", 1, badCount); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("count mismatch: want invalid argument, got %v", err)
	}

	badWire := ConstraintDefinition{Modulus: "2", Constraints: []Constraint{
		con([]Term{term(3, "1")}, nil, nil),
		con(nil, nil, nil),
	}}
	if _, err := s.ImportConstraints("c", 1, badWire); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("wire out of range: want invalid argument, got %v", err)
	}

	// Nothing was stored: compile must report definition missing.
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompileCircuit("c", 1); !errors.Is(err, ErrDefinitionMissing) {
		t.Fatalf("compile without definition: want definition missing, got %v", err)
	}
}

func TestImportConstraintsReplaceWholesale(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1}); err != nil {
		t.Fatal(err)
	}
	first := ConstraintDefinition{Modulus: "2", Constraints: []Constraint{
		con([]Term{term(0, "1")}, nil, nil),
	}}
	if _, err := s.ImportConstraints("c", 1, first); err != nil {
		t.Fatal(err)
	}
	second := ConstraintDefinition{Modulus: "3", Constraints: []Constraint{
		con(nil, nil, []Term{term(0, "2")}),
	}}
	if _, err := s.ImportConstraints("c", 1, second); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	art, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if art.Modulus != "3" {
		t.Fatalf("replace did not take effect: %+v", art)
	}
}

func TestUpdateCountsRejectedWhenDefinitionBecomesIllegal(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 2, PublicInputs: 1, PrivateInputs: 1}); err != nil {
		t.Fatal(err)
	}
	def := ConstraintDefinition{Modulus: "2", Constraints: []Constraint{
		con([]Term{term(1, "1")}, nil, nil),
		con(nil, nil, []Term{term(2, "1")}),
	}}
	if _, err := s.ImportConstraints("c", 1, def); err != nil {
		t.Fatal(err)
	}

	// Constraint count 1 vs imported 2: rejected, whole update.
	if _, err := s.UpdateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1, PublicInputs: 1, PrivateInputs: 1}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("constraint count shrink: want invalid argument, got %v", err)
	}
	// Wire 2 (private) would become out of range with no inputs at all.
	if _, err := s.UpdateCircuit(Circuit{Name: "c", Version: 1, Constraints: 2, PublicInputs: 0, PrivateInputs: 0}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("input removal: want invalid argument, got %v", err)
	}
	// Legal change: keep counts, tweak description.
	if _, err := s.UpdateCircuit(Circuit{Name: "c", Version: 1, Constraints: 2, PublicInputs: 1, PrivateInputs: 1, Description: "ok"}); err != nil {
		t.Fatalf("legal update rejected: %v", err)
	}
	got, _ := s.GetCircuit("c", 1)
	if got.Description != "ok" || got.Constraints != 2 {
		t.Fatalf("legal update did not apply: %+v", got)
	}
}

// --- canonicalization / hash ----------------------------------------------

func canonicalDef() ConstraintDefinition {
	return ConstraintDefinition{Modulus: "2147483647", Constraints: []Constraint{
		con([]Term{term(0, "3"), term(1, "2")}, []Term{term(2, "1")}, []Term{term(0, "6"), term(2, "2")}),
	}}
}

// compileInFreshStore imports def into c@1, freezes and compiles, returning
// the artifact hash.
func compileInFreshStore(t *testing.T, def ConstraintDefinition, name string, pub, priv int) string {
	t.Helper()
	s := openTestStore(t)
	art := setupCompiled(t, s, name, 1, pub, priv, def)
	return art.Hash
}

func TestArtifactHashCanonicalization(t *testing.T) {
	base := canonicalDef()
	baseHash := compileInFreshStore(t, base, "c", 1, 1)

	// Whitespace, term order, duplicates, zero terms and coefficients
	// differing by multiples of the modulus must not change the hash.
	variantRaw := ` {
		"modulus" : "2147483647" ,
		"constraints" : [ {
			"c" : [ { "wire" : 2 , "coeff" : "2" } , { "wire" : 1 , "coeff" : "0" } , { "wire" : 0 , "coeff" : "6" } ] ,
			"b" : [ { "wire" : 2 , "coeff" : "1" } , { "wire" : 2 , "coeff" : "0" } ] ,
			"a" : [ { "wire" : 1 , "coeff" : "2147483649" } , { "wire" : 0 , "coeff" : "3" } ]
		} ]
	} `
	variant, err := ParseConstraintDefinition([]byte(variantRaw))
	if err != nil {
		t.Fatal(err)
	}
	if got := compileInFreshStore(t, variant, "c", 1, 1); got != baseHash {
		t.Fatalf("canonicalization changed the hash:\n base=%s\nvariant=%s", baseHash, got)
	}
}

func TestArtifactHashSensitivity(t *testing.T) {
	base := canonicalDef()
	baseHash := compileInFreshStore(t, base, "c", 1, 1)

	same := func(name string, def ConstraintDefinition, pub, priv int) {
		t.Helper()
		if got := compileInFreshStore(t, def, name, pub, priv); got == baseHash {
			t.Fatalf("%s: hash unexpectedly unchanged", name)
		}
	}

	// Constraint order.
	swapped := ConstraintDefinition{Modulus: "2147483647", Constraints: []Constraint{
		con([]Term{term(1, "1")}, []Term{term(1, "1")}, []Term{term(1, "1")}),
		con([]Term{term(0, "3"), term(1, "2")}, []Term{term(2, "1")}, []Term{term(0, "6"), term(2, "2")}),
	}}
	same("constraint order", swapped, 1, 1)
	// Modulus.
	same("modulus", ConstraintDefinition{Modulus: "3", Constraints: base.Constraints}, 1, 1)
	// Name: compile the same definition under a different name.
	if got := compileInFreshStore(t, base, "other-name", 1, 1); got == baseHash {
		t.Fatal("name change did not affect the hash")
	}
	// Version: compileInFreshStore always uses v1, so compare two versions.
	s2 := openTestStore(t)
	setupCompiled(t, s2, "d", 1, 1, 1, base)
	setupCompiled(t, s2, "d", 2, 1, 1, base)
	a1, _ := s2.CompileCircuit("d", 1)
	a2, _ := s2.CompileCircuit("d", 2)
	if a1.Hash == a2.Hash {
		t.Fatal("version change did not affect the hash")
	}
	// Input partition: same definition, different public/private split.
	same("input partition", base, 2, 0)
}

// --- compile ---------------------------------------------------------------

func TestCompileIdempotentAndIsolated(t *testing.T) {
	s := openTestStore(t)
	def := canonicalDef()
	first := setupCompiled(t, s, "c", 1, 1, 1, def)
	if first.Hash == "" || first.Modulus != "2147483647" || first.Constraints != 1 {
		t.Fatalf("first artifact wrong: %+v", first)
	}
	again, err := s.CompileCircuit("c", 1)
	if err != nil || again != first {
		t.Fatalf("recompile not idempotent: %+v %v", again, err)
	}

	// Adding other versions does not move the artifact.
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 2, Constraints: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit("c", 2); err != nil {
		t.Fatal(err)
	}
	after, err := s.CompileCircuit("c", 1)
	if err != nil || after != first {
		t.Fatalf("artifact drifted after new version: %+v %v", after, err)
	}
	// The draft v2 still cannot compile.
	if _, err := s.CompileCircuit("c", 2); !errors.Is(err, ErrDefinitionMissing) {
		t.Fatalf("compile v2 without definition: want definition missing, got %v", err)
	}
}

func TestCompileRequiresFrozen(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompileCircuit("c", 1); !errors.Is(err, ErrNotFrozen) {
		t.Fatalf("compile draft: want not frozen, got %v", err)
	}
	if _, err := s.CompileCircuit("ghost", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("compile unknown: want not found, got %v", err)
	}
}

// --- check -----------------------------------------------------------------

func TestCheckSatisfiedAndFailingIndex(t *testing.T) {
	s := openTestStore(t)
	// Constraint 1: (3 + 2p)*q = 6 + 2q  ->  (1+2p)*q = 6 mod m
	// Constraint 2: p*p = p
	def := ConstraintDefinition{Modulus: "2147483647", Constraints: []Constraint{
		con([]Term{term(0, "3"), term(1, "2")}, []Term{term(2, "1")}, []Term{term(0, "6"), term(2, "2")}),
		con([]Term{term(1, "1")}, []Term{term(1, "1")}, []Term{term(1, "1")}),
	}}
	art := setupCompiled(t, s, "c", 1, 1, 1, def)

	// p=0, q=6 satisfies both.
	res, err := s.CheckCircuit("c", 1, art.Hash, []string{"0"}, []string{"6"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Satisfied || res.FailedConstraint != 0 || res.ArtifactHash != art.Hash {
		t.Fatalf("satisfied witness: %+v", res)
	}

	// p=5, q=7 fails the first constraint.
	res, err = s.CheckCircuit("c", 1, art.Hash, []string{"5"}, []string{"7"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Satisfied || res.FailedConstraint != 1 {
		t.Fatalf("failing witness: %+v", res)
	}

	// p=2: constraint 1 holds with q = 6 * inv(5); constraint 2 fails.
	m := big.NewInt(2147483647)
	inv5 := new(big.Int).ModInverse(big.NewInt(5), m)
	q := new(big.Int).Mul(big.NewInt(6), inv5)
	q.Mod(q, m)
	res, err = s.CheckCircuit("c", 1, art.Hash, []string{"2"}, []string{q.String()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Satisfied || res.FailedConstraint != 2 {
		t.Fatalf("want failure at constraint 2, got %+v", res)
	}

	// Arbitrary-length signed values are reduced modulo the modulus.
	res, err = s.CheckCircuit("c", 1, art.Hash, []string{"-2147483647"}, []string{"6"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Satisfied {
		t.Fatalf("modulo-reduced witness should satisfy: %+v", res)
	}
}

func TestCheckInputFormatErrors(t *testing.T) {
	s := openTestStore(t)
	def := ConstraintDefinition{Modulus: "2", Constraints: []Constraint{
		con([]Term{term(1, "1")}, nil, nil),
	}}
	art := setupCompiled(t, s, "c", 1, 1, 0, def)

	cases := []struct {
		name    string
		public  []string
		private []string
	}{
		{"too few public", []string{}, []string{}},
		{"too many public", []string{"1", "2"}, []string{}},
		{"bad public value", []string{"1.5"}, []string{}},
		{"non-decimal public", []string{"abc"}, []string{}},
		{"empty public", []string{""}, []string{}},
		{"plus signed", []string{"+1"}, []string{}},
		{"hex", []string{"0x1"}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.CheckCircuit("c", 1, art.Hash, tc.public, tc.private); !errors.Is(err, ErrInputFormat) {
				t.Fatalf("want input format error, got %v", err)
			}
		})
	}

	// Private arrays are checked even when public is fine.
	if _, err := s.CheckCircuit("c", 1, art.Hash, []string{"1"}, []string{"1"}); !errors.Is(err, ErrInputFormat) {
		t.Fatalf("extra private: want input format error, got %v", err)
	}
}

func TestCheckGateErrors(t *testing.T) {
	s := openTestStore(t)
	def := ConstraintDefinition{Modulus: "2", Constraints: []Constraint{
		con([]Term{term(0, "1")}, nil, nil),
	}}
	if _, err := s.CreateCircuit(Circuit{Name: "draft", Version: 1, Constraints: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "frozen", Version: 1, Constraints: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints("frozen", 1, def); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit("frozen", 1); err != nil {
		t.Fatal(err)
	}

	// Unknown version.
	if _, err := s.CheckCircuit("ghost", 1, "x", nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("check unknown: want not found, got %v", err)
	}
	// Not frozen.
	if _, err := s.CheckCircuit("draft", 1, "x", nil, nil); !errors.Is(err, ErrNotFrozen) {
		t.Fatalf("check draft: want not frozen, got %v", err)
	}
	// Frozen, definition imported but not compiled: artifact missing.
	if _, err := s.CheckCircuit("frozen", 1, "x", nil, nil); !errors.Is(err, ErrArtifactMissing) {
		t.Fatalf("check uncompiled: want artifact missing, got %v", err)
	}
	if _, err := s.CompileCircuit("frozen", 1); err != nil {
		t.Fatal(err)
	}
	// Wrong hash: artifact mismatch.
	if _, err := s.CheckCircuit("frozen", 1, strings.Repeat("0", 64), nil, nil); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("check wrong hash: want artifact mismatch, got %v", err)
	}
	// Empty hash: invalid argument.
	if _, err := s.CheckCircuit("frozen", 1, "", nil, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("check empty hash: want invalid argument, got %v", err)
	}

	// Checking creates no jobs.
	jobs, _ := s.ListJobs()
	if len(jobs) != 0 {
		t.Fatalf("check created jobs: %+v", jobs)
	}
}

func TestCheckPrivateValuesNeverReturned(t *testing.T) {
	s := openTestStore(t)
	def := ConstraintDefinition{Modulus: "2147483647", Constraints: []Constraint{
		con([]Term{term(0, "1")}, []Term{term(1, "1")}, []Term{}), // 1*priv = 0 -> fails for priv=1
	}}
	art := setupCompiled(t, s, "c", 1, 0, 1, def)
	res, err := s.CheckCircuit("c", 1, art.Hash, nil, []string{"1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Satisfied || res.FailedConstraint != 1 {
		t.Fatalf("want failure at 1, got %+v", res)
	}
	// The result struct carries no witness values; only the hash and index.
	if res.ArtifactHash != art.Hash {
		t.Fatalf("result leaked no hash: %+v", res)
	}
}

// --- reopen ----------------------------------------------------------------

func TestDefinitionReopenPersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	def := ConstraintDefinition{Modulus: "2147483647", Constraints: []Constraint{
		con([]Term{term(0, "3"), term(1, "2")}, []Term{term(2, "1")}, []Term{term(0, "6"), term(2, "2")}),
	}}
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	art := setupCompiled(t, s1, "c", 1, 1, 1, def)
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got, err := s2.CompileCircuit("c", 1)
	if err != nil {
		t.Fatalf("recompile after reopen: %v", err)
	}
	if got.Hash != art.Hash {
		t.Fatalf("artifact changed after reopen: %s vs %s", got.Hash, art.Hash)
	}
	res, err := s2.CheckCircuit("c", 1, got.Hash, []string{"0"}, []string{"6"})
	if err != nil || !res.Satisfied {
		t.Fatalf("check after reopen: %+v %v", res, err)
	}
}

// --- corruption on load ----------------------------------------------------

func TestCorruptDefinitionRejectedOnOpen(t *testing.T) {
	cases := []struct {
		name    string
		circuit string
	}{
		{"definition without modulus", `{"name":"c","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":true,"definition":[{"a":[],"b":[],"c":[]}]}`},
		{"artifact without definition", `{"name":"c","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":true,"artifact":"` + strings.Repeat("0", 64) + `"}`},
		{"artifact hash mismatch", `{"name":"c","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":true,"modulus":"2","definition":[{"a":[],"b":[],"c":[]}],"artifact":"` + strings.Repeat("0", 64) + `"}`},
		{"artifact malformed", `{"name":"c","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":true,"modulus":"2","definition":[{"a":[],"b":[],"c":[]}],"artifact":"zzz"}`},
		{"non-canonical coeff", `{"name":"c","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":true,"modulus":"2","definition":[{"a":[{"wire":0,"coeff":"2"}],"b":[],"c":[]}]}`},
		{"negative coeff", `{"name":"c","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":true,"modulus":"2","definition":[{"a":[{"wire":0,"coeff":"-1"}],"b":[],"c":[]}]}`},
		{"definition count mismatch", `{"name":"c","version":1,"constraints":2,"public_inputs":0,"private_inputs":0,"frozen":true,"modulus":"2","definition":[{"a":[],"b":[],"c":[]}]}`},
		{"wire out of range", `{"name":"c","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":true,"modulus":"2","definition":[{"a":[{"wire":1,"coeff":"1"}],"b":[],"c":[]}]}`},
		{"bad modulus", `{"name":"c","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":true,"modulus":"4","definition":[{"a":[],"b":[],"c":[]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			body := `{"format":1,"circuits":[` + tc.circuit + `],"setups":[],"jobs":[]}`
			writeDataFile(t, dir, []byte(body))
			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			got, err := readDataFile(t, dir)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(body) {
				t.Fatalf("corrupt file was modified after refusal")
			}
		})
	}
}

func TestOldDataDirectoryStillOpens(t *testing.T) {
	// A pre-feature envelope: no modulus/definition/artifact fields.
	dir := t.TempDir()
	body := `{"format":1,"circuits":[{"name":"c","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":true,"description":"d"}],"setups":[],"jobs":[]}`
	writeDataFile(t, dir, []byte(body))
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("old data directory must still open: %v", err)
	}
	defer s.Close()
	c, err := s.GetCircuit("c", 1)
	if err != nil || !c.Frozen {
		t.Fatalf("old record lost: %+v %v", c, err)
	}
	// Compile on the old frozen version reports definition missing.
	if _, err := s.CompileCircuit("c", 1); !errors.Is(err, ErrDefinitionMissing) {
		t.Fatalf("compile old frozen version: want definition missing, got %v", err)
	}
}

// --- concurrency: import vs freeze leave complete states -------------------

func TestImportFreezeRaceNoPartialState(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		dir := t.TempDir()
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1}); err != nil {
			t.Fatal(err)
		}
		def := ConstraintDefinition{Modulus: "2", Constraints: []Constraint{
			con([]Term{term(0, "1")}, nil, nil),
		}}

		done := make(chan error, 2)
		go func() {
			_, err := s.ImportConstraints("c", 1, def)
			done <- err
		}()
		go func() {
			_, err := s.FreezeCircuit("c", 1)
			done <- err
		}()
		for i := 0; i < 2; i++ {
			if err := <-done; err != nil {
				if !errors.Is(err, ErrFrozen) && !errors.Is(err, ErrNotFound) {
					t.Fatalf("iter %d: unexpected error %v", iter, err)
				}
			}
		}

		got, err := s.GetCircuit("c", 1)
		if err != nil {
			t.Fatal(err)
		}
		if got.Frozen {
			// Frozen: the definition may or may not have won the race, but
			// the record must be internally consistent either way.
			if _, err := s.CompileCircuit("c", 1); err != nil {
				if !errors.Is(err, ErrDefinitionMissing) {
					t.Fatalf("iter %d: frozen compile: %v", iter, err)
				}
			}
			if _, err := s.ImportConstraints("c", 1, def); !errors.Is(err, ErrFrozen) {
				t.Fatalf("iter %d: post-freeze import: want frozen, got %v", iter, err)
			}
		} else {
			// Still a draft: definition may be present or absent, but the
			// record must be internally consistent.
			if _, err := s.CompileCircuit("c", 1); !errors.Is(err, ErrNotFrozen) {
				t.Fatalf("iter %d: draft compile: want not frozen, got %v", iter, err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("iter %d: reopen: %v", iter, err)
		}
		s2.Close()
	}
}
