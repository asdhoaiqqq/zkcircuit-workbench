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

// Saved proof-job records used to be read and field-checked twice: once while
// the committed envelope was tokenized to reject a repeated object key
// (scanJobArray) and again while each jobs element was decoded into a
// persistJob (persistJob.UnmarshalJSON). Each pass gathered the record's
// member keys on its own and carried its own copy of the pinned-version and
// compiled-binding spelling rules, so the rule for naming a job, pinning a
// version or reading the optional compiled hash lived in two places that had
// to agree by construction.
//
// This file is now the single home of a saved job's reading and validation
// rules: the persistJob shape, the accepted spellings of its named fields
// (id / circuit / version / compiled_hash), the id exact-readback rule, the
// owning-circuit and pinned-version uniqueness rules and the compiled-binding
// duplicate/lookalike/type rules, together with one per-record reading engine (readJobRecord) both read passes drive. The
// directory scan and the single-record decode hence tokenize one record once
// and judge its keys and structure through the same code, which also covers
// two byte-identical member keys — a case a post-decode member map could not
// see on its own.
//
// Only the reading of saved jobs is consolidated here. The public behavior —
// which values load, which are refused, how the failure is located and in
// what order findings surface — is unchanged; the commit and query entry
// points and the on-disk format stay as they were. In particular the two read
// passes keep their historical division of labor and precedence because an
// open always runs the directory scan before the typed decode: the scan owns
// structural findings, the pinned-version and owning-circuit ambiguities,
// exact repeated keys and nested repeated keys (jobRecordScan.judge), while
// the typed decode owns the
// binding-field lookalike, duplicate-binding and binding-value-type findings
// plus the ordinary struct decode (decodeJob).

type persistJob struct {
	ID       string `json:"id"`
	Circuit  string `json:"circuit"`
	Version  int    `json:"version"`
	Kind     string `json:"kind"`
	Attempt  int    `json:"attempt"`
	Artifact string `json:"artifact,omitempty"`
	// CompiledHash is the optional binding to the pinned version's compiled
	// artifact, stored exactly as submitted.
	CompiledHash string `json:"compiled_hash,omitempty"`
}

// The canonical spellings of the job-record fields the read path treats by
// rule rather than leaving entirely to a plain struct decode. Keys reach
// every rule below already JSON-unescaped, so a key written with a \uXXXX
// escape is judged on its decoded spelling too.
const (
	jobIDKey        = "id"
	jobCircuitKey   = "circuit"
	jobVersionKey   = "version"
	compiledHashKey = "compiled_hash"
)

// isASCIIFoldOf reports whether key names canonical under an ASCII-only
// letter-case fold: the two must have the same byte length and every byte
// must match after mapping A-Z onto a-z. A key of a different length, or one
// that needs a non-ASCII rune to match, returns false — use strings.EqualFold
// to detect those non-ASCII lookalikes separately.
func isASCIIFoldOf(key, canonical string) bool {
	if len(key) != len(canonical) {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != canonical[i] {
			return false
		}
	}
	return true
}

// isJobVersionKey reports whether key names a job record's pinned circuit
// version field under any spelling the ordinary decode fills Version from:
// canonical "version", an ASCII letter-case variant such as "VERSION" or
// "VeRsIoN", or a JSON-escaped spelling that unescapes onto either. Because
// encoding/json's struct-tag matching folds with Unicode case rules
// (strings.EqualFold), the long-s "verſion" (U+017F, written directly or as
// the u017f escape) names the field too.
func isJobVersionKey(key string) bool {
	return strings.EqualFold(key, jobVersionKey)
}

// isJobCircuitKey reports whether key names a job record's owning circuit
// field under any spelling the ordinary decode fills Circuit from: canonical
// "circuit" or an ASCII letter-case variant such as "CIRCUIT" or "CiRcUiT",
// including a JSON-escaped spelling that unescapes onto either (keys arrive
// here already unescaped). No non-ASCII rune case-folds onto any letter of
// "circuit", so the ASCII fold and encoding/json's Unicode-folded tag
// matching recognize exactly the same keys for this field.
func isJobCircuitKey(key string) bool {
	return isASCIIFoldOf(key, jobCircuitKey)
}

