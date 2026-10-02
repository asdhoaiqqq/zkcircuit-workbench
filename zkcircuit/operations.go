package zkcircuit

import (
	"sort"
	"strings"
)

// This file holds the domain operations of the persistent workbench. Every
// mutating operation validates its request before touching state, runs under
// the shared process lock and either commits one complete new state or
// commits nothing.

// CreateCircuit creates a draft circuit version identified by Name+Version.
//
// The name must be non-empty and not only whitespace; Version must be
// positive; Constraints must be positive; the input counts must not be
// negative. Creating an already-existing version is idempotent when the
// description is identical (the stored record is returned unchanged); a
// differing description is reported as ErrConflict. Nothing about an
// existing version — frozen or not — is ever replaced by Create.
func (s *Store) CreateCircuit(c Circuit) (Circuit, error) {
	if err := validateCircuitKey(c.Name, c.Version); err != nil {
		return Circuit{}, err
	}
	if err := validateCounts(c.Constraints, c.PublicInputs, c.PrivateInputs); err != nil {
		return Circuit{}, err
	}

	var result Circuit
	err := s.withLock(func() (bool, error) {
		if existing := findCircuit(s.data.Circuits, c.Name, c.Version); existing != nil {
			if existing.Description != c.Description {
				return false, conflictf("circuit %q version %d already exists with a different description", c.Name, c.Version)
			}
			result = circuitFromPersist(*existing)
			return false, nil
		}
		record := persistCircuit{
			Name: c.Name, Version: c.Version,
			Constraints: c.Constraints, PublicInputs: c.PublicInputs, PrivateInputs: c.PrivateInputs,
			Frozen: false, Description: c.Description,
		}
		s.data.Circuits = append(s.data.Circuits, record)
		result = circuitFromPersist(record)
		return true, nil
	})
	return result, err
}

// UpdateCircuit modifies a draft circuit version. It must target an existing,
// unfrozen version explicitly: unknown versions yield ErrNotFound and frozen
// versions yield ErrFrozen. On success the counts and description are
// replaced as one atomic change; partial updates can never be observed.
func (s *Store) UpdateCircuit(c Circuit) (Circuit, error) {
	if err := validateCircuitKey(c.Name, c.Version); err != nil {
		return Circuit{}, err
	}
	if err := validateCounts(c.Constraints, c.PublicInputs, c.PrivateInputs); err != nil {
		return Circuit{}, err
	}

	var result Circuit
	err := s.withLock(func() (bool, error) {
		existing := findCircuit(s.data.Circuits, c.Name, c.Version)
		if existing == nil {
			return false, notFoundf("circuit %q version %d does not exist", c.Name, c.Version)
		}
		if existing.Frozen {
			return false, frozenf("circuit %q version %d is frozen and its description cannot be modified", c.Name, c.Version)
		}
		// When a definition is already imported, changing the three counts is
		// only accepted as a whole if the definition stays legal under them
		// (exact constraint count, all wires inside the new input layout).
		if existing.Definition != nil {
			parsed, perr := definitionFromPersist(*existing.Definition, existing.PublicInputs, existing.PrivateInputs)
			if perr != nil {
				return false, corruptf("stored definition for %q v%d is unreadable: %v", c.Name, c.Version, perr)
			}
			if !parsed.compatibleWith(c.Constraints, c.PublicInputs, c.PrivateInputs) {
				return false, invalidf("update rejected: it would make the imported constraint definition illegal (constraint count or wire layout mismatch); the whole change is refused")
			}
		}
		existing.Constraints = c.Constraints
		existing.PublicInputs = c.PublicInputs
		existing.PrivateInputs = c.PrivateInputs
		existing.Description = c.Description
		result = circuitFromPersist(*existing)
		return true, nil
	})
	return result, err
}

// PartialCircuit carries optional replacement fields for one draft circuit
// version. A nil pointer leaves the stored field untouched; a non-nil pointer
// — even to zero or to the empty string — replaces the field. This lets a
// caller change exactly one field without restating the others.
type PartialCircuit struct {
	Constraints   *int
	PublicInputs  *int
	PrivateInputs *int
	Description   *string
}

