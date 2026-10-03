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
	digits, neg, err := scanSignedDecimal(s)
	if err != nil {
		return nil, err
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

// scanSignedDecimal validates the hand-checked decimal grammar and returns
// the unsigned digit text with the sign separated out. The error it returns
// for an illegal byte deliberately quotes that byte: callers whose input is
// private (witness values) must not surface it and instead use
// signedDecimalKind to describe the failure without echoing any input.
func scanSignedDecimal(s string) (digits string, neg bool, err error) {
	if s == "" {
		return "", false, fmt.Errorf("empty string")
	}
	if s[0] == '-' {
		if len(s) == 1 {
			return "", false, fmt.Errorf("sign without digits")
		}
		neg = true
		s = s[1:]
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return "", false, fmt.Errorf("illegal character %q", s[i])
		}
	}
	return s, neg, nil
}

// signedDecimalKind classifies a witness string that failed the signed
// decimal grammar. It never quotes a character from the input: the returned
// description is structural (empty string / sign without digits / illegal
// character) so it is safe to report even when the value is private.
func signedDecimalKind(s string) string {
	if s == "" {
		return "empty string"
	}
	if s[0] == '-' {
		if len(s) == 1 {
			return "sign without digits"
		}
		s = s[1:]
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return "illegal character"
		}
	}
	return "not a decimal integer"
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
// groups must be present as arrays, their lengths must match the
// declaration, and every value must be an arbitrarily long signed decimal
// string.
//
// Privacy rule. Diagnostics for the private group never quote a private
// value, a character or fragment taken from one, an object key found inside
// the private array, or the raw bytes the parser ran into. Private problems
// are identified structurally instead — the 1-based private input index, the
// group name and the required shape — so a caller can repair the document
// without learning its private contents. Public diagnostics keep their full
// detail. When a document is too damaged to tell which group a token belongs
// to, it is rejected generically as malformed JSON with no source quoted.
func parseWitnessDocument(raw []byte, public, private int) (Witness, error) {
	members, err := splitWitnessObject(raw)
	if err != nil {
		return Witness{}, err
	}
	publicRaw, havePublic := members["public"]
	privateRaw, havePrivate := members["private"]
	if !havePublic {
		return Witness{}, invalidf("input is missing required field %q: the public group must be an array of decimal strings", "public")
	}
	if !havePrivate {
		return Witness{}, invalidf("input is missing required field %q: the private group must be an array of decimal strings", "private")
	}
	pubValues, err := parsePublicArray(publicRaw)
	if err != nil {
		return Witness{}, err
	}
	privValues, err := parsePrivateArray(privateRaw)
	if err != nil {
		return Witness{}, err
	}
	return witnessFromArrays(pubValues, privValues, public, private)
}

// splitWitnessObject walks only the top-level {"public":…,"private":…}
// envelope and returns each member's raw value. It exists to attribute a
// lexical failure to the right group before the value is decoded, so a
// syntax error inside the private value can be reported without quoting the
// character the decoder met there.
//
// Top-level key names are fixed schema names ("public"/"private"), so
// quoting an unknown or duplicated top-level field cannot disclose a
// private value. A failure while reading a key or the opening/closing brace
// is unattributable and reported as plain malformed JSON with no source text.
func splitWitnessObject(raw []byte) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, invalidf("input must be a JSON object with %q and %q arrays", "public", "private")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	open, err := dec.Token()
	if err != nil || open != json.Delim('{') {
		return nil, invalidf("input is not valid JSON")
	}
	members := make(map[string]json.RawMessage)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, invalidf("input is not valid JSON")
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, invalidf("input is not valid JSON")
		}
		if key != "public" && key != "private" {
			return nil, invalidf("input has unknown field %q: only %q and %q are allowed", key, "public", "private")
		}
		if _, duplicated := members[key]; duplicated {
			return nil, invalidf("input contains duplicate field %q", key)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			if key == "private" {
				// Inside the private section: describe the problem, never the
				// character, offset or fragment the parser encountered.
				return nil, invalidf("input field %q is not valid JSON: the private group must be an array of decimal strings", key)
			}
			return nil, invalidf("input field %q is not valid JSON: %v", key, err)
		}
		members[key] = value
	}
	if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim('}') {
		return nil, invalidf("input is not valid JSON")
	}
	// Anything after the top-level object (including more JSON tokens) is
	// trailing data at an unattributable location; report it generically.
	if _, err := dec.Token(); err != io.EOF {
		return nil, invalidf("input is not valid JSON: trailing data after the object")
	}
	return members, nil
}

