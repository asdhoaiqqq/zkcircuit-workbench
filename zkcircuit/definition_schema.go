package zkcircuit

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// This file is the single source of the structural rules every constraint
// definition document follows, on both of its read paths: a JSON file
// imported by the user (parseDefinitionJSON in constraints.go) and a
// definition already committed in the data directory
// (persistDefinition.UnmarshalJSON). The two paths used to each maintain
// their own copy of the same three-level shape checks; they now share this
// one schema, so the structure they accept can never drift apart.
//
// The shared rules cover exactly the three-level shape — definition,
// constraint, term:
//
//	definition: an object naming "modulus" and "constraints" exactly once
//	constraint: an object naming "a", "b" and "c" exactly once
//	term:       an object naming "wire" and "coeff" exactly once
//
// Every required field must be present and non-null; unknown fields, repeated
// fields (including the same field spelled twice through JSON string
// escapes, even with identical values) and wrongly typed values are all
// rejected. An explicitly empty term array is the zero linear combination and
// an explicit wire 0 names the constant wire: both are ordinary values, never
// defaults for a missing field.
//
// The paths differ only in how a structural failure is reported — the import
// path rejects the request with ErrInvalidArgument, the committed-data path
// refuses the read with ErrDataCorrupt — and in the words each uses to name
// the document. Everything else is one rule set. The semantic domain rules
// (prime modulus, constraint count, wire layout, coefficient grammar) are
// likewise shared: they live in definitionShape.canonicalize and run after
// this structural decode on both paths.
//
// One scan, not three. The document is tokenized a single time into the
// jvalue tree: syntax is checked by the one leading json.Unmarshal and the
// tree walk accounts for every object member exactly once, so a repeated key
// nested many levels deep is seen during that one descent instead of once per
// enclosing object. The outer definition, every constraint and every term all
// take their members from the same tree; nothing is re-unmarshaled or
// re-walked per level. The order in which findings are surfaced is fixed
// exactly as it was before the consolidation: syntax first, then an unknown
// top-level field, then the first repeated key anywhere in the document (a
// document-level finding, never attributed to one constraint), and only then
// the level-by-level shape and type checks.

// jsonShape is the strict-JSON rule set the definition schema is built from:
// objects name only allowed keys, required members are present, scalars have
// their declared JSON type and no object repeats a key. The fail constructor
// tags each rejection with the reading path's error kind; nullSuffix is
// appended to object/array null rejections so the committed-data path can
// name null explicitly where the import grammar simply rejects the shape.
type jsonShape struct {
	fail       func(format string, args ...any) error
	nullSuffix string
}

// jvalue is one JSON value as found during the single structural tokenization
// pass. Objects keep their members in document order; raw is the value's own
// byte span within the document (the key text excluded), which the later
// typed reads slice directly instead of re-decoding the surrounding document.
// A null member is represented by kind == jnull; scalar leaves carry their
// json token already decoded (string/number/bool), so scalar type checks need
// no second parse to *decide the type* — only the error text of a rejected
// scalar goes through json.Unmarshal, keeping the diagnostic byte-identical.
type jvalue struct {
	kind    byte // '{', '[', 's' string, 'n' number, 'b' bool, '0' null
	raw     []byte
	str     string
	boolean bool
	members []jmember
	items   []*jvalue
}

// jmember is one object value paired with its already-JSON-unescaped key.
type jmember struct {
	key   string
	value *jvalue
}

// documentBuilder is the state of the one structural tokenization: it owns
// the decoder and remembers the first repeated key in token order while the
// whole document is still walked, so an unknown top-level field encountered
// later in the document is allowed to win over a deeper duplicate, exactly as
// the unmarshal-then-walk ordering did before consolidation.
type documentBuilder struct {
	shape  jsonShape
	dec    *json.Decoder
	doc    []byte
	what   string
	dupErr error // first duplicate field, surfaced only after the top scan
}