// UpdateCircuitPartial modifies selected fields of a draft circuit version.
//
// Only fields present in patch are changed; omitted fields keep their stored
// values. The whole merged result is validated before anything is committed:
// constraints must stay positive and input counts non-negative, and when a
// constraint definition is already imported the merged counts must keep it
// legal (exact constraint count, every referenced wire inside the new input
// layout). Any rejected field refuses the entire change — description,
// counts and the stored definition all remain as they were.
//
// Unknown versions yield ErrNotFound and frozen versions ErrFrozen even when
// no field is being changed. A patch that provides no modifiable field at
// all returns the stored record unchanged and commits nothing (no new
// version, no data write).
func (s *Store) UpdateCircuitPartial(name string, version int, patch PartialCircuit) (Circuit, error) {
	if err := validateCircuitKey(name, version); err != nil {
		return Circuit{}, err
	}

	var result Circuit
	err := s.withLock(func() (bool, error) {
		existing := findCircuit(s.data.Circuits, name, version)
		if existing == nil {
			return false, notFoundf("circuit %q version %d does not exist", name, version)
		}
		if existing.Frozen {
			return false, frozenf("circuit %q version %d is frozen and cannot be modified", name, version)
		}
		// Merge the provided fields over the stored record; omitted fields
		// keep their committed values.
		merged := *existing
		if patch.Constraints != nil {
			merged.Constraints = *patch.Constraints
		}
		if patch.PublicInputs != nil {
			merged.PublicInputs = *patch.PublicInputs
		}
		if patch.PrivateInputs != nil {
			merged.PrivateInputs = *patch.PrivateInputs
		}
		if patch.Description != nil {
			merged.Description = *patch.Description
		}
		if err := validateCounts(merged.Constraints, merged.PublicInputs, merged.PrivateInputs); err != nil {
			return false, err
		}
		// When a definition is already imported, the merged counts must keep
		// it legal (exact constraint count, all wires inside the new input
		// layout). The definition itself is never touched here.
		if existing.Definition != nil {
			parsed, perr := definitionFromPersist(*existing.Definition, existing.PublicInputs, existing.PrivateInputs)
			if perr != nil {
				return false, corruptf("stored definition for %q v%d is unreadable: %v", name, version, perr)
			}
			if !parsed.compatibleWith(merged.Constraints, merged.PublicInputs, merged.PrivateInputs) {
				return false, invalidf("update rejected: it would make the imported constraint definition illegal (constraint count or wire layout mismatch); the whole change is refused")
			}
		}
		// No modifiable field was provided: return the stored record without
		// committing anything.
		if patch.Constraints == nil && patch.PublicInputs == nil && patch.PrivateInputs == nil && patch.Description == nil {
			result = circuitFromPersist(*existing)
			return false, nil
		}
		existing.Constraints = merged.Constraints
		existing.PublicInputs = merged.PublicInputs
		existing.PrivateInputs = merged.PrivateInputs
		existing.Description = merged.Description
		result = circuitFromPersist(*existing)
		return true, nil
	})
	return result, err
}

// CopyCircuit copies a frozen circuit version into a new draft version of the
// same circuit, identified by toVersion.
//
// The source version must exist (ErrNotFound) and be frozen (ErrNotFrozen);
// the source is checked before any existing target is considered. toVersion
// must be a positive integer different from version — it need not be
// consecutive or larger. The copy carries the source's description, the three
// counts and, when the source has an imported constraint definition, the
// whole definition (modulus, constraint order and wire layout) in its
// canonical form. A source with only registered counts and no definition
// copies into a target that likewise has no definition, ready for a later
// import. The new version is an ordinary draft: update, partial update,
// import and freeze all apply to it, and changing it never touches the
// source's definition, frozen state, compiled artifact or jobs.
//
// Trusted setups, compiled artifacts and jobs belong to their own version:
// the copy gets none of them, and no hash of the source is bound to the
// target. The target needs its own setup registration before it accepts prove
// jobs, and once frozen (with a definition) it compiles into an artifact
// whose hash carries the target's own name+version identity.
//
// Repeating the same copy is idempotent: when the target is still a draft
// whose description, three counts and definition all match the source (by the
// canonical definition semantics — no definition and some definition are
// different states), the stored target is returned unchanged and nothing is
// committed. A frozen target, or a draft target whose description, counts or
// definition differ — however it was created — yields ErrConflict, and
// nothing is added or changed.
func (s *Store) CopyCircuit(name string, version, toVersion int) (Circuit, error) {
	if err := validateCircuitKey(name, version); err != nil {
		return Circuit{}, err
	}
	if toVersion <= 0 {
		return Circuit{}, invalidf("circuit %q target version must be a positive integer, got %d", name, toVersion)
	}
	if version == toVersion {
		return Circuit{}, invalidf("circuit %q target version %d must differ from the source version", name, toVersion)
	}

	var result Circuit
	err := s.withLock(func() (bool, error) {
		source := findCircuit(s.data.Circuits, name, version)
		if source == nil {
			return false, notFoundf("circuit %q version %d does not exist", name, version)
		}
		if !source.Frozen {
			return false, notFrozenf("circuit %q version %d must be frozen before it can be copied", name, version)
		}
		if existing := findCircuit(s.data.Circuits, name, toVersion); existing != nil {
			if existing.Frozen {
				return false, conflictf("circuit %q version %d is frozen and cannot be replaced by a copy", name, toVersion)
			}
			if existing.Description == source.Description &&
				existing.Constraints == source.Constraints &&
				existing.PublicInputs == source.PublicInputs &&
				existing.PrivateInputs == source.PrivateInputs &&
				sameDefinition(existing.Definition, source.Definition) {
				result = circuitFromPersist(*existing)
				return false, nil
			}
			return false, conflictf("circuit %q version %d already exists with different content", name, toVersion)
		}
		record := persistCircuit{
			Name: name, Version: toVersion,
			Constraints: source.Constraints, PublicInputs: source.PublicInputs, PrivateInputs: source.PrivateInputs,
			Frozen: false, Description: source.Description,
			Definition: cloneDefinition(source.Definition),
		}
		s.data.Circuits = append(s.data.Circuits, record)
		result = circuitFromPersist(record)
		return true, nil
	})
	return result, err
}

