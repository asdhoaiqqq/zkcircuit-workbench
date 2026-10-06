package zkcircuit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"
)

// Saved circuit records used to be read and field-checked twice: once while
// the committed envelope was tokenized to reject a repeated object key
// (scanCircuitArray) and again while each circuits element was decoded into a
// persistCircuit (persistCircuit.UnmarshalJSON). Each pass gathered the
// record's member keys on its own and carried its own copy of the object,
// required-field and duplicate-key rules, so the judgment of what one
// committed circuit record may look like lived in two places that had to
// agree by construction.
//
// This file is now the single home of a saved circuit record's reading rules:
// the persistCircuit shape, the exact member spellings it accepts
// (name / version / constraints / public_inputs / private_inputs / frozen /
// description, plus the one optional definition), the strict scalar typing
// and the name exact-readback rule, together with one per-record reading
// engine (readCircuitRecord) both read passes drive. The directory scan and
// the single-record decode hence tokenize one record once and judge its keys
// and structure through the same code, which also covers two byte-identical
// member keys — a case a post-decode member map could not see on its own.
//
// Circuit fields are stricter than envelope and job fields: they are
// recognized ONLY under their exact lowercase wire spellings. Keys reach
// every rule below already JSON-unescaped, so a key written with a \uXXXX
// escape is judged on its decoded spelling, but no letter-case folding is
// applied — "FROZEN", "Name" and a long-s lookalike such as "frozeſ" are all
// ordinary unknown fields and corrupt the read the same way "note" does.
//
// Only the reading of saved circuit records is consolidated here. The public
// behavior — which values load, which are refused, how the failure is located
// and in what order findings surface — is unchanged; the commands, the Go
// entry points and the on-disk format stay as they were. In particular the
// two read passes keep their historical division of labor and precedence
// because an open always runs the directory scan before the typed decode: the
// scan owns structural findings and exact repeated keys (readCircuitRecord as
// driven by scanCircuitArray) over every record first, while the typed decode
// owns unknown fields, the per-field required/type checks, the name
// exact-readback rule and the definition decode (decodeCircuit).

type persistCircuit struct {
	Name          string             `json:"name"`
	Version       int                `json:"version"`
	Constraints   int                `json:"constraints"`
	PublicInputs  int                `json:"public_inputs"`
	PrivateInputs int                `json:"private_inputs"`
	Frozen        bool               `json:"frozen"`
	Description   string             `json:"description"`
	Definition    *persistDefinition `json:"definition,omitempty"`
}

// persistCircuitFields are the eight fields a committed circuit record may
// carry, under their exact (standard lowercase) wire spellings. Seven are
// required with the exact JSON type the writer emits; definition alone is
// optional (absent or null both mean the legacy counts-only state).
var persistCircuitFields = []string{
	"name", "version", "constraints", "public_inputs", "private_inputs",
	"frozen", "description", "definition",
}

// storedRecordShape is the shared strict-JSON rule set (definition_schema.go)
// tagged for committed data: every structural failure in a stored circuit or
// job record is data corruption, never a bad request.
var storedRecordShape = jsonShape{fail: corruptf}

// circuitMember is one member of a saved circuit record as seen during the
// single per-record pass: its already-JSON-unescaped key and its raw value.
type circuitMember struct {
	key   string
	value json.RawMessage
}

