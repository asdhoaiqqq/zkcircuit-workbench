package zkcircuit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// Saved circuit records used to be read and field-checked twice: once while
// the committed envelope was tokenized to reject a repeated object key
// (scanCircuitArray) and again while each circuits element was decoded into a
// persistCircuit (persistCircuit.UnmarshalJSON). Each pass walked the record's
// members on its own, recognized the record object on its own and carried its
// own copy of the repeated-key rule, so the judgment of what makes one stored
// circuit record structurally sound lived in two places that had to agree by
// construction.
//
// This file is now the single home of a saved circuit record's reading rules:
// the persistCircuit shape, its accepted member set, the name exact-readback
// rule and one per-record reading engine (readCircuitRecord) both read passes
// drive. The directory scan and the single-record decode hence tokenize one
// record once and recognize its object and member keys through the same code,
// which also covers two byte-identical member keys — a case a post-decode
// member map could not see on its own.
//
// Only the reading of saved circuit records is consolidated here. The public
// behavior — which values load, which are refused, how the failure is located
// and in what order findings surface — is unchanged; the commit and query
// entry points and the on-disk format stay as they were. In particular the two
// read passes keep their historical division of labor and precedence because
// an open always runs the directory scan before the typed decode: the scan
// owns structural object recognition and exact repeated-key findings
// (circuitRecordScan.judgeScan, located by the record's 1-based position),
// while the typed decode owns the known-field set, the required fields and
// their types, the name exact-readback rule and the nested definition
// (decodeCircuit), naming the record as a "stored circuit record" before the
// envelope reader adds its position. The definition's own strict decoder and
// the domain checks in validateEnvelope are likewise untouched.

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

// persistCircuitFields are the eight members a stored circuit record may
// carry; keys reach the reader already JSON-unescaped, so a key written with a
// \uXXXX escape is judged on its decoded spelling too.
var persistCircuitFields = []string{
	"name", "version", "constraints", "public_inputs", "private_inputs",
	"frozen", "description", "definition",
}

// circuitMember is one member of a saved circuit record as seen during the
// single per-record pass: its already-unescaped key and its raw value.
type circuitMember struct {
	key   string
	value json.RawMessage
}

// circuitRecordScan is the one read of one saved circuit record. Both read
// passes — the envelope-wide duplicate-key scan (scanCircuitArray) and the
// typed decode (decodeCircuit) — build one through readCircuitRecord. It
// records only what the shared structural rule needs: the members in document
// order and the first member key repeated under that same exact spelling.
// Turning the members into a typed persistCircuit is left to decodeCircuit,
// which runs once afterwards.
type circuitRecordScan struct {
	positional string
	members    []circuitMember
	dupKey     string
}

// memberMap views the single read's members as a lookup for the typed decode.
// The repeated-key check has already passed by the time this is used, so no
// member name collides and every key maps to its one value.
func (r *circuitRecordScan) memberMap() map[string]json.RawMessage {
	members := make(map[string]json.RawMessage, len(r.members))
	for _, m := range r.members {
		members[m.key] = m.value
	}
	return members
}

// readCircuitRecord consumes exactly one circuit record value the decoder is
// positioned at — including its opening token — and returns the single scan.
// Every token is consumed whether or not the record is damaged. A null
// element or a non-object element is structural damage named by positional.
// positional is the name used to locate the record: the directory scan passes
// "circuit record #<1-based index>", the typed decode passes
// "stored circuit record".
//
// Member values are consumed whole and never descended into: a nested object
// (the constraint definition) owns its own, more precise strict decoder. Keys
// are compared after JSON unescaping (the decoder already unescapes the key
// token), so a key repeated through a \uXXXX spelling is a duplicate even when
// both values are byte-identical; the first such key in document order is
// remembered so the finding never depends on which value an
// encoding/json-style last-value-wins decode would have kept.
func readCircuitRecord(dec *json.Decoder, positional string) (*circuitRecordScan, error) {
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

	r := &circuitRecordScan{positional: positional}
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
		if seen[key] && r.dupKey == "" {
			r.dupKey = key
		}
		seen[key] = true
		// Consume the whole value without descending: nested objects
		// (definition) own their own strict duplicate checks. A value that
		// fails to parse after an earlier repeated key never outranks that
		// repeated-key finding.
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			if r.dupKey != "" {
				return nil, corruptf("%s contains duplicate field %q", positional, r.dupKey)
			}
			return nil, corruptf("%s is not valid JSON: %v", positional, err)
		}
		r.members = append(r.members, circuitMember{key: key, value: value})
	}
	if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim('}') {
		return nil, corruptf("%s is not valid JSON", positional)
	}
	return r, nil
}

