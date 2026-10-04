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
	"math"
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

// checkedWitness is a witness that has passed the layout and decimal-grammar
// rules, with every value already converted from its decimal string to a big
// integer — exactly once per value. Evaluation reduces these integers into
// the field without ever reconverting the original strings, so a long legal
// input pays the text-to-integer conversion a single time across validation
// and constraint checking. The integers are kept at full precision; the
// modulus is only applied when the wire values are built for evaluation.
type checkedWitness struct {
	public  []*big.Int
	private []*big.Int
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
	bound, ok := wireUpperBound(public, private)
	if !ok {
		return false
	}
	return d.maxWire() <= bound
}

// wireUpperBound returns the highest input-wire index the declared layout
// names — public + private — reporting ok=false when that sum would wrap the
// platform's int. Every reachable caller has already passed the layout rule
// 1 + public + private <= M, so ok=false marks an unrepresentable layout and
// is never silently treated as a small (possibly negative) bound.
func wireUpperBound(public, private int) (bound int, ok bool) {
	if public > math.MaxInt-private {
		return 0, false
	}
	return public + private, true
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

// ---- definition parsing ---------------------------------------------------
//
// The structural half of parsing — the three-level definition/constraint/
// term shape with its required fields, types and duplicate rejection — is
// the shared schema in definition_schema.go, used verbatim by both the
// import path here and the committed-data read path. The semantic half is
// the shared canonicalize below.

// parseDefinitionJSON parses and fully validates a definition document
// against the version's declared counts. The structural rules (object
// shapes, required fields, types, duplicates) are the shared schema in
// definition_schema.go — the same rules the store applies when re-reading a
// committed definition — tagged here as ErrInvalidArgument because the
// document comes from a user request.
//
// Rejected inputs include malformed JSON, trailing data, duplicate/unknown
// or missing fields (top level, per constraint and per term), a non-prime or
// out-of-range modulus, malformed coefficients, wire indexes outside
// [0, 1+public+private) and a constraint count that does not equal the
// circuit version's declared count. The wire layout is wire 0 = constant 1,
// followed by the declared public inputs and then the private inputs.
func parseDefinitionJSON(raw []byte, public, private, wantCount int) (*canonicalDefinition, error) {
	shape, err := decodeDefinitionShape(raw, importDefinitionSource)
	if err != nil {
		return nil, err
	}
	return shape.canonicalize(public, private, wantCount)
}

// canonicalize applies the domain rules to a structurally decoded
// definition and returns the canonical form: terms merged per wire,
// coefficients reduced modulo p, zeros dropped, wires sorted ascending
// within each linear combination. The modulus must be a prime in
// [2, maxModulus], the constraint count must equal the version's declared
// count, every coefficient must be a signed decimal integer and every wire
// must lie inside the declared public/private input layout. Both definition
// sources run through this same pipeline: the importer on the user's JSON
// document, the store on the shape re-decoded from committed data.
func (s definitionShape) canonicalize(public, private, wantCount int) (*canonicalDefinition, error) {
	p, err := parseModulus(s.modulus)
	if err != nil {
		return nil, err
	}
	if len(s.constraints) != wantCount {
		return nil, invalidf("constraint count mismatch: definition has %d constraints, version declares %d",
			len(s.constraints), wantCount)
	}

	parsed := &canonicalDefinition{
		modulus:     p,
		public:      public,
		private:     private,
		constraints: make([]canonicalConstraint, 0, len(s.constraints)),
	}
	maxIndex, representable := wireUpperBound(public, private)
	if !representable {
		return nil, invalidf("input layout is not representable: 1 constant wire plus the declared public and private inputs exceeds the platform wire limit")
	}
	for i, con := range s.constraints {
		canonical, err := con.canonicalize(p, maxIndex)
		if err != nil {
			return nil, fmt.Errorf("constraint #%d: %w", i+1, err)
		}
		parsed.constraints = append(parsed.constraints, canonical)
	}
	return parsed, nil
}

// canonicalize reduces one constraint's three sides to canonical form.
func (c constraintShape) canonicalize(p int64, maxIndex int) (canonicalConstraint, error) {
	var out canonicalConstraint
	for _, side := range []struct {
		key   string
		terms []termShape
		dst   *[]parsedTerm
	}{
		{"a", c.a, &out.a}, {"b", c.b, &out.b}, {"c", c.c, &out.c},
	} {
		terms, err := canonicalizeTerms(side.terms, p, maxIndex, side.key)
		if err != nil {
			return canonicalConstraint{}, err
		}
		*side.dst = terms
	}
	return out, nil
}

// canonicalizeTerms validates one side's terms and returns the canonical
// combination: duplicate wires merged, coefficients reduced modulo p, zeros
// dropped, wires sorted ascending.
func canonicalizeTerms(terms []termShape, p int64, maxIndex int, side string) ([]parsedTerm, error) {
	bigP := big.NewInt(p)
	merged := make(map[int]*big.Int)
	for j, t := range terms {
		if t.wire < 0 || t.wire > maxIndex {
			return nil, invalidf("term %s[%d] references wire %d outside [0,%d]", side, j, t.wire, maxIndex)
		}
		coeff, err := parseBigSignedDecimal(t.coeff)
		if err != nil {
			return nil, invalidf("term %s[%d] coefficient %q is not a decimal integer: %v", side, j, t.coeff, err)
		}
		coeff.Mod(coeff, bigP) // into [0,p); Go's Mod keeps the sign of p
		if existing, ok := merged[t.wire]; ok {
			existing.Add(existing, coeff)
			existing.Mod(existing, bigP)
		} else {
			merged[t.wire] = new(big.Int).Set(coeff)
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
// used both at load time (integrity check) and before compile/check. The
// stored shape was already checked against the shared structural schema when
// the data file was decoded (persistDefinition.UnmarshalJSON), so what
// remains is the shared semantic pipeline every definition also runs at
// import time.
func definitionFromPersist(p persistDefinition, public, private int) (*canonicalDefinition, error) {
	return shapeFromPersist(p).canonicalize(public, private, len(p.Constraints))
}

// ---- input checking -------------------------------------------------------

// parseWitness validates the check input JSON and tags every rejection as an
// input format error (ErrInvalidInput), distinct from domain rule failures.
func parseWitness(raw []byte, public, private int) (Witness, error) {
	w, _, err := parseWitnessDocument(raw, public, private)
	if err != nil {
		return Witness{}, tagWitnessError(err)
	}
	return w, nil
}

// parseWitnessChecked is the check-flow form of parseWitness: same document
// rules and the same input-format tagging, but it returns the checked
// witness whose values were converted to integers once during validation, so
// the evaluation that follows never parses the decimal strings again.
func parseWitnessChecked(raw []byte, public, private int) (checkedWitness, error) {
	_, checked, err := parseWitnessDocument(raw, public, private)
	if err != nil {
		return checkedWitness{}, tagWitnessError(err)
	}
	return checked, nil
}

// tagWitnessError tags any witness rejection as an input format error
// (ErrInvalidInput), keeping the detailed message but replacing the kind.
func tagWitnessError(err error) error {
	var se StoreError
	if errors.As(err, &se) {
		return StoreError{Kind: ErrInvalidInput.Kind, Detail: se.Detail}
	}
	return inputFormatf("%v", err)
}

// parseWitnessDocument parses and validates a check input document: both
// groups must be present as arrays, their lengths must match the
// declaration, and every value must be an arbitrarily long signed decimal
// string. It returns both the string form and the checked form whose values
// were converted to integers during validation, so callers bound for
// evaluation never parse the same strings a second time.
//
// Privacy rule. Diagnostics for the private group never quote a private
// value, a character or fragment taken from one, an object key found inside
// the private array, or the raw bytes the parser ran into. Private problems
// are identified structurally instead — the 1-based private input index, the
// group name and the required shape — so a caller can repair the document
// without learning its private contents. Public diagnostics keep their full
// detail. When a document is too damaged to tell which group a token belongs
// to, it is rejected generically as malformed JSON with no source quoted.
func parseWitnessDocument(raw []byte, public, private int) (Witness, checkedWitness, error) {
	members, err := splitWitnessObject(raw)
	if err != nil {
		return Witness{}, checkedWitness{}, err
	}
	publicRaw, havePublic := members["public"]
	privateRaw, havePrivate := members["private"]
	if !havePublic {
		return Witness{}, checkedWitness{}, invalidf("input is missing required field %q: the public group must be an array of decimal strings", "public")
	}
	if !havePrivate {
		return Witness{}, checkedWitness{}, invalidf("input is missing required field %q: the private group must be an array of decimal strings", "private")
	}
	pubValues, err := parsePublicArray(publicRaw)
	if err != nil {
		return Witness{}, checkedWitness{}, err
	}
	privValues, err := parsePrivateArray(privateRaw)
	if err != nil {
		return Witness{}, checkedWitness{}, err
	}
	checked, err := witnessFromArrays(pubValues, privValues, public, private)
	if err != nil {
		return Witness{}, checkedWitness{}, err
	}
	return Witness{Public: pubValues, Private: privValues}, checked, nil
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
//
// Every value is converted from its decimal string to a big integer here,
// once, and the converted form is returned for evaluation — this is the only
// text-to-integer conversion a checked input ever goes through.
func witnessFromArrays(pubValues, privValues []string, public, private int) (checkedWitness, error) {
	if len(pubValues) != public {
		return checkedWitness{}, invalidf("public input array has %d values but the version requires %d decimal strings",
			len(pubValues), public)
	}
	if len(privValues) != private {
		return checkedWitness{}, invalidf("private input array has %d values but the version requires %d decimal strings",
			len(privValues), private)
	}
	pub, err := parseDecimalValues(pubValues, "public", true)
	if err != nil {
		return checkedWitness{}, err
	}
	priv, err := parseDecimalValues(privValues, "private", false)
	if err != nil {
		return checkedWitness{}, err
	}
	return checkedWitness{public: pub, private: priv}, nil
}

// parseDecimalValues checks that every entry is an arbitrarily long signed
// decimal string and returns the parsed integers in the same order. Public
// values may be echoed with the parser's detailed reason; private values are
// never quoted, and their failure is described structurally (see
// signedDecimalKind) with a 1-based index.
func parseDecimalValues(values []string, which string, echo bool) ([]*big.Int, error) {
	parsed := make([]*big.Int, len(values))
	for i, v := range values {
		n, err := parseBigSignedDecimal(v)
		if err != nil {
			if echo {
				return nil, invalidf("%s input #%d value %q is not a decimal integer: %v", which, i+1, v, err)
			}
			return nil, invalidf("%s input #%d is not a decimal integer: %s", which, i+1, signedDecimalKind(v))
		}
		parsed[i] = n
	}
	return parsed, nil
}

// evaluate checks every constraint against the witness and returns the
// 1-based index of the first failure, or 0 when all hold. It converts the
// decimal strings itself; the check entry points instead validate and
// convert once, then call evaluateChecked, so a checked input is never
// parsed twice.
func (d *canonicalDefinition) evaluate(w Witness) int {
	values := make([]int64, 1+d.public+d.private)
	values[0] = 1
	bigP := big.NewInt(d.modulus)
	for i, s := range w.Public {
		n, _ := parseBigSignedDecimal(s)
		values[1+i] = n.Mod(n, bigP).Int64()
	}
	for i, s := range w.Private {
		n, _ := parseBigSignedDecimal(s)
		values[1+d.public+i] = n.Mod(n, bigP).Int64()
	}
	return d.evaluateValues(values)
}

// evaluateChecked is evaluate for an already-validated witness: the values
// were converted from their decimal strings once during validation, so all
// that remains here is reducing them into the field.
func (d *canonicalDefinition) evaluateChecked(checked checkedWitness) int {
	values := make([]int64, 1+d.public+d.private)
	values[0] = 1
	bigP := big.NewInt(d.modulus)
	for i, n := range checked.public {
		values[1+i] = new(big.Int).Mod(n, bigP).Int64()
	}
	for i, n := range checked.private {
		values[1+d.public+i] = new(big.Int).Mod(n, bigP).Int64()
	}
	return d.evaluateValues(values)
}

// evaluateValues runs the a·b = c check over fully reduced wire values —
// wire 0 is the constant 1, then the public inputs, then the private
// inputs — and returns the 1-based index of the first failing constraint,
// or 0 when every constraint holds.
func (d *canonicalDefinition) evaluateValues(values []int64) int {
	p := d.modulus
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