// valueStartAfterKey returns the offset at which an object member's value
// begins: after a key token the decoder is positioned at the colon, so skip
// the colon and surrounding JSON whitespace. The leading grammar pass already
// proved a colon and value follow, so the scan is exact.
func (b *documentBuilder) valueStartAfterKey() int64 {
	i := b.dec.InputOffset()
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
	for i < int64(len(b.doc)) && isSpace(b.doc[i]) {
		i++
	}
	if i < int64(len(b.doc)) && b.doc[i] == ':' {
		i++
	}
	for i < int64(len(b.doc)) && isSpace(b.doc[i]) {
		i++
	}
	return i
}

// parseJValue decodes the complete JSON value the decoder is positioned at,
// tracking the raw span and, for objects, recording a repeated member key.
// Keys are compared after JSON unescaping (the decoder already unescapes the
// key token), so a key repeated through a \uXXXX spelling is a duplicate even
// when both values are byte-identical. Every token of the document is visited
// exactly once in this single descent; a duplicate does not stop the walk, so
// every top-level member is still seen before any finding is surfaced.
func (b *documentBuilder) parseJValue(start int64) (*jvalue, error) {
	tok, err := b.dec.Token()
	if err != nil {
		return nil, b.shape.fail("%s is not valid JSON: %v", b.what, err)
	}
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			out := &jvalue{kind: '{'}
			seen := make(map[string]bool)
			for b.dec.More() {
				keyTok, err := b.dec.Token()
				if err != nil {
					return nil, b.shape.fail("%s is not valid JSON: %v", b.what, err)
				}
				key := keyTok.(string)
				if seen[key] && b.dupErr == nil {
					b.dupErr = b.shape.fail("%s contains duplicate field %q", b.what, key)
				}
				seen[key] = true
				child, err := b.parseJValue(b.valueStartAfterKey())
				if err != nil {
					return nil, err
				}
				out.members = append(out.members, jmember{key: key, value: child})
			}
			if _, err := b.dec.Token(); err != nil { // closing brace
				return nil, b.shape.fail("%s is not valid JSON: %v", b.what, err)
			}
			out.raw = b.doc[start:b.dec.InputOffset()]
			return out, nil
		case '[':
			out := &jvalue{kind: '['}
			for b.dec.More() {
				child, err := b.parseJValue(b.dec.InputOffset())
				if err != nil {
					return nil, err
				}
				out.items = append(out.items, child)
			}
			if _, err := b.dec.Token(); err != nil { // closing bracket
				return nil, b.shape.fail("%s is not valid JSON: %v", b.what, err)
			}
			out.raw = b.doc[start:b.dec.InputOffset()]
			return out, nil
		}
	case string:
		return &jvalue{kind: 's', raw: b.doc[start:b.dec.InputOffset()], str: v}, nil
	case json.Number:
		return &jvalue{kind: 'n', raw: b.doc[start:b.dec.InputOffset()]}, nil
	case bool:
		return &jvalue{kind: 'b', raw: b.doc[start:b.dec.InputOffset()], boolean: v}, nil
	case nil:
		return &jvalue{kind: '0', raw: b.doc[start:b.dec.InputOffset()]}, nil
	}
	return nil, b.shape.fail("%s is not valid JSON", b.what)
}

// buildDocumentTree applies the object-shape gate, then one grammar pass, then
// one whole-document walk, returning the value tree and the first repeated
// key found. The gate and the grammar pass reproduce the original object()
// precedence exactly: a null document is named as such, a document that does
// not start with '{' (an empty input, an array, a scalar) is "must be a JSON
// object" without a grammar parse, and only an object candidate is fully
// parsed, which also rejects trailing data after the single top-level value.
//
// The walk is the single place a repeated object key is detected at any
// depth. The what argument names the whole document: a duplicate nested in a
// constraint or a term is, as before, a document-level finding rather than a
// per-constraint one. dupErr is deferred: the caller checks the top-level
// field set first, so an unknown top-level field still outranks a deeper
// duplicate.
func (s jsonShape) buildDocumentTree(raw []byte, what string) (root *jvalue, dupErr error, err error) {
	trimmed := bytes.TrimSpace(raw)
	if string(trimmed) == "null" {
		return nil, nil, s.fail("%s must be a JSON object%s", what, s.nullSuffix)
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, nil, s.fail("%s must be a JSON object", what)
	}
	// One grammar pass over the whole document before any member is judged:
	// json.Unmarshal also rejects trailing data after the top-level value,
	// including a second complete value, with its own wording.
	var head json.RawMessage
	if err := json.Unmarshal(trimmed, &head); err != nil {
		return nil, nil, s.fail("%s is not valid JSON: %v", what, err)
	}
	b := &documentBuilder{
		shape: s,
		dec:   json.NewDecoder(bytes.NewReader(trimmed)),
		doc:   trimmed,
		what:  what,
	}
	b.dec.UseNumber()
	vroot, err := b.parseJValue(0)
	if err != nil {
		return nil, nil, err
	}
	return vroot, b.dupErr, nil
}