// FreezeCircuit freezes a circuit version. Unknown versions yield
// ErrNotFound. Freezing an already-frozen version returns that version
// unchanged (idempotent). Once frozen, name, version and the three counts
// can never change again.
func (s *Store) FreezeCircuit(name string, version int) (Circuit, error) {
	if err := validateCircuitKey(name, version); err != nil {
		return Circuit{}, err
	}

	var result Circuit
	err := s.withLock(func() (bool, error) {
		existing := findCircuit(s.data.Circuits, name, version)
		if existing == nil {
			return false, notFoundf("circuit %q version %d does not exist", name, version)
		}
		if existing.Frozen {
			result = circuitFromPersist(*existing)
			return false, nil
		}
		existing.Frozen = true
		result = circuitFromPersist(*existing)
		return true, nil
	})
	return result, err
}

// RecordSetup records a trusted setup for one frozen circuit version. The
// record belongs exclusively to that name+version. Unknown versions yield
// ErrNotFound; non-frozen versions yield ErrNotFrozen. Re-registering a
// setup for the same version is idempotent and adds no duplicate record.
func (s *Store) RecordSetup(name string, version int) (Setup, error) {
	if err := validateCircuitKey(name, version); err != nil {
		return Setup{}, err
	}

	var result Setup
	err := s.withLock(func() (bool, error) {
		circuit := findCircuit(s.data.Circuits, name, version)
		if circuit == nil {
			return false, notFoundf("circuit %q version %d does not exist", name, version)
		}
		if !circuit.Frozen {
			return false, notFrozenf("circuit %q version %d must be frozen before its trusted setup is recorded", name, version)
		}
		if existing := findSetup(s.data.Setups, name, version); existing != nil {
			result = setupFromPersist(*existing)
			return false, nil
		}
		record := persistSetup{Name: name, Version: version}
		s.data.Setups = append(s.data.Setups, record)
		result = setupFromPersist(record)
		return true, nil
	})
	return result, err
}