// readCircuitRecord consumes exactly one circuit record value the decoder is
// positioned at — including its opening token — and returns the members. The
// two passes drive it with a different key rule, so one tokenization serves
// both historical tiers:
//
//   - The directory scan passes allowedFields == nil: it does not judge which
//     keys are valid, only structure and a repeated key of ANY spelling. That
//     matches the old envelope scan, which attributed a repeated member to the
//     record whether or not the name was known.
//   - The typed decode passes the eight allowed spellings: at each key
//     position an unknown spelling is named first and a repeated allowed key
//     second, in document order — the precedence of the old strict member
//     walk (so an unknown member appearing before another key's second
//     occurrence is named as unknown, and an unknown key appearing twice is
//     named as unknown rather than duplicate).
//
// In both tiers the key judgment happens before that occurrence's value is
// decoded, the point at which both old passes rejected, so a malformed value
// following the offending key never substitutes a syntax error. Every other
// token is consumed unless the record is damaged. A null element or a
// non-object element is structural damage named by positional. positional is
// the name used to locate the record: the directory scan passes
// "circuit record #<1-based index>", the typed decode passes
// "stored circuit record".
//
// Member values are captured whole without descending: a nested value such as
// the constraint definition owns its own (more precise) strict decoder
// during the typed decode.
func readCircuitRecord(dec *json.Decoder, positional string, allowedFields []string) ([]circuitMember, error) {
	open, err := dec.Token()
	if err != nil {
		return nil, corruptf("%s is not valid JSON: %v", positional, err)
	}
	if open == nil {
		return nil, corruptf("%s must be a JSON object, not null", positional)
	}
	if open != json.Delim('{') {
		return nil, corruptf("%s must be a JSON object", positional)
	}

	var allowed map[string]bool
	if allowedFields != nil {
		allowed = make(map[string]bool, len(allowedFields))
		for _, f := range allowedFields {
			allowed[f] = true
		}
	}
	var members []circuitMember
	seen := make(map[string]bool)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, corruptf("%s is not valid JSON: %v", positional, err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, corruptf("%s is not valid JSON", positional)
		}
		// Keys arrive already JSON-unescaped, so a key spelled through a
		// \uXXXX escape is judged on its decoded spelling. The typed tier
		// names an unknown spelling ahead of a repetition; the scan tier only
		// sees repetitions. Both judgments pre-date the value decode, as they
		// always did.
		if allowed != nil && !allowed[key] {
			return nil, corruptf("%s has unknown field %q", positional, key)
		}
		if seen[key] {
			return nil, corruptf("%s contains duplicate field %q", positional, key)
		}
		seen[key] = true
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, corruptf("%s is not valid JSON: %v", positional, err)
		}
		members = append(members, circuitMember{key: key, value: value})
	}
	if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim('}') {
		return nil, corruptf("%s is not valid JSON", positional)
	}
	return members, nil
}

// scanCircuitArray consumes one "circuits" value positioned at its opening
// bracket and reads every record through the shared per-record engine in
// scan mode (no allowed-field list), which names each structural problem or
// repeated key before the scan moves on. It is the directory-wide pre-check
// counterpart of decodeCircuit and exists so a structural problem inside one
// circuit is attributed to that record by its 1-based position rather than to
// an opaque "circuits" element index. A null array stays the pre-existing
// "no records" reading.
func scanCircuitArray(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return corruptf("data file envelope field \"circuits\" is not valid JSON: %v", err)
	}
	if tok == nil { // null: json.Unmarshal would produce no records
		return nil
	}
	if tok != json.Delim('[') {
		return corruptf("data file envelope field \"circuits\" must be an array")
	}
	index := 0
	for dec.More() {
		index++
		if _, err := readCircuitRecord(dec, fmt.Sprintf("circuit record #%d", index), nil); err != nil {
			return err
		}
	}
	if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim(']') {
		return corruptf("data file envelope field \"circuits\" is not valid JSON")
	}
	return nil
}