// member looks up a named member of an object value.
func (v *jvalue) member(key string) (*jvalue, bool) {
	for i := range v.members {
		if v.members[i].key == key {
			return v.members[i].value, true
		}
	}
	return nil, false
}

// requireObject rejects anything that is not an object (null included) and
// returns the value. The nullSuffix distinguishes the committed-data wording
// ("…, not null") from the import wording, which simply names the shape.
func (s jsonShape) requireObject(v *jvalue, what string) (*jvalue, error) {
	if v == nil || v.kind != '{' {
		if v != nil && v.kind == '0' {
			return nil, s.fail("%s must be a JSON object%s", what, s.nullSuffix)
		}
		return nil, s.fail("%s must be a JSON object", what)
	}
	return v, nil
}

// allowOnlyMembers rejects an object carrying any key outside allowed. It
// inspects only this object's own level; nested objects are judged when their
// own level is read.
func (s jsonShape) allowOnlyMembers(v *jvalue, allowed []string, what string) error {
	allowedSet := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		allowedSet[k] = true
	}
	for _, m := range v.members {
		if !allowedSet[m.key] {
			return s.fail("%s has unknown field %q", what, m.key)
		}
	}
	return nil
}

// requiredMember returns a required member of an object value from the
// single-pass tree.
func (s jsonShape) requiredMember(v *jvalue, key, what string) (*jvalue, error) {
	m, ok := v.member(key)
	if !ok {
		return nil, s.fail("%s is missing required field %q", what, key)
	}
	return m, nil
}

// arrayElements requires v to be an array and returns its element values.
// Null is rejected: a required array must be provided explicitly, even when
// empty. A wrongly typed value keeps its unmarshal error text.
func (s jsonShape) arrayElements(v *jvalue, what string) ([]*jvalue, error) {
	if v.kind == '0' {
		return nil, s.fail("%s must be an array%s", what, s.nullSuffix)
	}
	if v.kind != '[' {
		var elements []json.RawMessage
		if err := json.Unmarshal(v.raw, &elements); err != nil {
			return nil, s.fail("%s must be an array: %v", what, err)
		}
	}
	return v.items, nil
}

// string requires a JSON string and returns it.
func (s jsonShape) stringValue(v *jvalue, what string) (string, error) {
	if v.kind == 's' {
		return v.str, nil
	}
	if v.kind == '0' {
		return "", s.fail("%s must be a JSON string, not null", what)
	}
	var out string
	if err := json.Unmarshal(v.raw, &out); err != nil {
		return "", s.fail("%s must be a JSON string: %v", what, err)
	}
	return out, nil
}

// int requires a JSON integer (no floats, strings, null, booleans).
func (s jsonShape) intValue(v *jvalue, what string) (int, error) {
	if v.kind == '0' {
		return 0, s.fail("%s must be a JSON integer, not null", what)
	}
	if v.kind == 'n' {
		var n int
		if err := json.Unmarshal(v.raw, &n); err != nil {
			return 0, s.fail("%s must be a JSON integer: %v", what, err)
		}
		return n, nil
	}
	var n int
	if err := json.Unmarshal(v.raw, &n); err != nil {
		return 0, s.fail("%s must be a JSON integer", what)
	}
	return n, nil
}