// judgeScan applies the structural finding the envelope-wide pre-check owns
// for one circuit record: an exact repeated member key (the first such key in
// document order, including one repeated through a JSON escape or carrying an
// identical value). It is deliberately all the scan judges — the known-field
// set, required fields and value types belong to the typed decode, which runs
// after the scan and keeps its own wording and precedence.
func (r *circuitRecordScan) judgeScan() error {
	if r.dupKey != "" {
		return corruptf("%s contains duplicate field %q", r.positional, r.dupKey)
	}
	return nil
}

// scanCircuitArray consumes one "circuits" value positioned at its opening
// bracket and reads every record through the shared per-record engine, judging
// each before moving on. It is the directory-read counterpart of decodeCircuit
// and exists so a structural problem inside one circuit record is attributed
// to that record by its 1-based position rather than to an opaque "circuits"
// element index. A null array stays the pre-existing "no records" reading.
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
		r, err := readCircuitRecord(dec, fmt.Sprintf("circuit record #%d", index))
		if err != nil {
			return err
		}
		if err := r.judgeScan(); err != nil {
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
// public_inputs, private_inputs, frozen and description exactly once, with the
// exact JSON type the writer emits (string / integer / integer / integer /
// integer / boolean / string). A missing key, a null, a wrong type or a
// repeated key is data corruption and refuses the whole directory read: a
// missing or null frozen flag must not silently read as false (turning a
// frozen version back into an editable draft), and a repeated key must not
// resolve to its last value. An explicit 0, false or "" is an ordinary value
// and stays legal. The definition member remains the one optional field: it
// may be absent or null (the legacy counts-only state); when present it goes
// through the strict persistDefinition decoder.
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
// engine and fills out. what is the name the typed decode locates the record
// under ("stored circuit record"); the envelope reader prefixes the record's
// position around whatever this returns.
func decodeCircuit(raw []byte, what string, out *persistCircuit) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return corruptf("%s must be a JSON object", what)
	}
	if string(trimmed) == "null" {
		return corruptf("%s must be a JSON object, not null", what)
	}
	if trimmed[0] != '{' {
		return corruptf("%s must be a JSON object", what)
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	r, err := readCircuitRecord(dec, what)
	if err != nil {
		return err
	}

	// The typed decode owns the field set. Walk the one shared member list in
	// document order so an unknown member and a repeated member surface in
	// exactly the precedence the old strict member walk had: each member's
	// unknown-key check precedes its own repeated-key check, and the first
	// offending member in document order wins. The envelope-wide scan already
	// enforced the repeated-key rule in its own right before this decode runs;
	// the pass stays self-contained for the single-record path.
	allowed := make(map[string]bool, len(persistCircuitFields))
	for _, f := range persistCircuitFields {
		allowed[f] = true
	}
	seen := make(map[string]bool)
	for _, m := range r.members {
		if !allowed[m.key] {
			return corruptf("%s has unknown field %q", what, m.key)
		}
		if seen[m.key] {
			return corruptf("%s contains duplicate field %q", what, m.key)
		}
		seen[m.key] = true
	}
	// One complete object is the whole record, checked after the member walk
	// (the old strict member walk reported member damage ahead of trailing
	// data). At open this is already guaranteed by the directory scan; the
	// check keeps the single-record decode self-contained.
	if _, err := dec.Token(); err != io.EOF {
		return corruptf("%s is not valid JSON: trailing data after the object", what)
	}

	members := r.memberMap()
	var c0 persistCircuit

	requireString := func(key string, dst *string) error {
		raw, err := storedRecordShape.require(members, key, what)
		if err != nil {
			return err
		}
		v, err := storedRecordShape.string(raw, what+" field "+strconv.Quote(key))
		if err != nil {
			return err
		}
		*dst = v
		return nil
	}
	requireInt := func(key string, dst *int) error {
		raw, err := storedRecordShape.require(members, key, what)
		if err != nil {
			return err
		}
		v, err := storedRecordShape.int(raw, what+" field "+strconv.Quote(key))
		if err != nil {
			return err
		}
		*dst = v
		return nil
	}

	if err := requireString("name", &c0.Name); err != nil {
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
	if err := requireInt("version", &c0.Version); err != nil {
		return err
	}
	if err := requireInt("constraints", &c0.Constraints); err != nil {
		return err
	}
	if err := requireInt("public_inputs", &c0.PublicInputs); err != nil {
		return err
	}
	if err := requireInt("private_inputs", &c0.PrivateInputs); err != nil {
		return err
	}
	frozenRaw, err := storedRecordShape.require(members, "frozen", what)
	if err != nil {
		return err
	}
	c0.Frozen, err = storedRecordShape.bool(frozenRaw, what+" field "+strconv.Quote("frozen"))
	if err != nil {
		return err
	}
	if err := requireString("description", &c0.Description); err != nil {
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
			c0.Definition = &def
		}
	}

	*out = c0
	return nil
}
