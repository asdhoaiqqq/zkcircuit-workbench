package zkcircuit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// ---- definition parsing ---------------------------------------------------

func writeTempJSON(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "doc.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const validDef = `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`

func TestParseDefinitionRejectsBadDocuments(t *testing.T) {
	const pub, priv, count = 1, 1, 1
	cases := []struct {
		name string
		doc  string
	}{
		{"not json", `{not json`},
		{"trailing data", `{"modulus":"7","constraints":[]} garbage`},
		{"second value", `{"modulus":"7","constraints":[]}{}`},
		{"unknown top field", `{"modulus":"7","constraints":[],"x":1}`},
		{"missing modulus", `{"constraints":[]}`},
		{"missing constraints", `{"modulus":"7"}`},
		{"modulus not string", `{"modulus":7,"constraints":[]}`},
		{"modulus negative", `{"modulus":"-7","constraints":[]}`},
		{"modulus one", `{"modulus":"1","constraints":[]}`},
		{"modulus too large", `{"modulus":"2147483648","constraints":[]}`},
		{"modulus composite", `{"modulus":"9","constraints":[]}`},
		{"modulus even", `{"modulus":"4","constraints":[]}`},
		{"constraints not array", `{"modulus":"7","constraints":{}}`},
		{"constraints null", `{"modulus":"7","constraints":null}`},
		{"count mismatch", `{"modulus":"7","constraints":[]}`},
		{"constraint not object", `{"modulus":"7","constraints":[1]}`},
		{"unknown constraint field", `{"modulus":"7","constraints":[{"a":[],"b":[],"c":[],"z":1}]}`},
		{"missing side a", `{"modulus":"7","constraints":[{"b":[],"c":[]}]}`},
		{"side null", `{"modulus":"7","constraints":[{"a":null,"b":[],"c":[]}]}`},
		{"side not array", `{"modulus":"7","constraints":[{"a":{},"b":[],"c":[]}]}`},
		{"term not object", `{"modulus":"7","constraints":[{"a":[5],"b":[],"c":[]}]}`},
		{"unknown term field", `{"modulus":"7","constraints":[{"a":[{"wire":1,"coeff":"1","q":0}],"b":[],"c":[]}]}`},
		{"missing wire", `{"modulus":"7","constraints":[{"a":[{"coeff":"1"}],"b":[],"c":[]}]}`},
		{"missing coeff", `{"modulus":"7","constraints":[{"a":[{"wire":1}],"b":[],"c":[]}]}`},
		{"wire null", `{"modulus":"7","constraints":[{"a":[{"wire":null,"coeff":"1"}],"b":[],"c":[]}]}`},
		{"wire float", `{"modulus":"7","constraints":[{"a":[{"wire":1.5,"coeff":"1"}],"b":[],"c":[]}]}`},
		{"wire string", `{"modulus":"7","constraints":[{"a":[{"wire":"1","coeff":"1"}],"b":[],"c":[]}]}`},
		{"coeff number", `{"modulus":"7","constraints":[{"a":[{"wire":1,"coeff":1}],"b":[],"c":[]}]}`},
		{"coeff plus sign", `{"modulus":"7","constraints":[{"a":[{"wire":1,"coeff":"+1"}],"b":[],"c":[]}]}`},
		{"coeff underscore", `{"modulus":"7","constraints":[{"a":[{"wire":1,"coeff":"1_0"}],"b":[],"c":[]}]}`},
		{"coeff empty", `{"modulus":"7","constraints":[{"a":[{"wire":1,"coeff":""}],"b":[],"c":[]}]}`},
		{"coeff sign only", `{"modulus":"7","constraints":[{"a":[{"wire":1,"coeff":"-"}],"b":[],"c":[]}]}`},
		{"wire negative", `{"modulus":"7","constraints":[{"a":[{"wire":-1,"coeff":"1"}],"b":[],"c":[]}]}`},
		{"wire out of range", `{"modulus":"7","constraints":[{"a":[{"wire":3,"coeff":"1"}],"b":[],"c":[]}]}`},
		{"duplicate top key", `{"modulus":"7","modulus":"11","constraints":[
			{"a":[{"wire":1,"coeff":"1"}],"b":[],"c":[]}]}`},
		{"duplicate wire key", `{"modulus":"7","constraints":[
			{"a":[{"wire":1,"wire":2,"coeff":"1"}],"b":[],"c":[]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseDefinitionJSON([]byte(tc.doc), pub, priv, count); err == nil {
				t.Fatalf("expected rejection for %s", tc.name)
			} else if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("want ErrInvalidArgument for %s, got %v", tc.name, err)
			}
		})
	}
}

func TestParseDefinitionAcceptsCanonicalShapes(t *testing.T) {
	// Empty side arrays mean zero; wire 0 is always available; duplicate
	// wires merge; zero terms are dropped.
	doc := `{"modulus":"7","constraints":[
		{"a":[],"b":[{"wire":0,"coeff":"1"}],"c":[]},
		{"a":[{"wire":2,"coeff":"8"},{"wire":2,"coeff":"-1"},{"wire":1,"coeff":"0"}],"b":[],"c":[{"wire":2,"coeff":"0"}]}]}`
	d, err := parseDefinitionJSON([]byte(doc), 1, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if d.modulus != 7 || len(d.constraints) != 2 {
		t.Fatalf("unexpected parse: %+v", d)
	}
	// 8 + (-1) = 7 ≡ 0 mod 7, so the merged wire-2 term must have been
	// dropped; the explicit zero wire-1 term too.
	if len(d.constraints[1].a) != 0 {
		t.Fatalf("expected merged-to-zero terms dropped, got %+v", d.constraints[1].a)
	}
	if len(d.constraints[1].c) != 0 {
		t.Fatalf("expected explicit zero term dropped, got %+v", d.constraints[1].c)
	}
}

func TestDeterministicPrimality(t *testing.T) {
	primes := []int64{2, 3, 5, 7, 11, 13, 2147483647, 2147483629}
	for _, p := range primes {
		if !isPrime(p) {
			t.Errorf("%d is prime but reported composite", p)
		}
	}
	composites := []int64{1, 4, 9, 561, 1105, 25326001, 2147483646}
	for _, n := range composites {
		if isPrime(n) {
			t.Errorf("%d is composite but reported prime", n)
		}
	}
}

// ---- hashing --------------------------------------------------------------

func hashOfDoc(t *testing.T, doc string, name string, version, pub, priv, count int) string {
	t.Helper()
	d, err := parseDefinitionJSON([]byte(doc), pub, priv, count)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, doc)
	}
	return artifactHash(name, version, d)
}

func TestArtifactHashInvariance(t *testing.T) {
	base := `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"2"},{"wire":0,"coeff":"0"}],"c":[{"wire":0,"coeff":"3"}]}]}`
	want := hashOfDoc(t, base, "c", 1, 1, 1, 1)

	variants := []string{
		// JSON whitespace only.
		"{  \"modulus\" : \"7\" ,\n \"constraints\" : [ " +
			"{ \"a\" : [ {\"wire\":1,\"coeff\":\"1\"} ], \"b\" : [ {\"wire\":2,\"coeff\":\"2\"} ], \"c\" : [ {\"wire\":0,\"coeff\":\"3\"} ] } ] }",
		// Term order within a side.
		`{"modulus":"7","constraints":[
			{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":0,"coeff":"0"},{"wire":2,"coeff":"2"}],"c":[{"wire":0,"coeff":"3"}]}]}`,
		// Duplicate terms that merge.
		`{"modulus":"7","constraints":[
			{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"},{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"3"}]}]}`,
		// Zero term added.
		`{"modulus":"7","constraints":[
			{"a":[{"wire":1,"coeff":"1"},{"wire":2,"coeff":"0"}],"b":[{"wire":2,"coeff":"2"}],"c":[{"wire":0,"coeff":"3"}]}]}`,
		// Coefficients differing by a multiple of the modulus (7, 14, -7).
		`{"modulus":"7","constraints":[
			{"a":[{"wire":1,"coeff":"8"}],"b":[{"wire":2,"coeff":"16"},{"wire":2,"coeff":"-7"}],"c":[{"wire":0,"coeff":"-4"}]}]}`,
		// A huge coefficient congruent to the small one mod 7 (1 + 7·10^24).
		`{"modulus":"7","constraints":[
			{"a":[{"wire":1,"coeff":"7000000000000000000000001"}],"b":[{"wire":2,"coeff":"2"}],"c":[{"wire":0,"coeff":"3"}]}]}`,
	}
	for i, v := range variants {
		if got := hashOfDoc(t, v, "c", 1, 1, 1, 1); got != want {
			t.Errorf("variant %d changed the hash:\n got %s\nwant %s", i, got, want)
		}
	}
}

func TestArtifactHashVariance(t *testing.T) {
	doc := func(mod string) string {
		return `{"modulus":"` + mod + `","constraints":[
			{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"1"}]},
			{"a":[],"b":[],"c":[]}]}`
	}
	d7 := doc("7")
	d11 := doc("11")
	base := hashOfDoc(t, d7, "c", 1, 1, 1, 2)

	changed := map[string]string{
		"name":      hashOfDoc(t, d7, "other", 1, 1, 1, 2),
		"version":   hashOfDoc(t, d7, "c", 2, 1, 1, 2),
		"modulus":   hashOfDoc(t, d11, "c", 1, 1, 1, 2),
		"partition": hashOfDoc(t, d7, "c", 1, 2, 0, 2),
	}
	for what, got := range changed {
		if got == base {
			t.Errorf("hash unchanged despite %s change", what)
		}
	}
	// Constraint order must change the hash.
	reordered := `{"modulus":"7","constraints":[
		{"a":[],"b":[],"c":[]},
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"1"}]}]}`
	if hashOfDoc(t, reordered, "c", 1, 1, 1, 2) == base {
		t.Error("constraint reordering did not change the hash")
	}
}

// ---- store lifecycle ------------------------------------------------------

func seedDraftWithDef(t *testing.T, s *Store, name string, v, pub, priv int, def string) {
	t.Helper()
	count := 1
	if _, err := s.CreateCircuit(Circuit{Name: name, Version: v, Constraints: count,
		PublicInputs: pub, PrivateInputs: priv, Description: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints(name, v, writeTempJSON(t, def)); err != nil {
		t.Fatalf("import: %v", err)
	}
}

func TestImportConstraintsDraftRulesAndWholeReplace(t *testing.T) {
	s := openTestStore(t)

	if _, err := s.ImportConstraints("ghost", 1, writeTempJSON(t, validDef)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("import on unknown version: %v", err)
	}
	seedDraftWithDef(t, s, "c", 1, 1, 1, validDef)

	// An illegal replacement leaves the previous definition intact.
	bad := `{"modulus":"9","constraints":[
		{"a":[{"wire":1,"coeff":"1"}],"b":[],"c":[]}]}` // 9 not prime
	if _, err := s.ImportConstraints("c", 1, writeTempJSON(t, bad)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad import: %v", err)
	}
	got, err := s.GetDefinition("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Modulus != "7" || len(got.Constraints) != 1 {
		t.Fatalf("rejected import mutated the stored definition: %+v", got)
	}

	// A legal replacement swaps the whole definition.
	other := `{"modulus":"11","constraints":[
		{"a":[{"wire":1,"coeff":"2"}],"b":[],"c":[]}]}`
	if _, err := s.ImportConstraints("c", 1, writeTempJSON(t, other)); err != nil {
		t.Fatalf("legal replace: %v", err)
	}
	got, _ = s.GetDefinition("c", 1)
	if got.Modulus != "11" {
		t.Fatalf("definition not replaced: %+v", got)
	}

	// After freezing the definition can no longer be replaced.
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints("c", 1, writeTempJSON(t, other)); !errors.Is(err, ErrFrozen) {
		t.Fatalf("frozen import: %v", err)
	}
	got, _ = s.GetDefinition("c", 1)
	if got.Modulus != "11" {
		t.Fatalf("frozen definition changed: %+v", got)
	}
}

func TestUpdateCountsRejectedWhenDefinitionBecomesIllegal(t *testing.T) {
	s := openTestStore(t)
	seedDraftWithDef(t, s, "c", 1, 1, 1, validDef) // uses wire 2

	// Fewer constraints than the definition -> illegal, whole change refused.
	if _, err := s.UpdateCircuit(Circuit{Name: "c", Version: 1, Constraints: 2, Description: "grow"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("constraint-count change should be refused: %v", err)
	}
	// Shrinking the input layout below a referenced wire -> illegal.
	if _, err := s.UpdateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1,
		PublicInputs: 1, PrivateInputs: 0, Description: "shrink"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("wire-layout change should be refused: %v", err)
	}
	c, _ := s.GetCircuit("c", 1)
	if c.PrivateInputs != 1 || c.Description != "c" {
		t.Fatalf("refused update leaked: %+v", c)
	}
	// A count change that keeps the definition legal is accepted.
	if _, err := s.UpdateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1,
		PublicInputs: 2, PrivateInputs: 3, Description: "wider"}); err != nil {
		t.Fatalf("compatible update: %v", err)
	}
}

func TestCompileGatingAndIdempotence(t *testing.T) {
	s := openTestStore(t)

	if _, err := s.CompileCircuit("ghost", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("compile unknown: %v", err)
	}
	// Draft cannot compile.
	if _, err := s.CreateCircuit(Circuit{Name: "draft", Version: 1, Constraints: 1, Description: "d"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompileCircuit("draft", 1); !errors.Is(err, ErrNotFrozen) {
		t.Fatalf("compile draft: %v", err)
	}
	// Counts-only frozen version: freeze/setup/job all work, compile does not.
	if _, err := s.CreateCircuit(Circuit{Name: "bare", Version: 1, Constraints: 1, Description: "d"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit("bare", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSetup("bare", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitJob(Job{ID: "jb", Circuit: "bare", Version: 1, Kind: "prove", Attempt: 1}); err != nil {
		t.Fatalf("counts-only job should be accepted: %v", err)
	}
	if _, err := s.CompileCircuit("bare", 1); !errors.Is(err, ErrDefinitionMissing) {
		t.Fatalf("compile without definition: %v", err)
	}

	// A real defined version compiles and is idempotent.
	seedDraftWithDef(t, s, "c", 1, 1, 1, validDef)
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	a1, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if a1.Modulus != 7 || a1.Constraints != 1 || len(a1.Hash) != 64 {
		t.Fatalf("bad artifact: %+v", a1)
	}
	a2, err := s.CompileCircuit("c", 1)
	if err != nil || a2 != a1 {
		t.Fatalf("recompile not idempotent: %+v %v", a2, err)
	}
	got, err := s.GetArtifact("c", 1)
	if err != nil || got != a1 {
		t.Fatalf("get artifact: %+v %v", got, err)
	}
	// A newly added version does not disturb the existing artifact.
	seedDraftWithDef(t, s, "c", 2, 0, 0,
		`{"modulus":"13","constraints":[{"a":[],"b":[],"c":[]}]}`)
	if _, err := s.FreezeCircuit("c", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompileCircuit("c", 2); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetArtifact("c", 1); got != a1 {
		t.Fatalf("existing artifact changed after adding v2: %+v", got)
	}
}

func TestCheckInputVerdictsAndGating(t *testing.T) {
	s := openTestStore(t)
	// One satisfying constraint 1*2 = 6 mod 7? use pub*priv = c:
	// wire1 * wire2 = 6, satisfied by (2,3) and (6,1).
	seedDraftWithDef(t, s, "c", 1, 1, 1, validDef)
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	artifact, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}

	ok := Witness{Public: []string{"2"}, Private: []string{"3"}}
	res, err := s.CheckInput("c", 1, artifact.Hash, ok)
	if err != nil || !res.Satisfied || res.FirstFailure != 0 || res.Hash != artifact.Hash {
		t.Fatalf("satisfied check: %+v %v", res, err)
	}
	// Negative and arbitrary-length values interpreted mod p.
	// -5 ≡ 2 and -4 ≡ 3 mod 7, product 20 ≡ 6.
	okNeg := Witness{Public: []string{"-5"}, Private: []string{"-4"}}
	res, err = s.CheckInput("c", 1, artifact.Hash, okNeg)
	if err != nil || !res.Satisfied {
		t.Fatalf("negative witness mod p should satisfy: %+v %v", res, err)
	}
	// (2 + 7·10^24) · 3 ≡ 6: huge coefficients reduce before evaluation.
	okBig := Witness{Public: []string{"7000000000000000000000002"}, Private: []string{"3"}}
	res, err = s.CheckInput("c", 1, artifact.Hash, okBig)
	if err != nil || !res.Satisfied {
		t.Fatalf("huge witness mod p should satisfy: %+v %v", res, err)
	}

	bad := Witness{Public: []string{"2"}, Private: []string{"4"}} // 2*4=1 != 6
	res, err = s.CheckInput("c", 1, artifact.Hash, bad)
	if err != nil {
		t.Fatal(err)
	}
	if res.Satisfied || res.FirstFailure != 1 {
		t.Fatalf("want unsatisfied first_failure=1, got %+v", res)
	}

	// First failing constraint is reported 1-based.
	two := `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]},
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"0"}]}]}`
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 3, Constraints: 2,
		PublicInputs: 1, PrivateInputs: 1, Description: "two"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints("c", 3, writeTempJSON(t, two)); err != nil {
		t.Fatal(err)
	}
	s.FreezeCircuit("c", 3)
	a3, _ := s.CompileCircuit("c", 3)
	res, err = s.CheckInput("c", 3, a3.Hash, Witness{Public: []string{"2"}, Private: []string{"3"}})
	if err != nil || res.Satisfied || res.FirstFailure != 2 {
		t.Fatalf("want first_failure=2, got %+v %v", res, err)
	}

	// Gating failures.
	if _, err := s.CheckInput("ghost", 1, artifact.Hash, ok); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown version: %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "d", Version: 1, Constraints: 1, Description: "d"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckInput("d", 1, artifact.Hash, ok); !errors.Is(err, ErrNotFrozen) {
		t.Fatalf("unfrozen: %v", err)
	}
	// Frozen but never compiled -> missing artifact.
	s.FreezeCircuit("d", 1)
	if _, err := s.CheckInput("d", 1, artifact.Hash, ok); !errors.Is(err, ErrArtifactMissing) {
		t.Fatalf("missing artifact: %v", err)
	}
	// Foreign/wrong hash -> mismatch.
	if _, err := s.CheckInput("c", 1, artifact.Hash+"00", ok); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("wrong hash: %v", err)
	}
	// Input-format failures never evaluate.
	for _, w := range []Witness{
		{Public: []string{}, Private: []string{"3"}},
		{Public: []string{"2", "x"}, Private: []string{"3"}},
		{Public: []string{"2"}, Private: []string{"xx"}},
		{Public: []string{"2"}, Private: []string{}},
	} {
		if _, err := s.CheckInput("c", 1, artifact.Hash, w); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("want input format error for %+v, got %v", w, err)
		}
	}
}

func TestNoProofJobCreatedByCheck(t *testing.T) {
	s := openTestStore(t)
	seedDraftWithDef(t, s, "c", 1, 1, 1, validDef)
	s.FreezeCircuit("c", 1)
	a, _ := s.CompileCircuit("c", 1)
	if _, err := s.CheckInput("c", 1, a.Hash, Witness{Public: []string{"2"}, Private: []string{"3"}}); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.ListJobs()
	if len(jobs) != 0 {
		t.Fatalf("input checking must not create proof jobs: %+v", jobs)
	}
}

// Both entry points run the same binding and verdict rules: the same
// frozen version, hash and legal inputs yield identical conclusions, and
// the same gating errors in the same order.
func TestCheckInputEntryPointsEquivalent(t *testing.T) {
	s := openTestStore(t)
	seedDraftWithDef(t, s, "c", 1, 1, 1, validDef)
	s.FreezeCircuit("c", 1)
	a, _ := s.CompileCircuit("c", 1)

	docs := map[string]string{
		"good":       `{"public":["2"],"private":["3"]}`,
		"negative":   `{"public":["-5"],"private":["-4"]}`,
		"huge":       `{"public":["7000000000000000000000002"],"private":["3"]}`,
		"unsatisfy":  `{"public":["2"],"private":["4"]}`,
		"badwitness": `{"public":["2"]}`,
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			path := writeTempJSON(t, doc)
			fileRes, fileErr := s.CheckInputFile("c", 1, a.Hash, path)
			if name == "badwitness" {
				if !errors.Is(fileErr, ErrInvalidInput) {
					t.Fatalf("file entry: want input format error, got %v", fileErr)
				}
				// The JSON witness's strict grammar rejects the document
				// before the shared layout rules run; the in-API witness
				// instead supplies the arrays directly.
				if _, err := s.CheckInput("c", 1, a.Hash, Witness{Public: []string{"2", "x"}, Private: []string{"3"}}); !errors.Is(err, ErrInvalidInput) {
					t.Fatalf("in-API entry: want input format error, got %v", err)
				}
				return
			}
			if fileErr != nil {
				t.Fatalf("file entry: %v", fileErr)
			}
			w, werr := parseWitness([]byte(doc), 1, 1)
			if werr != nil {
				t.Fatal(werr)
			}
			directRes, err := s.CheckInput("c", 1, a.Hash, w)
			if err != nil {
				t.Fatalf("direct entry: %v", err)
			}
			if directRes != fileRes {
				t.Fatalf("verdicts differ: direct=%+v file=%+v", directRes, fileRes)
			}
		})
	}

	// Empty/nil slices are accepted for declared-zero groups through the
	// direct entry; the JSON entry needs explicit arrays.
	zero := `{"modulus":"7","constraints":[
		{"a":[{"wire":0,"coeff":"1"}],"b":[{"wire":0,"coeff":"1"}],"c":[{"wire":0,"coeff":"1"}]}]}`
	if _, err := s.CreateCircuit(Circuit{Name: "z", Version: 1, Constraints: 1, Description: "z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints("z", 1, writeTempJSON(t, zero)); err != nil {
		t.Fatal(err)
	}
	s.FreezeCircuit("z", 1)
	az, _ := s.CompileCircuit("z", 1)
	for _, w := range []Witness{
		{Public: []string{}, Private: []string{}},
		{Public: nil, Private: nil},
	} {
		res, err := s.CheckInput("z", 1, az.Hash, w)
		if err != nil || !res.Satisfied || res.Hash != az.Hash {
			t.Fatalf("nil/empty direct witness %+v: %+v %v", w, res, err)
		}
	}
	path := writeTempJSON(t, `{"public":[],"private":[]}`)
	res, err := s.CheckInputFile("z", 1, az.Hash, path)
	if err != nil || !res.Satisfied {
		t.Fatalf("explicit empty JSON arrays: %+v %v", res, err)
	}
	for _, doc := range []string{`{"private":[]}`, `{"public":[]}`, `{"public":null,"private":[]}`} {
		if _, err := s.CheckInputFile("z", 1, az.Hash, writeTempJSON(t, doc)); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("JSON doc %q must stay an input format error, got %v", doc, err)
		}
	}
}

// Gating precedence is identical at both entry points, and a readable but
// malformed file never masks a failed binding: business errors are reported
// before the file's contents are parsed.
func TestCheckInputFileGatingPrecedence(t *testing.T) {
	s := openTestStore(t)
	seedDraftWithDef(t, s, "c", 1, 1, 1, validDef)
	s.FreezeCircuit("c", 1)
	a, _ := s.CompileCircuit("c", 1)
	badDoc := writeTempJSON(t, `{not json`)

	if _, err := s.CreateCircuit(Circuit{Name: "d", Version: 1, Constraints: 1, Description: "d"}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CheckInputFile("ghost", 1, a.Hash, badDoc); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown version beats malformed file: %v", err)
	}
	if _, err := s.CheckInputFile("d", 1, a.Hash, badDoc); !errors.Is(err, ErrNotFrozen) {
		t.Fatalf("unfrozen beats malformed file: %v", err)
	}
	s.FreezeCircuit("d", 1)
	if _, err := s.CheckInputFile("d", 1, a.Hash, badDoc); !errors.Is(err, ErrArtifactMissing) {
		t.Fatalf("missing artifact beats malformed file: %v", err)
	}
	if _, err := s.CheckInputFile("c", 1, a.Hash+"00", badDoc); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("hash mismatch beats malformed file: %v", err)
	}
	// Direct entry shares the same order.
	if _, err := s.CheckInput("ghost", 1, a.Hash, Witness{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("direct unknown version: %v", err)
	}
	if _, err := s.CheckInput("c", 1, a.Hash+"00", Witness{Public: []string{"2"}, Private: []string{"3"}}); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("direct wrong hash: %v", err)
	}

	// A missing/unreadable file keeps the read error; binding validation
	// never rewrites it as another cause.
	if _, err := s.CheckInputFile("c", 1, a.Hash, filepath.Join(t.TempDir(), "missing.json")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unreadable file must be an input format error, got %v", err)
	}
}

func TestCompileArtifactsSurviveReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedDraftWithDef(t, s, "c", 1, 1, 1, validDef)
	s.FreezeCircuit("c", 1)
	a, err := s.CompileCircuit("c", 1)
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
	got, err := s2.GetArtifact("c", 1)
	if err != nil {
		t.Fatalf("artifact lost: %v", err)
	}
	if got != a {
		t.Fatalf("artifact changed across reopen: %+v want %+v", got, a)
	}
	res, err := s2.CheckInput("c", 1, a.Hash, Witness{Public: []string{"2"}, Private: []string{"3"}})
	if err != nil || !res.Satisfied {
		t.Fatalf("check after reopen: %+v %v", res, err)
	}
}

// A tampered stored definition or an artifact inconsistent with its
// definition is refused on open like any other read failure.
func TestTamperedDefinitionAndArtifactRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, _ := Open(dir)
	seedDraftWithDef(t, s, "c", 1, 1, 1, validDef)
	s.FreezeCircuit("c", 1)
	if _, err := s.CompileCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	s.Close()
	dataFile := filepath.Join(dir, dirDataFile)

	tamper := func(t *testing.T, mutate func([]byte) []byte) {
		t.Helper()
		original, err := os.ReadFile(dataFile)
		if err != nil {
			t.Fatal(err)
		}
		bad := mutate(append([]byte(nil), original...))
		if err := os.WriteFile(dataFile, bad, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
			t.Fatalf("want data corrupt, got %v", err)
		}
		// File must be left untouched on refusal.
		left, _ := os.ReadFile(dataFile)
		if string(left) != string(bad) {
			t.Fatal("damaged file was overwritten on refusal")
		}
		if err := os.WriteFile(dataFile, original, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("artifact hash", func(t *testing.T) {
		tamper(t, func(b []byte) []byte {
			return replaceFirst(b, []byte(`"hash": "`), []byte(`"hash": "deadbeef`))
		})
	})
	t.Run("definition modulus", func(t *testing.T) {
		tamper(t, func(b []byte) []byte {
			return replaceFirst(b, []byte(`"modulus": "7"`), []byte(`"modulus": "11"`))
		})
	})
	t.Run("definition coeff", func(t *testing.T) {
		tamper(t, func(b []byte) []byte {
			return replaceFirst(b, []byte(`"coeff": "6"`), []byte(`"coeff": "5"`))
		})
	})
	t.Run("artifact count", func(t *testing.T) {
		tamper(t, func(b []byte) []byte {
			return replaceFirst(b, []byte(`"constraints": 1,`), []byte(`"constraints": 2,`))
		})
	})

	// After restoring, the directory reads cleanly.
	if _, err := Open(dir); err != nil {
		t.Fatalf("restored dir should open: %v", err)
	}
}

func replaceFirst(haystack, marker, replacement []byte) []byte {
	i := indexBytes(haystack, marker)
	if i < 0 {
		return haystack
	}
	out := append([]byte(nil), haystack[:i]...)
	out = append(out, replacement...)
	out = append(out, haystack[i+len(marker):]...)
	return out
}

func indexBytes(haystack, needle []byte) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return i
		}
	}
	return -1
}