// ---- raw-member helpers for the committed record decoders -----------------
//
// The circuit and job records in data.json are decoded member-by-member from
// the already-isolated raw members produced by the single per-record reader
// each record type drives (readCircuitRecord in circuit_record.go and
// readJobRecord in job_record.go), one level per record with the values left
// to their own decoders. These four helpers read one such raw member: a
// required lookup and the strict string/integer/boolean scalars. They share
// the jsonShape error tagging but operate on raw members rather than the
// definition document tree.

// requireRaw returns the raw value of a required member.
func (s jsonShape) require(members map[string]json.RawMessage, key, what string) (json.RawMessage, error) {
	raw, ok := members[key]
	if !ok {
		return nil, s.fail("%s is missing required field %q", what, key)
	}
	return raw, nil
}

// string decodes a raw required member as a JSON string.
func (s jsonShape) string(raw json.RawMessage, what string) (string, error) {
	if string(bytes.TrimSpace(raw)) == "null" {
		return "", s.fail("%s must be a JSON string, not null", what)
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", s.fail("%s must be a JSON string: %v", what, err)
	}
	return v, nil
}

// int decodes a raw required member as a JSON integer (no floats, strings,
// null).
func (s jsonShape) int(raw json.RawMessage, what string) (int, error) {
	trimmed := bytes.TrimSpace(raw)
	if string(trimmed) == "null" || len(trimmed) == 0 {
		return 0, s.fail("%s must be a JSON integer, not null", what)
	}
	if trimmed[0] != '-' && (trimmed[0] < '0' || trimmed[0] > '9') {
		return 0, s.fail("%s must be a JSON integer", what)
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, s.fail("%s must be a JSON integer: %v", what, err)
	}
	return n, nil
}

// bool decodes a raw required member as a JSON boolean (true/false; not null,
// a number or a string).
func (s jsonShape) bool(raw json.RawMessage, what string) (bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if string(trimmed) == "true" {
		return true, nil
	}
	if string(trimmed) == "false" {
		return false, nil
	}
	if string(trimmed) == "null" || len(trimmed) == 0 {
		return false, s.fail("%s must be a JSON boolean, not null", what)
	}
	return false, s.fail("%s must be a JSON boolean", what)
}

// ---- the shared three-level definition shape ------------------------------

// definitionShape is a constraint definition document after the shared
// structural decode and before any domain rule is applied: the modulus text
// as written and, for each constraint, its three term arrays.
type definitionShape struct {
	modulus     string
	constraints []constraintShape
}

// constraintShape is one structurally decoded {"a":[…],"b":[…],"c":[…]}.
type constraintShape struct {
	a, b, c []termShape
}

// termShape is one structurally decoded {"wire":int,"coeff":string}.
type termShape struct {
	wire  int
	coeff string
}

// definitionShapeSource carries everything that differs between the two
// definition read paths: the error kind failures are tagged with and the
// names used to describe the document. The structural rules themselves are
// shared verbatim.
type definitionShapeSource struct {
	shape      jsonShape
	definition string // names the definition object
	modulus    string // names the modulus field
	constraint string // names one constraint object
	// term names one term object, given its side and 0-based position.
	term func(side string, index int) string
	// wrapConstraint adds the 1-based constraint position to a failure
	// inside it; nil leaves the failure unadorned.
	wrapConstraint func(index int, err error) error
}

// importDefinitionSource describes a definition document supplied by the
// user through the import file: failures reject the request as invalid
// arguments, and constraints and terms are named with their positions.
var importDefinitionSource = definitionShapeSource{
	shape:      jsonShape{fail: invalidf},
	definition: "constraint definition",
	modulus:    "modulus",
	constraint: "constraint",
	term: func(side string, index int) string {
		return fmt.Sprintf("term %s[%d]", side, index)
	},
	wrapConstraint: func(index int, err error) error {
		return fmt.Errorf("constraint #%d: %w", index+1, err)
	},
}