// isCompiledHashKey reports whether key is an accepted spelling of the
// compiled-artifact binding field: only "compiled_hash" itself and ASCII
// letter-case variants such as "COMPILED_HASH" or "CoMpIlEd_HaSh". Non-ASCII
// letters are never folded here; compiledHashLookalike rejects those.
func isCompiledHashKey(key string) bool {
	return isASCIIFoldOf(key, compiledHashKey)
}

// rejectAmbiguousJobVersion enforces the pinned-version uniqueness rule shared
// by the directory scan and the single-record decode (both drive readJobRecord):
// the version a job writes is the sole authority for which frozen circuit — and
// therefore which trusted setup and compiled artifact — the job belongs to, so
// one record may carry the field under at most one recognized spelling
// (isJobVersionKey). Two spellings on one record make the bound version depend
// on key order, since encoding/json keeps the last value, and corrupt the read:
// the whole record is refused whether the values agree or differ, whether both
// are legal versions, and whether unrelated members sit between them — neither
// value is ever picked to carry on. A single spelling of any recognized kind —
// including a lone long-s "verſion" — is the one version field and keeps its
// current reading.
//
// keys are the record's member keys in any order; the recognized spellings are
// filtered and sorted here so the message is deterministic. where names the
// record exactly as the calling read path already located it (by id when the
// scan read one, else by position), so both paths name the problem the same
// way.
func rejectAmbiguousJobVersion(where string, keys []string) error {
	var versionKeys []string
	for _, key := range keys {
		if isJobVersionKey(key) {
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
	return corruptf("%s carries the pinned version field %q more than once (as %s)",
		where, jobVersionKey, strings.Join(quoted, ", "))
}

// rejectAmbiguousJobCircuit enforces the owning-circuit uniqueness rule shared
// by the directory scan and the single-record decode (both drive readJobRecord):
// the circuit a job names is the sole authority for which circuit the job
// belongs to — and therefore which frozen version, trusted setup and compiled
// artifact it is read against — so one record may carry the field under at
// most one recognized spelling (isJobCircuitKey). Two spellings on one record
// make the owning circuit depend on key order, since encoding/json keeps the
// last value, and corrupt the read: the whole record is refused whether the
// values agree or differ, whether both name legal circuits, and whether
// unrelated members (the version, the attempt count, …) sit between them —
// neither value is ever picked to carry on, and swapping the two keys never
// changes the refusal. A single spelling of any recognized kind is the one
// circuit field and keeps its current reading: the value is matched against
// circuit names exactly as written, with no case folding or space trimming.
//
// keys are the record's member keys in any order; the recognized spellings are
// filtered and sorted here so the message is deterministic. where names the
// record exactly as the calling read path already located it (by id when the
// scan read one, else by position), so both paths name the problem the same
// way.
func rejectAmbiguousJobCircuit(where string, keys []string) error {
	var circuitKeys []string
	for _, key := range keys {
		if isJobCircuitKey(key) {
			circuitKeys = append(circuitKeys, key)
		}
	}
	if len(circuitKeys) <= 1 {
		return nil
	}
	sort.Strings(circuitKeys)
	quoted := make([]string, len(circuitKeys))
	for i, k := range circuitKeys {
		quoted[i] = strconv.Quote(k)
	}
	return corruptf("%s carries the circuit field %q more than once (as %s)",
		where, jobCircuitKey, strings.Join(quoted, ", "))
}

// compiledHashLookalike reports a member key encoding/json would match onto
// the binding field's struct tag yet which is not an accepted ASCII spelling
// (isCompiledHashKey). Struct-tag matching in encoding/json folds with the
// Unicode simple case-folding rules, so a non-ASCII lookalike —
// "compiled_haſh" with U+017F long s, whether written directly or as the
// JSON escape u017f — silently fills CompiledHash and would let a damaged
// binding (null above all) read as unbound, then vanish on the next commit.
// Such a key is damage, regardless of its value or of a correctly spelled
// sibling key being present. Several aliases are reported in lexicographic
// order so the message is stable.
func compiledHashLookalike(keys []string) string {
	var aliases []string
	for _, key := range keys {
		if isCompiledHashKey(key) {
			continue
		}
		if strings.EqualFold(key, compiledHashKey) {
			aliases = append(aliases, key)
		}
	}
	if len(aliases) == 0 {
		return ""
	}
	sort.Strings(aliases)
	return aliases[0]
}

// jobMember is one member of a saved job record as seen during the single
// per-record pass: its already-unescaped key and its raw value.
type jobMember struct {
	key   string
	value json.RawMessage
}

// jobRecordScan is the one read of one saved job record. Both read passes —
// the envelope-wide duplicate-key scan (scanJobArray) and the typed decode
// (decodeJob) — build one through readJobRecord. It records only what the
// shared rules need: the members in document order (exact spelling), the
// first member key repeated under that same exact spelling, the best-effort
// id used to name the record, any duplicate key nested inside a member's
// own object/array value, and any exact-readback damage to the id token
// (invalid UTF-8 bytes or an unpaired \uXXXX surrogate). Turning the members
// into a typed persistJob is left to the ordinary struct decode, which runs
// once afterwards.
type jobRecordScan struct {
	positional    string
	members       []jobMember
	dupKey        string
	id            string
	nestedErr     error
	idEncodingErr error
}

// scanName locates the record for a directory-scan finding: by its id when an
// exact "id" spelling held a legible, reliably readable string, else by its
// 1-based position. The id probe is deliberately exact — an "ID" case
// variant is not the id for scan attribution — and is best-effort: a missing,
// non-string or undecodable id, or one whose bytes encoding/json would repair
// on decode (invalid UTF-8, an unpaired surrogate), leaves the positional
// name, since such an id cannot be recovered to quote faithfully.
func (r *jobRecordScan) scanName() string {
	if r.id != "" && r.idEncodingErr == nil {
		return fmt.Sprintf("stored job record %q", r.id)
	}
	return r.positional
}

func (r *jobRecordScan) memberKeys() []string {
	keys := make([]string, 0, len(r.members))
	for _, m := range r.members {
		keys = append(keys, m.key)
	}
	return keys
}

func (r *jobRecordScan) valueOf(key string) json.RawMessage {
	for _, m := range r.members {
		if m.key == key {
			return m.value
		}
	}
	return nil
}

// bindingKeys lists the record's accepted compiled_hash spellings (canonical
// and ASCII case variants), in document order.
func (r *jobRecordScan) bindingKeys() []string {
	var keys []string
	for _, m := range r.members {
		if isCompiledHashKey(m.key) {
			keys = append(keys, m.key)
		}
	}
	return keys
}

// readJobRecord consumes exactly one job record value the decoder is
// positioned at — including its opening token — and returns the single scan.
// Every token is consumed whether or not the record is damaged. A null
// element or a non-object element is structural damage named by positional.
// positional is the name used when no legible id is read: the directory scan
// passes "job record #<1-based index>", the typed decode passes
// "stored job record".
func readJobRecord(dec *json.Decoder, positional string) (*jobRecordScan, error) {
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

	r := &jobRecordScan{positional: positional}
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
		// Consume the whole value first; the judgments below use only the key
		// (and, for the id probe, the already-captured bytes). Job members are
		// scalars, but an unknown member may carry an object/array whose own
		// duplicate keys must still fail the read.
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, corruptf("%s is not valid JSON: %v", positional, err)
		}
		// The id is the job's identity and must read back exactly as the
		// bytes on disk carry it. encoding/json silently rewrites invalid
		// UTF-8 bytes and unpaired \uXXXX surrogate escapes to U+FFFD, so the
		// raw token of every spelling the ordinary decode fills ID from
		// (canonical "id" and ASCII case variants such as "ID") is judged
		// after the read. The exact "id" token also supplies the best-effort
		// name used to locate the record. A non-string value (null, a number
		// …) is left to the ordinary decode's own failure: the encoding walk
		// below is defined only over one complete JSON string token.
		isIDSpelling := key == jobIDKey || isASCIIFoldOf(key, jobIDKey)
		if isIDSpelling && bytes.HasPrefix(bytes.TrimSpace(value), []byte{'"'}) {
			if key == jobIDKey {
				if idVal, ierr := storedRecordShape.string(value, positional+` field "id"`); ierr == nil {
					r.id = idVal
				}
			}
			// The field is always quoted by its canonical name "id" in the
			// diagnostic, whatever accepted spelling the damaged token was
			// written under (an "ID" variant, a \uXXXX-escaped key).
			if r.idEncodingErr == nil {
				r.idEncodingErr = checkStoredStringEncoding(value,
					positional+` field "id"`, "committed job id")
			}
		}
		if seenExact[key] && r.dupKey == "" {
			r.dupKey = key
		}
		seenExact[key] = true
		r.members = append(r.members, jobMember{key: key, value: value})
		trimmed := bytes.TrimSpace(value)
		if r.nestedErr == nil && len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
			nd := json.NewDecoder(bytes.NewReader(trimmed))
			r.nestedErr = skipValueWithDupKeys(nd, positional+" field "+strconv.Quote(key))
		}
	}
	if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim('}') {
		return nil, corruptf("%s is not valid JSON", positional)
	}
	return r, nil
}

