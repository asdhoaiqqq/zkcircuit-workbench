package zkcircuit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// MaxModulus is the largest accepted field modulus (2^31 - 1, itself a
// prime). The bound keeps every field element representable in 64-bit
// arithmetic while remaining a genuine cryptographic-size prime field.
const MaxModulus = 2147483647

// Term is one coefficient*wire product inside a constraint side. Wire 0 is
// the constant 1; wires 1..public_inputs are public witnesses and wires
// public_inputs+1..public_inputs+private_inputs are private witnesses.
type Term struct {
	Wire  int    `json:"wire"`
	Coeff string `json:"coeff"`
}

// Constraint is one rank-1 constraint A*B = C evaluated modulo the circuit
// modulus. Each side is a sum of coefficient*wire products; an empty side
// sum is zero.
type Constraint struct {
	A []Term `json:"a"`
	B []Term `json:"b"`
	C []Term `json:"c"`
}

// ConstraintDefinition is the importable description of a circuit version's
// constraint system: a prime modulus and an ordered list of constraints.
type ConstraintDefinition struct {
	Modulus     string       `json:"modulus"`
	Constraints []Constraint `json:"constraints"`
}

// Artifact is the compiled constraint system bound to one circuit version.
// Hash is the SHA-256 digest (lowercase hex) of the canonical definition.
type Artifact struct {
	Name        string `json:"name"`
	Version     int    `json:"version"`
	Modulus     string `json:"modulus"`
	Constraints int    `json:"constraints"`
	Hash        string `json:"hash"`
}

// CheckResult is the outcome of checking a witness against a compiled
// artifact. FailedConstraint is 1-based and is 0 when the witness satisfies
// every constraint.
type CheckResult struct {
	Satisfied        bool   `json:"satisfied"`
	ArtifactHash     string `json:"artifact_hash"`
	FailedConstraint int    `json:"failed_constraint"`
}

// Witness is the public/private input arrays of a check request. Values are
// decimal integer strings of arbitrary length, interpreted modulo the
// circuit modulus.
type Witness struct {
	Public  []string `json:"public"`
	Private []string `json:"private"`
}

// --- on-disk JSON shapes --------------------------------------------------
//
// Pointer fields distinguish a missing field from an explicitly present
// empty value, so omitted "a"/"coeff"/... can be rejected rather than
// silently read as zero.

type definitionFile struct {
	Modulus     *string           `json:"modulus"`
	Constraints *[]constraintFile `json:"constraints"`
}

type constraintFile struct {
	A *[]termFile `json:"a"`
	B *[]termFile `json:"b"`
	C *[]termFile `json:"c"`
}

type termFile struct {
	Wire  *int    `json:"wire"`
	Coeff *string `json:"coeff"`
}

type witnessFile struct {
	Public  *[]string `json:"public"`
	Private *[]string `json:"private"`
}

// ParseConstraintDefinition parses a constraint definition JSON document.
// The modulus must be a decimal integer string naming a prime in
// [2, MaxModulus]; every term must carry a non-negative integer wire and a
// decimal integer coefficient string; every constraint must provide all
// three sides (empty arrays are allowed). Structural problems are reported
// as ErrInvalidArgument; the caller's stored state is never touched.
func ParseConstraintDefinition(raw []byte) (ConstraintDefinition, error) {
	var f definitionFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return ConstraintDefinition{}, invalidf("invalid constraint definition: %v", err)
	}
	if f.Modulus == nil {
		return ConstraintDefinition{}, invalidf("invalid constraint definition: missing required field \"modulus\"")
	}
	if f.Constraints == nil {
		return ConstraintDefinition{}, invalidf("invalid constraint definition: missing required field \"constraints\"")
	}
	m, err := parseModulus(*f.Modulus)
	if err != nil {
		return ConstraintDefinition{}, err
	}
	def := ConstraintDefinition{Modulus: m.String()}
	for i, cf := range *f.Constraints {
		c, err := parseConstraintFile(cf, i)
		if err != nil {
			return ConstraintDefinition{}, err
		}
		def.Constraints = append(def.Constraints, c)
	}
	return def, nil
}

// ParseWitnessFile parses a check-request JSON document. Both "public" and
// "private" must be present as arrays of strings; length and value checks
// happen against the target version at check time.
func ParseWitnessFile(raw []byte) (Witness, error) {
	var w witnessFile
	if err := json.Unmarshal(raw, &w); err != nil {
		return Witness{}, inputFormatf("invalid witness file: %v", err)
	}
	if w.Public == nil {
		return Witness{}, inputFormatf("invalid witness file: missing required field \"public\"")
	}
	if w.Private == nil {
		return Witness{}, inputFormatf("invalid witness file: missing required field \"private\"")
	}
	return Witness{Public: *w.Public, Private: *w.Private}, nil
}