// storedDefinitionSource describes a definition committed in the data file:
// the same structural damage is an integrity failure of committed data, so
// failures are tagged as data corruption and null is named explicitly.
var storedDefinitionSource = definitionShapeSource{
	shape:      jsonShape{fail: corruptf, nullSuffix: ", not null"},
	definition: "stored constraint definition",
	modulus:    "stored modulus",
	constraint: "stored constraint",
	term: func(side string, index int) string {
		return "stored term"
	},
}

// decodeDefinitionShape applies the shared structural rules to a raw
// definition document: exactly the modulus and constraints fields at the top
// level, exactly the a/b/c arrays per constraint and exactly the wire/coeff
// pair per term, each present, non-null and correctly typed.
//
// The document is tokenized once into a tree, and that one tree answers every
// level. Findings surface in the fixed order grammar/shape gate → unknown
// top-level field → repeated key anywhere → level-by-level required-field and
// type checks.
func decodeDefinitionShape(raw []byte, src definitionShapeSource) (definitionShape, error) {
	obj, dupErr, err := src.shape.buildDocumentTree(raw, src.definition)
	if err != nil {
		return definitionShape{}, err
	}
	// buildDocumentTree has already enforced the object gate, so obj is an
	// object here. An unknown top-level field outranks a deeper duplicate;
	// only afterwards is the first repeated key anywhere surfaced.
	if err := src.shape.allowOnlyMembers(obj, []string{"modulus", "constraints"}, src.definition); err != nil {
		return definitionShape{}, err
	}
	if dupErr != nil {
		return definitionShape{}, dupErr
	}
	modulusRaw, err := src.shape.requiredMember(obj, "modulus", src.definition)
	if err != nil {
		return definitionShape{}, err
	}
	constraintsRaw, err := src.shape.requiredMember(obj, "constraints", src.definition)
	if err != nil {
		return definitionShape{}, err
	}
	modulusText, err := src.shape.stringValue(modulusRaw, src.modulus)
	if err != nil {
		return definitionShape{}, err
	}
	constraintValues, err := src.shape.arrayElements(constraintsRaw, src.definition+` field "constraints"`)
	if err != nil {
		return definitionShape{}, err
	}
	out := definitionShape{
		modulus:     modulusText,
		constraints: make([]constraintShape, 0, len(constraintValues)),
	}
	for i, cval := range constraintValues {
		con, err := decodeConstraintValue(cval, src)
		if err != nil {
			if src.wrapConstraint != nil {
				return definitionShape{}, src.wrapConstraint(i, err)
			}
			return definitionShape{}, err
		}
		out.constraints = append(out.constraints, con)
	}
	return out, nil
}

// decodeConstraintValue decodes one {"a":[…],"b":[…],"c":[…]} value taken
// from the shared tree.
func decodeConstraintValue(v *jvalue, src definitionShapeSource) (constraintShape, error) {
	obj, err := src.shape.requireObject(v, src.constraint)
	if err != nil {
		return constraintShape{}, err
	}
	if err := src.shape.allowOnlyMembers(obj, []string{"a", "b", "c"}, src.constraint); err != nil {
		return constraintShape{}, err
	}
	var out constraintShape
	for _, side := range []struct {
		key string
		dst *[]termShape
	}{
		{"a", &out.a}, {"b", &out.b}, {"c", &out.c},
	} {
		sideRaw, err := src.shape.requiredMember(obj, side.key, src.constraint)
		if err != nil {
			return constraintShape{}, err
		}
		terms, err := decodeTermArrayValue(sideRaw, src, side.key)
		if err != nil {
			return constraintShape{}, err
		}
		*side.dst = terms
	}
	return out, nil
}

// decodeTermArrayValue decodes one a/b/c side: an explicit array (possibly
// empty) of term objects, read from the shared tree.
func decodeTermArrayValue(v *jvalue, src definitionShapeSource, side string) ([]termShape, error) {
	termValues, err := src.shape.arrayElements(v, fmt.Sprintf("%s side %q", src.constraint, side))
	if err != nil {
		return nil, err
	}
	terms := make([]termShape, 0, len(termValues))
	for j, tval := range termValues {
		term, err := decodeTermValue(tval, src, side, j)
		if err != nil {
			return nil, err
		}
		terms = append(terms, term)
	}
	return terms, nil
}

