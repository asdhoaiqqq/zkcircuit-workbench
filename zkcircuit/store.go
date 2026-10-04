package zkcircuit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// on-disk layout inside the data directory:
//
//	data.json    – the committed state (JSON envelope, atomically replaced)
//	data.json.tmp– staging file for the next commit (rename(2) target)
//	lock         – flock file serializing concurrent processes
//
// The envelope carries an explicit format version: files written by an
// incompatible future build are refused rather than silently migrated or
// overwritten.
const (
	dirDataFile = "data.json"
	dirTmpFile  = "data.json.tmp"
	dirLockFile = "lock"

	// FormatVersion is the on-disk format this build writes and accepts.
	FormatVersion = 1
)

// envelope is the persisted state container.
//
// Circuits are kept as raw JSON during decoding so persistCircuit's strict
// UnmarshalJSON — rather than encoding/json's silent zero-value filling —
// decides what a committed circuit record may look like. UnmarshalJSON first
// walks the token stream (scanEnvelopeDuplicates) and rejects a repeated
// object key anywhere encoding/json would otherwise keep the last value.
type envelope struct {
	Format    int               `json:"format"`
	Circuits  []persistCircuit  `json:"circuits"`
	Setups    []persistSetup    `json:"setups"`
	Jobs      []persistJob      `json:"jobs"`
	Artifacts []persistArtifact `json:"artifacts,omitempty"`
}

// envelopeWire is the decoding-only shape: circuit records arrive raw so
// their own strict decoder can require every field explicitly.
type envelopeWire struct {
	Format    int               `json:"format"`
	Circuits  []json.RawMessage `json:"circuits"`
	Setups    []persistSetup    `json:"setups"`
	Jobs      []persistJob      `json:"jobs"`
	Artifacts []persistArtifact `json:"artifacts,omitempty"`
}

func (e *envelope) UnmarshalJSON(raw []byte) error {
	// Reject repeated keys before decoding: encoding/json silently keeps the
	// last value of a duplicate. The walk is layered so a repeated field is
	// attributed to the record it belongs to (a duplicate "frozen" inside
	// circuit #3, not a generic envelope complaint).
	if err := scanEnvelopeDuplicates(raw); err != nil {
		return err
	}
	var wire envelopeWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		var se StoreError
		if errors.As(err, &se) && se.Kind == ErrDataCorrupt.Kind {
			return err // an inner record decoder already named the problem
		}
		return corruptf("data file envelope is not valid JSON: %v", err)
	}
	e.Format = wire.Format
	e.Setups = wire.Setups
	e.Jobs = wire.Jobs
	e.Artifacts = wire.Artifacts
	if wire.Circuits == nil {
		e.Circuits = nil
	} else {
		e.Circuits = make([]persistCircuit, len(wire.Circuits))
	}
	for i, craw := range wire.Circuits {
		var record persistCircuit
		if err := json.Unmarshal(craw, &record); err != nil {
			var se StoreError
			if errors.As(err, &se) {
				// Re-tag as one corruption error carrying the record index,
				// so the detail survives errors.As unwrapping in loadLocked.
				return StoreError{Kind: ErrDataCorrupt.Kind,
					Detail: fmt.Sprintf("circuit record #%d: %s", i+1, se.Detail)}
			}
			return corruptf("circuit record #%d: %v", i+1, err)
		}
		e.Circuits[i] = record
	}
	return nil
}

// scanEnvelopeDuplicates tokenizes the committed envelope and rejects every
// repeated object key. Circuit records are scanned one level deep only —
// their top-level fields are checked here (with the record's 1-based index
// reported), while values such as the constraint definition are skipped and
// left to their own strict decoders. Every other envelope member is scanned
// recursively so a duplicated key anywhere in committed data fails the read
// instead of silently resolving to its last value.
func scanEnvelopeDuplicates(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)))
	dec.UseNumber()
	open, err := dec.Token()
	if err != nil || open != json.Delim('{') {
		return corruptf("data file envelope must be a JSON object")
	}
	seenTop := make(map[string]bool)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return corruptf("data file envelope is not valid JSON: %v", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return corruptf("data file envelope is not valid JSON")
		}
		if seenTop[key] {
			return corruptf("data file envelope contains duplicate field %q", key)
		}
		seenTop[key] = true
		if key == "circuits" {
			if err := scanCircuitArrayDuplicates(dec); err != nil {
				return err
			}
			continue
		}
		if err := skipValueWithDupKeys(dec, "data file envelope field "+strconv.Quote(key)); err != nil {
			return err
		}
	}
	if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim('}') {
		return corruptf("data file envelope is not valid JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return corruptf("data file envelope is not valid JSON: trailing data after the object")
	}
	return nil
}

