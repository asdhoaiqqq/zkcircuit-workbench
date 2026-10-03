package zkcircuit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
	"sort"
)

// This file implements R1CS-style constraint definitions: their strict JSON
// form, validation against the declared wire layout of a circuit version, a
// canonical form used both for persistence and for artifact hashing, and the
// modular a·b = c evaluation used by input checking.

// maxModulus bounds accepted moduli. The bound keeps every reduced field
// element and coefficient inside int32, which makes all constraint
// arithmetic exact in int64 (a product of two residues is at most
// (p-1)^2 < 2^62).
const maxModulus = 2147483647

// Term is one wire reference with its coefficient: coeff * value(wire).
type Term struct {
	Wire  int    `json:"wire"`
	Coeff string `json:"coeff"`
}

// Constraint is one a·b = c equation modulo the definition's field. Each
// slice is a sum of terms; an empty slice is the zero linear combination.
type Constraint struct {
	A []Term `json:"a"`
	B []Term `json:"b"`
	C []Term `json:"c"`
}

// Definition is an importable constraint set. The JSON file carries modulus
// as a decimal string and an ordered constraints array.
type Definition struct {
	Modulus     string       `json:"modulus"`
	Constraints []Constraint `json:"constraints"`
}

// Artifact is the result of compiling one frozen circuit version. It binds
// the version's name, version number, modulus and constraint count to a
// stable SHA-256 hash; the same logical circuit always compiles to the same
// artifact.
type Artifact struct {
	Name        string
	Version     int
	Modulus     int
	Constraints int
	Hash        string
}

// CheckResult is the verdict of testing one input assignment against a
// compiled artifact. Satisfied reports whether every constraint holds;
// FirstFailure is the 1-based index of the first failing constraint, zero
// when the assignment satisfies the circuit.
type CheckResult struct {
	Satisfied    bool
	Hash         string
	FirstFailure int
}

// Witness is one input assignment: values for the declared public and
// private wires, in declaration order. Values are arbitrary-length signed
// decimal strings interpreted modulo the circuit's field.
type Witness struct {
	Public  []string
	Private []string
}

// ---- parsed / canonical in-memory representation -------------------------

// parsedTerm is a validated term whose coefficient has been reduced into
// [0, modulus).
type parsedTerm struct {
	wire  int
	value int64 // coefficient mod modulus
}

// canonicalConstraint groups merged terms by wire.
type canonicalConstraint struct {
	a, b, c []parsedTerm
}

// canonicalDefinition is a fully validated constraint set in its canonical
// form: terms merged per wire, reduced modulo p, zeros removed, wires sorted
// ascending within each linear combination.
type canonicalDefinition struct {
	modulus     int64
	public      int
	private     int
	constraints []canonicalConstraint
}

// maxWire returns the largest wire index referenced anywhere.
func (d *canonicalDefinition) maxWire() int {
	max := 0
	for _, con := range d.constraints {
		for _, group := range [][]parsedTerm{con.a, con.b, con.c} {
			for _, t := range group {
				if t.wire > max {
					max = t.wire
				}
			}
		}
	}
	return max
}

// compatibleWith reports whether the definition stays legal when its
// version declares the given counts: the constraint count must match and
// every referenced wire must still lie inside the declared layout.
func (d *canonicalDefinition) compatibleWith(constraints, public, private int) bool {
	if len(d.constraints) != constraints {
		return false
	}
	return d.maxWire() <= public+private
}

// toExport renders the canonical form as the public Definition type, with
// coefficients reduced to decimal strings in [0, modulus).
func (d *canonicalDefinition) toExport() Definition {
	combo := func(terms []parsedTerm) []Term {
		r := make([]Term, 0, len(terms))
		for _, t := range terms {
			r = append(r, Term{Wire: t.wire, Coeff: fmt.Sprintf("%d", t.value)})
		}
		return r
	}
	cons := make([]Constraint, 0, len(d.constraints))
	for _, con := range d.constraints {
		cons = append(cons, Constraint{A: combo(con.a), B: combo(con.b), C: combo(con.c)})
	}
	return Definition{Modulus: fmt.Sprintf("%d", d.modulus), Constraints: cons}
}

// ---- strict JSON helpers --------------------------------------------------

// strictObject decodes raw as a JSON object whose keys must all be allowed,
// rejecting syntax errors, trailing data, duplicates, non-objects and
// unknown keys. It returns the raw members for typed decoding.
func strictObject(raw []byte, allowed []string, what string) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, invalidf("%s must be a JSON object", what)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &members); err != nil {
		// Unmarshal also rejects trailing data after the top-level value.
		return nil, invalidf("%s is not valid JSON: %v", what, err)
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		allowedSet[k] = true
	}
	for key := range members {
		if !allowedSet[key] {
			return nil, invalidf("%s has unknown field %q", what, key)
		}
	}
	if err := rejectDuplicateKeys(trimmed, what); err != nil {
		return nil, err
	}
	return members, nil
}