// judge applies the findings the directory scan owns, in the order they
// surface:
//
//  1. two recognized spellings of the pinned version field — equal or
//     differing values, both legal, even with unrelated members interleaved —
//     so the bound version is never chosen by key order;
//  2. two recognized spellings of the owning circuit field, under the same
//     rule — the circuit a job belongs to is never chosen by key order;
//  3. an exact repeated member key (the first such key in document order,
//     including two byte-identical keys a member map could not retain);
//  4. a duplicate key nested inside a member's own object/array value;
//  5. an id token that would not read back unchanged — invalid UTF-8 bytes
//     or an unpaired surrogate escape under the canonical "id" or any ASCII
//     case spelling of it — so a saved job can never be re-read under a
//     U+FFFD-repaired identity; the record is located positionally when the
//     id itself cannot be recovered faithfully.
//
// The binding-field lookalike, duplicate-binding and binding-value-type rules
// are deliberately not here: the typed decode (which runs after the scan) owns
// those, so the historical precedence — every scan finding outranking every
// binding finding — is preserved when several problems coexist.
func (r *jobRecordScan) judge(where string) error {
	if err := rejectAmbiguousJobVersion(where, r.memberKeys()); err != nil {
		return err
	}
	if err := rejectAmbiguousJobCircuit(where, r.memberKeys()); err != nil {
		return err
	}
	if r.dupKey != "" {
		return corruptf("%s contains duplicate field %q", where, r.dupKey)
	}
	if r.nestedErr != nil {
		return r.nestedErr
	}
	if r.idEncodingErr != nil {
		return r.idEncodingErr
	}
	return nil
}