// scanCircuitArrayDuplicates consumes one "circuits" value positioned at its
// opening bracket and checks only each record's own top-level keys. A null
// array stays the pre-existing "no records" reading.
func scanCircuitArrayDuplicates(dec *json.Decoder) error {
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
		what := fmt.Sprintf("circuit record #%d", index)
		open, err := dec.Token()
		if err != nil {
			return corruptf("%s is not valid JSON: %v", what, err)
		}
		if open == nil {
			return corruptf("%s must be a JSON object, not null", what)
		}
		if open != json.Delim('{') {
			return corruptf("%s must be a JSON object", what)
		}
		seen := make(map[string]bool)
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return corruptf("%s is not valid JSON: %v", what, err)
			}
			key, ok := keyTok.(string)
			if !ok {
				return corruptf("%s is not valid JSON", what)
			}
			if seen[key] {
				return corruptf("%s contains duplicate field %q", what, key)
			}
			seen[key] = true
			// Skip the whole value without descending: nested objects
			// (definition) own their own strict duplicate checks.
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return corruptf("%s is not valid JSON: %v", what, err)
			}
		}
		if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim('}') {
			return corruptf("%s is not valid JSON", what)
		}
	}
	if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim(']') {
		return corruptf("data file envelope field \"circuits\" is not valid JSON")
	}
	return nil
}

// skipValueWithDupKeys consumes one JSON value positioned at its first token,
// recursively rejecting duplicate object keys.
func skipValueWithDupKeys(dec *json.Decoder, what string) error {
	tok, err := dec.Token()
	if err != nil {
		return corruptf("%s is not valid JSON: %v", what, err)
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil // scalar (including null)
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return corruptf("%s is not valid JSON: %v", what, err)
			}
			key, ok := keyTok.(string)
			if !ok {
				return corruptf("%s is not valid JSON", what)
			}
			if seen[key] {
				return corruptf("%s contains duplicate field %q", what, key)
			}
			seen[key] = true
			if err := skipValueWithDupKeys(dec, what+" "+strconv.Quote(key)); err != nil {
				return err
			}
		}
		if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim('}') {
			return corruptf("%s is not valid JSON", what)
		}
	case '[':
		index := 0
		for dec.More() {
			index++
			if err := skipValueWithDupKeys(dec, fmt.Sprintf("%s element #%d", what, index)); err != nil {
				return err
			}
		}
		if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim(']') {
			return corruptf("%s is not valid JSON", what)
		}
	}
	return nil
}

type persistTerm struct {
	Wire  int    `json:"wire"`
	Coeff string `json:"coeff"`
}

type persistConstraint struct {
	A []persistTerm `json:"a"`
	B []persistTerm `json:"b"`
	C []persistTerm `json:"c"`
}

// persistDefinition is the canonical, already-validated constraint set bound
// to one circuit version. The public/private partition is owned by the
// circuit record and is deliberately not duplicated here, so it can never
// drift from the declared counts.
type persistDefinition struct {
	Modulus     string              `json:"modulus"`
	Constraints []persistConstraint `json:"constraints"`
}

// persistArtifact is a compiled artifact: the version's identity and
// constraint count bound to its recomputable SHA-256 hash.
type persistArtifact struct {
	Name        string `json:"name"`
	Version     int    `json:"version"`
	Modulus     int64  `json:"modulus"`
	Constraints int    `json:"constraints"`
	Hash        string `json:"hash"`
}

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
// may be absent or null (the legacy counts-only state); when present it goes
// through the strict persistDefinition decoder.
//
// Only shape is judged here. The domain rules (non-blank name, positive
// version and constraint count, non-negative input counts) keep being
// re-checked by validateEnvelope.