// Strict decoding of committed circuit records.
//
// A circuit record in data.json must name each of name, version, constraints,
// public_inputs, private_inputs, frozen and description exactly once, with
// the exact JSON type the writer emits (string / integer / integer / integer
// / integer / boolean / string). A missing key, a null, a wrong type or a
// repeated key is data corruption and refuses the whole directory read: a
// missing or null frozen flag must not silently read as false (turning a
// frozen version back into an editable draft), and a repeated key must not
// resolve to its last value. An explicit 0, false or "" is an ordinary value
// and stays legal. The definition member remains the one optional field: it
// may be absent or null (the legacy counts-only state, which still compiles
// no further than "constraint definition missing"); when present it goes
// through the strict persistDefinition decoder, keeping the constraint order,
// input layout and compiled hash of defined versions intact.
//
// The name field carries one rule beyond shape: its raw JSON string token
// must denote its value exactly — valid UTF-8 in the literal portions and no
// unpaired \uXXXX surrogate escapes (checkStoredStringEncoding). Both damage
// shapes decode to U+FFFD replacement characters under encoding/json, which
// would rename the circuit on read; they are data corruption, and the whole
// directory read is refused. Only the name is judged this way; every other
// field keeps the ordinary decoding rules.
//
// Only shape is judged here. The domain rules (non-blank name, positive
// version and constraint count, non-negative input counts) keep being
// re-checked by validateEnvelope.

func (c *persistCircuit) UnmarshalJSON(raw []byte) error {
	const what = "stored circuit record"
	return decodeCircuit(raw, what, c)
}

// decodeCircuit reads one saved circuit record from raw through the shared
// engine and fills out. what is the generic name for a record located
// positionally; the envelope read prefixes it with the record index when it
// re-tags the error.
//
// Findings surface in the fixed order the two historical passes produced
// together: the object gate and trailing data, then the first unknown or
// repeated member in document order (an unknown spelling is named before a
// repetition at the same key position, exactly as the old strict member walk
// ordered it), then the field-by-field required/type checks with the name
// encoding rule right after the name string read, and only then the optional
// definition's own decode.
func decodeCircuit(raw []byte, what string, out *persistCircuit) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return corruptf("%s must be a JSON object", what)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	memberList, err := readCircuitRecord(dec, what, persistCircuitFields)
	if err != nil {
		return err
	}

	// A single complete object is the whole record. Unknown and repeated
	// members were already rejected during the single tokenization above, in
	// the old strict-member order (unknown before repetition, in document
	// order); this trailing-data check follows that gate, exactly as before.
	// At open the directory scan already guarantees it; the check keeps the
	// single-record decoder self-contained.
	if _, err := dec.Token(); err != io.EOF {
		return corruptf("%s is not valid JSON: trailing data after the object", what)
	}

	var c persistCircuit

	// The members are the allowed spellings without repeats, so the strict
	// required/scalar reads can address them by name.
	members := make(map[string]json.RawMessage, len(memberList))
	for _, m := range memberList {
		members[m.key] = m.value
	}

	requireString := func(key string, dst *string) error {
		v, err := storedRecordShape.require(members, key, what)
		if err != nil {
			return err
		}
		s, err := storedRecordShape.string(v, what+" field "+strconv.Quote(key))
		if err != nil {
			return err
		}
		*dst = s
		return nil
	}
	requireInt := func(key string, dst *int) error {
		v, err := storedRecordShape.require(members, key, what)
		if err != nil {
			return err
		}
		n, err := storedRecordShape.int(v, what+" field "+strconv.Quote(key))
		if err != nil {
			return err
		}
		*dst = n
		return nil
	}

	if err := requireString("name", &c.Name); err != nil {
		return err
	}
	// The name is the version's identity and must read back as the exact
	// value that was committed. encoding/json silently rewrites both invalid
	// UTF-8 bytes and unpaired \uXXXX surrogate escapes to U+FFFD, so the
	// decoded string alone cannot tell a damaged name apart from one that
	// legitimately contains "�". Judge the raw JSON token instead.
	if err := checkStoredStringEncoding(members["name"], what+` field "name"`, "committed name"); err != nil {
		return err
	}
	if err := requireInt("version", &c.Version); err != nil {
		return err
	}
	if err := requireInt("constraints", &c.Constraints); err != nil {
		return err
	}
	if err := requireInt("public_inputs", &c.PublicInputs); err != nil {
		return err
	}
	if err := requireInt("private_inputs", &c.PrivateInputs); err != nil {
		return err
	}
	frozenRaw, err := storedRecordShape.require(members, "frozen", what)
	if err != nil {
		return err
	}
	c.Frozen, err = storedRecordShape.bool(frozenRaw, what+" field "+strconv.Quote("frozen"))
	if err != nil {
		return err
	}
	if err := requireString("description", &c.Description); err != nil {
		return err
	}

	// definition is the only optional member: absent or null both mean the
	// counts-only legacy state.
	if defRaw, present := members["definition"]; present {
		if string(bytes.TrimSpace(defRaw)) != "null" {
			var def persistDefinition
			if err := json.Unmarshal(defRaw, &def); err != nil {
				var se StoreError
				if errors.As(err, &se) {
					return err // already reported as data corruption
				}
				return corruptf("%s field %s is not a valid constraint definition: %v", what, strconv.Quote("definition"), err)
			}
			c.Definition = &def
		}
	}

	*out = c
	return nil
}