// scanJobArray consumes one "jobs" value positioned at its opening bracket and
// reads every record through the shared per-record engine, judging each before
// moving on. It is the directory-read counterpart of decodeJob and exists so a
// structural problem inside one job is attributed to that job — by its id when
// the id is a legible string that reads back unchanged, else by its 1-based
// position — rather than to an opaque "jobs" element index. A null array
// stays the pre-existing "no jobs" reading.
func scanJobArray(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return corruptf("data file envelope field \"jobs\" is not valid JSON: %v", err)
	}
	if tok == nil { // null: json.Unmarshal would produce no jobs
		return nil
	}
	if tok != json.Delim('[') {
		return corruptf("data file envelope field \"jobs\" must be an array")
	}
	index := 0
	for dec.More() {
		index++
		r, err := readJobRecord(dec, fmt.Sprintf("job record #%d", index))
		if err != nil {
			return err
		}
		if err := r.judge(r.scanName()); err != nil {
			return err
		}
	}
	if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim(']') {
		return corruptf("data file envelope field \"jobs\" is not valid JSON")
	}
	return nil
}

// Strict decoding of a committed job record, including its identity id and its
// compiled-artifact binding.
//
// The id is the job's identity and is held to the same exact-readback rule as
// a circuit's name: its raw JSON string token must denote its value exactly —
// valid UTF-8 in its literal portions and no unpaired \uXXXX surrogate
// escapes under any spelling the ordinary decode fills ID from (canonical
// "id" and ASCII case variants such as "ID", a JSON-escaped spelling of
// either included). Both damage shapes decode to U+FFFD under encoding/json,
// so the job would read back under a repaired identity — a follow-up GetJob
// by the submitted id would miss it, and it could collide with an id that
// genuinely contains "�". Such a record is data corruption: the read is
// refused (named by record position when the id cannot be recovered
// faithfully), no partial results are returned and the file is never
// rewritten. A real "�", a correctly paired surrogate and the literal text
// \uD800 all stay ordinary ids. The shared readJobRecord engine judges this
// for both read passes.
//
// compiled_hash is a field whose presence is meaningful: records written
// before bindings existed simply omit it, and an explicit "" is the unbound
// state. But once the key appears its value must be a JSON string. A null is
// not "no binding" — it is damage to a binding the record claims to carry,
// and so is a number, boolean, array or object in its place. Reading either
// as the zero string would silently turn a bound job into an unbound one
// (and the next commit would drop the field entirely), so the read is
// refused as data corruption instead. A legal non-empty string keeps being
// matched against the pinned version's artifact by validateEnvelope, exactly
// as written.
//
// The field is recognized under any ASCII letter-case spelling, so a record
// carrying "COMPILED_HASH" (or any mixed-case form, escaped or not) is the
// same binding and follows the same rules: a single such key is read with
// the full string validation, and two spellings of the field on one record —
// even with byte-identical values — are a duplicate binding and corrupt the
// read rather than being resolved by key order.
//
// Non-ASCII spellings are not accepted even though encoding/json itself
// would fold them onto the field: its struct-tag matching uses Unicode case
// folding, under which "compiled_haſh" (U+017F long s, written directly or
// as the JSON escape u017f) names CompiledHash just like "compiled_hash".
// Such a lookalike is damage to the binding the record claims to carry and
// is refused whatever its value — null, a legal hash or an explicit "" —
// and even when a correctly spelled "compiled_hash" sits beside it, in
// either key order and whether the two values agree. The damaged binding
// must never read as an unbound job and disappear on the next commit.
//
// The pinned version field is held to its own uniqueness rule
// (rejectAmbiguousJobVersion): every spelling the ordinary decode fills
// Version from counts as that one field (isJobVersionKey): canonical
// "version", an ASCII letter-case variant such as "VERSION", and, since
// struct-tag matching folds with Unicode case rules, the long-s "verſion"
// (U+017F, direct or u017f-escaped). Two such spellings on one record are
// refused as data corruption whether the values agree or differ, whether
// both values are legal versions, and even when unrelated fields are
// interleaved between them. A non-ASCII "verſion" is refused only when
// another recognized spelling sits beside it; appearing by itself it is the
// single version field and keeps the current reading (the ordinary decode
// folds it onto Version). An illegal single value — a string, a float,
// null, or a non-positive version — is rejected exactly as before, by the
// ordinary decode or validateEnvelope.
//
// The owning circuit field is held to its own uniqueness rule
// (rejectAmbiguousJobCircuit): every spelling the ordinary decode fills
// Circuit from counts as that one field (isJobCircuitKey): canonical
// "circuit" and ASCII letter-case variants such as "CIRCUIT" or "CiRcUiT",
// JSON-escaped spellings included. Two such spellings on one record are
// refused as data corruption whether the values agree or differ, whether
// both name legal frozen circuits with trusted setups, and even when
// unrelated fields are interleaved between them — the job's owning circuit
// is never resolved by key order. A single recognized spelling keeps the
// current reading, and the value keeps being matched against circuit names
// exactly as written: "alpha" and "ALPHA" stay distinct circuits and no
// whitespace is trimmed.
//
// The shared readJobRecord scan enforces every key and structural rule once
// for both read passes; after it clears, this decode applies the rules the
// scan cannot — the binding-field lookalike, at-most-one binding and its
// string type — and then runs the ordinary struct decode for every other
// job field.
func (j *persistJob) UnmarshalJSON(raw []byte) error {
	const what = "stored job record"
	return decodeJob(raw, what, j)
}