// SubmitJob accepts and stores a prove job. Only kind "prove" is accepted;
// no proving computation runs, the accepted request alone is persisted.
//
// The job id must be non-empty and not only whitespace, circuit name and
// version must be explicit and Attempt must be positive. Failure reasons are
// distinguishable: ErrNotFound (version unknown), ErrNotFrozen (version is
// still a draft) and ErrSetupMissing (no trusted setup for that version). A
// refused submission leaves no job behind.
//
// A non-empty CompiledHash additionally binds the job to the pinned
// version's compiled artifact, matched as the exact string: a version with
// no compiled artifact yields ErrArtifactMissing and a hash that differs
// from that artifact's hash yields ErrArtifactMismatch. Artifacts of other
// names or versions never satisfy the binding. An empty CompiledHash leaves
// the job unbound and keeps the register-only behavior.
//
// Resubmitting the same id with the same request — including the same
// optional hash — returns the stored job; the same id with different content
// yields ErrConflict, reported before any artifact check of the new request.
// A stored job stays pinned to the version submitted, regardless of circuit
// versions added later.
func (s *Store) SubmitJob(job Job) (Job, error) {
	if strings.TrimSpace(job.ID) == "" {
		return Job{}, invalidf("job id must be non-empty and not only whitespace")
	}
	if strings.TrimSpace(job.Circuit) == "" {
		return Job{}, invalidf("job %q must name a circuit", job.ID)
	}
	if job.Version <= 0 {
		return Job{}, invalidf("job %q must pin a positive circuit version", job.ID)
	}
	if job.Attempt <= 0 {
		return Job{}, invalidf("job %q attempt must be a positive integer", job.ID)
	}
	if job.Kind != "prove" {
		return Job{}, StoreError{Kind: ErrUnsupportedKind.Kind,
			Detail: "job " + job.ID + " has unsupported kind " + job.Kind + "; only \"prove\" jobs are accepted"}
	}

	var result Job
	err := s.withLock(func() (bool, error) {
		if existing := findJob(s.data.Jobs, job.ID); existing != nil {
			if !sameJobRequest(*existing, job) {
				return false, conflictf("job %q already exists with a different request", job.ID)
			}
			result = jobFromPersist(*existing)
			return false, nil
		}
		circuit := findCircuit(s.data.Circuits, job.Circuit, job.Version)
		if circuit == nil {
			return false, notFoundf("job %q references unknown circuit %q version %d", job.ID, job.Circuit, job.Version)
		}
		if !circuit.Frozen {
			return false, notFrozenf("job %q references circuit %q version %d which is not frozen", job.ID, job.Circuit, job.Version)
		}
		if findSetup(s.data.Setups, job.Circuit, job.Version) == nil {
			return false, setupMissingf("job %q references frozen circuit %q version %d which has no recorded trusted setup",
				job.ID, job.Circuit, job.Version)
		}
		if job.CompiledHash != "" {
			artifact := findArtifact(s.data.Artifacts, job.Circuit, job.Version)
			if artifact == nil {
				return false, artifactMissingf("job %q references circuit %q version %d which has no compiled artifact",
					job.ID, job.Circuit, job.Version)
			}
			if artifact.Hash != job.CompiledHash {
				return false, artifactMismatchf("artifact hash %q does not belong to circuit %q version %d (bound hash %q)",
					job.CompiledHash, job.Circuit, job.Version, artifact.Hash)
			}
		}
		record := persistJob{
			ID: job.ID, Circuit: job.Circuit, Version: job.Version,
			Kind: job.Kind, Attempt: job.Attempt, Artifact: job.Artifact,
			CompiledHash: job.CompiledHash,
		}
		s.data.Jobs = append(s.data.Jobs, record)
		result = jobFromPersist(record)
		return true, nil
	})
	return result, err
}

// GetCircuit returns one circuit version, sorted-field populated exactly as
// stored. Unknown versions yield ErrNotFound.
func (s *Store) GetCircuit(name string, version int) (Circuit, error) {
	if err := validateCircuitKey(name, version); err != nil {
		return Circuit{}, err
	}
	var result Circuit
	err := s.withLockRead(func() error {
		existing := findCircuit(s.data.Circuits, name, version)
		if existing == nil {
			return notFoundf("circuit %q version %d does not exist", name, version)
		}
		result = circuitFromPersist(*existing)
		return nil
	})
	return result, err
}

// ListCircuits returns all stored circuit versions ordered by circuit name
// (lexicographically) and then by ascending version number.
func (s *Store) ListCircuits() ([]Circuit, error) {
	var result []Circuit
	err := s.withLockRead(func() error {
		ordered := append([]persistCircuit(nil), s.data.Circuits...)
		sort.Slice(ordered, func(i, j int) bool {
			if ordered[i].Name != ordered[j].Name {
				return ordered[i].Name < ordered[j].Name
			}
			return ordered[i].Version < ordered[j].Version
		})
		result = make([]Circuit, len(ordered))
		for i, c := range ordered {
			result[i] = circuitFromPersist(c)
		}
		return nil
	})
	return result, err
}

// GetSetup reports whether a trusted setup is registered for the named
// frozen version. A missing record yields ErrNotFound.
func (s *Store) GetSetup(name string, version int) (Setup, bool, error) {
	if err := validateCircuitKey(name, version); err != nil {
		return Setup{}, false, err
	}
	var result Setup
	found := false
	err := s.withLockRead(func() error {
		existing := findSetup(s.data.Setups, name, version)
		if existing == nil {
			return nil
		}
		result = setupFromPersist(*existing)
		found = true
		return nil
	})
	return result, found, err
}