var persistCircuitFields = []string{
	"name", "version", "constraints", "public_inputs", "private_inputs",
	"frozen", "description", "definition",
}

// storedRecordShape is the shared strict-JSON rule set (definition_schema.go)
// tagged for committed data: every structural failure in a stored circuit
// record is data corruption, never a bad request.
var storedRecordShape = jsonShape{fail: corruptf}

func (c *persistCircuit) UnmarshalJSON(raw []byte) error {
	const what = "stored circuit record"
	if string(bytes.TrimSpace(raw)) == "null" {
		return corruptf("%s must be a JSON object, not null", what)
	}
	members, err := strictCircuitObject(raw, what)
	if err != nil {
		return err
	}

	var out persistCircuit

	requireString := func(key string, dst *string) error {
		r, err := storedRecordShape.require(members, key, what)
		if err != nil {
			return err
		}
		v, err := storedRecordShape.string(r, what+" field "+strconv.Quote(key))
		if err != nil {
			return err
		}
		*dst = v
		return nil
	}
	requireInt := func(key string, dst *int) error {
		r, err := storedRecordShape.require(members, key, what)
		if err != nil {
			return err
		}
		v, err := storedRecordShape.int(r, what+" field "+strconv.Quote(key))
		if err != nil {
			return err
		}
		*dst = v
		return nil
	}

	if err := requireString("name", &out.Name); err != nil {
		return err
	}
	if err := requireInt("version", &out.Version); err != nil {
		return err
	}
	if err := requireInt("constraints", &out.Constraints); err != nil {
		return err
	}
	if err := requireInt("public_inputs", &out.PublicInputs); err != nil {
		return err
	}
	if err := requireInt("private_inputs", &out.PrivateInputs); err != nil {
		return err
	}
	frozenRaw, err := storedRecordShape.require(members, "frozen", what)
	if err != nil {
		return err
	}
	out.Frozen, err = storedRecordShape.bool(frozenRaw, what+" field "+strconv.Quote("frozen"))
	if err != nil {
		return err
	}
	if err := requireString("description", &out.Description); err != nil {
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
			out.Definition = &def
		}
	}

	*c = out
	return nil
}

// strictCircuitObject decodes one committed circuit record's own member map,
// demanding a JSON object with exactly the known top-level fields once.
func strictCircuitObject(raw []byte, what string) (map[string]json.RawMessage, error) {
	return strictObjectMembers(raw, what, persistCircuitFields)
}

// strictObjectMembers decodes one committed record's own member map,
// demanding a JSON object without repeated keys. A non-nil allowed list
// restricts which keys may appear; nil allows any key. It scans only the
// record's own level: the envelope scanner already attributes a duplicated
// top-level key to this record, and nested values such as the constraint
// definition own their (more precise) strict decoders. Keys are compared
// after JSON unescaping, so a field repeated through a \uXXXX spelling is
// still a duplicate, even when both values are identical.
func strictObjectMembers(raw []byte, what string, allowedFields []string) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, corruptf("%s must be a JSON object", what)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	open, err := dec.Token()
	if err != nil || open != json.Delim('{') {
		return nil, corruptf("%s must be a JSON object", what)
	}
	var allowed map[string]bool
	if allowedFields != nil {
		allowed = make(map[string]bool, len(allowedFields))
		for _, f := range allowedFields {
			allowed[f] = true
		}
	}
	members := make(map[string]json.RawMessage)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, corruptf("%s is not valid JSON: %v", what, err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, corruptf("%s is not valid JSON", what)
		}
		if allowed != nil && !allowed[key] {
			return nil, corruptf("%s has unknown field %q", what, key)
		}
		if _, repeated := members[key]; repeated {
			return nil, corruptf("%s contains duplicate field %q", what, key)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, corruptf("%s is not valid JSON: %v", what, err)
		}
		members[key] = value
	}
	if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim('}') {
		return nil, corruptf("%s is not valid JSON", what)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, corruptf("%s is not valid JSON: trailing data after the object", what)
	}
	return members, nil
}