// decodeJob reads one saved job record from raw through the shared engine and
// fills out. what is the generic name for a record with no legible id.
func decodeJob(raw []byte, what string, out *persistJob) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return corruptf("%s must be a JSON object", what)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	r, err := readJobRecord(dec, what)
	if err != nil {
		return err
	}
	// A single complete object is the whole record, checked before any field
	// is judged (the old strict member walk led with shape and trailing-data
	// checks). At open this is already guaranteed by the directory scan; the
	// check keeps the single-record decode self-contained.
	if _, err := dec.Token(); err != io.EOF {
		return corruptf("%s is not valid JSON: trailing data after the object", what)
	}

	// Name the record the way this pass always has: an ordinary struct probe
	// folds id spellings the same way the final decode does (so an "ID"
	// variant still names the record), and a missing or non-string id leaves
	// the generic name. The probe only labels errors; the id is decoded for
	// real by the ordinary pass below. An id whose bytes encoding/json would
	// repair (invalid UTF-8, an unpaired surrogate) is never quoted: it cannot
	// be recovered faithfully, so the generic positional name stands and the
	// idEncodingErr message below locates the record and field by position.
	where := what
	var idProbe struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &idProbe); err == nil && idProbe.ID != "" && r.idEncodingErr == nil {
		where = fmt.Sprintf("stored job record %q", idProbe.ID)
	}

	// The scan-owned findings (version and circuit ambiguity, exact and
	// nested duplicate keys) outrank every binding finding, exactly as when the directory scan
	// ran ahead of this decode. (During an open the scan has already enforced
	// them, so this is a no-op there; it keeps the single-record decoder
	// self-contained.)
	if err := r.judge(where); err != nil {
		return err
	}

	// Reject a non-ASCII spelling that encoding/json's Unicode-folded tag
	// matching would silently read as this field ("compiled_haſh" with long
	// s, directly written or ſ-escaped), whatever its value and whether or
	// not a correctly spelled key accompanies it. It must never be ignored as
	// an unknown member, which would let a damaged binding read unbound.
	if alias := compiledHashLookalike(r.memberKeys()); alias != "" {
		return corruptf("%s carries the binding field %q under non-ASCII spelling %q; such a lookalike spelling is data corruption",
			where, compiledHashKey, alias)
	}
	// Every accepted spelling of the binding field counts as the same one
	// field; the record may carry it at most once, however it is capitalized.
	bindingKeys := r.bindingKeys()
	if len(bindingKeys) > 1 {
		sort.Strings(bindingKeys)
		quoted := make([]string, len(bindingKeys))
		for i, k := range bindingKeys {
			quoted[i] = strconv.Quote(k)
		}
		return corruptf("%s carries the binding field %q more than once (as %s)",
			where, compiledHashKey, strings.Join(quoted, ", "))
	}
	compiledHash := ""
	hasBinding := false
	if len(bindingKeys) == 1 {
		// The field is always quoted by its canonical name in the diagnostic,
		// regardless of the accepted spelling it was written under.
		hash, err := storedRecordShape.string(r.valueOf(bindingKeys[0]), where+` field "compiled_hash"`)
		if err != nil {
			return err
		}
		compiledHash, hasBinding = hash, true
	}
	// With zero or one recognized version spelling, the ordinary decode below
	// already yields the current value: a single canonical/ASCII/long-s
	// spelling folds onto Version, and a missing version reads as 0 and is
	// rejected by validateEnvelope as a job bound to an unknown circuit. Illegal
	// single values (a string, a float, null, zero or negative) keep being
	// rejected by that same decode or the envelope validation, so nothing is
	// picked here.
	type plainJob persistJob
	var decoded plainJob
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return corruptf("%s is not valid JSON: %v", what, err)
	}
	if hasBinding {
		decoded.CompiledHash = compiledHash
	}
	*out = persistJob(decoded)
	return nil
}
