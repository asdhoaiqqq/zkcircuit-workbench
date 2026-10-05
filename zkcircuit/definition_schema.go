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

// object decodes raw as a JSON object whose keys must all be allowed,
// rejecting syntax errors, trailing data, non-objects and unknown keys. It
// returns the raw members for typed decoding.
//
// It checks its own object's level only: the member map (unknown keys,
// required/type judgments made by the caller) is decoded here, while a
// repeated key anywhere in the document is rejected once, up front, by the
// document-wide rejectDuplicates the top-level decoder runs before any nested
// object is touched. object itself therefore neither re-scans its subtree nor
// is re-scanned by an ancestor: every nested object is structurally
// interpreted exactly once by the decoder that owns its level.
func (s jsonShape) object(raw []byte, allowed []string, what string) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if string(trimmed) == "null" {
		return nil, s.fail("%s must be a JSON object%s", what, s.nullSuffix)
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, s.fail("%s must be a JSON object", what)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &members); err != nil {
		// Unmarshal also rejects trailing data after the top-level value.
		return nil, s.fail("%s is not valid JSON: %v", what, err)
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		allowedSet[k] = true
	}
	for key := range members {
		if !allowedSet[key] {
			return nil, s.fail("%s has unknown field %q", what, key)
		}
	}
	return members, nil
}

// rejectDuplicates walks one whole definition document's JSON token stream
// once, rejecting any object that names the same key twice; encoding/json
// silently keeps the last value. Keys are compared after JSON unescaping, so a
// key repeated through a \uXXXX spelling is still a duplicate, even when both
// values are identical.
//
// This is the only duplicate scan a document undergoes. It runs over the full
// raw document at the top level before any constraint or term object is
// decoded, so a repeated field is found wherever it is nested but each nested
// object is walked just this once — never again by a per-object scan. The
// failure is reported with the document's own name (and the import path's
// constraint wrapping is deliberately not applied: the document-wide scan
// attributes the problem to the document as a whole).
func (s jsonShape) rejectDuplicates(raw []byte, what string) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return s.fail("%s is not valid JSON: %v", what, err)
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
					return s.fail("%s is not valid JSON: %v", what, err)
				}
				key := keyTok.(string)
				if seen[key] {
					return s.fail("%s contains duplicate field %q", what, key)
				}
				seen[key] = true
				if err := walk(); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // closing brace
				return s.fail("%s is not valid JSON: %v", what, err)
			}
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil { // closing bracket
				return s.fail("%s is not valid JSON: %v", what, err)
			}
		}
		return nil
	}
	return walk()
}

// require returns the raw value of a required member.
func (s jsonShape) require(members map[string]json.RawMessage, key, what string) (json.RawMessage, error) {
	raw, ok := members[key]
	if !ok {
		return nil, s.fail("%s is missing required field %q", what, key)
	}
	return raw, nil
}

// array requires raw to be a JSON array and returns its raw elements. Null
// is rejected: a required array must be provided explicitly, even when empty.
func (s jsonShape) array(raw json.RawMessage, what string) ([]json.RawMessage, error) {
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil, s.fail("%s must be an array%s", what, s.nullSuffix)
	}
	var elements []json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil {
		return nil, s.fail("%s must be an array: %v", what, err)
	}
	return elements, nil
}

// string requires raw to be a JSON string and returns it.
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

// int requires raw to be a JSON integer (no floats, strings, null).
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

// bool requires raw to be a JSON boolean (true/false; not null, a number or
// a string).
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
func decodeDefinitionShape(raw []byte, src definitionShapeSource) (definitionShape, error) {
	members, err := src.shape.object(raw, []string{"modulus", "constraints"}, src.definition)
	if err != nil {
		return definitionShape{}, err
	}
	// One document-wide duplicate pass over the raw bytes, run before any
	// nested object is decoded. It is the only duplicate scan the document
	// undergoes: nested object decoders no longer each re-walk their subtree,
	// so a term is not tokenized by the definition pass, again by its
	// constraint and again by itself.
	if err := src.shape.rejectDuplicates(bytes.TrimSpace(raw), src.definition); err != nil {
		return definitionShape{}, err
	}
	modulusRaw, err := src.shape.require(members, "modulus", src.definition)
	if err != nil {
		return definitionShape{}, err
	}
	constraintsRaw, err := src.shape.require(members, "constraints", src.definition)
	if err != nil {
		return definitionShape{}, err
	}
	modulusText, err := src.shape.string(modulusRaw, src.modulus)
	if err != nil {
		return definitionShape{}, err
	}
	constraintRaws, err := src.shape.array(constraintsRaw, src.definition+` field "constraints"`)
	if err != nil {
		return definitionShape{}, err
	}
	out := definitionShape{
		modulus:     modulusText,
		constraints: make([]constraintShape, 0, len(constraintRaws)),
	}
	for i, craw := range constraintRaws {
		con, err := decodeConstraintShape(craw, src)
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

// decodeConstraintShape decodes one {"a":[…],"b":[…],"c":[…]} object.
func decodeConstraintShape(raw json.RawMessage, src definitionShapeSource) (constraintShape, error) {
	members, err := src.shape.object(raw, []string{"a", "b", "c"}, src.constraint)
	if err != nil {
		return constraintShape{}, err
	}
	var out constraintShape
	for _, side := range []struct {
		key string
		dst *[]termShape
	}{
		{"a", &out.a}, {"b", &out.b}, {"c", &out.c},
	} {
		sideRaw, err := src.shape.require(members, side.key, src.constraint)
		if err != nil {
			return constraintShape{}, err
		}
		terms, err := decodeTermArray(sideRaw, src, side.key)
		if err != nil {
			return constraintShape{}, err
		}
		*side.dst = terms
	}
	return out, nil
}

// decodeTermArray decodes one a/b/c side: an explicit array (possibly empty)
// of term objects.
func decodeTermArray(raw json.RawMessage, src definitionShapeSource, side string) ([]termShape, error) {
	termRaws, err := src.shape.array(raw, fmt.Sprintf("%s side %q", src.constraint, side))
	if err != nil {
		return nil, err
	}
	terms := make([]termShape, 0, len(termRaws))
	for j, traw := range termRaws {
		term, err := decodeTermShape(traw, src, side, j)
		if err != nil {
			return nil, err
		}
		terms = append(terms, term)
	}
	return terms, nil
}

// decodeTermShape decodes one {"wire":int,"coeff":string} object.
func decodeTermShape(raw json.RawMessage, src definitionShapeSource, side string, index int) (termShape, error) {
	what := src.term(side, index)
	members, err := src.shape.object(raw, []string{"wire", "coeff"}, what)
	if err != nil {
		return termShape{}, err
	}
	wireRaw, err := src.shape.require(members, "wire", what)
	if err != nil {
		return termShape{}, err
	}
	coeffRaw, err := src.shape.require(members, "coeff", what)
	if err != nil {
		return termShape{}, err
	}
	wire, err := src.shape.int(wireRaw, what+" wire")
	if err != nil {
		return termShape{}, err
	}
	coeff, err := src.shape.string(coeffRaw, what+" coefficient")
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