// rejectDuplicateKeys walks the JSON token stream rejecting any object that
// names the same key twice; encoding/json silently keeps the last value.
func rejectDuplicateKeys(raw []byte, what string) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return invalidf("%s is not valid JSON: %v", what, err)
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil // scalar
		}
		switch delim {
		case '{':
			seen := make(map[string]bool)
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return invalidf("%s is not valid JSON: %v", what, err)
				}
				key := keyTok.(string)
				if seen[key] {
					return invalidf("%s contains duplicate field %q", what, key)
				}
				seen[key] = true
				if err := walk(); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // closing brace
				return invalidf("%s is not valid JSON: %v", what, err)
			}
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // closing bracket
				return invalidf("%s is not valid JSON: %v", what, err)
			}
		}
		return nil
	}
	return walk()
}

func requireMember(members map[string]json.RawMessage, key, what string) (json.RawMessage, error) {
	raw, ok := members[key]
	if !ok {
		return nil, invalidf("%s is missing required field %q", what, key)
	}
	return raw, nil
}

// decodeJSONString requires raw to be a JSON string and returns it.
func decodeJSONString(raw json.RawMessage, what string) (string, error) {
	if string(bytes.TrimSpace(raw)) == "null" {
		return "", invalidf("%s must be a JSON string, not null", what)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", invalidf("%s must be a JSON string: %v", what, err)
	}
	return s, nil
}

// decodeJSONInt requires raw to be a JSON integer (no floats, strings,
// null).
func decodeJSONInt(raw json.RawMessage, what string) (int, error) {
	trimmed := bytes.TrimSpace(raw)
	if string(trimmed) == "null" || len(trimmed) == 0 {
		return 0, invalidf("%s must be a JSON integer, not null", what)
	}
	if trimmed[0] != '-' && (trimmed[0] < '0' || trimmed[0] > '9') {
		return 0, invalidf("%s must be a JSON integer", what)
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, invalidf("%s must be a JSON integer: %v", what, err)
	}
	return n, nil
}

// ---- definition parsing ---------------------------------------------------

// parseDefinitionJSON parses and fully validates a definition document
// against the version's declared counts.
//
// Rejected inputs include malformed JSON, trailing data, duplicate/unknown
// or missing fields (top level, per constraint and per term), a non-prime or
// out-of-range modulus, malformed coefficients, wire indexes outside
// [0, 1+public+private) and a constraint count that does not equal the
// circuit version's declared count. The wire layout is wire 0 = constant 1,
// followed by the declared public inputs and then the private inputs.
func parseDefinitionJSON(raw []byte, public, private, wantCount int) (*canonicalDefinition, error) {
	members, err := strictObject(raw, []string{"modulus", "constraints"}, "constraint definition")
	if err != nil {
		return nil, err
	}
	modulusRaw, err := requireMember(members, "modulus", "constraint definition")
	if err != nil {
		return nil, err
	}
	constraintsRaw, err := requireMember(members, "constraints", "constraint definition")
	if err != nil {
		return nil, err
	}
	modulusText, err := decodeJSONString(modulusRaw, "modulus")
	if err != nil {
		return nil, err
	}
	p, err := parseModulus(modulusText)
	if err != nil {
		return nil, err
	}
	if string(bytes.TrimSpace(constraintsRaw)) == "null" {
		return nil, invalidf("constraint definition field \"constraints\" must be an array")
	}
	var constraintRaws []json.RawMessage
	if err := json.Unmarshal(constraintsRaw, &constraintRaws); err != nil {
		return nil, invalidf("constraint definition field \"constraints\" must be an array: %v", err)
	}
	if len(constraintRaws) != wantCount {
		return nil, invalidf("constraint count mismatch: definition has %d constraints, version declares %d",
			len(constraintRaws), wantCount)
	}

	parsed := &canonicalDefinition{
		modulus:     p,
		public:      public,
		private:     private,
		constraints: make([]canonicalConstraint, 0, len(constraintRaws)),
	}
	maxIndex := public + private
	for i, craw := range constraintRaws {
		con, err := parseConstraint(craw, p, maxIndex)
		if err != nil {
			return nil, fmt.Errorf("constraint #%d: %w", i+1, err)
		}
		parsed.constraints = append(parsed.constraints, con)
	}
	return parsed, nil
}

