package zkcircuit

import (
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"testing"
)

// Arithmetic regression coverage for input checking.
//
// The earlier boundary test (TestBigModulusBoundary) only exercises the
// largest legal prime 2147483647 with tiny inputs and a single-term multiply
// per side. These tests pin the behavior that matters once coefficients and
// input values live near the modulus ceiling and every side is a sum of
// several numbered terms:
//
//   - the constant wire 0, a public input and a private input all occur in the
//     same constraint (no empty arrays, no zero coefficients doing the work),
//   - products and linear sums whose integer magnitudes exceed the signed
//     32-bit limit are still judged correctly, so fixed-width arithmetic
//     without modular reduction can never turn a real failure into a pass or
//     vice versa,
//   - the verdict is only {satisfied, bound hash, first failure}; an
//     unsatisfied assignment is a normal, error-free completion,
//   - "first failure" means the first failing constraint in definition order
//     (1-based): a later failure while earlier constraints hold, and the
//     earliest of several simultaneous failures,
//   - witnesses that differ by an integer multiple of the modulus — including
//     negative and far-beyond-machine-int representations — give identical
//     verdicts and failure positions,
//   - legal definitions using repeated wires and canceling coefficients are
//     evaluated as the constraint they actually represent, including a side
//     that cancels to zero while the other side keeps the equation false.
//
// Every test works against a definition imported into a draft, frozen and
// compiled through the public Store API, and checks inputs bound to that
// compilation's own artifact hash — never a hard-coded or foreign hash. An
// independent math/big oracle re-derives every expected verdict.

// nearCeilingDef is one constraint over modulus 2147483647 (the largest
// accepted prime) in which all three sides are multi-term sums touching the
// constant wire, a public wire and a private wire together:
//
//	(1 + 2·u·x) · (u + x + y) = (p-5) + u·x + u·y   (mod p)
//
// p = 2147483647, u = p-1 = 2147483646, x = wire 1 (public), y = wire 2
// (private). The a-side repeats wire 1 with coefficient u twice, so import
// merging reduces 2u to p-2 rather than taking anything on faith. The
// satisfying assignment is x = u, y = (p+1)/2 = 1073741824 (the inverse of
// 2: 2·(p+1)/2 = p+1 ≡ 1). Over the integers the a-side sum
// 1 + 2·(p-1)^2 = 9223372019674906633 and the c-side sum
// (p-5) + (p-1)^2 + (p-1)(p+1)/2 = 6917529019051147262 — both far above the
// signed 32-bit limit 2^31-1 — and the reduced product 3·1073741822 =
// 3221225466 exceeds 2^31-1 as well. An evaluator that skipped modular
// reduction or leaned on 32-bit values would conclude wrongly here.
const nearCeilingDef = `{"modulus":"2147483647","constraints":[
		{"a":[{"wire":0,"coeff":"1"},{"wire":1,"coeff":"2147483646"},{"wire":1,"coeff":"2147483646"}],
		 "b":[{"wire":0,"coeff":"2147483646"},{"wire":1,"coeff":"1"},{"wire":2,"coeff":"1"}],
		 "c":[{"wire":0,"coeff":"2147483642"},{"wire":1,"coeff":"2147483646"},{"wire":2,"coeff":"2147483646"}]}]}`

