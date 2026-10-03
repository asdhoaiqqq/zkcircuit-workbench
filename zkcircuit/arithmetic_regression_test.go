package zkcircuit

import (
	"math/big"
	"testing"
)

// This file is an arithmetic regression guard for input checking. The
// pre-existing max-modulus test only multiplied two small single-term values;
// these cases exercise multi-term linear combinations (constant wire 0, public
// and private inputs together) at the largest legal prime 2147483647, where
// coefficients and inputs sit close to the modulus and the unreduced products
// and linear-combination sums exceed the int32 range. They also pin down the
// "first failing constraint only" semantics and the meaning of repeated wires
// and cancelling coefficients.

const regressionMaxPrime = "2147483647"

// regressionCircuit creates a draft with count declared constraints, imports
// def, freezes and compiles it, returning the store and the compiled
// artifact. Every check is then bound to the artifact's own hash, the only
// public way a check can run.
func regressionCircuit(t *testing.T, name string, version, pub, priv, count int, def string) (*Store, Artifact) {
	t.Helper()
	s := openTestStore(t)
	if _, err := s.CreateCircuit(Circuit{Name: name, Version: version, Constraints: count,
		PublicInputs: pub, PrivateInputs: priv, Description: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints(name, version, writeTempJSON(t, def)); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := s.FreezeCircuit(name, version); err != nil {
		t.Fatal(err)
	}
	artifact, err := s.CompileCircuit(name, version)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return s, artifact
}

// assertSatisfied and assertUnsatisfied pin the complete verdict contract:
// a check that completes (satisfiable or not) is never a call failure, the
// verdict carries only the conclusion, the bound hash (echoing the request)
// and the 1-based first-failure index.
func assertSatisfied(t *testing.T, s *Store, name string, version int, hash string, w Witness) {
	t.Helper()
	res, err := s.CheckInput(name, version, hash, w)
	if err != nil {
		t.Fatalf("satisfying check returned a call error: %v", err)
	}
	if !res.Satisfied || res.FirstFailure != 0 {
		t.Fatalf("want satisfied with first_failure=0, got %+v", res)
	}
	if res.Hash != hash {
		t.Fatalf("bound hash %q does not match request %q", res.Hash, hash)
	}
}

func assertUnsatisfied(t *testing.T, s *Store, name string, version int, hash string, w Witness, wantFailure int) {
	t.Helper()
	res, err := s.CheckInput(name, version, hash, w)
	if err != nil {
		t.Fatalf("an unsatisfied constraint is a completed check, not a call error: %v", err)
	}
	if res.Satisfied {
		t.Fatalf("want unsatisfied, got %+v", res)
	}
	if res.FirstFailure != wantFailure {
		t.Fatalf("want first_failure=%d, got %+v", wantFailure, res)
	}
	if res.Hash != hash {
		t.Fatalf("bound hash %q does not match request %q", res.Hash, hash)
	}
}

// TestMaxPrimeMultiTermNoOverflow is the central regression the old coverage
// could not catch. The single constraint mixes wire 0 (constant 1), a public
// input and a private input on every side, with coefficients and input values
// all close to p = 2147483647:
//
//	a = (p-15)·1 + (p-2)·x
//	b = (p-28)·1 + (p-4)·y
//	c = (p-63)·1 + (p-7)·x + (p-8)·y
//
// For x = p-5, y = p-6 the reduced values are a = p-5, b = p-4 and
// a·b ≡ 20 ≡ c (mod p). Both the reduced product (p-5)·(p-4) and the
// individual term products (p-2)·(p-5), (p-4)·(p-6) are ~4.61·10^18, above
// the int32 maximum: an evaluator that used 32-bit arithmetic at any step —
// accumulating a side or multiplying the two reduced combinations — would
// draw the wrong conclusion here.
func TestMaxPrimeMultiTermNoOverflow(t *testing.T) {
	def := `{"modulus":"` + regressionMaxPrime + `","constraints":[
		{"a":[{"wire":0,"coeff":"2147483632"},{"wire":1,"coeff":"2147483645"}],
		 "b":[{"wire":0,"coeff":"2147483619"},{"wire":2,"coeff":"2147483643"}],
		 "c":[{"wire":0,"coeff":"2147483584"},{"wire":1,"coeff":"2147483640"},{"wire":2,"coeff":"2147483639"}]}]}`
	s, artifact := regressionCircuit(t, "big", 1, 1, 1, 1, def)

	// Guard the premise of the test: the reduced combination product and the
	// intermediate term sums really do exceed int32, so any 32-bit-overflow
	// step is exposed.
	termProduct := new(big.Int).Mul(big.NewInt(2147483645), big.NewInt(2147483642))    // (p-2)(p-5)
	linearSum := new(big.Int).Add(big.NewInt(2147483632), termProduct)                 // (p-15)+(p-2)(p-5)
	residueProduct := new(big.Int).Mul(big.NewInt(2147483642), big.NewInt(2147483643)) // (p-5)(p-4)
	int32Max := big.NewInt(2147483647)
	if termProduct.Cmp(int32Max) <= 0 || linearSum.Cmp(int32Max) <= 0 || residueProduct.Cmp(int32Max) <= 0 {
		t.Fatalf("test premise lost: products/sums must exceed int32, got term=%s linear=%s residue=%s",
			termProduct, linearSum, residueProduct)
	}
	// And they stay inside int64, which is the implementation's own
	// exactness guarantee: this case is decided by modular reduction, not by
	// big-int fallback masking a narrower type.
	int64Max, _ := new(big.Int).SetString("9223372036854775807", 10)
	if termProduct.Cmp(int64Max) > 0 || linearSum.Cmp(int64Max) > 0 || residueProduct.Cmp(int64Max) > 0 {
		t.Fatalf("test values leave the int64 range the evaluator relies on")
	}

	good := Witness{Public: []string{"2147483642"}, Private: []string{"2147483641"}} // x=p-5, y=p-6
	assertSatisfied(t, s, "big", 1, artifact.Hash, good)

	// Same reduced assignment expressed with a negative private value
	// (-6 ≡ p-6 mod p): the decimal-string rules interpret it modulo p.
	neg := Witness{Public: []string{"2147483642"}, Private: []string{"-6"}}
	assertSatisfied(t, s, "big", 1, artifact.Hash, neg)

	// Changing the private input breaks the equation: y = p-7 makes a·b ≡ 0
	// while c ≡ 28 mod p. This must read unsatisfied, never "satisfied by
	// overflow".
	badPriv := Witness{Public: []string{"2147483642"}, Private: []string{"2147483640"}}
	assertUnsatisfied(t, s, "big", 1, artifact.Hash, badPriv, 1)

	// Changing the public input instead must likewise fail (a·b ≡ 12 ≠ 27).
	badPub := Witness{Public: []string{"2147483641"}, Private: []string{"2147483641"}}
	assertUnsatisfied(t, s, "big", 1, artifact.Hash, badPub, 1)

	// The satisfying witness genuinely depends on all three wire kinds
	// rather than on vacuous sides: zeroing both inputs gives
	// (p-15)·(p-28) ≡ 420 ≠ p-63, so it must not satisfy.
	zero := Witness{Public: []string{"0"}, Private: []string{"0"}}
	assertUnsatisfied(t, s, "big", 1, artifact.Hash, zero, 1)
}

// TestMaxPrimeFirstFailureOrdering pins the meaning of "only the first
// failing constraint is reported". One frozen version carries three
// constraints in definition order; witnesses are chosen so that the verdict
// points at the constraint that actually fails first:
//
//	wires: w1,w2 public = 5,4 ; w3,w4,w5 private = 7,2,29
//	C1: ((p-1) + 2·w1)·(1 + 3·w3) = 198
//	C2: (1 + 2·w2)·((p-1) + 3·w4) = 45
//	C3: (1 + w5)·1 = 30
//
// Every side is a genuine multi-term linear combination (constant + inputs),
// and the products run into the 10^18 range at p = 2147483647.
func TestMaxPrimeFirstFailureOrdering(t *testing.T) {
	def := `{"modulus":"` + regressionMaxPrime + `","constraints":[
		{"a":[{"wire":0,"coeff":"-1"},{"wire":1,"coeff":"2"}],
		 "b":[{"wire":0,"coeff":"1"},{"wire":3,"coeff":"3"}],
		 "c":[{"wire":0,"coeff":"198"}]},
		{"a":[{"wire":0,"coeff":"1"},{"wire":2,"coeff":"2"}],
		 "b":[{"wire":0,"coeff":"-1"},{"wire":4,"coeff":"3"}],
		 "c":[{"wire":0,"coeff":"45"}]},
		{"a":[{"wire":0,"coeff":"1"},{"wire":5,"coeff":"1"}],
		 "b":[{"wire":0,"coeff":"1"}],
		 "c":[{"wire":0,"coeff":"30"}]}]}`
	s, artifact := regressionCircuit(t, "ord", 1, 2, 3, 3, def)

	// Baseline: everything holds; first_failure is 0.
	base := Witness{Public: []string{"5", "4"}, Private: []string{"7", "2", "29"}}
	assertSatisfied(t, s, "ord", 1, artifact.Hash, base)

	// Earlier constraints hold, a later one fails: the result points at the
	// later constraint's real position.
	onlyC3 := Witness{Public: []string{"5", "4"}, Private: []string{"7", "2", "100"}}
	assertUnsatisfied(t, s, "ord", 1, artifact.Hash, onlyC3, 3)

	onlyC2 := Witness{Public: []string{"5", "4"}, Private: []string{"7", "8", "29"}}
	assertUnsatisfied(t, s, "ord", 1, artifact.Hash, onlyC2, 2)

	// Several constraints fail at once (here C2 and C3, C1 still holds): the
	// earliest failing one wins.
	c2AndC3 := Witness{Public: []string{"5", "10"}, Private: []string{"7", "2", "100"}}
	assertUnsatisfied(t, s, "ord", 1, artifact.Hash, c2AndC3, 2)

	// The first constraint failing must hide every later failure.
	c1First := Witness{Public: []string{"6", "10"}, Private: []string{"7", "2", "29"}}
	assertUnsatisfied(t, s, "ord", 1, artifact.Hash, c1First, 1)
}

// TestMaxPrimeCongruentRepresentationsAgree checks that, against one frozen
// definition, replacing inputs with other legal decimal representations that
// differ by an integer multiple of the modulus leaves both the satisfaction
// state and the first-failure position unchanged. The alternatives are
// negative and far longer than machine integers.
func TestMaxPrimeCongruentRepresentationsAgree(t *testing.T) {
	// Same three-constraint circuit as the ordering test, so the
	// first-failure index is meaningful in the unsatisfying family too.
	def := `{"modulus":"` + regressionMaxPrime + `","constraints":[
		{"a":[{"wire":0,"coeff":"-1"},{"wire":1,"coeff":"2"}],
		 "b":[{"wire":0,"coeff":"1"},{"wire":3,"coeff":"3"}],
		 "c":[{"wire":0,"coeff":"198"}]},
		{"a":[{"wire":0,"coeff":"1"},{"wire":2,"coeff":"2"}],
		 "b":[{"wire":0,"coeff":"-1"},{"wire":4,"coeff":"3"}],
		 "c":[{"wire":0,"coeff":"45"}]},
		{"a":[{"wire":0,"coeff":"1"},{"wire":5,"coeff":"1"}],
		 "b":[{"wire":0,"coeff":"1"}],
		 "c":[{"wire":0,"coeff":"30"}]}]}`
	s, artifact := regressionCircuit(t, "cong", 1, 2, 3, 3, def)
	p := big.NewInt(2147483647)
	pow10 := func(n int64) *big.Int {
		return new(big.Int).Exp(big.NewInt(10), big.NewInt(n), nil)
	}

	// Satisfying family: (w1,w2,w3,w4,w5) = (5,4,7,2,29). Each entry below is
	// congruent to the corresponding base value modulo p; the rewrites mix
	// huge positives and huge negatives.
	satisfying := []Witness{
		{Public: []string{"5", "4"}, Private: []string{"7", "2", "29"}},
		{
			Public: []string{
				new(big.Int).Add(big.NewInt(5), new(big.Int).Mul(pow10(20), p)).String(),
				new(big.Int).Sub(big.NewInt(4), new(big.Int).Mul(pow10(12), p)).String(),
			},
			Private: []string{
				new(big.Int).Add(big.NewInt(7), p).String(),
				new(big.Int).Sub(big.NewInt(2), new(big.Int).Add(p, p)).String(),
				new(big.Int).Add(big.NewInt(29), new(big.Int).Mul(pow10(8), p)).String(),
			},
		},
	}
	for i, w := range satisfying {
		res, err := s.CheckInput("cong", 1, artifact.Hash, w)
		if err != nil {
			t.Fatalf("satisfying case %d: %v", i, err)
		}
		if !res.Satisfied || res.FirstFailure != 0 || res.Hash != artifact.Hash {
			t.Fatalf("congruent satisfying case %d changed the verdict: %+v", i, res)
		}
	}

	// Unsatisfying family: base (5,4,7,8,29) fails first at C2. Congruent
	// rewrites (a 40-digit positive, two huge negatives) must keep the exact
	// same unsatisfied state and first-failure position.
	unsatisfying := []Witness{
		{Public: []string{"5", "4"}, Private: []string{"7", "8", "29"}},
		{
			Public: []string{
				new(big.Int).Sub(big.NewInt(5), new(big.Int).Mul(pow10(10), p)).String(),
				"4",
			},
			Private: []string{
				"7",
				new(big.Int).Add(big.NewInt(8), new(big.Int).Mul(pow10(30), p)).String(),
				new(big.Int).Sub(big.NewInt(29), p).String(),
			},
		},
	}
	for i, w := range unsatisfying {
		res, err := s.CheckInput("cong", 1, artifact.Hash, w)
		if err != nil {
			t.Fatalf("unsatisfying case %d returned a call error: %v", i, err)
		}
		if res.Satisfied || res.FirstFailure != 2 || res.Hash != artifact.Hash {
			t.Fatalf("congruent unsatisfying case %d changed the verdict: %+v", i, res)
		}
	}
}

// TestRepeatedWiresAndCancellingCoefficients checks that duplicate wire
// references are really summed (merged) and that coefficients that cancel to
// zero behave as the zero linear combination — including the discriminating
// case where one side cancels to zero but the other side still makes the
// equation false. Modulus 11 keeps the modular bookkeeping transparent.
//
// Wires: w1 public, w2 private. Four constraints in definition order:
//
//	C1: w1·w1 = 4                         (9·9 = 81 ≡ 4; fails for w1=3)
//	C2: (3·w1 + 8·w1)·1 = 5·w1 + 6·w1     -> 0 = 0 for every w1
//	C3: (2·w1 + 3·w1 + 6·w1)·w2 = 5·w2 + 6·w2 -> 0 = 0 for every w1,w2
//	C4: (5·w2 - 5·w2)·w1 = w2             -> left cancels; holds iff w2 = 0
func TestRepeatedWiresAndCancellingCoefficients(t *testing.T) {
	def := `{"modulus":"11","constraints":[
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":1,"coeff":"1"}],"c":[{"wire":0,"coeff":"4"}]},
		{"a":[{"wire":1,"coeff":"3"},{"wire":1,"coeff":"8"}],"b":[{"wire":0,"coeff":"1"}],"c":[{"wire":1,"coeff":"5"},{"wire":1,"coeff":"6"}]},
		{"a":[{"wire":1,"coeff":"2"},{"wire":1,"coeff":"3"},{"wire":1,"coeff":"6"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":2,"coeff":"5"},{"wire":2,"coeff":"6"}]},
		{"a":[{"wire":2,"coeff":"5"},{"wire":2,"coeff":"-5"}],"b":[{"wire":1,"coeff":"1"}],"c":[{"wire":2,"coeff":"1"}]}]}`
	s, artifact := regressionCircuit(t, "dup", 1, 1, 1, 4, def)

	// Satisfying: w1 = 9 (nonzero, so the cancellations do real work),
	// w2 = 0. C1 holds (81 ≡ 4), C2/C3 are 0 = 0, C4 is 0 = 0.
	good := Witness{Public: []string{"9"}, Private: []string{"0"}}
	assertSatisfied(t, s, "dup", 1, artifact.Hash, good)

	// The cancellation discriminator: w1 = 9 keeps C1-C3 holding, but C4's
	// left side cancels to zero by construction while its right side is 4:
	// the equation is false and must be recognised as such — a side that
	// merged to zero is not a wildcard. The verdict points at C4, proving
	// C1-C3 genuinely held rather than every constraint being skipped.
	discriminator := Witness{Public: []string{"9"}, Private: []string{"4"}}
	assertUnsatisfied(t, s, "dup", 1, artifact.Hash, discriminator, 4)

	// A failing first constraint hides the also-failing C4: w1 = 3 gives
	// 3·3 = 9 ≠ 4, so the first reported failure is 1.
	firstWins := Witness{Public: []string{"3"}, Private: []string{"4"}}
	assertUnsatisfied(t, s, "dup", 1, artifact.Hash, firstWins, 1)

	// Congruent representations of the satisfying witness: -2 ≡ 9 mod 11
	// and 11·10^18 ≡ 0; the verdict must stay satisfied/first_failure=0.
	congruentGood := Witness{Public: []string{"-2"}, Private: []string{"11000000000000000000"}}
	assertSatisfied(t, s, "dup", 1, artifact.Hash, congruentGood)
	// And 4 + 11·10^20 ≡ 4 must still fail at the cancellation constraint.
	congruentBad := Witness{
		Public:  []string{"-2"},
		Private: []string{new(big.Int).Add(big.NewInt(4), new(big.Int).Mul(big.NewInt(11), bigPow10(20))).String()},
	}
	assertUnsatisfied(t, s, "dup", 1, artifact.Hash, congruentBad, 4)

	// Inspect the merged canonical form behind the frozen version: the
	// coefficients on C2 side a (3 and 8 on w1) merge to 11 ≡ 0 and are
	// dropped, while C4 side c keeps its single non-cancelling w2 term.
	got, err := s.GetDefinition("dup", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Constraints[1].A) != 0 {
		t.Fatalf("C2 side a should have merged to zero and been dropped, got %+v", got.Constraints[1].A)
	}
	if len(got.Constraints[3].C) != 1 || got.Constraints[3].C[0] != (Term{Wire: 2, Coeff: "1"}) {
		t.Fatalf("C4 side c must keep its single non-cancelling term, got %+v", got.Constraints[3].C)
	}
}

func bigPow10(n int64) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(n), nil)
}