// parseConstraint parses one {"a":[…],"b":[…],"c":[…]} object.
func parseConstraint(raw json.RawMessage, p int64, maxIndex int) (canonicalConstraint, error) {
	members, err := strictObject(raw, []string{"a", "b", "c"}, "constraint")
	if err != nil {
		return canonicalConstraint{}, err
	}
	var out canonicalConstraint
	for _, side := range []struct {
		key string
		dst *[]parsedTerm
	}{
		{"a", &out.a}, {"b", &out.b}, {"c", &out.c},
	} {
		sideRaw, err := requireMember(members, side.key, "constraint")
		if err != nil {
			return canonicalConstraint{}, err
		}
		terms, err := parseLinearCombo(sideRaw, p, maxIndex, side.key)
		if err != nil {
			return canonicalConstraint{}, err
		}
		*side.dst = terms
	}
	return out, nil
}

// parseLinearCombo parses an array of {"wire":int,"coeff":string} terms and
// returns the canonical combination: duplicate wires merged, coefficients
// reduced modulo p, zeros dropped, wires sorted ascending.
func parseLinearCombo(raw json.RawMessage, p int64, maxIndex int, side string) ([]parsedTerm, error) {
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil, invalidf("constraint side %q must be an array", side)
	}
	var termRaws []json.RawMessage
	if err := json.Unmarshal(raw, &termRaws); err != nil {
		return nil, invalidf("constraint side %q must be an array: %v", side, err)
	}
	bigP := big.NewInt(p)
	merged := make(map[int]*big.Int)
	for j, traw := range termRaws {
		members, err := strictObject(traw, []string{"wire", "coeff"}, fmt.Sprintf("term %s[%d]", side, j))
		if err != nil {
			return nil, err
		}
		wireRaw, err := requireMember(members, "wire", fmt.Sprintf("term %s[%d]", side, j))
		if err != nil {
			return nil, err
		}
		coeffRaw, err := requireMember(members, "coeff", fmt.Sprintf("term %s[%d]", side, j))
		if err != nil {
			return nil, err
		}
		wire, err := decodeJSONInt(wireRaw, fmt.Sprintf("term %s[%d] wire", side, j))
		if err != nil {
			return nil, err
		}
		if wire < 0 || wire > maxIndex {
			return nil, invalidf("term %s[%d] references wire %d outside [0,%d]", side, j, wire, maxIndex)
		}
		coeffText, err := decodeJSONString(coeffRaw, fmt.Sprintf("term %s[%d] coefficient", side, j))
		if err != nil {
			return nil, err
		}
		coeff, err := parseBigSignedDecimal(coeffText)
		if err != nil {
			return nil, invalidf("term %s[%d] coefficient %q is not a decimal integer: %v", side, j, coeffText, err)
		}
		coeff.Mod(coeff, bigP) // into [0,p); Go's Mod keeps the sign of p
		if existing, ok := merged[wire]; ok {
			existing.Add(existing, coeff)
			existing.Mod(existing, bigP)
		} else {
			merged[wire] = new(big.Int).Set(coeff)
		}
	}
	out := make([]parsedTerm, 0, len(merged))
	for wire, value := range merged {
		v := value.Int64()
		if v == 0 {
			continue // canonical form drops zero terms
		}
		out = append(out, parsedTerm{wire: wire, value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].wire < out[j].wire })
	return out, nil
}

// parseModulus parses a decimal integer in [2, maxModulus] and requires it to
// be prime.
func parseModulus(s string) (int64, error) {
	n, err := parseBigSignedDecimal(s)
	if err != nil {
		return 0, invalidf("modulus %q is not a decimal integer: %v", s, err)
	}
	if n.Sign() < 0 || !n.IsInt64() || n.Int64() > maxModulus {
		return 0, invalidf("modulus %s must be between 2 and %d", s, maxModulus)
	}
	v := n.Int64()
	if v < 2 {
		return 0, invalidf("modulus %s must be at least 2", s)
	}
	if !isPrime(v) {
		return 0, invalidf("modulus %s is not prime", s)
	}
	return v, nil
}

// ---- decimal grammar ------------------------------------------------------

// parseBigSignedDecimal accepts exactly an optional single '-' followed by
// one or more ASCII digits, of arbitrary length. strconv/big alone also
// accept '+' and underscore separators, so the grammar is checked by hand
// before big.Int parses the digits.
func parseBigSignedDecimal(s string) (*big.Int, error) {
	if s == "" {
		return nil, fmt.Errorf("empty string")
	}
	digits := s
	neg := false
	if s[0] == '-' {
		if len(s) == 1 {
			return nil, fmt.Errorf("sign without digits")
		}
		neg = true
		digits = s[1:]
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return nil, fmt.Errorf("illegal character %q", digits[i])
		}
	}
	n, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return nil, fmt.Errorf("not an integer")
	}
	if neg {
		n.Neg(n)
	}
	return n, nil
}