// compileRegressionVersion creates a draft with the declared input layout,
// imports def, freezes and compiles it, returning the bound artifact.
func compileRegressionVersion(t *testing.T, s *Store, name string, v, pub, priv int, def string) Artifact {
	t.Helper()
	var d Definition
	if err := json.Unmarshal([]byte(def), &d); err != nil {
		t.Fatalf("bad fixture definition: %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: name, Version: v, Constraints: len(d.Constraints),
		PublicInputs: pub, PrivateInputs: priv, Description: name}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.ImportConstraints(name, v, writeTempJSON(t, def)); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := s.FreezeCircuit(name, v); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	a, err := s.CompileCircuit(name, v)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(a.Hash) != 64 || a.Constraints != len(d.Constraints) || a.Modulus != int(parseOracleModulus(t, def)) {
		t.Fatalf("unexpected artifact: %+v", a)
	}
	return a
}

// TestMaxPrimeMultiTermSatisfied pins the near-ceiling multi-term case: the
// satisfying assignment returns satisfied with first failure 0 and the result
// hash equal to the hash of the compilation the check was bound to.
func TestMaxPrimeMultiTermSatisfied(t *testing.T) {
	s := openTestStore(t)
	a := compileRegressionVersion(t, s, "ceil", 1, 1, 1, nearCeilingDef)

	if !oracleHolds(t, nearCeilingDef, []string{"2147483646"}, []string{"1073741824"}) {
		t.Fatal("oracle: fixture witness must satisfy the fixture constraint")
	}

	w := Witness{Public: []string{"2147483646"}, Private: []string{"1073741824"}}
	res, err := s.CheckInput("ceil", 1, a.Hash, w)
	if err != nil {
		t.Fatalf("satisfied check must be a normal completion, got error %v", err)
	}
	if !res.Satisfied || res.FirstFailure != 0 {
		t.Fatalf("want satisfied/first_failure=0, got %+v", res)
	}
	if res.Hash != a.Hash {
		t.Fatalf("verdict hash %q must equal bound artifact hash %q", res.Hash, a.Hash)
	}
	// The verdict carries only the conclusion, bound hash and failure index.
	if !reflect.DeepEqual(res, CheckResult{Satisfied: true, Hash: a.Hash, FirstFailure: 0}) {
		t.Fatalf("CheckResult gained or lost fields/values: %+v", res)
	}
}

// TestMaxPrimeMultiTermUnsatisfied changes one input of the satisfying
// assignment; the equation no longer holds and the verdict must be
// unsatisfied. Because the integer sums/products here exceed 2^31-1, a
// fixed-width evaluator without reduction could report "satisfied" by
// overflow; this test locks in the correct modular conclusion.
func TestMaxPrimeMultiTermUnsatisfied(t *testing.T) {
	s := openTestStore(t)
	a := compileRegressionVersion(t, s, "ceil", 1, 1, 1, nearCeilingDef)

	// Public x = p-2 instead of p-1: 1+2(p-1)(p-2) ≡ 5 (not 3); b changes to
	// (p+1)/2 - 1; 5·1073741821 ≡ 1073741811 ≠ c ≡ 1073741820.
	if oracleHolds(t, nearCeilingDef, []string{"2147483645"}, []string{"1073741824"}) {
		t.Fatal("oracle: perturbed witness must not satisfy")
	}
	w := Witness{Public: []string{"2147483645"}, Private: []string{"1073741824"}}
	res, err := s.CheckInput("ceil", 1, a.Hash, w)
	if err != nil {
		t.Fatalf("an unsatisfied verdict is not a call failure: %v", err)
	}
	if res.Satisfied || res.FirstFailure != 1 {
		t.Fatalf("want unsatisfied/first_failure=1, got %+v", res)
	}
	if res.Hash != a.Hash {
		t.Fatalf("verdict must stay bound to hash %q, got %q", a.Hash, res.Hash)
	}

	// Changing only the private input fails just the same.
	if oracleHolds(t, nearCeilingDef, []string{"2147483646"}, []string{"1073741823"}) {
		t.Fatal("oracle: perturbed private witness must not satisfy")
	}
	res2, err := s.CheckInput("ceil", 1, a.Hash,
		Witness{Public: []string{"2147483646"}, Private: []string{"1073741823"}})
	if err != nil || res2.Satisfied || res2.FirstFailure != 1 || res2.Hash != a.Hash {
		t.Fatalf("perturbed private: %+v %v", res2, err)
	}
}

// TestMaxPrimeModEquivalentWitnesses: on one frozen definition, legal
// representations of the same field element — a negative value, values beyond
// machine-int width, and positive multiples of p added or subtracted — must
// all give the same satisfaction state and failure position, for both the
// satisfying and the perturbed assignment.
func TestMaxPrimeModEquivalentWitnesses(t *testing.T) {
	s := openTestStore(t)
	a := compileRegressionVersion(t, s, "ceil", 1, 1, 1, nearCeilingDef)

	// p·10^12: a multiple of p wider than any machine integer.
	const hugeMultiple = "2147483647000000000000"

	satisfying := []Witness{
		{Public: []string{"2147483646"}, Private: []string{"1073741824"}}, // canonical
		{Public: []string{"-1"}, Private: []string{"-1073741823"}},        // -1 ≡ p-1
		{Public: []string{"4294967293"}, Private: []string{"3221225471"}}, // 2p-1, (3p+1)/2
		{
			// u + p·10^12 (22 digits, beyond int64); (p+1)/2 + p·10^12.
			Public:  []string{"2147483647002147483646"},
			Private: []string{"2147483647001073741824"},
		},
		{
			// u - p·10^12 (negative, beyond int64); private stays canonical to
			// mix representations in one assignment.
			Public:  []string{"-2147483646997852516354"},
			Private: []string{"1073741824"},
		},
		{
			Public:  []string{"-1"},
			Private: []string{addDecimal(hugeMultiple, "1073741824")},
		},
	}
	for i, w := range satisfying {
		if !oracleHolds(t, nearCeilingDef, w.Public, w.Private) {
			t.Fatalf("oracle: satisfying case %d (%+v) must hold", i, w)
		}
		res, err := s.CheckInput("ceil", 1, a.Hash, w)
		if err != nil || !res.Satisfied || res.FirstFailure != 0 || res.Hash != a.Hash {
			t.Fatalf("satisfying equivalent %d (%+v): %+v %v", i, w, res, err)
		}
	}

	// Perturbed public x ≡ -2 (mod p): every representative must fail at
	// constraint 1 with the same verdict: -2, p-2, 2p-2, -p-2, and ±huge
	// multiples beyond int64.
	perturbed := []Witness{
		{Public: []string{"-2"}, Private: []string{"1073741824"}},
		{Public: []string{"2147483645"}, Private: []string{"1073741824"}},
		{Public: []string{"4294967292"}, Private: []string{"-1073741823"}},
		{Public: []string{"-2147483649"}, Private: []string{"1073741824"}}, // -p-2 ≡ -2
		{
			Public:  []string{addDecimal("-"+hugeMultiple, "-2")},
			Private: []string{addDecimal(hugeMultiple, "1073741824")},
		},
	}
	for i, w := range perturbed {
		if oracleHolds(t, nearCeilingDef, w.Public, w.Private) {
			t.Fatalf("oracle: perturbed case %d (%+v) must not hold", i, w)
		}
		res, err := s.CheckInput("ceil", 1, a.Hash, w)
		if err != nil {
			t.Fatalf("unsatisfied is a normal completion, case %d: %v", i, err)
		}
		if res.Satisfied || res.FirstFailure != 1 || res.Hash != a.Hash {
			t.Fatalf("perturbed equivalent %d (%+v): want false/1/%s, got %+v", i, w, a.Hash, res)
		}
	}
}

// orderingDef has three constraints in a fixed definition order over the
// prime 100003 (x = wire 1 public, y = wire 2 private):
//
//	1: (x + y)·(y - 1) = 0      holds iff y = 1 or x = -y
//	2: x·y = 3
//	3: (x + y - 2)·1 = 0        holds iff x + y = 2
//
// The witnesses separate "a later constraint fails while earlier ones hold"
// from "several constraints fail at once" and "only the last one fails":
// evaluation must always stop at the first failing index in definition order.
const orderingDef = `{"modulus":"100003","constraints":[
		{"a":[{"wire":1,"coeff":"1"},{"wire":2,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"},{"wire":0,"coeff":"-1"}],"c":[{"wire":0,"coeff":"1"},{"wire":0,"coeff":"-1"}]},
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"3"}]},
		{"a":[{"wire":1,"coeff":"1"},{"wire":2,"coeff":"1"},{"wire":0,"coeff":"-2"}],"b":[{"wire":0,"coeff":"1"}],"c":[{"wire":0,"coeff":"1"},{"wire":0,"coeff":"-1"}]}]}`

func TestFirstFailureInDefinitionOrder(t *testing.T) {
	s := openTestStore(t)
	a := compileRegressionVersion(t, s, "ord", 1, 1, 1, orderingDef)

	cases := []struct {
		name      string
		pub, priv string
		want      int // 0 == satisfied
	}{
		// x = p-1, y = 1: constraint 1 ((p-1+1)·0 = 0) holds; constraint 2:
		// p-1 ≠ 3, so the FIRST failure is constraint 2 (constraint 3 fails
		// too, but must not be reported).
		{"later fails while first holds", "100002", "1", 2},
		// x = 0, y = 2: constraint 1: 2·1 = 2 ≠ 0 fails immediately, and
		// constraint 2 (0·2 = 3) also fails; the answer must be 1.
		{"earliest of several failures", "0", "2", 1},
		// x = 3, y = 1: con1: 4·0 = 0; con2: 3·1 = 3; con3: (3+1-2)·1 = 2 ≠ 0,
		// so only the third fails — proving middle constraints are not skipped.
		{"only third fails", "3", "1", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := oracleFirstFailure(t, orderingDef, []string{tc.pub}, []string{tc.priv}); got != tc.want {
				t.Fatalf("oracle first failure = %d, fixture wants %d", got, tc.want)
			}
			res, err := s.CheckInput("ord", 1, a.Hash,
				Witness{Public: []string{tc.pub}, Private: []string{tc.priv}})
			if err != nil {
				t.Fatalf("a verdict is a normal completion even when unsatisfied: %v", err)
			}
			if tc.want == 0 {
				if !res.Satisfied || res.FirstFailure != 0 {
					t.Fatalf("want satisfied, got %+v", res)
				}
			} else {
				if res.Satisfied || res.FirstFailure != tc.want {
					t.Fatalf("want first_failure=%d, got satisfied=%v first_failure=%d",
						tc.want, res.Satisfied, res.FirstFailure)
				}
			}
			if res.Hash != a.Hash {
				t.Fatalf("bound hash mismatch: %q != %q", res.Hash, a.Hash)
			}
		})
	}
}