// checkStoredStringEncoding enforces an identity field's exact-readback rule
// on the raw JSON string token of a committed value (a circuit record's name,
// a job record's id). The string has already been decoded by the strict
// member read, so the token is known to be one complete, well-formed JSON
// string; this walk judges only what that decode silently repairs:
//
//   - invalid UTF-8 bytes in the literal (unescaped) portions — a lone
//     continuation byte, a truncated multi-byte character, an overlong
//     form — which encoding/json rewrites to U+FFFD, and
//   - \uXXXX escapes forming an unpaired surrogate — a high surrogate not
//     immediately followed by its low-surrogate escape, or a low surrogate
//     standing alone — which are rewritten to U+FFFD the same way.
//
// Both would make the record read back under a different value than the bytes
// on disk carry, so the record is data corruption rather than a value that
// legitimately decodes to a replacement character it never had. noun is the
// word naming the value in diagnostics ("committed name" / "committed job
// id"); what locates the record and field, e.g.
// `stored circuit record field "name"` or `stored job record "j1" field
// "id"`. A legal surrogate pair decodes to its astral character, an actually
// committed "�" (written directly or as a U+FFFD escape) is an ordinary
// value, and the literal six characters "\uD800" — written with an escaped
// backslash — carry no escape at all: all three keep their exact value.
func checkStoredStringEncoding(raw json.RawMessage, what, noun string) error {
	token := bytes.TrimSpace(raw)
	// The token is a complete JSON string (the strict string read above
	// succeeded), so every escape is well-formed and the walk stays in
	// bounds; only the replacement-prone content is judged.
	for i := 1; i < len(token)-1; {
		c := token[i]
		if c == '\\' {
			if token[i+1] != 'u' {
				i += 2 // a simple escape: \" \\ \/ \b \f \n \r \t
				continue
			}
			code, _ := strconv.ParseUint(string(token[i+2:i+6]), 16, 32)
			switch {
			case code >= 0xD800 && code <= 0xDBFF:
				// A high surrogate is whole only when its low-surrogate
				// escape follows immediately.
				if i+12 <= len(token) && token[i+6] == '\\' && token[i+7] == 'u' {
					lo, _ := strconv.ParseUint(string(token[i+8:i+12]), 16, 32)
					if lo >= 0xDC00 && lo <= 0xDFFF {
						i += 12
						continue
					}
				}
				return corruptf("%s contains an unpaired high surrogate escape (\\u%04X); the %s would not read back unchanged", what, code, noun)
			case code >= 0xDC00 && code <= 0xDFFF:
				return corruptf("%s contains an unpaired low surrogate escape (\\u%04X); the %s would not read back unchanged", what, code, noun)
			}
			i += 6
			continue
		}
		if c < utf8.RuneSelf {
			i++
			continue
		}
		r, size := utf8.DecodeRune(token[i:])
		if r == utf8.RuneError && size == 1 {
			return corruptf("%s contains invalid UTF-8 bytes; the %s would not read back unchanged", what, noun)
		}
		i += size
	}
	return nil
}