// decodeTermValue decodes one {"wire":int,"coeff":string} value taken from
// the shared tree.
func decodeTermValue(v *jvalue, src definitionShapeSource, side string, index int) (termShape, error) {
	what := src.term(side, index)
	obj, err := src.shape.requireObject(v, what)
	if err != nil {
		return termShape{}, err
	}
	if err := src.shape.allowOnlyMembers(obj, []string{"wire", "coeff"}, what); err != nil {
		return termShape{}, err
	}
	wireRaw, err := src.shape.requiredMember(obj, "wire", what)
	if err != nil {
		return termShape{}, err
	}
	coeffRaw, err := src.shape.requiredMember(obj, "coeff", what)
	if err != nil {
		return termShape{}, err
	}
	wire, err := src.shape.intValue(wireRaw, what+" wire")
	if err != nil {
		return termShape{}, err
	}
	coeff, err := src.shape.stringValue(coeffRaw, what+" coefficient")
	if err != nil {
		return termShape{}, err
	}
	return termShape{wire: wire, coeff: coeff}, nil
}

// ---- conversions between the shape and the persisted form -----------------

// toPersist renders the structural decode in the persisted shape. Empty term
// arrays stay explicit empty arrays (never null), matching what the importer
// writes.
func (s definitionShape) toPersist() persistDefinition {
	out := persistDefinition{
		Modulus:     s.modulus,
		Constraints: make([]persistConstraint, 0, len(s.constraints)),
	}
	for _, con := range s.constraints {
		out.Constraints = append(out.Constraints, persistConstraint{
			A: termsToPersist(con.a),
			B: termsToPersist(con.b),
			C: termsToPersist(con.c),
		})
	}
	return out
}

func termsToPersist(terms []termShape) []persistTerm {
	out := make([]persistTerm, 0, len(terms))
	for _, t := range terms {
		out = append(out, persistTerm{Wire: t.wire, Coeff: t.coeff})
	}
	return out
}

// shapeFromPersist views an already-decoded stored definition as the shared
// structural shape, so the domain rules can re-run without a JSON round
// trip.
func shapeFromPersist(p persistDefinition) definitionShape {
	out := definitionShape{
		modulus:     p.Modulus,
		constraints: make([]constraintShape, 0, len(p.Constraints)),
	}
	for _, con := range p.Constraints {
		out.constraints = append(out.constraints, constraintShape{
			a: termsFromPersist(con.A),
			b: termsFromPersist(con.B),
			c: termsFromPersist(con.C),
		})
	}
	return out
}

func termsFromPersist(terms []persistTerm) []termShape {
	out := make([]termShape, 0, len(terms))
	for _, t := range terms {
		out = append(out, termShape{wire: t.Wire, coeff: t.Coeff})
	}
	return out
}

// ---- strict decoding of a persisted definition ----------------------------
//
// persistDefinition's decoder is the committed-data endpoint of the shared
// schema: a stored definition must have exactly the shape the importer
// writes, and every shape failure is data corruption, not a bad request.
// This runs while the envelope is being read, before validateEnvelope: a
// definition that omits a side array or a term's wire/coeff — which a plain
// struct decode would silently turn into a nil slice or a zero int/string —
// is refused at read time instead of being mistaken for an empty array, wire
// 0 or an empty coefficient. An explicitly empty side array still decodes to
// the zero linear combination and an explicit wire:0 still names the
// constant wire; both are ordinary values and stay legal. The circuit
// record's definition pointer itself may be absent or null: encoding/json
// leaves the pointer nil without invoking this method, which is exactly the
// legacy counts-only state.
//
// The decoder enforces shape only. The semantic rules (prime modulus, wire
// range, constraint count) are re-checked by validateEnvelope through the
// same definitionFromPersist pipeline used after a clean import.
func (p *persistDefinition) UnmarshalJSON(raw []byte) error {
	shape, err := decodeDefinitionShape(raw, storedDefinitionSource)
	if err != nil {
		return err
	}
	*p = shape.toPersist()
	return nil
}
