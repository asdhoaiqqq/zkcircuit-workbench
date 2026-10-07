package zkcircuit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Reading rules for a saved trusted-setup record.
//
// A setup belongs exclusively to the frozen circuit version named in the
// record: the "name" is the sole authority for which circuit it belongs to
// and the "version" is the sole authority for which version of that circuit.
// This file gives both read passes one per-record judge for the envelope's
// "setups" array — scanSetupArray while the committed envelope is tokenized
// ahead of the typed decode, and persistSetup.UnmarshalJSON (decodeSetup) on
// the typed decode itself, mirroring the job record's shared engine — so that
// every spelling the ordinary struct decode would fill Name or Version from
// counts as that one field and may appear at most once in a record.
//
// Keys reach every rule below already JSON-unescaped, so a key written with a
// \uXXXX escape is judged on its decoded spelling too. Only the name and
// version fields get a uniqueness rule; a setup record carries no other
// member.

type persistSetup struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// The canonical wire spellings of the two fields a setup record treats by
// rule rather than leaving entirely to a plain struct decode.
const (
	setupNameKey    = "name"
	setupVersionKey = "version"
)

// isSetupNameKey reports whether key names a setup record's circuit name
// field under any spelling the ordinary decode fills Name from: canonical
// "name" or an ASCII letter-case variant such as "NAME" or "NaMe", including
// a JSON-escaped spelling that unescapes onto either (keys arrive here
// already unescaped). No non-ASCII rune case-folds onto any letter of
// "name", so the ASCII fold and encoding/json's Unicode-folded tag matching
// recognize exactly the same keys for this field.
func isSetupNameKey(key string) bool {
	return isASCIIFoldOf(key, setupNameKey)
}

// isSetupVersionKey reports whether key names a setup record's version field
// under any spelling the ordinary struct decode fills Version from: canonical
// "version", an ASCII letter-case variant such as "VERSION" or "VeRsIoN", or a
// JSON-escaped spelling that unescapes onto either. Because encoding/json's
// struct-tag matching folds with Unicode case rules (strings.EqualFold), the
// long-s "verſion" (U+017F, written directly or as the u017f escape) names
// the field too.
func isSetupVersionKey(key string) bool {
	return strings.EqualFold(key, setupVersionKey)
}

// rejectAmbiguousSetupName enforces the owning-circuit-name uniqueness rule on
// one saved setup record: the name decides which frozen circuit the setup
// belongs to, so one record may carry the field under at most one recognized
// spelling (isSetupNameKey) — canonical "name", an ASCII letter-case variant
// such as "NAME" or "NaMe", or a JSON-escaped spelling that unescapes onto
// either. Two spellings on one record make the owning circuit depend on key
// order, since encoding/json keeps the last value, and corrupt the whole
// directory read: the record is refused whether the two values agree or
// differ (a setup registered only for "a" whose record also spells the name
// "b" is exactly the dangerous case — the last value would query b as having
// a setup and let a proof job for b pass the setup gate), whether either
// named circuit exists or is frozen, whether the two name fields are adjacent
// or unrelated members such as the version sit between them, and in whichever
// key order they appear. A record carrying only case variants with no
// lowercase "name" ("NAME" beside "NaMe", …) is refused on the same terms —
// the pair still names one field twice — and two byte-identical spellings are
// covered as well. A single spelling of any recognized kind is the one name
// field and keeps its current reading.
//
// keys are the record's member keys in any order; the recognized spellings
// are filtered and sorted here so the message is deterministic. where names
// the record positionally ("setup record #<1-based index>"): the record
// refuses to settle which circuit it is for, so the finding is never
// attributed to either candidate value.
func rejectAmbiguousSetupName(where string, keys []string) error {
	var nameKeys []string
	for _, key := range keys {
		if isSetupNameKey(key) {
			nameKeys = append(nameKeys, key)
		}
	}
	if len(nameKeys) <= 1 {
		return nil
	}
	sort.Strings(nameKeys)
	quoted := make([]string, len(nameKeys))
	for i, k := range nameKeys {
		quoted[i] = strconv.Quote(k)
	}
	return corruptf("%s carries the circuit name field %q more than once (as %s); the circuit a trusted setup belongs to must not depend on field order",
		where, setupNameKey, strings.Join(quoted, ", "))
}