// Concurrent import vs freeze must leave only a complete definition (old or
// new), never a frozen version with a half-applied or missing definition,
// and the on-disk state must reopen consistently.
func TestImportFreezeRaceCompleteness(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		dir := t.TempDir()
		s, _ := Open(dir)
		seedDraftWithDef(t, s, "c", 1, 1, 1, validDef)

		alt := `{"modulus":"7","constraints":[
			{"a":[{"wire":1,"coeff":"3"}],"b":[],"c":[]}]}`
		altPath := writeTempJSON(t, alt)

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			for range 200 {
				if _, err := s.ImportConstraints("c", 1, altPath); err != nil {
					if !errors.Is(err, ErrFrozen) {
						t.Errorf("unexpected import error: %v", err)
					}
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			s.FreezeCircuit("c", 1)
		}()
		close(start)
		wg.Wait()

		c, err := s.GetCircuit("c", 1)
		if err != nil {
			t.Fatal(err)
		}
		if !c.Frozen {
			t.Fatalf("iter %d: version not frozen", iter)
		}
		def, err := s.GetDefinition("c", 1)
		if err != nil {
			t.Fatalf("iter %d: frozen version must keep a complete definition: %v", iter, err)
		}
		// Every import of the same alt content is byte-identical, and the
		// original is fully distinct: a mixed body is impossible. Just check
		// the modulus is coherent and it recompiles to a stable hash.
		a1, err := s.CompileCircuit("c", 1)
		if err != nil {
			t.Fatalf("iter %d: compile after race: %v", iter, err)
		}
		a2, _ := s.CompileCircuit("c", 1)
		if a1 != a2 {
			t.Fatalf("iter %d: artifact not stable: %+v %+v", iter, a1, a2)
		}
		if len(def.Constraints) != 1 {
			t.Fatalf("iter %d: bad definition %+v", iter, def)
		}
		s.Close()

		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("iter %d: reopen: %v", iter, err)
		}
		got, err := s2.GetArtifact("c", 1)
		s2.Close()
		if err != nil || got != a1 {
			t.Fatalf("iter %d: reopened artifact mismatch: %+v %v", iter, got, err)
		}
	}
}

