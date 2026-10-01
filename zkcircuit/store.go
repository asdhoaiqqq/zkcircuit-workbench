package zkcircuit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// FormatVersion is the on-disk data format version. Files written by this
// build carry this number; files carrying a different number are refused
// rather than migrated or overwritten.
const FormatVersion = 1

// Distinguishable failure reasons returned by store operations.
var (
	// ErrCircuitNotFound: no circuit with the given name and version exists.
	ErrCircuitNotFound = errors.New("circuit version not found")
	// ErrCircuitFrozen: a draft-only operation was attempted on a frozen circuit.
	ErrCircuitFrozen = errors.New("circuit is frozen")
	// ErrCircuitNotFrozen: the circuit exists but has not been frozen yet.
	ErrCircuitNotFrozen = errors.New("circuit is not frozen")
	// ErrTrustedSetupMissing: no trusted setup has been recorded for the version.
	ErrTrustedSetupMissing = errors.New("trusted setup not recorded")
	// ErrConflict: the request conflicts with an existing record.
	ErrConflict = errors.New("conflicting request")
	// ErrInvalidInput: the request failed validation.
	ErrInvalidInput = errors.New("invalid input")
	// ErrCorruptData: persisted data is missing, truncated, or uses an
	// unsupported format version. The original files are left untouched.
	ErrCorruptData = errors.New("corrupt or unsupported data")
)

// SetupRecord is a trusted setup recorded for one frozen circuit version.
type SetupRecord struct {
	Circuit string
	Version int
	SetupID string
}

// Store is a persistent, concurrency-safe circuit workbench rooted at a
// local data directory. All records survive process restarts.
type Store struct {
	dir string
	mu  sync.Mutex
}

// Open opens (and if needed creates) a store rooted at dir.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("%w: data directory must not be empty", ErrInvalidInput)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// CreateCircuit registers a new circuit version.
//
// The circuit is identified by (name, version). Creating a version that
// already exists is idempotent: an identical description returns the
// original record, a different description reports a conflict.
func (s *Store) CreateCircuit(circuit Circuit) (Circuit, error) {
	var out Circuit
	err := s.withLock(func() error {
		name, err := validateCircuit(circuit)
		if err != nil {
			return err
		}
		data, err := s.load()
		if err != nil {
			return err
		}
		key := circuitKey(name, circuit.Version)
		for _, r := range data.Circuits {
			if circuitKey(r.Name, r.Version) == key {
				if r.Description != circuit.Description {
					return fmt.Errorf("%w: circuit %q version %d already exists with a different description", ErrConflict, name, circuit.Version)
				}
				out = r.toCircuit()
				return nil
			}
		}
		rec := circuitRecord{
			Name:        name,
			Version:     circuit.Version,
			Description: circuit.Description,
			Constraints: circuit.Constraints,
			Public:      circuit.PublicInputs,
			Private:     circuit.PrivateInputs,
		}
		data.Circuits = append(data.Circuits, rec)
		if err := s.save(data); err != nil {
			return err
		}
		out = rec.toCircuit()
		return nil
	})
	return out, err
}