func modInt64(v, p int64) int64 {
	r := v % p
	if r < 0 {
		r += p
	}
	return r
}

// ---- deterministic primality ---------------------------------------------

// isPrime reports whether n is prime for 2 <= n <= maxModulus. It uses
// deterministic Miller-Rabin with a witness set that is exact for the whole
// uint32 range, so results never depend on randomness.
func isPrime(n int64) bool {
	if n < 2 {
		return false
	}
	for _, small := range []int64{2, 3, 5, 7, 11, 13, 17, 19, 23, 29, 31, 37} {
		if n == small {
			return true
		}
		if n%small == 0 {
			return false
		}
	}
	// Write n-1 = d · 2^s with d odd.
	d := n - 1
	s := 0
	for d&1 == 0 {
		d >>= 1
		s++
	}
	// Bases {2,3,5,7,11} are deterministic for every n < 2.15·10^12, which
	// covers the full accepted range.
	for _, a := range []int64{2, 3, 5, 7, 11} {
		if !millerRabinWitness(n, d, s, a) {
			return false
		}
	}
	return true
}

func millerRabinWitness(n, d int64, s int, a int64) bool {
	x := new(big.Int).Exp(big.NewInt(a), big.NewInt(d), big.NewInt(n))
	if x.Int64() == 1 || x.Int64() == n-1 {
		return true
	}
	for range s - 1 {
		x.Mul(x, x)
		x.Mod(x, big.NewInt(n))
		if x.Int64() == n-1 {
			return true
		}
	}
	return false
}

// ---- artifact hashing -----------------------------------------------------