// GetJob returns one stored job by id. Unknown ids yield ErrNotFound.
func (s *Store) GetJob(id string) (Job, error) {
	if strings.TrimSpace(id) == "" {
		return Job{}, invalidf("job id must be non-empty and not only whitespace")
	}
	var result Job
	err := s.withLockRead(func() error {
		existing := findJob(s.data.Jobs, id)
		if existing == nil {
			return notFoundf("job %q does not exist", id)
		}
		result = jobFromPersist(*existing)
		return nil
	})
	return result, err
}

// ListJobs returns all accepted jobs ordered by job id lexicographically.
func (s *Store) ListJobs() ([]Job, error) {
	var result []Job
	err := s.withLockRead(func() error {
		ordered := append([]persistJob(nil), s.data.Jobs...)
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
		result = make([]Job, len(ordered))
		for i, j := range ordered {
			result[i] = jobFromPersist(j)
		}
		return nil
	})
	return result, err
}

// --- validation helpers ---------------------------------------------------

func validateCircuitKey(name string, version int) error {
	if strings.TrimSpace(name) == "" {
		return invalidf("circuit name must be non-empty and not only whitespace")
	}
	if version <= 0 {
		return invalidf("circuit %q version must be a positive integer, got %d", name, version)
	}
	return nil
}

func validateCounts(constraints, publicInputs, privateInputs int) error {
	if constraints <= 0 {
		return invalidf("constraints must be greater than zero, got %d", constraints)
	}
	if publicInputs < 0 {
		return invalidf("public input count must not be negative, got %d", publicInputs)
	}
	if privateInputs < 0 {
		return invalidf("private input count must not be negative, got %d", privateInputs)
	}
	return nil
}

// --- lookup / mapping helpers --------------------------------------------

func findCircuit(cs []persistCircuit, name string, version int) *persistCircuit {
	for i := range cs {
		if cs[i].Name == name && cs[i].Version == version {
			return &cs[i]
		}
	}
	return nil
}

func findSetup(ps []persistSetup, name string, version int) *persistSetup {
	for i := range ps {
		if ps[i].Name == name && ps[i].Version == version {
			return &ps[i]
		}
	}
	return nil
}

func findJob(js []persistJob, id string) *persistJob {
	for i := range js {
		if js[i].ID == id {
			return &js[i]
		}
	}
	return nil
}

func sameJobRequest(stored persistJob, request Job) bool {
	return stored.Circuit == request.Circuit &&
		stored.Version == request.Version &&
		stored.Kind == request.Kind &&
		stored.Attempt == request.Attempt &&
		stored.CompiledHash == request.CompiledHash
}

// cloneDefinition deep-copies a stored definition so the copy and its source
// never share mutable state.
func cloneDefinition(d *persistDefinition) *persistDefinition {
	if d == nil {
		return nil
	}
	out := persistDefinition{
		Modulus:     d.Modulus,
		Constraints: make([]persistConstraint, len(d.Constraints)),
	}
	for i, c := range d.Constraints {
		out.Constraints[i] = persistConstraint{
			A: cloneTerms(c.A),
			B: cloneTerms(c.B),
			C: cloneTerms(c.C),
		}
	}
	return &out
}

func cloneTerms(ts []persistTerm) []persistTerm {
	if ts == nil {
		return nil
	}
	return append([]persistTerm(nil), ts...)
}

// sameDefinition reports whether two stored definitions are equal. Stored
// definitions are always kept in canonical form, so structural equality is
// exactly the canonical equality semantics; a missing definition matches only
// another missing one.
func sameDefinition(a, b *persistDefinition) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	if a.Modulus != b.Modulus || len(a.Constraints) != len(b.Constraints) {
		return false
	}
	for i := range a.Constraints {
		if !sameTerms(a.Constraints[i].A, b.Constraints[i].A) ||
			!sameTerms(a.Constraints[i].B, b.Constraints[i].B) ||
			!sameTerms(a.Constraints[i].C, b.Constraints[i].C) {
			return false
		}
	}
	return true
}

func sameTerms(a, b []persistTerm) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func circuitFromPersist(c persistCircuit) Circuit {
	return Circuit{
		Name: c.Name, Version: c.Version,
		Constraints: c.Constraints, PublicInputs: c.PublicInputs, PrivateInputs: c.PrivateInputs,
		Frozen: c.Frozen, Description: c.Description,
	}
}

func setupFromPersist(p persistSetup) Setup {
	return Setup{Name: p.Name, Version: p.Version}
}

func jobFromPersist(j persistJob) Job {
	return Job{
		ID: j.ID, Circuit: j.Circuit, Version: j.Version,
		Kind: j.Kind, Attempt: j.Attempt, Artifact: j.Artifact,
		CompiledHash: j.CompiledHash,
	}
}
