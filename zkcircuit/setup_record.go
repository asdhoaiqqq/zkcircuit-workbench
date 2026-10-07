package zkcircuit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Saved trusted-setup records carry exactly two fields — the circuit name and
// the version the setup belongs to — and a trusted setup may belong only to
// the circuit version named at registration. The version is the record's sole
// authority for which frozen circuit version the setup covers, so any
// spelling the ordinary read would fill Version from names that one field:
// the canonical "version", an ASCII letter-case variant such as "VERSION" or
// "VeRsIoN", a JSON-escaped spelling that unescapes onto either (keys reach
// every rule here already JSON-unescaped), and — because encoding/json's
// struct-tag matching folds with Unicode case rules (strings.EqualFold) —
// the long-s "verſion" (U+017F, written directly or as the u017f escape).
//
// Two such spellings on one record make the covered version a function of key
// order, since encoding/json keeps the last value: a record spelling version
// both 1 and 2 reads as the v2 setup, so a query for the v1 setup misses and
// a v2 prove job passes the setup check on a record that was never registered
// for it. Such a record is data corruption: the whole directory read is
// refused, whether the two values agree or differ, whether both are legal
// frozen versions with no other conflict, and whether unrelated members sit
// between the two spellings — neither value is ever picked to carry on, and
// swapping the keys never changes the refusal. A single recognized spelling
// of any kind — canonical, an ASCII case variant, or a lone long-s "verſion",
// direct or escaped — is the one version field and keeps its current reading
// (the ordinary decode folds it onto Version), so legal old records keep
// loading exactly as they did.
//
// Only the version field's uniqueness is tightened here. Every other reading
// rule stays as it was: unknown members keep being ignored by the ordinary
// struct decode, a missing or illegal version value keeps being rejected by
// that decode or the envelope validation, and an exact repeated key of any
// spelling keeps being a duplicate-field finding.

type persistSetup struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// setupVersionKey is the canonical spelling the diagnostics always quote,
// whatever accepted spelling a damaged record was written under.
const setupVersionKey = "version"

// isSetupVersionKey reports whether key names a setup record's version field
// under any spelling the ordinary decode fills Version from: canonical
// "version", an ASCII letter-case variant such as "VERSION" or "VeRsIoN", or
// a JSON-escaped spelling that unescapes onto either. Because encoding/json's
// struct-tag matching folds with Unicode case rules (strings.EqualFold), the
// long-s "verſion" (U+017F, written directly or as the u017f escape) names
// the field too.
func isSetupVersionKey(key string) bool {
	return strings.EqualFold(key, setupVersionKey)
}

// rejectAmbiguousSetupVersion enforces the version uniqueness rule of the
// directory scan (driven by scanSetupRecord): one setup record may carry the
// version field under at most one recognized spelling (isSetupVersionKey).
// Two spellings on one record — equal or differing values, both legal, two
// byte-identical spellings included, and even with unrelated members
// interleaved — make the covered version depend on key order and corrupt the
// read: the record is refused outright rather than resolved to either value.
//
// keys are the record's member keys in any order; the recognized spellings
// are filtered and sorted here so the message is deterministic. where names
// the record positionally ("setup record #<1-based index>").
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
	return corruptf("%s carries the version field %q more than once (as %s)",
		where, setupVersionKey, strings.Join(quoted, ", "))
}

// scanSetupRecord consumes exactly one setup record value the decoder is
// positioned at — including its opening token — and applies the directory
// scan's per-record findings: the version-field uniqueness rule above, then
// the pre-existing exact-duplicate-key and nested-duplicate-key checks the
// generic recursive walk used to own for these records. Every token is
// consumed unless the record is damaged. positional names the record by its
// 1-based position in the setups array ("setup record #<n>").
//
// A non-object element keeps its pre-existing reading exactly: null and
// scalars pass the scan (the typed decode or the envelope validation rejects
// them as before), and an array/object element is walked for nested duplicate
// keys the way the generic walk always did.
func scanSetupRecord(dec *json.Decoder, positional string) error {
	open, err := dec.Token()
	if err != nil {
		return corruptf("%s is not valid JSON: %v", positional, err)
	}
	if open != json.Delim('{') {
		return skipValueRest(dec, open, positional)
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
		// Consume the whole value first; the judgments below use only the
		// key. Setup members are scalars, but an unknown member may carry an
		// object/array whose own duplicate keys must still fail the read.
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return corruptf("%s is not valid JSON: %v", positional, err)
		}
		keys = append(keys, key)
		if seenExact[key] && dupKey == "" {
			dupKey = key
		}
		seenExact[key] = true
		trimmed := bytes.TrimSpace(value)
		if nestedErr == nil && len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
			nd := json.NewDecoder(bytes.NewReader(trimmed))
			nestedErr = skipValueWithDupKeys(nd, positional+" field "+strconv.Quote(key))
		}
	}
	if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim('}') {
		return corruptf("%s is not valid JSON", positional)
	}

	// The version uniqueness finding outranks the exact-duplicate and nested
	// findings: a record whose version cannot be settled is named for that
	// first, whatever else is wrong with it.
	if err := rejectAmbiguousSetupVersion(positional, keys); err != nil {
		return err
	}
	if dupKey != "" {
		return corruptf("%s contains duplicate field %q", positional, dupKey)
	}
	return nestedErr
}

// scanSetupArray consumes one "setups" value positioned at its opening token
// and reads every record through scanSetupRecord, so a version-field ambiguity
// inside one setup is attributed to that record by its 1-based position rather
// than surfacing as a silently misread version. A null array stays the
// pre-existing "no setups" reading, and a non-array value keeps the reading
// the generic recursive walk gave it (the typed decode owns the shape
// failure), exactly as before.
func scanSetupArray(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return corruptf("data file envelope field \"setups\" is not valid JSON: %v", err)
	}
	if tok != json.Delim('[') {
		return skipValueRest(dec, tok, "data file envelope field \"setups\"")
	}
	index := 0
	for dec.More() {
		index++
		if err := scanSetupRecord(dec, fmt.Sprintf("setup record #%d", index)); err != nil {
			return err
		}
	}
	if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim(']') {
		return corruptf("data file envelope field \"setups\" is not valid JSON")
	}
	return nil
}
