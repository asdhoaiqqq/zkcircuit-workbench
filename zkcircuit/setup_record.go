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
//
// Beyond uniqueness the one name value carries an exact-readback rule
// (checkStoredStringEncoding): its raw JSON string token must denote the
// owning circuit exactly. Invalid UTF-8 bytes in its literal portions, or
// \uXXXX escapes forming an unpaired surrogate, would be silently rewritten
// to U+FFFD by encoding/json, letting the setup read as belonging to a
// different circuit whose name genuinely contains "�"; such a record is data
// corruption and fails the whole read. The rule is judged in judgeSetupRecord,
// which both the directory scan and persistSetup's own typed decode drive.

type persistSetup struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// Strict decoding of a committed trusted-setup record, including the
// exact-readback rule on the circuit name.
//
// Beyond the name and version spelling-uniqueness rules (shared with the
// directory scan through judgeSetupRecord), the name value carries the same
// rule a circuit's own name and a job's circuit name carry: its raw JSON
// string token must denote the owning circuit exactly — valid UTF-8 in the
// literal portions and no unpaired \uXXXX surrogate escapes under every
// spelling isSetupNameKey recognizes (canonical "name", an ASCII letter-case
// variant such as "NAME", a JSON-escaped spelling included). encoding/json
// rewrites both damage shapes to U+FFFD, so without the check a setup whose
// saved name carried a trailing invalid byte or a lone \uD800 could read
// back as belonging to a different, genuinely existing frozen circuit whose
// name contains "�", and a proof job for that circuit would then pass the
// trusted-setup gate against a setup it never had. The owning circuit must be
// determined by the saved bytes alone, so such a record is data corruption:
// the read is refused — named by the record's 1-based position in the setups
// array and the name field — even when the repaired name exists and the
// version is frozen; no partial results are returned and the file is never
// rewritten. A legal Chinese or emoji value, an actually committed "�", a
// correctly paired surrogate and the literal six-character text \uD800 are
// all ordinary values matched exactly.
//
// The scan enforces all of these rules at open; this decoder keeps the
// single-record typed pass self-contained so the rule cannot be bypassed by
// reaching the struct decoder with another spelling the scan checked.
func (p *persistSetup) UnmarshalJSON(raw []byte) error {
	const what = "stored setup record"
	return decodeSetup(raw, what, p)
}

// decodeSetup reads one saved setup record from raw through the shared judge
// and then the ordinary struct decode. what is the generic positional name
// the envelope read prefixes with the record index.
func decodeSetup(raw json.RawMessage, what string, out *persistSetup) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return corruptf("%s must be a JSON object", what)
	}
	if err := judgeSetupRecord(trimmed, what); err != nil {
		return err
	}
	type plainSetup persistSetup
	var decoded plainSetup
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return corruptf("%s is not valid JSON: %v", what, err)
	}
	*out = persistSetup(decoded)
	return nil
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
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) > 0 && trimmed[0] == '[' {
			// An array where a record should be can never type-decode; keep the
			// recursive duplicate-key walk with its historical naming.
			what := fmt.Sprintf("data file envelope field \"setups\" element #%d", index)
			if err := skipValueWithDupKeys(json.NewDecoder(bytes.NewReader(trimmed)), what); err != nil {
				return err
			}
			continue
		}
		if err := judgeSetupRecord(trimmed, positional); err != nil {
			return err
		}
	}
	if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim(']') {
		return corruptf("data file envelope field \"setups\" is not valid JSON")
	}
	return nil
}

// judgeSetupRecord applies the scan-owned rules to one already-trimmed setups
// element. Both read passes drive it: the directory scan passes the element's
// 1-based position ("setup record #n") as positional; the typed decode passes
// "stored setup record".
//
// A null or scalar element (a number, a string, …) is structurally legal JSON
// and stays left to the typed decode and validateEnvelope, which already
// reject it; an array element is handled by scanSetupArray with its
// recursive duplicate-key walk and historical naming. For an object the
// shallow member keys drive the name and version uniqueness rules and the
// name exact-readback rule, and every member value — plus a nested array
// element — is still walked for its own duplicate keys, so the scan loses
// none of the envelope-wide repeated-key coverage it had through
// skipValueWithDupKeys.
func judgeSetupRecord(trimmed []byte, positional string) error {
	if len(trimmed) == 0 {
		return corruptf("%s is not valid JSON", positional)
	}
	if trimmed[0] != '{' {
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
	// The raw token of the first recognized name spelling; judged below with
	// the exact-readback rule. Two recognized spellings are already refused by
	// rejectAmbiguousSetupName (which outranks this check), so the first token
	// is the one name field when the record reaches that judgment. A non-string
	// value (null, a number, …) is left to the typed decode / validateEnvelope:
	// the encoding walk is defined only over one complete JSON string token.
	var nameTok json.RawMessage
	nameTokSeen := false
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
		// The name a setup carries is the sole authority for which frozen
		// circuit version it belongs to, and encoding/json silently rewrites
		// invalid UTF-8 bytes and unpaired \uXXXX surrogate escapes to U+FFFD.
		// Capture every recognized spelling's raw token (canonical "name", an
		// ASCII letter-case variant, and any JSON-escaped spelling of them) so
		// the judge can demand it read back unchanged; otherwise a damaged
		// value could collide with a circuit whose name genuinely contains
		// "�" and borrow that circuit's frozen version and trusted setup.
		if isSetupNameKey(key) && !nameTokSeen &&
			bytes.HasPrefix(bytes.TrimSpace(value), []byte{'"'}) {
			nameTok = value
			nameTokSeen = true
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
	if nameTokSeen {
		if err := checkStoredStringEncoding(nameTok,
			positional+` field "name"`, "committed setup circuit name"); err != nil {
			return err
		}
	}
	return nil
}