// cancellationDef encodes equations using repeated wires and coefficients
// that cancel modulo p = 2147483647. The definition is legal and must be
// judged as the equation it reduces to:
//
//	1: (x + (p-1)·x) · 1 = (1 - 1)        -> both sides cancel to 0;
//	   0·1 = 0 holds for every witness.
//	2: (5·x + (p-4)·x + 1 + (p-1)) · y = 3
//	   5+(p-4) = p+1 ≡ 1 (a repeated wire merged, not dropped), 1+(p-1)=p
//	   cancels, so the side is x: x·y = 3.
//	3: (y - y) · x = 1                    -> a-side cancels to 0 while the
//	   c-side is the constant 1, so 0 = 1 NEVER holds: one side zero while the
//	   other keeps the equation false.
//
// No term array is empty and no coefficient is literally "0" in the import;
// satisfaction cannot be obtained vacuously.
const cancellationDef = `{"modulus":"2147483647","constraints":[
		{"a":[{"wire":1,"coeff":"1"},{"wire":1,"coeff":"2147483646"}],
		 "b":[{"wire":0,"coeff":"1"}],
		 "c":[{"wire":0,"coeff":"1"},{"wire":0,"coeff":"-1"}]},
		{"a":[{"wire":1,"coeff":"5"},{"wire":1,"coeff":"2147483643"},{"wire":0,"coeff":"1"},{"wire":0,"coeff":"2147483646"}],
		 "b":[{"wire":2,"coeff":"1"}],
		 "c":[{"wire":0,"coeff":"3"}]},
		{"a":[{"wire":2,"coeff":"1"},{"wire":2,"coeff":"-1"}],
		 "b":[{"wire":1,"coeff":"1"}],
		 "c":[{"wire":0,"coeff":"1"}]}]}`