// artifactHash computes the stable SHA-256 of a compiled version. It folds
// in everything the version's correctness depends on: name, version,
// modulus, the public/private split and the ordered, canonical constraints.
//
// Consequently only semantically equivalent rewrites hash alike: JSON
// whitespace, term order, duplicate terms that merge, added/removed zero
// terms, and coefficients differing by a multiple of the modulus. Constraint
// order, modulus, name, version and the input partition change the hash.
func artifactHash(name string, version int, d *canonicalDefinition) string {
	h := sha256.New()
	writeLine := func(prefix, value string) {
		io.WriteString(h, prefix+"="+value+"\n")
	}
	writeLine("name", name)
	writeLine("version", fmt.Sprintf("%d", version))
	writeLine("modulus", fmt.Sprintf("%d", d.modulus))
	writeLine("public_inputs", fmt.Sprintf("%d", d.public))
	writeLine("private_inputs", fmt.Sprintf("%d", d.private))
	writeLine("constraint_count", fmt.Sprintf("%d", len(d.constraints)))
	for i, con := range d.constraints {
		fmt.Fprintf(h, "constraint=%d\n", i)
		writeCombo(h, "a", con.a)
		writeCombo(h, "b", con.b)
		writeCombo(h, "c", con.c)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeCombo(h hash.Hash, side string, terms []parsedTerm) {
	fmt.Fprintf(h, "%s=%d", side, len(terms))
	for _, t := range terms {
		fmt.Fprintf(h, " %d:%d", t.wire, t.value)
	}
	io.WriteString(h, "\n")
}

// ---- canonical JSON for persistence --------------------------------------

func (d *canonicalDefinition) toPersist() persistDefinition {
	out := persistDefinition{
		Modulus:     fmt.Sprintf("%d", d.modulus),
		Constraints: make([]persistConstraint, 0, len(d.constraints)),
	}
	combo := func(terms []parsedTerm) []persistTerm {
		tt := make([]persistTerm, 0, len(terms))
		for _, t := range terms {
			tt = append(tt, persistTerm{Wire: t.wire, Coeff: fmt.Sprintf("%d", t.value)})
		}
		return tt
	}
	for _, con := range d.constraints {
		out.Constraints = append(out.Constraints, persistConstraint{
			A: combo(con.a), B: combo(con.b), C: combo(con.c),
		})
	}
	return out
}

// definitionFromPersist re-validates a stored definition against its
// version's declared input counts and rebuilds its canonical form. It is
// used both at load time (integrity check) and before compile/check.
func definitionFromPersist(p persistDefinition, public, private int) (*canonicalDefinition, error) {
	combo := func(tt []persistTerm) []Term {
		r := make([]Term, 0, len(tt))
		for _, t := range tt {
			r = append(r, Term{Wire: t.Wire, Coeff: t.Coeff})
		}
		return r
	}
	cons := make([]Constraint, 0, len(p.Constraints))
	for _, c := range p.Constraints {
		cons = append(cons, Constraint{A: combo(c.A), B: combo(c.B), C: combo(c.C)})
	}
	raw, err := json.Marshal(Definition{Modulus: p.Modulus, Constraints: cons})
	if err != nil {
		return nil, err
	}
	return parseDefinitionJSON(raw, public, private, len(cons))
}

// ---- input checking -------------------------------------------------------

// parseWitness validates the check input JSON and tags every rejection as an
// input format error (ErrInvalidInput), distinct from domain rule failures.
func parseWitness(raw []byte, public, private int) (Witness, error) {
	w, err := parseWitnessDocument(raw, public, private)
	if err != nil {
		var se StoreError
		if errors.As(err, &se) {
			return Witness{}, StoreError{Kind: ErrInvalidInput.Kind, Detail: se.Detail}
		}
		return Witness{}, inputFormatf("%v", err)
	}
	return w, nil
}

// parseWitnessDocument parses and validates a check input document: both
// arrays must be present and arrays, their lengths must match the
// declaration, and every value must be an arbitrarily long signed decimal
// string. parseWitness tags rejections as input format errors for the file
// entry point.
//
// Diagnostics are privacy-aware: the private section is parsed with a
// hand-written scanner whose messages never quote document text — no private
// value, character, fragment, object key or raw token can reach an error.
// Syntax failures are attributed to the public/private group being scanned
// when that is reliably known; otherwise only a document-wide "malformed"
// reason is given. Structural positions (which group, which 1-based input)
// still identify what to fix.
func parseWitnessDocument(raw []byte, public, private int) (Witness, error) {
	fields, err := scanWitnessEnvelope(raw)
	if err != nil {
		return Witness{}, err
	}
	pubField, privField := fields["public"], fields["private"]
	if !pubField.present {
		return Witness{}, invalidf("input is missing the \"public\" group: it must be an array of %d decimal strings", public)
	}
	if !privField.present {
		return Witness{}, invalidf("input is missing the \"private\" group: it must be an array of %d decimal strings", private)
	}
	pubValues, err := parseWitnessArray(pubField.raw, "public", public)
	if err != nil {
		return Witness{}, err
	}
	privValues, err := parseWitnessArray(privField.raw, "private", private)
	if err != nil {
		return Witness{}, err
	}
	return witnessFromArrays(pubValues, privValues, public, private)
}

// witnessFromArrays applies the single witness grammar and layout rule
// shared by both check entry points: each group must contain exactly the
// declared number of decimal strings. A nil slice is accepted wherever an
// empty one would be (an in-API witness declaring zero inputs); unlike the
// JSON entry point nothing here distinguishes "omitted" from "empty".
// Private values are never echoed; malformed private strings are identified
// by their 1-based position only.
func witnessFromArrays(pubValues, privValues []string, public, private int) (Witness, error) {
	if len(pubValues) != public {
		return Witness{}, invalidf("public input length mismatch: got %d values, version requires an array of %d decimal strings",
			len(pubValues), public)
	}
	if len(privValues) != private {
		return Witness{}, invalidf("private input length mismatch: got %d values, version requires an array of %d decimal strings",
			len(privValues), private)
	}
	if err := validateWitnessValues(pubValues, "public", true); err != nil {
		return Witness{}, err
	}
	if err := validateWitnessValues(privValues, "private", false); err != nil {
		return Witness{}, err
	}
	return Witness{Public: pubValues, Private: privValues}, nil
}

// validateWitnessValues checks that every entry is an arbitrarily long
// signed decimal string. Public values may be echoed with the parser's
// detailed reason; private values are never echoed, nor is any character or
// fragment taken from them — only the 1-based position is reported, with a
// fixed vocabulary describing why the grammar failed.
func validateWitnessValues(values []string, which string, echo bool) error {
	for i, v := range values {
		if _, err := parseBigSignedDecimal(v); err != nil {
			if echo {
				return invalidf("%s input #%d value %q is not a decimal integer: %v", which, i+1, v, err)
			}
			switch {
			case v == "":
				return invalidf("%s input #%d is not a decimal integer: the string is empty", which, i+1)
			case v == "-":
				return invalidf("%s input #%d is not a decimal integer: it is a minus sign without digits", which, i+1)
			default:
				return invalidf("%s input #%d is not a decimal integer", which, i+1)
			}
		}
	}
	return nil
}

// ---- private-value-safe witness document scanner --------------------------
//
// encoding/json cannot parse this document for us: its errors quote the
// offending text (e.g. `invalid character 'Q'`), and a generic duplicate-key
// walk quotes object keys — both leak private material when the failure is in
// the private section. The scanner below validates JSON syntax by hand and
// returns only fixed grammar phrases plus the region (public/private/none)
// that was being scanned; callers turn those into StoreErrors without ever
// copying bytes out of the document.

// witnessField is one located top-level member of a scanned input document.
type witnessField struct {
	raw     []byte
	present bool
}

// scanWitnessEnvelope performs a strict syntax pass over an input document
// and returns the trimmed raw values of its "public" and "private" members.
// Syntax errors, trailing data, duplicate groups and a non-object document
// are rejected; an unknown top-level field is rejected by name (an envelope
// key is structural, not a private value).
func scanWitnessEnvelope(raw []byte) (map[string]witnessField, error) {
	data := raw
	i := skipJSONSpace(data, 0)
	if i >= len(data) || data[i] != '{' {
		return nil, invalidf("input document must be a JSON object with \"public\" and \"private\" arrays of decimal strings")
	}
	i++ // past '{'
	fields := make(map[string]witnessField)
	seen := make(map[string]bool)
	for {
		i = skipJSONSpace(data, i)
		if i >= len(data) {
			return nil, witnessMalformedf("", "expected a member key or '}'")
		}
		if data[i] == '}' {
			i++
			break
		}
		if data[i] != '"' {
			return nil, witnessMalformedf("", "expected a member key")
		}
		key, next, reason := scanWitnessKey(data, i)
		if reason != "" {
			return nil, witnessMalformedf("", reason)
		}
		i = skipJSONSpace(data, next)
		if i >= len(data) || data[i] != ':' {
			return nil, witnessMalformedf("", "expected ':' after a member key")
		}
		if key != "public" && key != "private" {
			return nil, invalidf("input document has unknown field %q; only \"public\" and \"private\" are allowed", key)
		}
		if seen[key] {
			return nil, invalidf("input document contains the %q group more than once; each group may appear exactly once", key)
		}
		seen[key] = true
		start := i + 1
		end, reason := scanWitnessValue(data, start)
		if reason != "" {
			return nil, witnessMalformedf(key, reason)
		}
		fields[key] = witnessField{raw: bytes.TrimSpace(data[start:end]), present: true}
		i = skipJSONSpace(data, end)
		if i >= len(data) {
			return nil, witnessMalformedf("", "expected ',' or '}'")
		}
		switch data[i] {
		case ',':
			i++
			continue
		case '}':
			i++
			if skipJSONSpace(data, i) != len(data) {
				return nil, witnessMalformedf("", "trailing data after the document")
			}
			return fields, nil
		default:
			return nil, witnessMalformedf("", "expected ',' or '}'")
		}
	}
	if skipJSONSpace(data, i) != len(data) {
		return nil, witnessMalformedf("", "trailing data after the document")
	}
	return fields, nil
}

// witnessMalformedf builds a syntax rejection. region is "public", "private"
// or "" (unknown/envelope). The reason is a fixed grammar phrase chosen by
// the scanner; it never contains bytes read from the document, so it is safe
// for the private region.
func witnessMalformedf(region, reason string) error {
	switch region {
	case "private":
		return invalidf("input document is malformed in the private input group: %s", reason)
	case "public":
		return invalidf("input document is malformed in the public input group: %s", reason)
	default:
		return invalidf("input document is malformed: %s", reason)
	}
}

// parseWitnessArray turns a located group value into its string elements.
// Syntax has already been validated by scanWitnessEnvelope, so this walk only
// classifies element types (and re-checks duplicate keys inside object
// elements). Every message names the group and its required form and never
// quotes an element; a wrong-type element is identified by its 1-based
// position and JSON type.
func parseWitnessArray(raw []byte, group string, want int) ([]string, error) {
	t := bytes.TrimSpace(raw)
	if string(t) == "null" {
		return nil, invalidf("input field %q must be an array of %d decimal strings, not null", group, want)
	}
	if len(t) == 0 || t[0] != '[' {
		return nil, invalidf("input field %q must be an array of %d decimal strings", group, want)
	}
	dec := json.NewDecoder(bytes.NewReader(t))
	dec.UseNumber()
	if _, err := dec.Token(); err != nil { // opening '['
		return nil, witnessMalformedf(group, "expected a JSON array")
	}
	var values []string
	idx := 0
	for dec.More() {
		idx++
		tok, err := dec.Token()
		if err != nil {
			return nil, witnessMalformedf(group, "expected a JSON string element")
		}
		switch v := tok.(type) {
		case string:
			values = append(values, v)
		case json.Delim:
			// Consume the whole object/array (detecting duplicate keys
			// inside) before reporting the type mismatch; its contents are
			// never quoted.
			if err := skipWitnessDecoderRest(dec, v, group, idx); err != nil {
				return nil, err
			}
			return nil, witnessElementTypef(group, idx, witnessContainerKind(v))
		case json.Number:
			return nil, witnessElementTypef(group, idx, "a number")
		case bool:
			return nil, witnessElementTypef(group, idx, "a boolean")
		case nil:
			return nil, witnessElementTypef(group, idx, "null")
		}
	}
	if _, err := dec.Token(); err != nil { // closing ']'
		return nil, witnessMalformedf(group, "expected the array to close with ']'")
	}
	return values, nil
}

// witnessElementTypef reports a non-string array element by position only.
func witnessElementTypef(group string, idx int, kind string) error {
	return invalidf("%s input #%d must be a decimal string (a JSON string), not %s", group, idx, kind)
}

func witnessContainerKind(d json.Delim) string {
	if d == '{' {
		return "an object"
	}
	return "an array"
}

// skipWitnessDecoderRest consumes the remainder of an object/array whose
// opening delim has already been read. Duplicate keys inside an object are
// rejected as a malformed group; the offending key is never named. elem is
// the 1-based position of the array element being inspected.
func skipWitnessDecoderRest(dec *json.Decoder, open json.Delim, group string, elem int) error {
	if open == '{' {
		seen := make(map[string]bool)
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return witnessMalformedf(group, "expected an object key")
			}
			key, _ := keyTok.(string)
			if seen[key] {
				return witnessObjectDupf(group, elem)
			}
			seen[key] = true
			if err := skipWitnessDecoderValue(dec, group, elem); err != nil {
				return err
			}
		}
	} else {
		for dec.More() {
			if err := skipWitnessDecoderValue(dec, group, elem); err != nil {
				return err
			}
		}
	}
	if _, err := dec.Token(); err != nil {
		return witnessMalformedf(group, "expected the container to close")
	}
	return nil
}