// ---- witness parsing ------------------------------------------------------

func TestParseWitness(t *testing.T) {
	cases := []struct {
		name      string
		doc       string
		pub, priv int
		ok        bool
	}{
		{"good", `{"public":["1","-2"],"private":["999999999999999999999999"]}`, 2, 1, true},
		{"empty arrays", `{"public":[],"private":[]}`, 0, 0, true},
		{"missing public", `{"private":[]}`, 0, 0, false},
		{"missing private", `{"public":[]}`, 0, 0, false},
		{"unknown field", `{"public":[],"private":[],"x":1}`, 0, 0, false},
		{"null public", `{"public":null,"private":[]}`, 0, 0, false},
		{"public not array", `{"public":1,"private":[]}`, 0, 0, false},
		{"value number", `{"public":[1],"private":[]}`, 1, 0, false},
		{"bad value", `{"public":["1.0"],"private":[]}`, 1, 0, false},
		{"plus value", `{"public":["+1"],"private":[]}`, 1, 0, false},
		{"garbage", `{bad`, 0, 0, false},
		{"trailing", `{"public":[],"private":[]} x`, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseWitness([]byte(tc.doc), tc.pub, tc.priv)
			if tc.ok && err != nil {
				t.Fatalf("want ok, got %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("want ErrInvalidInput, got %v", err)
			}
		})
	}
}