func TestRepeatedAndCancelingTerms(t *testing.T) {
	s := openTestStore(t)
	a := compileRegressionVersion(t, s, "cancel", 1, 1, 1, cancellationDef)

	// x = 3, y = 1: constraint 1 holds (a-side 0), constraint 2 holds
	// (3·1 = 3), constraint 3 is 0 = 1 and fails -> first failure 3.
	if got, want := oracleFirstFailure(t, cancellationDef, []string{"3"}, []string{"1"}), 3; got != want {
		t.Fatalf("oracle: want first failure %d, got %d", want, got)
	}
	res, err := s.CheckInput("cancel", 1, a.Hash,
		Witness{Public: []string{"3"}, Private: []string{"1"}})
	if err != nil {
		t.Fatalf("normal completion: %v", err)
	}
	if res.Satisfied || res.FirstFailure != 3 {
		t.Fatalf("canceling-side case must fail at constraint 3 (0=1), got %+v", res)
	}

	// x = 1, y = 3: constraint 1 holds, constraint 2: 1·3 = 3 holds,
	// constraint 3 still 0 = 1 -> failure 3 (the cancellation is structural).
	res, err = s.CheckInput("cancel", 1, a.Hash,
		Witness{Public: []string{"1"}, Private: []string{"3"}})
	if err != nil || res.Satisfied || res.FirstFailure != 3 {
		t.Fatalf("second witness: want false/3, got %+v %v", res, err)
	}

	// x = 3, y = 2: constraint 1 holds, constraint 2: 3·2 = 6 ≠ 3 fails at 2,
	// so even though constraint 3 also fails, the reported position is 2.
	if got, want := oracleFirstFailure(t, cancellationDef, []string{"3"}, []string{"2"}), 2; got != want {
		t.Fatalf("oracle: want first failure %d, got %d", want, got)
	}
	res, err = s.CheckInput("cancel", 1, a.Hash,
		Witness{Public: []string{"3"}, Private: []string{"2"}})
	if err != nil || res.Satisfied || res.FirstFailure != 2 {
		t.Fatalf("want first failure at constraint 2, got %+v %v", res, err)
	}

	// A genuinely-satisfiable definition with cancellation proves true
	// satisfaction is not collapsed into the always-failing 0=1 shape:
	// (1 + x + (p-1)·x) · y = y, i.e. 1·y = y for every assignment.
	triviallyTrue := `{"modulus":"2147483647","constraints":[
		{"a":[{"wire":0,"coeff":"1"},{"wire":1,"coeff":"1"},{"wire":1,"coeff":"2147483646"}],
		 "b":[{"wire":2,"coeff":"1"}],
		 "c":[{"wire":2,"coeff":"1"}]}]}`
	a2 := compileRegressionVersion(t, s, "cancelok", 2, 1, 1, triviallyTrue)
	for _, w := range []Witness{
		{Public: []string{"2147483646"}, Private: []string{"2147483646"}},
		{Public: []string{"-1"}, Private: []string{"-214748364999999999000"}}, // y minus a huge multiple of p
		{Public: []string{"0"}, Private: []string{"0"}},
	} {
		if !oracleHolds(t, triviallyTrue, w.Public, w.Private) {
			t.Fatalf("oracle: identity witness %+v must hold", w)
		}
		res, err := s.CheckInput("cancelok", 2, a2.Hash, w)
		if err != nil || !res.Satisfied || res.FirstFailure != 0 || res.Hash != a2.Hash {
			t.Fatalf("identity constraint with %+v: %+v %v", w, res, err)
		}
	}
}