// UpdateCircuit changes the mutable fields of a draft circuit version.
// Frozen circuits cannot be modified.
func (s *Store) UpdateCircuit(circuit Circuit) (Circuit, error) {
	var out Circuit
	err := s.withLock(func() error {
		name, err := validateCircuit(circuit)
		if err != nil {
			return err
		}
		data, err := s.load()
		if err != nil {
			return err
		}
		idx := -1
		for i, r := range data.Circuits {
			if r.Name == name && r.Version == circuit.Version {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("%w: circuit %q version %d", ErrCircuitNotFound, name, circuit.Version)
		}
		if data.Circuits[idx].Frozen {
			return fmt.Errorf("%w: circuit %q version %d is frozen", ErrCircuitFrozen, name, circuit.Version)
		}
		data.Circuits[idx].Description = circuit.Description
		data.Circuits[idx].Constraints = circuit.Constraints
		data.Circuits[idx].Public = circuit.PublicInputs
		data.Circuits[idx].Private = circuit.PrivateInputs
		if err := s.save(data); err != nil {
			return err
		}
		out = data.Circuits[idx].toCircuit()
		return nil
	})
	return out, err
}

// FreezeCircuit freezes a circuit version. Freezing is idempotent:
// freezing an already-frozen version returns the same record.
func (s *Store) FreezeCircuit(name string, version int) (Circuit, error) {
	name = strings.TrimSpace(name)
	var out Circuit
	err := s.withLock(func() error {
		if name == "" {
			return fmt.Errorf("%w: circuit name must not be empty", ErrInvalidInput)
		}
		if version <= 0 {
			return fmt.Errorf("%w: circuit version must be a positive integer", ErrInvalidInput)
		}
		data, err := s.load()
		if err != nil {
			return err
		}
		idx := -1
		for i, r := range data.Circuits {
			if r.Name == name && r.Version == version {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("%w: circuit %q version %d", ErrCircuitNotFound, name, version)
		}
		if !data.Circuits[idx].Frozen {
			data.Circuits[idx].Frozen = true
			if err := s.save(data); err != nil {
				return err
			}
		}
		out = data.Circuits[idx].toCircuit()
		return nil
	})
	return out, err
}

// RegisterTrustedSetup records a trusted setup for a frozen circuit
// version. The setup belongs to that version only; registration is
// idempotent and adds no duplicate record.
func (s *Store) RegisterTrustedSetup(name string, version int) (SetupRecord, error) {
	name = strings.TrimSpace(name)
	var out SetupRecord
	err := s.withLock(func() error {
		if name == "" {
			return fmt.Errorf("%w: circuit name must not be empty", ErrInvalidInput)
		}
		if version <= 0 {
			return fmt.Errorf("%w: circuit version must be a positive integer", ErrInvalidInput)
		}
		data, err := s.load()
		if err != nil {
			return err
		}
		idx := -1
		for i, r := range data.Circuits {
			if r.Name == name && r.Version == version {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("%w: circuit %q version %d", ErrCircuitNotFound, name, version)
		}
		if !data.Circuits[idx].Frozen {
			return fmt.Errorf("%w: circuit %q version %d is not frozen", ErrCircuitNotFrozen, name, version)
		}
		for _, su := range data.Setups {
			if su.Circuit == name && su.Version == version {
				out = su.toSetup()
				return nil
			}
		}
		rec := setupRecord{
			Circuit: name,
			Version: version,
			SetupID: setupID(data.Circuits[idx].toCircuit()),
		}
		data.Setups = append(data.Setups, rec)
		if err := s.save(data); err != nil {
			return err
		}
		out = rec.toSetup()
		return nil
	})
	return out, err
}

// SubmitJob accepts a prove job for a frozen circuit version with a
// recorded trusted setup. Only accepted jobs are saved; no proof is
// computed. Rejections leave no trace in the store.
//
// Submitting the same request under the same id is idempotent; a
// different request under the same id reports a conflict.
func (s *Store) SubmitJob(job Job) (Job, error) {
	var out Job
	err := s.withLock(func() error {
		if strings.TrimSpace(job.ID) == "" {
			return fmt.Errorf("%w: job id must not be empty or blank", ErrInvalidInput)
		}
		name := strings.TrimSpace(job.Circuit)
		if name == "" {
			return fmt.Errorf("%w: circuit name must not be empty or blank", ErrInvalidInput)
		}
		if job.Version <= 0 {
			return fmt.Errorf("%w: circuit version must be a positive integer", ErrInvalidInput)
		}
		if job.Attempt <= 0 {
			return fmt.Errorf("%w: attempt must be a positive integer", ErrInvalidInput)
		}
		kind := job.Kind
		if kind == "" {
			kind = "prove"
		}
		if kind != "prove" {
			return fmt.Errorf("%w: only prove jobs are accepted, got %q", ErrInvalidInput, kind)
		}
		data, err := s.load()
		if err != nil {
			return err
		}
		idx := -1
		for i, r := range data.Circuits {
			if r.Name == name && r.Version == job.Version {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("%w: circuit %q version %d", ErrCircuitNotFound, name, job.Version)
		}
		if !data.Circuits[idx].Frozen {
			return fmt.Errorf("%w: circuit %q version %d is not frozen", ErrCircuitNotFrozen, name, job.Version)
		}
		hasSetup := false
		for _, su := range data.Setups {
			if su.Circuit == name && su.Version == job.Version {
				hasSetup = true
				break
			}
		}
		if !hasSetup {
			return fmt.Errorf("%w: circuit %q version %d has no recorded trusted setup", ErrTrustedSetupMissing, name, job.Version)
		}
		for _, j := range data.Jobs {
			if j.ID == job.ID {
				if j.Circuit == name && j.Version == job.Version && j.Kind == kind && j.Attempt == job.Attempt {
					out = j.toJob()
					return nil
				}
				return fmt.Errorf("%w: job id %q already exists with a different request", ErrConflict, job.ID)
			}
		}
		rec := jobRecord{
			ID:       job.ID,
			Circuit:  name,
			Version:  job.Version,
			Kind:     kind,
			Attempt:  job.Attempt,
			Artifact: "accepted",
		}
		data.Jobs = append(data.Jobs, rec)
		if err := s.save(data); err != nil {
			return err
		}
		out = rec.toJob()
		return nil
	})
	return out, err
}

// GetCircuit returns one circuit version.
func (s *Store) GetCircuit(name string, version int) (Circuit, error) {
	var out Circuit
	err := s.withLock(func() error {
		data, err := s.load()
		if err != nil {
			return err
		}
		for _, r := range data.Circuits {
			if r.Name == name && r.Version == version {
				out = r.toCircuit()
				return nil
			}
		}
		return fmt.Errorf("%w: circuit %q version %d", ErrCircuitNotFound, name, version)
	})
	return out, err
}

// GetSetup returns the trusted setup recorded for one circuit version.
func (s *Store) GetSetup(name string, version int) (SetupRecord, error) {
	var out SetupRecord
	err := s.withLock(func() error {
		data, err := s.load()
		if err != nil {
			return err
		}
		for _, su := range data.Setups {
			if su.Circuit == name && su.Version == version {
				out = su.toSetup()
				return nil
			}
		}
		return fmt.Errorf("%w: no trusted setup for circuit %q version %d", ErrTrustedSetupMissing, name, version)
	})
	return out, err
}

// ListCircuits returns all circuits ordered by name ascending, then
// version ascending.
func (s *Store) ListCircuits() ([]Circuit, error) {
	var out []Circuit
	err := s.withLock(func() error {
		data, err := s.load()
		if err != nil {
			return err
		}
		out = make([]Circuit, 0, len(data.Circuits))
		for _, r := range data.Circuits {
			out = append(out, r.toCircuit())
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Name != out[j].Name {
				return out[i].Name < out[j].Name
			}
			return out[i].Version < out[j].Version
		})
		return nil
	})
	return out, err
}

// ListSetups returns all trusted setup records ordered by circuit name
// ascending, then version ascending.
func (s *Store) ListSetups() ([]SetupRecord, error) {
	var out []SetupRecord
	err := s.withLock(func() error {
		data, err := s.load()
		if err != nil {
			return err
		}
		out = make([]SetupRecord, 0, len(data.Setups))
		for _, su := range data.Setups {
			out = append(out, su.toSetup())
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Circuit != out[j].Circuit {
				return out[i].Circuit < out[j].Circuit
			}
			return out[i].Version < out[j].Version
		})
		return nil
	})
	return out, err
}

// ListJobs returns all jobs ordered by id ascending.
func (s *Store) ListJobs() ([]Job, error) {
	var out []Job
	err := s.withLock(func() error {
		data, err := s.load()
		if err != nil {
			return err
		}
		out = make([]Job, 0, len(data.Jobs))
		for _, j := range data.Jobs {
			out = append(out, j.toJob())
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// internal persistence
// ---------------------------------------------------------------------------

const (
	circuitsFile = "circuits.json"
	setupsFile   = "setups.json"
	jobsFile     = "jobs.json"
	lockFile     = "store.lock"
)

type storeData struct {
	Circuits []circuitRecord
	Setups   []setupRecord
	Jobs     []jobRecord
}

type circuitRecord struct {
	Name        string `json:"name"`
	Version     int    `json:"version"`
	Description string `json:"description"`
	Constraints int    `json:"constraints"`
	Public      int    `json:"public_inputs"`
	Private     int    `json:"private_inputs"`
	Frozen      bool   `json:"frozen"`
}

type setupRecord struct {
	Circuit string `json:"circuit"`
	Version int    `json:"version"`
	SetupID string `json:"setup_id"`
}

type jobRecord struct {
	ID       string `json:"id"`
	Circuit  string `json:"circuit"`
	Version  int    `json:"version"`
	Kind     string `json:"kind"`
	Attempt  int    `json:"attempt"`
	Artifact string `json:"artifact"`
}

type circuitsDoc struct {
	FormatVersion int             `json:"format_version"`
	Circuits      []circuitRecord `json:"circuits"`
}

type setupsDoc struct {
	FormatVersion int           `json:"format_version"`
	Setups        []setupRecord `json:"setups"`
}

type jobsDoc struct {
	FormatVersion int         `json:"format_version"`
	Jobs          []jobRecord `json:"jobs"`
}

func (r circuitRecord) toCircuit() Circuit {
	return Circuit{
		Name:          r.Name,
		Version:       r.Version,
		Description:   r.Description,
		Constraints:   r.Constraints,
		PublicInputs:  r.Public,
		PrivateInputs: r.Private,
		Frozen:        r.Frozen,
	}
}

func (r setupRecord) toSetup() SetupRecord {
	return SetupRecord{Circuit: r.Circuit, Version: r.Version, SetupID: r.SetupID}
}

func (r jobRecord) toJob() Job {
	return Job{
		ID:       r.ID,
		Circuit:  r.Circuit,
		Version:  r.Version,
		Kind:     r.Kind,
		Attempt:  r.Attempt,
		Artifact: r.Artifact,
	}
}

func circuitKey(name string, version int) string {
	return fmt.Sprintf("%s\x00%d", name, version)
}

func setupID(c Circuit) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%d|%d", c.Name, c.Version, c.Constraints, c.PublicInputs, c.PrivateInputs)))
	return hex.EncodeToString(sum[:])
}

func validateCircuit(c Circuit) (string, error) {
	name := strings.TrimSpace(c.Name)
	if name == "" {
		return "", fmt.Errorf("%w: circuit name must not be empty or blank", ErrInvalidInput)
	}
	if c.Version <= 0 {
		return "", fmt.Errorf("%w: circuit version must be a positive integer, got %d", ErrInvalidInput, c.Version)
	}
	if c.Constraints <= 0 {
		return "", fmt.Errorf("%w: constraints must be greater than zero, got %d", ErrInvalidInput, c.Constraints)
	}
	if c.PublicInputs < 0 {
		return "", fmt.Errorf("%w: public inputs must not be negative, got %d", ErrInvalidInput, c.PublicInputs)
	}
	if c.PrivateInputs < 0 {
		return "", fmt.Errorf("%w: private inputs must not be negative, got %d", ErrInvalidInput, c.PrivateInputs)
	}
	return name, nil
}

// withLock serializes all store operations: a process-local mutex plus an
// exclusive flock on the data directory, so concurrent processes on the
// same directory cannot interleave reads and writes.
func (s *Store) withLock(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.OpenFile(filepath.Join(s.dir, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return err
	}
	defer func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}()

	return fn()
}

// load reads all three data files. A missing file means an empty
// collection; a truncated, malformed, or unsupported file is reported as
// ErrCorruptData and the file is never modified.
func (s *Store) load() (*storeData, error) {
	data := &storeData{}

	circuits, err := loadDoc[circuitsDoc](s, circuitsFile)
	if err != nil {
		return nil, err
	}
	if circuits != nil {
		data.Circuits = circuits.Circuits
	}
	setups, err := loadDoc[setupsDoc](s, setupsFile)
	if err != nil {
		return nil, err
	}
	if setups != nil {
		data.Setups = setups.Setups
	}
	jobs, err := loadDoc[jobsDoc](s, jobsFile)
	if err != nil {
		return nil, err
	}
	if jobs != nil {
		data.Jobs = jobs.Jobs
	}
	return data, nil
}

// loadDoc reads one versioned data file. A missing file returns nil, nil.
// Empty, malformed, or unsupported files are ErrCorruptData.
func loadDoc[T any](s *Store, name string) (*T, error) {
	raw, err := os.ReadFile(filepath.Join(s.dir, name))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: %s is empty", ErrCorruptData, name)
	}
	var head struct {
		FormatVersion int `json:"format_version"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("%w: %s is not valid JSON: %v", ErrCorruptData, name, err)
	}
	if head.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("%w: %s has format version %d, want %d", ErrCorruptData, name, head.FormatVersion, FormatVersion)
	}
	var doc T
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%w: %s is not valid data: %v", ErrCorruptData, name, err)
	}
	return &doc, nil
}

// save writes every data file atomically: a temp file in the same
// directory, fsync, rename, then fsync of the directory. A crash at any
// point leaves either the previous complete state or the new complete
// state, never a partial one.
func (s *Store) save(data *storeData) error {
	docs := []struct {
		name string
		doc  any
	}{
		{circuitsFile, circuitsDoc{FormatVersion: FormatVersion, Circuits: data.Circuits}},
		{setupsFile, setupsDoc{FormatVersion: FormatVersion, Setups: data.Setups}},
		{jobsFile, jobsDoc{FormatVersion: FormatVersion, Jobs: data.Jobs}},
	}
	for _, d := range docs {
		raw, err := json.MarshalIndent(d.doc, "", "  ")
		if err != nil {
			return err
		}
		raw = append(raw, '\n')
		if err := writeFileAtomic(filepath.Join(s.dir, d.name), raw); err != nil {
			return err
		}
	}
	return nil
}

func writeFileAtomic(path string, raw []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if _, err := f.Write(raw); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
