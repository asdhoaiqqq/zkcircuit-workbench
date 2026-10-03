package zkcircuit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
type envelope struct {
	Format    int               `json:"format"`
	Circuits  []persistCircuit  `json:"circuits"`
	Setups    []persistSetup    `json:"setups"`
	Jobs      []persistJob      `json:"jobs"`
	Artifacts []persistArtifact `json:"artifacts,omitempty"`
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

// circuitRecordFields is the exact member set of a stored circuit record:
// the seven required scalar fields plus the optional constraint definition.
var circuitRecordFields = []string{
	"name", "version", "constraints", "public_inputs", "private_inputs",
	"frozen", "description", "definition",
}

// UnmarshalJSON decodes a stored circuit record strictly. Like the persisted
// definition decoders, this record is committed data rather than an import
// request, so every scalar field must be present exactly once with its
// declared JSON type: name and description strings, version and the three
// counts integers, frozen a boolean. A missing, null, mistyped or duplicated
// field is reported as data corruption — never silently read as the zero
// value, because a dropped "frozen" would otherwise unfreeze the version and
// a dropped count would silently change its declared layout. Duplicates are
// rejected even when both values agree, and a key reached through JSON
// escapes ("frosen") still names the same field. Explicit zero input
// counts, frozen:false and the empty description are ordinary values and
// decode normally. The definition member stays optional: absent or null is
// the legacy counts-only state.
func (c *persistCircuit) UnmarshalJSON(raw []byte) error {
	const what = "stored circuit record"
	if string(bytes.TrimSpace(raw)) == "null" {
		return corruptf("%s must be a JSON object, not null", what)
	}
	members, err := strictObject(raw, circuitRecordFields, what)
	if err != nil {
		return asCorrupt(err)
	}
	field := func(key string) string {
		return fmt.Sprintf("%s field %q", what, key)
	}
	stringMember := func(key string) (string, error) {
		raw, err := requireMember(members, key, what)
		if err != nil {
			return "", asCorrupt(err)
		}
		s, err := decodeJSONString(raw, field(key))
		if err != nil {
			return "", asCorrupt(err)
		}
		return s, nil
	}
	intMember := func(key string) (int, error) {
		raw, err := requireMember(members, key, what)
		if err != nil {
			return 0, asCorrupt(err)
		}
		n, err := decodeJSONInt(raw, field(key))
		if err != nil {
			return 0, asCorrupt(err)
		}
		return n, nil
	}

	name, err := stringMember("name")
	if err != nil {
		return err
	}
	version, err := intMember("version")
	if err != nil {
		return err
	}
	constraints, err := intMember("constraints")
	if err != nil {
		return err
	}
	publicInputs, err := intMember("public_inputs")
	if err != nil {
		return err
	}
	privateInputs, err := intMember("private_inputs")
	if err != nil {
		return err
	}
	frozenRaw, err := requireMember(members, "frozen", what)
	if err != nil {
		return asCorrupt(err)
	}
	frozen, err := decodeJSONBool(frozenRaw, field("frozen"))
	if err != nil {
		return asCorrupt(err)
	}
	description, err := stringMember("description")
	if err != nil {
		return err
	}

	// Absent or null definition is the counts-only state; anything else must
	// be a well-formed stored definition (its own decoder tags corruption).
	var def *persistDefinition
	if defRaw, ok := members["definition"]; ok && string(bytes.TrimSpace(defRaw)) != "null" {
		var d persistDefinition
		if err := json.Unmarshal(defRaw, &d); err != nil {
			var se StoreError
			if errors.As(err, &se) {
				return err // already reported as data corruption
			}
			return corruptf("%s field %q is not a valid constraint definition: %v", what, "definition", err)
		}
		def = &d
	}

	*c = persistCircuit{
		Name: name, Version: version, Constraints: constraints,
		PublicInputs: publicInputs, PrivateInputs: privateInputs,
		Frozen: frozen, Description: description, Definition: def,
	}
	return nil
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