// rejectAmbiguousSetupVersion enforces the version uniqueness rule on one
// saved setup record: the version decides which frozen circuit version the
// setup belongs to, so one record may carry the field under at most one
// recognized spelling (isSetupVersionKey) — canonical "version", an ASCII
// letter-case variant, a JSON-escaped spelling, or the Unicode-folded long-s
// "verſion" (U+017F). Two spellings on one record make the bound version
// depend on key order, since encoding/json keeps the last value, and corrupt
// the whole directory read: the record is refused whether the two values agree
// or differ (both 1 and 2 frozen with setups is exactly the dangerous case),
// whether both are legal versions, whether unrelated members such as the name
// sit between them, and in whichever key order they appear. A record carrying
// only case variants with no lowercase "version" is refused on the same terms,
// and two byte-identical spellings are covered as well. A single spelling of
// any recognized kind — including a lone long-s "verſion" — is the one
// version field and keeps its current reading.
//
// keys are the record's member keys in any order; the recognized spellings are
// filtered and sorted here so the message is deterministic. where names the
// record positionally ("setup record #<1-based index>"): the record refuses to
// settle which version it is, so the finding is never attributed to either
// candidate value.
func rejectAmbiguousSetupVersion(where string, keys []string) error {
	var versionKeys []string
	for _, key := range keys {
		if isSetupVersionKey(key) {
			versionKeys = append(versionKeys, key)
		}
	}
	if len(versionKeys) <= 1 {
		return nil
	}
	sort.Strings(versionKeys)
	quoted := make([]string, len(versionKeys))
	for i, k := range versionKeys {
		quoted[i] = strconv.Quote(k)
	}
	return corruptf("%s carries the version field %q more than once (as %s); the circuit version a trusted setup belongs to must not depend on field order",
		where, setupVersionKey, strings.Join(quoted, ", "))
}

// scanSetupArray consumes one "setups" value positioned at its opening bracket
// and reads every record through its own per-record engine, naming each
// structural problem, repeated member key or ambiguous name/version field
// before the scan moves on. It is the setup counterpart of scanJobArray and
// scanCircuitArray: a problem inside one setup is attributed to that record by
// its 1-based position ("setup record #n") rather than to an opaque "setups"
// element index. A null array stays the pre-existing "no setups" reading.
func scanSetupArray(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return corruptf("data file envelope field \"setups\" is not valid JSON: %v", err)
	}
	if tok == nil { // null: json.Unmarshal would produce no setups
		return nil
	}
	if tok != json.Delim('[') {
		return corruptf("data file envelope field \"setups\" must be an array")
	}
	index := 0
	for dec.More() {
		index++
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return corruptf("data file envelope field \"setups\" is not valid JSON: %v", err)
		}
		positional := fmt.Sprintf("setup record #%d", index)
		if err := judgeSetupRecord(raw, positional, index); err != nil {
			return err
		}
	}
	if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim(']') {
		return corruptf("data file envelope field \"setups\" is not valid JSON")
	}
	return nil
}

// setupRecordScan is the one read of one saved setup record. Both read
// passes — the envelope scan (scanSetupArray) and the typed decode
// (decodeSetup) — build one through readSetupRecord. It records only what the
// shared rules need: the members in document order (exact spelling), the
// first member key repeated under that same exact spelling, and any duplicate
// key nested inside a member's own object/array value. Turning the members
// into a typed persistSetup is left to the ordinary struct decode, which
// runs once afterwards.
type setupRecordScan struct {
	positional string
	members    []setupMember
	dupKey     string
	nestedErr  error
}

// setupMember is one member of a saved setup record as seen during the
// single per-record pass: its already-unescaped key and its raw value.
type setupMember struct {
	key   string
	value json.RawMessage
}

func (r *setupRecordScan) memberKeys() []string {
	keys := make([]string, 0, len(r.members))
	for _, m := range r.members {
		keys = append(keys, m.key)
	}
	return keys
}

// readSetupRecord consumes exactly one setup record value the decoder is
// positioned at — including its opening token — and returns the single scan.
// Every token is consumed whether or not the record is damaged. A null
// element or a non-object element is structural damage named by positional.
// positional names the record the way the calling pass locates it: the
// directory scan passes "setup record #<1-based index>", the typed decode
// passes "stored setup record".
func readSetupRecord(dec *json.Decoder, positional string) (*setupRecordScan, error) {
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

	r := &setupRecordScan{positional: positional}
	seenExact := make(map[string]bool)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, corruptf("%s is not valid JSON: %v", positional, err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, corruptf("%s is not valid JSON", positional)
		}
		// Consume the whole value first; the judgments below use only the key.
		// Setup members are scalars as written, but a hand-edited member may
		// carry an object/array whose own duplicate keys must still fail the
		// read.
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, corruptf("%s is not valid JSON: %v", positional, err)
		}
		// The name and version ambiguities outrank an exact repetition of
		// another member, matching the job record precedence; the first exact
		// repeated key is remembered for the subsequent finding.
		if seenExact[key] && r.dupKey == "" {
			r.dupKey = key
		}
		seenExact[key] = true
		r.members = append(r.members, setupMember{key: key, value: value})
		v := bytes.TrimSpace(value)
		if r.nestedErr == nil && len(v) > 0 && (v[0] == '{' || v[0] == '[') {
			r.nestedErr = skipValueWithDupKeys(json.NewDecoder(bytes.NewReader(v)),
				positional+" field "+strconv.Quote(key))
		}
	}
	if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim('}') {
		return nil, corruptf("%s is not valid JSON", positional)
	}
	return r, nil
}

