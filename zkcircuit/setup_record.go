package zkcircuit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Reading rules for a saved trusted-setup record.
//
// A setup belongs exclusively to the frozen circuit version named in the
// record, and both members that pin that binding carry a uniqueness rule. The
// "version" is the sole authority for which version the setup is for, and the
// "name" is the sole authority for which circuit owns it: just as a doubled
// version could rebind the setup across two frozen versions of one circuit, a
// doubled circuit name (canonical "name" beside an ASCII case variant such as
// "NAME") would let encoding/json keep the last value and rebind a setup
// registered only for circuit a onto a likewise-frozen circuit b. This file
// gives the directory scan one per-record reader for the envelope's "setups"
// array (scanSetupArray) so that, just as for job records, every spelling the
// ordinary struct decode fills Version or Name from counts as that one field
// and may appear at most once in a record.
//
// Keys reach every rule below already JSON-unescaped, so a key written with a
// \uXXXX escape is judged on its decoded spelling too. Only the name and
// version fields get these uniqueness rules; a setup record's other members
// keep their prior reading behavior.

type persistSetup struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// setupNameKey is the canonical wire spelling of the field naming the circuit
// a setup belongs to.
const setupNameKey = "name"

// isSetupNameKey reports whether key names a setup record's owning-circuit
// field under any spelling the ordinary struct decode fills Name from:
// canonical "name" or an ASCII letter-case variant such as "NAME" or "NaMe",
// including a JSON-escaped spelling that unescapes onto either (keys arrive
// here already unescaped). No non-ASCII rune case-folds onto any letter of
// "name" under encoding/json's Unicode-folded tag matching either, so the
// ASCII fold recognizes exactly the keys that bind to the field.
func isSetupNameKey(key string) bool {
	return isASCIIFoldOf(key, setupNameKey)
}

// rejectAmbiguousSetupName enforces the owning-circuit-name uniqueness rule on
// one saved setup record: the name decides which circuit the setup belongs to,
// so one record may carry the field under at most one recognized spelling
// (isSetupNameKey) — canonical "name", an ASCII letter-case variant, or a
// JSON-escaped spelling that unescapes onto either. Two spellings on one record
// make the owning circuit depend on key order, since encoding/json keeps the
// last value, and corrupt the whole directory read: the record is refused
// whether the two values agree or differ (a setup registered only for a whose
// record also names b is exactly the dangerous case, with a and b both frozen
// at version 1), whether both name circuits that exist and are frozen, whether
// the two keys are adjacent or the version sits between them, and in whichever
// key order they appear. A record carrying only case variants with no
// lowercase "name" ("NAME" beside "Name", …) is refused on the same terms, and
// two byte-identical spellings are covered as well. A single spelling of any
// recognized kind is the one name field and keeps its current reading.
//
// keys are the record's member keys in any order; the recognized spellings are
// filtered and sorted here so the message is deterministic. where names the
// record positionally ("setup record #<1-based index>"): the record refuses to
// settle which circuit it belongs to, so the finding is never attributed to
// either candidate name.
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

// setupVersionKey is the canonical wire spelling of the field naming the
// circuit version a setup belongs to.
const setupVersionKey = "version"

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

// judgeSetupRecord applies the scan-owned rules to one setups element already
// captured whole. elementIndex names nested container elements the same way
// the envelope-wide recursive scanner did ("setups" element #n).
//
// A null or scalar element (a number, a string, …) is structurally legal JSON
// and stays left to the typed decode and validateEnvelope, which already
// reject it; only object and array elements carry member keys to judge. For an
// object the shallow member keys drive the name and version uniqueness rules,
// and every member value — plus a nested array element — is still walked for
// its own duplicate keys, so the scan loses none of the envelope-wide
// repeated-key coverage it had through skipValueWithDupKeys.
func judgeSetupRecord(raw json.RawMessage, positional string, elementIndex int) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return corruptf("%s is not valid JSON", positional)
	}
	switch trimmed[0] {
	case '{':
		// Decode below names findings as they fall through.
	case '[':
		// An array where a record should be can never type-decode; keep the
		// recursive duplicate-key walk with its historical naming.
		what := fmt.Sprintf("data file envelope field \"setups\" element #%d", elementIndex)
		return skipValueWithDupKeys(json.NewDecoder(bytes.NewReader(trimmed)), what)
	default:
		return nil // null or a scalar: typing/validation rejects it as before
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	open, err := dec.Token()
	if err != nil || open != json.Delim('{') {
		return corruptf("%s is not valid JSON", positional)
	}
	var keys []string
	seenExact := make(map[string]bool)
	dupKey := ""
	var nestedErr error
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return corruptf("%s is not valid JSON: %v", positional, err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return corruptf("%s is not valid JSON", positional)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return corruptf("%s is not valid JSON: %v", positional, err)
		}
		// The name and version ambiguities outrank an exact repetition of
		// another member, matching the job record precedence; the first exact
		// repeated key is remembered for the subsequent finding.
		if seenExact[key] && dupKey == "" {
			dupKey = key
		}
		seenExact[key] = true
		keys = append(keys, key)
		// Setup members are scalars as written, but a hand-edited member may
		// carry an object/array whose own duplicate keys must still fail the
		// directory read.
		v := bytes.TrimSpace(value)
		if nestedErr == nil && len(v) > 0 && (v[0] == '{' || v[0] == '[') {
			nestedErr = skipValueWithDupKeys(json.NewDecoder(bytes.NewReader(v)),
				positional+" field "+strconv.Quote(key))
		}
	}
	if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim('}') {
		return corruptf("%s is not valid JSON", positional)
	}
	if err := rejectAmbiguousSetupName(positional, keys); err != nil {
		return err
	}
	if err := rejectAmbiguousSetupVersion(positional, keys); err != nil {
		return err
	}
	if dupKey != "" {
		return corruptf("%s contains duplicate field %q", positional, dupKey)
	}
	if nestedErr != nil {
		return nestedErr
	}
	return nil
}