func parseConstraintFile(cf constraintFile, index int) (Constraint, error) {
	var c Constraint
	for _, side := range []struct {
		name  string
		terms *[]termFile
		dst   *[]Term
	}{
		{"a", cf.A, &c.A},
		{"b", cf.B, &c.B},
		{"c", cf.C, &c.C},
	} {
		if side.terms == nil {
			return Constraint{}, invalidf("invalid constraint definition: constraint #%d is missing field %q", index+1, side.name)
		}
		parsed := make([]Term, 0, len(*side.terms))
		for _, tf := range *side.terms {
			t, err := parseTermFile(tf, index, side.name)
			if err != nil {
				return Constraint{}, err
			}
			parsed = append(parsed, t)
		}
		*side.dst = parsed
	}
	return c, nil
}

func parseTermFile(tf termFile, index int, side string) (Term, error) {
	if tf.Wire == nil {
		return Term{}, invalidf("invalid constraint definition: constraint #%d field %q has a term missing \"wire\"", index+1, side)
	}
	if *tf.Wire < 0 {
		return Term{}, invalidf("invalid constraint definition: constraint #%d field %q references negative wire %d", index+1, side, *tf.Wire)
	}
	if tf.Coeff == nil {
		return Term{}, invalidf("invalid constraint definition: constraint #%d field %q has a term missing \"coeff\"", index+1, side)
	}
	if err := checkDecimalCoeff(*tf.Coeff, index, side); err != nil {
		return Term{}, err
	}
	return Term{Wire: *tf.Wire, Coeff: *tf.Coeff}, nil
}

func checkDecimalCoeff(coeff string, index int, side string) error {
	if !isDecimalInteger(coeff) {
		return invalidf("invalid constraint definition: constraint #%d field %q has non-decimal coeff %q", index+1, side, coeff)
	}
	if _, ok := new(big.Int).SetString(coeff, 10); !ok {
		return invalidf("invalid constraint definition: constraint #%d field %q has unparseable coeff %q", index+1, side, coeff)
	}
	return nil
}

// validateDefinitionShape re-checks a hand-built definition (the parsed form
// is already checked, but the Go API accepts any ConstraintDefinition) and
// returns its parsed modulus.
func validateDefinitionShape(def ConstraintDefinition) (*big.Int, error) {
	m, err := parseModulus(def.Modulus)
	if err != nil {
		return nil, err
	}
	for i, c := range def.Constraints {
		for _, side := range []struct {
			name  string
			terms []Term
		}{{"a", c.A}, {"b", c.B}, {"c", c.C}} {
			for _, t := range side.terms {
				if t.Wire < 0 {
					return nil, invalidf("constraint #%d field %q references negative wire %d", i+1, side.name, t.Wire)
				}
				if err := checkDecimalCoeff(t.Coeff, i, side.name); err != nil {
					return nil, err
				}
			}
		}
	}
	return m, nil
}

func parseModulus(s string) (*big.Int, error) {
	if !isDecimalInteger(s) {
		return nil, invalidf("modulus %q is not a decimal integer string", s)
	}
	m, ok := new(big.Int).SetString(s, 10)
	if !ok || m.Sign() <= 0 {
		return nil, invalidf("modulus %q is not a positive decimal integer", s)
	}
	if !m.IsInt64() || m.Int64() < 2 || m.Int64() > MaxModulus {
		return nil, invalidf("modulus %q must be a prime between 2 and %d", s, MaxModulus)
	}
	v := m.Int64()
	if !isPrime(v) {
		return nil, invalidf("modulus %d is not prime", v)
	}
	return big.NewInt(v), nil
}