// TestMultiTermResultShapeAndBinding asserts the public result contract on
// the multi-term max-prime circuit for both verdicts: exactly three fields,
// hash echoing the check request's bound artifact, and no error for an
// unsatisfied (but well-formed and bound) evaluation.
func TestMultiTermResultShapeAndBinding(t *testing.T) {
	s := openTestStore(t)
	a := compileRegressionVersion(t, s, "shape", 1, 1, 1, nearCeilingDef)

	good, err := s.CheckInput("shape", 1, a.Hash,
		Witness{Public: []string{"2147483646"}, Private: []string{"1073741824"}})
	if err != nil {
		t.Fatalf("satisfied: %v", err)
	}
	bad, err := s.CheckInput("shape", 1, a.Hash,
		Witness{Public: []string{"2147483645"}, Private: []string{"1073741824"}})
	if err != nil {
		t.Fatalf("unsatisfied is a completed check, not a failure: %v", err)
	}
	if good == bad {
		t.Fatalf("satisfied and unsatisfied verdicts must differ: %+v", good)
	}
	if good.Hash != a.Hash || bad.Hash != a.Hash {
		t.Fatalf("both verdicts must carry the bound hash: %q %q", good.Hash, bad.Hash)
	}
	if !(good.Satisfied && good.FirstFailure == 0) || bad.Satisfied || bad.FirstFailure != 1 {
		t.Fatalf("unexpected verdict pair: good=%+v bad=%+v", good, bad)
	}
	// Recompiling returns the same artifact; a subsequent check keeps binding
	// the same hash for the frozen version.
	aAgain, err := s.CompileCircuit("shape", 1)
	if err != nil || aAgain != a {
		t.Fatalf("recompile changed the artifact: %+v %v", aAgain, err)
	}
}