// judge applies the findings the record owns, in the order they surface:
//
//  1. two recognized spellings of the circuit name field — equal or differing
//     values, naming an existing/frozen circuit or not, even with unrelated
//     members interleaved and even with no lowercase "name" among them — so
//     the owning circuit is never chosen by field order; this finding names
//     the record positionally (positional), never by a candidate name;
//  2. two recognized spellings of the version field, under the same rule;
//  3. an exact repeated member key (the first such key in document order,
//     including two byte-identical keys a member map could not retain);
//  4. a duplicate key nested inside a member's own object/array value.
func (r *setupRecordScan) judge() error {
	keys := r.memberKeys()
	if err := rejectAmbiguousSetupName(r.positional, keys); err != nil {
		return err
	}
	if err := rejectAmbiguousSetupVersion(r.positional, keys); err != nil {
		return err
	}
	if r.dupKey != "" {
		return corruptf("%s contains duplicate field %q", r.positional, r.dupKey)
	}
	if r.nestedErr != nil {
		return r.nestedErr
	}
	return nil
}

// judgeSetupRecord applies the scan-owned rules to one setups element already
// captured whole, the way the envelope-wide scan drives them. elementIndex
// names nested container elements the same way the envelope-wide recursive
// scanner did ("setups" element #n).
//
// A null or scalar element (a number, a string, …) is structurally legal JSON
// and stays left to the typed decode and validateEnvelope, which already
// reject it; only object and array elements carry member keys to judge. An
// array element can never type-decode and keeps the recursive duplicate-key
// walk with its historical naming; an object element goes through the same
// shared per-record engine (readSetupRecord) the typed decode uses.
func judgeSetupRecord(raw json.RawMessage, positional string, elementIndex int) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return corruptf("%s is not valid JSON", positional)
	}
	switch trimmed[0] {
	case '{':
		// Decoded through the shared engine below.
	case '[':
		// An array where a record should be can never type-decode; keep the
		// recursive duplicate-key walk with its historical naming.
		what := fmt.Sprintf("data file envelope field \"setups\" element #%d", elementIndex)
		return skipValueWithDupKeys(json.NewDecoder(bytes.NewReader(trimmed)), what)
	default:
		return nil // null or a scalar: typing/validation rejects it as before
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	r, err := readSetupRecord(dec, positional)
	if err != nil {
		return err
	}
	return r.judge()
}

// Strict decoding of a committed trusted-setup record.
//
// A setup belongs exclusively to the frozen circuit version named in the
// record: the name is the sole authority for which circuit it is for and the
// version for which version of that circuit. Each is therefore held to its
// own uniqueness rule, and every spelling the ordinary struct decode would
// fill either field from counts as that one field:
//
//   - rejectAmbiguousSetupName: canonical "name" and ASCII letter-case
//     variants such as "NAME" or "NaMe", JSON-escaped spellings included
//     (isSetupNameKey). "name" has no non-ASCII case fold, so those are
//     exactly the recognized spellings. Two on one record are refused as data
//     corruption whether the values agree or differ, whether either named
//     circuit exists or is frozen, even when the version or another member is
//     interleaved between them, and even when neither spelling is lowercase
//     "name" — the owning circuit must never be resolved by key order.
//   - rejectAmbiguousSetupVersion: canonical "version", an ASCII case variant
//     or a JSON-escaped spelling, and, since struct-tag matching folds with
//     Unicode case rules, the long-s "verſion" (U+017F, direct or
//     u017f-escaped), under the same uniqueness rule.
//
// A single recognized spelling of either field keeps the current reading, and
// its value keeps matching exactly as written — circuit names are not case
// folded, trimmed or otherwise altered, so "a" and "A" stay distinct. An
// illegal single value (a missing field, a string where the version must be a
// number, null, …) is rejected exactly as before, by the ordinary decode or
// validateEnvelope.
//
// The shared readSetupRecord scan enforces every key and structural rule once
// for both read passes; after it clears, the ordinary struct decode fills
// this record exactly as it did before.
func (p *persistSetup) UnmarshalJSON(raw []byte) error {
	const what = "stored setup record"
	return decodeSetup(raw, what, p)
}

// decodeSetup reads one saved setup record from raw through the shared engine
// and fills out. what is the generic name for a record located positionally;
// the envelope read has already enforced every rule here through the
// directory scan, so during an open this decoder only re-confirms them — it
// keeps the single-record decode self-contained the way decodeJob does.
func decodeSetup(raw []byte, what string, out *persistSetup) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return corruptf("%s must be a JSON object", what)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	r, err := readSetupRecord(dec, what)
	if err != nil {
		return err
	}
	// A single complete object is the whole record. At open this is already
	// guaranteed by the directory scan; the check keeps this decoder
	// self-contained.
	if _, err := dec.Token(); err != io.EOF {
		return corruptf("%s is not valid JSON: trailing data after the object", what)
	}
	if err := r.judge(); err != nil {
		return err
	}
	// With at most one recognized spelling of each field, the ordinary decode
	// already yields the current value: a single canonical/ASCII/long-s
	// spelling folds onto the matching member, and a missing member reads as
	// the zero value and is rejected by validateEnvelope. Illegal single
	// values keep being rejected by that same decode or the validation, so
	// nothing is picked here.
	type plainSetup persistSetup
	var decoded plainSetup
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return corruptf("%s is not valid JSON: %v", what, err)
	}
	*out = persistSetup(decoded)
	return nil
}