// witnessObjectDupf reports a duplicate key in an object element without
// naming the key (it may be private data); the element position identifies
// what to fix.
func witnessObjectDupf(group string, elem int) error {
	return invalidf("%s input #%d must be a decimal string (a JSON string), not an object, and the object contains a duplicate key",
		group, elem)
}

// skipWitnessDecoderValue consumes one complete JSON value from dec, keeping
// the duplicate-key rule for every nested object.
func skipWitnessDecoderValue(dec *json.Decoder, group string, elem int) error {
	tok, err := dec.Token()
	if err != nil {
		return witnessMalformedf(group, "expected a JSON value")
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil // scalar
	}
	return skipWitnessDecoderRest(dec, d, group, elem)
}

// ---- minimal strict JSON syntax scanner -----------------------------------
//
// Each scanner returns the index just past the scanned construct and, on
// failure, a fixed grammar phrase. No phrase and no returned index leaks
// document text; the caller supplies region attribution.

func skipJSONSpace(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// scanWitnessKey scans an object key at data[i] == '"' and returns its
// decoded text (for matching against the structural names only — never for
// inclusion in an error) and the index past the closing quote.
func scanWitnessKey(data []byte, i int) (string, int, string) {
	end, reason := scanJSONStringEnd(data, i)
	if reason != "" {
		return "", 0, reason
	}
	var key string
	if err := json.Unmarshal(data[i:end], &key); err != nil {
		return "", 0, "invalid string escape"
	}
	return key, end, ""
}

// scanWitnessValue scans one complete JSON value of unknown type.
func scanWitnessValue(data []byte, i int) (int, string) {
	i = skipJSONSpace(data, i)
	if i >= len(data) {
		return 0, "expected a JSON value"
	}
	switch data[i] {
	case '"':
		end, reason := scanJSONStringEnd(data, i)
		return end, reason
	case '{':
		return scanWitnessObject(data, i)
	case '[':
		return scanWitnessArray(data, i)
	case 't':
		return scanWitnessLiteral(data, i, "true")
	case 'f':
		return scanWitnessLiteral(data, i, "false")
	case 'n':
		return scanWitnessLiteral(data, i, "null")
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return scanWitnessNumber(data, i)
	default:
		return 0, "expected a JSON value"
	}
}

// scanJSONStringEnd validates a JSON string starting at data[start] == '"'
// and returns the index just past its closing quote.
func scanJSONStringEnd(data []byte, start int) (int, string) {
	i := start + 1
	for i < len(data) {
		c := data[i]
		switch {
		case c == '"':
			return i + 1, ""
		case c == '\\':
			i++
			if i >= len(data) {
				return 0, "unterminated string"
			}
			switch data[i] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				i++
			case 'u':
				if i+4 >= len(data) {
					return 0, "invalid string escape"
				}
				for k := 1; k <= 4; k++ {
					if !isJSONHex(data[i+k]) {
						return 0, "invalid string escape"
					}
				}
				i += 5
			default:
				return 0, "invalid string escape"
			}
		case c < 0x20:
			return 0, "unescaped control character in string"
		default:
			i++
		}
	}
	return 0, "unterminated string"
}

func isJSONHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// scanWitnessObject scans an object starting at data[i] == '{'.
func scanWitnessObject(data []byte, i int) (int, string) {
	i++ // past '{'
	i = skipJSONSpace(data, i)
	if i < len(data) && data[i] == '}' {
		return i + 1, ""
	}
	for {
		i = skipJSONSpace(data, i)
		if i >= len(data) || data[i] != '"' {
			return 0, "expected an object key"
		}
		_, next, reason := scanWitnessKey(data, i)
		if reason != "" {
			return 0, reason
		}
		i = skipJSONSpace(data, next)
		if i >= len(data) || data[i] != ':' {
			return 0, "expected ':' after a member key"
		}
		end, reason := scanWitnessValue(data, i+1)
		if reason != "" {
			return 0, reason
		}
		i = skipJSONSpace(data, end)
		if i >= len(data) {
			return 0, "expected ',' or '}'"
		}
		switch data[i] {
		case ',':
			i++
		case '}':
			return i + 1, ""
		default:
			return 0, "expected ',' or '}'"
		}
	}
}

// scanWitnessArray scans an array starting at data[i] == '['.
func scanWitnessArray(data []byte, i int) (int, string) {
	i++ // past '['
	i = skipJSONSpace(data, i)
	if i < len(data) && data[i] == ']' {
		return i + 1, ""
	}
	for {
		end, reason := scanWitnessValue(data, i)
		if reason != "" {
			return 0, reason
		}
		i = skipJSONSpace(data, end)
		if i >= len(data) {
			return 0, "expected ',' or ']'"
		}
		switch data[i] {
		case ',':
			i = skipJSONSpace(data, i+1)
			if i >= len(data) {
				return 0, "expected another array element"
			}
		case ']':
			return i + 1, ""
		default:
			return 0, "expected ',' or ']'"
		}
	}
}