type persistSetup struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

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

// Strict decoding of the compiled-artifact binding on a committed job record.
//
// compiled_hash is a field whose presence is meaningful: records written
// before bindings existed simply omit it, and an explicit "" is the unbound
// state. But once the key appears its value must be a JSON string. A null is
// not "no binding" — it is damage to a binding the record claims to carry,
// and so is a number, boolean, array or object in its place. Reading either
// as the zero string would silently turn a bound job into an unbound one
// (and the next commit would drop the field entirely), so the read is
// refused as data corruption instead. A repeated key — including one
// repeated through JSON string escapes, even with identical values — is
// likewise refused rather than resolved to its last value. A legal non-empty
// string keeps being matched against the pinned version's artifact by
// validateEnvelope, exactly as written.
//
// Only the binding field is judged here; every other field keeps the
// ordinary struct decoding.
func (j *persistJob) UnmarshalJSON(raw []byte) error {
	const what = "stored job record"
	if string(bytes.TrimSpace(raw)) == "null" {
		return corruptf("%s must be a JSON object, not null", what)
	}
	members, err := strictObjectMembers(raw, what, nil)
	if err != nil {
		return err
	}
	// Name the job in a binding-field failure when its id is legible; the id
	// itself is decoded for real by the ordinary pass below.
	where := what
	if idRaw, ok := members["id"]; ok {
		if id, idErr := storedRecordShape.string(idRaw, what+` field "id"`); idErr == nil {
			where = fmt.Sprintf("stored job record %q", id)
		}
	}
	compiledHash := ""
	hasBinding := false
	if hashRaw, present := members["compiled_hash"]; present {
		hash, err := storedRecordShape.string(hashRaw, where+` field "compiled_hash"`)
		if err != nil {
			return err
		}
		compiledHash, hasBinding = hash, true
	}
	type plainJob persistJob
	var decoded plainJob
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return corruptf("%s is not valid JSON: %v", what, err)
	}
	if hasBinding {
		decoded.CompiledHash = compiledHash
	}
	*j = persistJob(decoded)
	return nil
}

// Store is a persistent workbench backed by one local data directory.
//
// All operations are safe for concurrent use by multiple goroutines and by
// several processes pointed at the same directory: an in-process mutex plus a
// process-shared flock serialize commits, and every commit is an atomic
// temp-file/fsync/rename replacement. Open the directory with Open and call
// Close when done.
type Store struct {
	dir  string
	mu   sync.Mutex
	lock *os.File

	data envelope
}

// Open opens (creating if needed) the workbench data in dir. If the
// directory does not exist it is created. Existing committed data is loaded
// and fully validated before Open returns; on a corrupt, truncated or
// unsupported-format file Open returns an error wrapping ErrDataCorrupt and
// leaves every file in the directory untouched.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, invalidf("data directory must not be empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create data directory %q: %w", dir, err)
	}
	lockPath := filepath.Join(dir, dirLockFile)
	lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("cannot open lock file in %q: %w", dir, err)
	}
	// Serialize against other processes while we read / validate.
	if err := lockExclusive(lock); err != nil {
		lock.Close()
		return nil, fmt.Errorf("cannot lock data directory %q: %w", dir, err)
	}

	s := &Store{dir: dir, lock: lock}
	if err := s.loadLocked(); err != nil {
		unlockLock(lock)
		lock.Close()
		return nil, err
	}
	// The lock is taken again per operation; holding it for the store's
	// lifetime would forbid two open handles on one directory.
	if err := unlockLock(lock); err != nil {
		lock.Close()
		return nil, fmt.Errorf("cannot release open lock on %q: %w", dir, err)
	}
	return s, nil
}

// Close releases the directory lock. It does not discard committed data.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	var first error
	if err := unlockLock(s.lock); err != nil {
		first = err
	}
	if err := s.lock.Close(); err != nil && first == nil {
		first = err
	}
	s.lock = nil
	return first
}