// ---- independent arbitrary-precision oracle ------------------------------
//
// The oracle parses the public Definition document and evaluates each
// constraint with math/big only. It shares no code with the production
// evaluator, so agreement catches mistakes in the int64 modular arithmetic
// rather than restating it.

type oracleDefinition struct {
	Modulus     string      `json:"modulus"`
	Constraints []oracleCon `json:"constraints"`
}

type oracleCon struct {
	A []oracleTerm `json:"a"`
	B []oracleTerm `json:"b"`
	C []oracleTerm `json:"c"`
}

type oracleTerm struct {
	Wire  int    `json:"wire"`
	Coeff string `json:"coeff"`
}

func parseOracleModulus(t *testing.T, def string) int64 {
	t.Helper()
	var d oracleDefinition
	if err := json.Unmarshal([]byte(def), &d); err != nil {
		t.Fatalf("oracle parse: %v", err)
	}
	p, ok := new(big.Int).SetString(d.Modulus, 10)
	if !ok || !p.IsInt64() {
		t.Fatalf("oracle: bad modulus %q", d.Modulus)
	}
	return p.Int64()
}

// oracleValues builds [1, public..., private...] reduced mod p.
func oracleValues(t *testing.T, p *big.Int, pub, priv []string) []*big.Int {
	t.Helper()
	values := make([]*big.Int, 1+len(pub)+len(priv))
	values[0] = big.NewInt(1)
	put := func(i int, text string) {
		n, ok := new(big.Int).SetString(text, 10)
		if !ok {
			t.Fatalf("oracle: bad decimal %q", text)
		}
		values[i] = n.Mod(n, p)
	}
	for i, v := range pub {
		put(1+i, v)
	}
	for i, v := range priv {
		put(1+len(pub)+i, v)
	}
	return values
}

func oracleCombo(t *testing.T, terms []oracleTerm, p *big.Int, values []*big.Int) *big.Int {
	t.Helper()
	sum := new(big.Int)
	for _, term := range terms {
		c, ok := new(big.Int).SetString(term.Coeff, 10)
		if !ok {
			t.Fatalf("oracle: bad coefficient %q", term.Coeff)
		}
		if term.Wire < 0 || term.Wire >= len(values) {
			t.Fatalf("oracle: wire %d out of range", term.Wire)
		}
		product := new(big.Int).Mul(new(big.Int).Mod(c, p), values[term.Wire])
		sum.Add(sum, product)
	}
	return sum.Mod(sum, p)
}

func oracleEvaluate(t *testing.T, def string, pub, priv []string) []bool {
	t.Helper()
	var d oracleDefinition
	if err := json.Unmarshal([]byte(def), &d); err != nil {
		t.Fatalf("oracle parse: %v", err)
	}
	p, ok := new(big.Int).SetString(d.Modulus, 10)
	if !ok {
		t.Fatalf("oracle: bad modulus %q", d.Modulus)
	}
	values := oracleValues(t, p, pub, priv)
	failures := make([]bool, len(d.Constraints))
	for i, con := range d.Constraints {
		a := oracleCombo(t, con.A, p, values)
		b := oracleCombo(t, con.B, p, values)
		c := oracleCombo(t, con.C, p, values)
		lhs := new(big.Int).Mul(a, b)
		lhs.Mod(lhs, p)
		failures[i] = lhs.Cmp(c) != 0
	}
	return failures
}

func oracleHolds(t *testing.T, def string, pub, priv []string) bool {
	t.Helper()
	for _, f := range oracleEvaluate(t, def, pub, priv) {
		if f {
			return false
		}
	}
	return true
}

func oracleFirstFailure(t *testing.T, def string, pub, priv []string) int {
	t.Helper()
	for i, f := range oracleEvaluate(t, def, pub, priv) {
		if f {
			return i + 1
		}
	}
	return 0
}

// addDecimal returns the decimal sum a+b (exact, arbitrary precision). It is
// used to spell out "x plus an integer multiple of p" without any native
// integer width assumption.
func addDecimal(a, b string) string {
	x, okx := new(big.Int).SetString(a, 10)
	y, oky := new(big.Int).SetString(b, 10)
	if !okx || !oky {
		panic(fmt.Sprintf("addDecimal: non-decimal inputs %q %q", a, b))
	}
	return new(big.Int).Add(x, y).String()
}