// isDecimalInteger reports whether s is an optional minus sign followed by
// one or more ASCII digits.
func isDecimalInteger(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '-' {
		i = 1
		if len(s) == 1 {
			return false
		}
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isPrime performs exact trial division up to sqrt(n). n is bounded by
// MaxModulus, so this is a one-time, fully deterministic check.
func isPrime(n int64) bool {
	if n < 2 {
		return false
	}
	if n == 2 {
		return true
	}
	if n%2 == 0 {
		return false
	}
	for d := int64(3); d*d <= n; d += 2 {
		if n%d == 0 {
			return false
		}
	}
	return true
}

// --- canonicalization -----------------------------------------------------

// canonicalizeConstraints turns a validated definition into its stored form:
// every coefficient is reduced modulo the modulus, zero terms are dropped,
// repeated wires are merged and each side is sorted by wire. This makes the
// stored form invariant under JSON whitespace, term order, duplicate
// merging, zero-term addition/removal and coefficient differences by
// multiples of the modulus.
func canonicalizeConstraints(cons []Constraint, m *big.Int) []persistConstraint {
	out := make([]persistConstraint, len(cons))
	for i, c := range cons {
		out[i] = persistConstraint{
			A: canonicalizeSide(c.A, m),
			B: canonicalizeSide(c.B, m),
			C: canonicalizeSide(c.C, m),
		}
	}
	return out
}

func canonicalizeSide(terms []Term, m *big.Int) []persistTerm {
	sums := make(map[int]*big.Int)
	for _, t := range terms {
		v, _ := new(big.Int).SetString(t.Coeff, 10)
		v.Mod(v, m)
		if v.Sign() == 0 {
			continue
		}
		cur, ok := sums[t.Wire]
		if !ok {
			cur = new(big.Int)
			sums[t.Wire] = cur
		}
		cur.Add(cur, v)
		cur.Mod(cur, m)
		if cur.Sign() == 0 {
			delete(sums, t.Wire)
		}
	}
	wires := make([]int, 0, len(sums))
	for w := range sums {
		wires = append(wires, w)
	}
	sort.Ints(wires)
	out := make([]persistTerm, 0, len(wires))
	for _, w := range wires {
		out = append(out, persistTerm{Wire: w, Coeff: sums[w].String()})
	}
	return out
}

// artifactDigest computes the SHA-256 digest binding name, version, modulus,
// input partition, constraint count and the canonical constraints. The
// framing is length-prefixed text so distinct fields can never collide;
// constraint order is preserved while term order within a side is not.
func artifactDigest(name string, version int, modulus string, public, private, ncons int, cons []persistConstraint) string {
	var b strings.Builder
	b.WriteString("zkcircuit-artifact-v1\n")
	fmt.Fprintf(&b, "name:%d:%s\n", len(name), name)
	fmt.Fprintf(&b, "version:%d\n", version)
	fmt.Fprintf(&b, "modulus:%s\n", modulus)
	fmt.Fprintf(&b, "public:%d\n", public)
	fmt.Fprintf(&b, "private:%d\n", private)
	fmt.Fprintf(&b, "constraints:%d\n", ncons)
	for i, c := range cons {
		fmt.Fprintf(&b, "constraint:%d\n", i)
		writeDigestSide(&b, "a", c.A)
		writeDigestSide(&b, "b", c.B)
		writeDigestSide(&b, "c", c.C)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func writeDigestSide(b *strings.Builder, label string, terms []persistTerm) {
	fmt.Fprintf(b, "side:%s\n", label)
	for _, t := range terms {
		fmt.Fprintf(b, "term:%d:%s\n", t.Wire, t.Coeff)
	}
}

// --- evaluation ------------------------------------------------------------

// evalConstraintSide sums coefficient*value[wire] over one side, modulo m.
func evalConstraintSide(terms []persistTerm, values []*big.Int, m *big.Int) *big.Int {
	sum := new(big.Int)
	for _, t := range terms {
		c, ok := new(big.Int).SetString(t.Coeff, 10)
		if !ok {
			c = new(big.Int)
		}
		term := new(big.Int).Mul(c, values[t.Wire])
		sum.Add(sum, term)
	}
	return sum.Mod(sum, m)
}

// checkConstraints returns the 1-based index of the first constraint that
// does not satisfy a*b = c modulo m, or 0 if all do.
func checkConstraints(cons []persistConstraint, values []*big.Int, m *big.Int) int {
	for i, c := range cons {
		a := evalConstraintSide(c.A, values, m)
		b := evalConstraintSide(c.B, values, m)
		right := evalConstraintSide(c.C, values, m)
		ab := new(big.Int).Mul(a, b)
		ab.Mod(ab, m)
		if ab.Cmp(right) != 0 {
			return i + 1
		}
	}
	return 0
}

// parseWitnessValue interprets one decimal integer string modulo m.
func parseWitnessValue(s string, m *big.Int) (*big.Int, error) {
	if !isDecimalInteger(s) {
		return nil, inputFormatf("value %q is not a decimal integer string", s)
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, inputFormatf("value %q is not a decimal integer string", s)
	}
	v.Mod(v, m)
	return v, nil
}