// Dir reports the data directory backing the store.
func (s *Store) Dir() string { return s.dir }

func (s *Store) dataPath() string { return filepath.Join(s.dir, dirDataFile) }
func (s *Store) tmpPath() string  { return filepath.Join(s.dir, dirTmpFile) }

// loadLocked reads and validates the committed file. Caller must hold the
// exclusive flock; the in-process mutex is taken by the public Open path.
func (s *Store) loadLocked() error {
	raw, err := os.ReadFile(s.dataPath())
	if errors.Is(err, os.ErrNotExist) {
		s.data = envelope{Format: FormatVersion}
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot read data file %q: %w", s.dataPath(), err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return corruptf("data file %q is empty or truncated; refusing to treat it as a new directory", s.dataPath())
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// Shape failures surfaced by the persisted-definition decoders are
		// integrity failures, not JSON syntax errors; report their detail
		// instead of the generic "not valid JSON" wording.
		var se StoreError
		if errors.As(err, &se) && se.Kind == ErrDataCorrupt.Kind {
			return corruptf("data file %q failed integrity validation: %s; original file left in place", s.dataPath(), se.Detail)
		}
		return corruptf("data file %q is not valid JSON (%v); original file left in place", s.dataPath(), err)
	}
	if env.Format == 0 {
		return corruptf("data file %q has no format version; original file left in place", s.dataPath())
	}
	if env.Format != FormatVersion {
		return corruptf("data file %q uses unsupported format version %d (this build supports %d); original file left in place",
			s.dataPath(), env.Format, FormatVersion)
	}
	if err := validateEnvelope(env); err != nil {
		return corruptf("data file %q failed integrity validation: %v; original file left in place", s.dataPath(), err)
	}
	s.data = env
	return nil
}

// commitLocked writes the pending state atomically: temp file → fsync →
// rename → fsync directory. Caller must hold both the in-process mutex and
// the exclusive flock; all in-memory mutations must already have happened and
// been rule-checked, so a failure here occurs before any success is reported.
func (s *Store) commitLocked() error {
	s.data.Format = FormatVersion
	// Stable output: records are stored sorted so files are deterministic.
	sortEnvelope(&s.data)

	payload, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode data: %w", err)
	}
	payload = append(payload, '\n')

	tmp := s.tmpPath()
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("cannot stage write in %q: %w", s.dir, err)
	}
	if _, err := f.Write(payload); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("cannot write staged data: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("cannot flush staged data: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("cannot finish staged data: %w", err)
	}
	if err := os.Rename(tmp, s.dataPath()); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("cannot commit data file: %w", err)
	}
	if err := syncDir(s.dir); err != nil {
		// The rename already happened; lack of dir fsync only weakens crash
		// durability, never correctness of what is visible now.
		return nil
	}
	return nil
}

// withLock runs fn under the in-process mutex and an exclusive flock, then
// atomically commits when fn requests it. The on-disk state is re-read after
// acquiring the lock, so concurrent commits from another process or another
// handle are always observed. Operations validate before mutating; a rejected
// operation (commit=false or an error) writes nothing and leaves committed
// state unchanged.
func (s *Store) withLock(fn func() (commit bool, err error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return invalidf("store is closed")
	}
	if err := lockExclusive(s.lock); err != nil {
		return fmt.Errorf("cannot lock data directory: %w", err)
	}
	defer unlockLock(s.lock)

	if err := s.loadLocked(); err != nil {
		return err
	}
	commit, err := fn()
	if err != nil {
		return err
	}
	if !commit {
		return nil
	}
	return s.commitLocked()
}

// withLockRead runs fn under a shared flock over freshly reloaded state.
func (s *Store) withLockRead(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return invalidf("store is closed")
	}
	if err := lockShared(s.lock); err != nil {
		return fmt.Errorf("cannot lock data directory: %w", err)
	}
	defer unlockLock(s.lock)
	if err := s.loadLocked(); err != nil {
		return err
	}
	return fn()
}