func TestDefinitionCountMismatch(t *testing.T) {
	// Document has 1 constraint, version declares 3.
	if _, err := parseDefinitionJSON([]byte(validDef), 1, 1, 3); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("count mismatch: %v", err)
	}
	// Ensure the import path surfaces the same error.
	s := openTestStore(t)
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 3,
		PublicInputs: 1, PrivateInputs: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints("c", 1, writeTempJSON(t, validDef)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("import count mismatch: %v", err)
	}
	if _, err := s.GetDefinition("c", 1); !errors.Is(err, ErrDefinitionMissing) {
		t.Fatalf("failed import must leave no definition: %v", err)
	}
}

func TestBigModulusBoundary(t *testing.T) {
	// 2147483647 prime accepted; one above rejected; evaluation works at the
	// top of the range.
	doc := fmt.Sprintf(`{"modulus":"2147483647","constraints":[
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`)
	d, err := parseDefinitionJSON([]byte(doc), 1, 1, 1)
	if err != nil {
		t.Fatalf("max prime: %v", err)
	}
	if d.modulus != 2147483647 {
		t.Fatalf("modulus %d", d.modulus)
	}
	if fail := d.evaluate(Witness{Public: []string{"2"}, Private: []string{"3"}}); fail != 0 {
		t.Fatalf("max-prime evaluate failure at %d", fail)
	}
}