func scanWitnessLiteral(data []byte, i int, lit string) (int, string) {
	if bytes.HasPrefix(data[i:], []byte(lit)) {
		return i + len(lit), ""
	}
	return 0, "invalid literal"
}

// scanWitnessNumber validates the JSON number grammar at data[i].
func scanWitnessNumber(data []byte, i int) (int, string) {
	bad := func() (int, string) { return 0, "invalid number" }
	if i < len(data) && data[i] == '-' {
		i++
	}
	if i >= len(data) {
		return bad()
	}
	switch {
	case data[i] == '0':
		i++
	case data[i] >= '1' && data[i] <= '9':
		for i < len(data) && data[i] >= '0' && data[i] <= '9' {
			i++
		}
	default:
		return bad()
	}
	if i < len(data) && data[i] == '.' {
		i++
		if i >= len(data) || data[i] < '0' || data[i] > '9' {
			return bad()
		}
		for i < len(data) && data[i] >= '0' && data[i] <= '9' {
			i++
		}
	}
	if i < len(data) && (data[i] == 'e' || data[i] == 'E') {
		i++
		if i < len(data) && (data[i] == '+' || data[i] == '-') {
			i++
		}
		if i >= len(data) || data[i] < '0' || data[i] > '9' {
			return bad()
		}
		for i < len(data) && data[i] >= '0' && data[i] <= '9' {
			i++
		}
	}
	return i, ""
}

// evaluate checks every constraint against the witness and returns the
// 1-based index of the first failure, or 0 when all hold.
func (d *canonicalDefinition) evaluate(w Witness) int {
	p := d.modulus
	// Wire layout: 0 = 1, then public inputs, then private inputs.
	values := make([]int64, 1+d.public+d.private)
	values[0] = 1
	bigP := big.NewInt(p)
	for i, s := range w.Public {
		n, _ := parseBigSignedDecimal(s)
		n.Mod(n, bigP)
		values[1+i] = n.Int64()
	}
	for i, s := range w.Private {
		n, _ := parseBigSignedDecimal(s)
		n.Mod(n, bigP)
		values[1+d.public+i] = n.Int64()
	}
	combo := func(terms []parsedTerm) int64 {
		var sum int64
		for _, t := range terms {
			// sum < p and the product < (p-1)^2 < 2^62, so this add never
			// overflows int64.
			sum = modInt64(sum+t.value*values[t.wire], p)
		}
		return sum
	}
	for i, con := range d.constraints {
		av := combo(con.a)
		bv := combo(con.b)
		cv := combo(con.c)
		if modInt64(av*bv, p) != cv {
			return i + 1
		}
	}
	return 0
}