func sortEnvelope(env *envelope) {
	sort.Slice(env.Circuits, func(i, j int) bool {
		if env.Circuits[i].Name != env.Circuits[j].Name {
			return env.Circuits[i].Name < env.Circuits[j].Name
		}
		return env.Circuits[i].Version < env.Circuits[j].Version
	})
	sort.Slice(env.Setups, func(i, j int) bool {
		if env.Setups[i].Name != env.Setups[j].Name {
			return env.Setups[i].Name < env.Setups[j].Name
		}
		return env.Setups[i].Version < env.Setups[j].Version
	})
	sort.Slice(env.Jobs, func(i, j int) bool { return env.Jobs[i].ID < env.Jobs[j].ID })
	sort.Slice(env.Artifacts, func(i, j int) bool {
		if env.Artifacts[i].Name != env.Artifacts[j].Name {
			return env.Artifacts[i].Name < env.Artifacts[j].Name
		}
		return env.Artifacts[i].Version < env.Artifacts[j].Version
	})
}

// validateEnvelope re-checks every invariant on load so a tampered or
// hand-edited file cannot bypass the domain rules.
func validateEnvelope(env envelope) error {
	seenCircuit := make(map[[2]string]bool)
	circuitOK := make(map[[2]string]bool)
	for i, c := range env.Circuits {
		key := [2]string{c.Name, itoa(c.Version)}
		if c.Name == "" || strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("circuit #%d has an empty or blank name", i+1)
		}
		if c.Version <= 0 {
			return fmt.Errorf("circuit %q version %d is not a positive integer", c.Name, c.Version)
		}
		if seenCircuit[key] {
			return fmt.Errorf("circuit %q version %d appears more than once", c.Name, c.Version)
		}
		seenCircuit[key] = true
		if c.Constraints <= 0 {
			return fmt.Errorf("circuit %q v%d: constraints must be > 0", c.Name, c.Version)
		}
		if c.PublicInputs < 0 || c.PrivateInputs < 0 {
			return fmt.Errorf("circuit %q v%d: input counts must not be negative", c.Name, c.Version)
		}
		// A layout that cannot be represented with this platform's int is
		// refused here too: the record is left in place and reported as
		// corruption rather than silently shrunk or skipped.
		if inputLayoutOverflows(c.PublicInputs, c.PrivateInputs) {
			return fmt.Errorf("circuit %q v%d: input layout exceeds the representable range (%d public + %d private inputs plus the constant wire 0)",
				c.Name, c.Version, c.PublicInputs, c.PrivateInputs)
		}
		if c.Definition != nil {
			if err := validatePersistDefinition(c.Name, c.Version, c.Constraints, c.PublicInputs, c.PrivateInputs, c.Definition); err != nil {
				return err
			}
		}
		circuitOK[key] = true
	}
	seenSetup := make(map[[2]string]bool)
	for i, p := range env.Setups {
		key := [2]string{p.Name, itoa(p.Version)}
		if !circuitOK[key] {
			return fmt.Errorf("setup #%d belongs to unknown circuit %q v%d", i+1, p.Name, p.Version)
		}
		c := findPersistCircuit(env.Circuits, p.Name, p.Version)
		if c == nil || !c.Frozen {
			return fmt.Errorf("setup belongs to non-frozen circuit %q v%d", p.Name, p.Version)
		}
		if seenSetup[key] {
			return fmt.Errorf("setup for %q v%d appears more than once", p.Name, p.Version)
		}
		seenSetup[key] = true
	}
	seenJob := make(map[string]bool)
	for i, j := range env.Jobs {
		if j.ID == "" || strings.TrimSpace(j.ID) == "" {
			return fmt.Errorf("job #%d has an empty or blank id", i+1)
		}
		if seenJob[j.ID] {
			return fmt.Errorf("job %q appears more than once", j.ID)
		}
		seenJob[j.ID] = true
		if j.Kind != "prove" {
			return fmt.Errorf("job %q has unsupported kind %q", j.ID, j.Kind)
		}
		if j.Attempt <= 0 {
			return fmt.Errorf("job %q: attempt must be a positive integer", j.ID)
		}
		key := [2]string{j.Circuit, itoa(j.Version)}
		if !circuitOK[key] {
			return fmt.Errorf("job %q binds to unknown circuit %q v%d", j.ID, j.Circuit, j.Version)
		}
		c := findPersistCircuit(env.Circuits, j.Circuit, j.Version)
		if c == nil || !c.Frozen {
			return fmt.Errorf("job %q binds to non-frozen circuit %q v%d", j.ID, j.Circuit, j.Version)
		}
		if !seenSetup[key] {
			return fmt.Errorf("job %q binds to circuit %q v%d without a trusted setup", j.ID, j.Circuit, j.Version)
		}
		// A recorded compiled-artifact binding must still resolve against the
		// pinned version's artifact; a dangling or mismatched binding makes
		// the file unreadable rather than silently dropping the binding.
		if j.CompiledHash != "" {
			artifact := findArtifact(env.Artifacts, j.Circuit, j.Version)
			if artifact == nil {
				return fmt.Errorf("job %q binds to compiled artifact of %q v%d which is missing", j.ID, j.Circuit, j.Version)
			}
			if artifact.Hash != j.CompiledHash {
				return fmt.Errorf("job %q compiled hash %q does not match the artifact of %q v%d (hash %q)",
					j.ID, j.CompiledHash, j.Circuit, j.Version, artifact.Hash)
			}
		}
	}
	seenArtifact := make(map[[2]string]bool)
	for i, a := range env.Artifacts {
		key := [2]string{a.Name, itoa(a.Version)}
		if !circuitOK[key] {
			return fmt.Errorf("artifact #%d belongs to unknown circuit %q v%d", i+1, a.Name, a.Version)
		}
		c := findPersistCircuit(env.Circuits, a.Name, a.Version)
		if c == nil || !c.Frozen {
			return fmt.Errorf("artifact belongs to non-frozen circuit %q v%d", a.Name, a.Version)
		}
		if seenArtifact[key] {
			return fmt.Errorf("artifact for %q v%d appears more than once", a.Name, a.Version)
		}
		seenArtifact[key] = true
		if c.Definition == nil {
			return fmt.Errorf("artifact for %q v%d has no constraint definition", a.Name, a.Version)
		}
		if a.Constraints != c.Constraints {
			return fmt.Errorf("artifact for %q v%d constraint count %d disagrees with the version's %d",
				a.Name, a.Version, a.Constraints, c.Constraints)
		}
		def, err := definitionFromPersist(*c.Definition, c.PublicInputs, c.PrivateInputs)
		if err != nil {
			return fmt.Errorf("artifact for %q v%d cannot be checked against its definition: %w", a.Name, a.Version, err)
		}
		if int64(a.Modulus) != def.modulus || len(def.constraints) != a.Constraints {
			return fmt.Errorf("artifact for %q v%d is inconsistent with its stored definition", a.Name, a.Version)
		}
		wantHash := artifactHash(a.Name, a.Version, def)
		if a.Hash != wantHash {
			return fmt.Errorf("artifact for %q v%d hash %q does not recompute from the definition (want %q)",
				a.Name, a.Version, a.Hash, wantHash)
		}
	}
	return nil
}

// validatePersistDefinition re-validates a stored definition against its
// version's declared counts: prime modulus, in-range wires and an exact
// constraint count match.
func validatePersistDefinition(name string, version, constraints, public, private int, def *persistDefinition) error {
	parsed, err := definitionFromPersist(*def, public, private)
	if err != nil {
		return fmt.Errorf("circuit %q v%d has a corrupt constraint definition: %w", name, version, err)
	}
	if !parsed.compatibleWith(constraints, public, private) {
		return fmt.Errorf("circuit %q v%d definition is incompatible with its declared counts", name, version)
	}
	return nil
}

func findPersistCircuit(cs []persistCircuit, name string, version int) *persistCircuit {
	for i := range cs {
		if cs[i].Name == name && cs[i].Version == version {
			return &cs[i]
		}
	}
	return nil
}

func itoa(v int) string { return fmt.Sprintf("%d", v) }