// parsePublicArray decodes the public group with full diagnostics. Public
// input is not confidential, so the underlying parser error — including the
// offending value — is retained.
func parsePublicArray(raw json.RawMessage) ([]string, error) {
	const which = "public"
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil, invalidf("input field %q must be an array of decimal strings, not null", which)
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, invalidf("input field %q must be an array of decimal strings: %v", which, err)
	}
	return values, nil
}

// parsePrivateArray decodes the private group without ever quoting its
// contents. Each element must be a JSON string; a number, boolean, null,
// object or nested array is rejected by its 1-based position alone. An
// object element is rejected at its opening brace and never descended into,
// so a duplicate key (or any key) inside it cannot be reported.
func parsePrivateArray(raw json.RawMessage) ([]string, error) {
	const which = "private"
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" || trimmed[0] != '[' {
		return nil, invalidf("input field %q must be an array of decimal strings", which)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if open, err := dec.Token(); err != nil || open != json.Delim('[') {
		return nil, invalidf("input field %q is not valid JSON: it must be an array of decimal strings", which)
	}
	values := []string{}
	index := 0
	for dec.More() {
		index++
		tok, err := dec.Token()
		if err != nil {
			return nil, invalidf("input field %q is not valid JSON: element #%d must be a decimal string", which, index)
		}
		switch t := tok.(type) {
		case string:
			values = append(values, t)
		case json.Delim:
			kind := "array"
			if t == '{' {
				kind = "object"
			}
			return nil, invalidf("private input #%d must be a decimal string, not an %s", index, kind)
		case json.Number, float64:
			return nil, invalidf("private input #%d must be a decimal string, not a number", index)
		case bool:
			return nil, invalidf("private input #%d must be a decimal string, not a boolean", index)
		case nil:
			return nil, invalidf("private input #%d must be a decimal string, not null", index)
		}
	}
	if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim(']') {
		return nil, invalidf("input field %q is not valid JSON: it must be an array of decimal strings", which)
	}
	return values, nil
}

// witnessFromArrays applies the single witness grammar and layout rule
// shared by both check entry points: each group must contain exactly the
// declared number of decimal strings. A nil slice is accepted wherever an
// empty one would be (an in-API witness declaring zero inputs); unlike the
// JSON entry point nothing here distinguishes "omitted" from "empty".
// Private values are never echoed; private problems are identified by index.
func witnessFromArrays(pubValues, privValues []string, public, private int) (Witness, error) {
	if len(pubValues) != public {
		return Witness{}, invalidf("public input array has %d values but the version requires %d decimal strings",
			len(pubValues), public)
	}
	if len(privValues) != private {
		return Witness{}, invalidf("private input array has %d values but the version requires %d decimal strings",
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
// detailed reason; private values are never quoted, and their failure is
// described structurally (see signedDecimalKind) with a 1-based index.
func validateWitnessValues(values []string, which string, echo bool) error {
	for i, v := range values {
		if _, err := parseBigSignedDecimal(v); err != nil {
			if echo {
				return invalidf("%s input #%d value %q is not a decimal integer: %v", which, i+1, v, err)
			}
			return invalidf("%s input #%d is not a decimal integer: %s", which, i+1, signedDecimalKind(v))
		}
	}
	return nil
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
