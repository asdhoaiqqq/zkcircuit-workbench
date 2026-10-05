package zkcircuit

import (
	"math"
	"sort"
	"strings"
)

// This file holds the domain operations of the persistent workbench. Every
// mutating operation validates its request before touching state, runs under
// the shared process lock and either commits one complete new state or
// commits nothing.

// CreateCircuit creates a draft circuit version identified by Name+Version.
//
// The name must be non-empty, not only whitespace, and complete, legal
// UTF-8: it is the version's identity, so it must survive committing and
// reading back byte-for-byte rather than being silently rewritten (invalid
// bytes would persist as U+FFFD and the circuit would change names).
// Version must be positive; Constraints must be positive; the input counts
// must not be negative. Creating an already-existing version is idempotent
// when the description is identical (the stored record is returned
// unchanged); a differing description is reported as ErrConflict. Nothing
// about an existing version — frozen or not — is ever replaced by Create.
func (s *Store) CreateCircuit(c Circuit) (Circuit, error) {
	if err := validateCircuitKey(c.Name, c.Version); err != nil {
		return Circuit{}, err
	}
	if err := validateCircuitNameUTF8(c.Name); err != nil {
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
//
// This is the whole-record replacement entry point: the three counts and the
// description carried by c override the stored record outright (zero values
// included). Field-by-field editing with omitted-field preservation lives in
// UpdateCircuitPartial; both entry points run the same draft-update rules
// through the circuitMutation helpers below.
func (s *Store) UpdateCircuit(c Circuit) (Circuit, error) {
	if err := validateCircuitKey(c.Name, c.Version); err != nil {
		return Circuit{}, err
	}
	mutation := circuitMutation{
		Constraints:   c.Constraints,
		PublicInputs:  c.PublicInputs,
		PrivateInputs: c.PrivateInputs,
		Description:   c.Description,
	}
	// Whole replacement judges the supplied counts before the target is
	// looked up, so an illegal replacement reports invalid argument even for
	// unknown or frozen versions.
	if err := validateMutationCounts(mutation); err != nil {
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
		if err := validateMutationDefinition(*existing, mutation); err != nil {
			return false, err
		}
		result = applyCircuitMutation(existing, mutation)
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
// values. A non-nil pointer still replaces its field even when it points at
// zero or the empty string (explicit zero clears an input count, an empty
// string clears the description). The whole merged result is validated
// before anything is committed: constraints must stay positive and input
// counts non-negative, and when a constraint definition is already imported
// the merged counts must keep it legal (exact constraint count, every
// referenced wire inside the new input layout). Any rejected field refuses
// the entire change — description, counts and the stored definition all
// remain as they were. The merged result is checked through the same
// circuitMutation helpers as UpdateCircuit's whole replacement.
//
// Existence and frozen state are settled before the merged counts are
// judged: unknown versions yield ErrNotFound and frozen versions
// ErrFrozen, even when the patch carries an illegal count or no field at
// all. A patch that provides no modifiable field at all returns the stored
// record unchanged and commits nothing (no new version, no data write).
func (s *Store) UpdateCircuitPartial(name string, version int, patch PartialCircuit) (Circuit, error) {
	if err := validateCircuitKey(name, version); err != nil {
		return Circuit{}, err
	}
	hasField := patch.Constraints != nil || patch.PublicInputs != nil ||
		patch.PrivateInputs != nil || patch.Description != nil

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
		// keep their committed values, so the merged counts are always the
		// complete result this modification would leave behind.
		mutation := circuitMutation{
			Constraints:   existing.Constraints,
			PublicInputs:  existing.PublicInputs,
			PrivateInputs: existing.PrivateInputs,
			Description:   existing.Description,
		}
		if patch.Constraints != nil {
			mutation.Constraints = *patch.Constraints
		}
		if patch.PublicInputs != nil {
			mutation.PublicInputs = *patch.PublicInputs
		}
		if patch.PrivateInputs != nil {
			mutation.PrivateInputs = *patch.PrivateInputs
		}
		if patch.Description != nil {
			mutation.Description = *patch.Description
		}
		if err := validateMutationCounts(mutation); err != nil {
			return false, err
		}
		if err := validateMutationDefinition(*existing, mutation); err != nil {
			return false, err
		}
		// No modifiable field was provided: return the stored record without
		// committing anything.
		if !hasField {
			result = circuitFromPersist(*existing)
			return false, nil
		}
		result = applyCircuitMutation(existing, mutation)
		return true, nil
	})
	return result, err
}

// --- draft update internals -----------------------------------------------
//
// circuitMutation is the complete set of updatable draft fields one
// modification would leave committed: the three counts and the description.
// Both public update entry points resolve their request into this type —
// UpdateCircuit takes all four values from the request, UpdateCircuitPartial
// merges request fields over the stored record — and then share the same
// count rules, imported-definition compatibility check and commit, so the
// two usages can never drift apart. A mutation never replaces the stored
// constraint definition; it is only checked against it.
type circuitMutation struct {
	Constraints   int
	PublicInputs  int
	PrivateInputs int
	Description   string
}

// validateMutationCounts holds the basic count rules for any draft update:
// constraints must be greater than zero and both input counts must not be
// negative. The rules are judged on the mutation's complete resulting
// counts, not on the supplied fields alone.
func validateMutationCounts(m circuitMutation) error {
	return validateCounts(m.Constraints, m.PublicInputs, m.PrivateInputs)
}

// validateMutationDefinition holds the rule shared with imported
// definitions: when the version already has one, the mutation's complete
// resulting counts must keep it legal — exactly as many constraints as the
// definition and every referenced wire inside the resulting public/private
// input layout. An unreadable stored definition is reported as data
// corruption. A version without a definition imposes no extra rule.
func validateMutationDefinition(existing persistCircuit, m circuitMutation) error {
	if existing.Definition == nil {
		return nil
	}
	parsed, perr := definitionFromPersist(*existing.Definition, existing.PublicInputs, existing.PrivateInputs)
	if perr != nil {
		return corruptf("stored definition for %q v%d is unreadable: %v", existing.Name, existing.Version, perr)
	}
	if !parsed.compatibleWith(m.Constraints, m.PublicInputs, m.PrivateInputs) {
		return invalidf("update rejected: it would make the imported constraint definition illegal (constraint count or wire layout mismatch); the whole change is refused")
	}
	return nil
}

// applyCircuitMutation commits a validated mutation onto the stored draft
// record, overwriting the three counts and the description together. The
// name, version, frozen flag and imported definition are never touched. The
// caller is responsible for all validation and for holding the store lock.
func applyCircuitMutation(existing *persistCircuit, m circuitMutation) Circuit {
	existing.Constraints = m.Constraints
	existing.PublicInputs = m.PublicInputs
	existing.PrivateInputs = m.PrivateInputs
	existing.Description = m.Description
	return circuitFromPersist(*existing)
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

// CopyCircuit creates a new draft version of a circuit name by copying a
// frozen source version. The target version must be a positive integer
// different from the source version; it need not be consecutive or greater.
//
// The copy carries over the source's description, constraint count, public
// and private input counts, and — when the source has one — its complete
// constraint definition (modulus, constraint order and wire layout). A
// counts-only source produces a counts-only target with no definition. The
// new draft is fully independent: updating, re-importing or freezing it
// never touches the source, and the source's compiled artifacts, trusted
// setup and registered jobs are never copied or bound to the target. The
// target must register its own setup and compile its own artifact after
// freezing; the source's hash cannot satisfy the target's input checks or
// job bindings.
//
// Copying is idempotent: if the target already exists as a draft whose
// description, counts and definition are semantically equal to the source,
// the stored target is returned unchanged and nothing is written. A frozen
// target or a target whose description, counts or definition has changed
// yields ErrConflict. An unknown source yields ErrNotFound and an unfrozen
// source ErrNotFrozen; the source is always checked before the target.
func (s *Store) CopyCircuit(name string, version, toVersion int) (Circuit, error) {
	if err := validateCircuitKey(name, version); err != nil {
		return Circuit{}, err
	}
	if err := validateCircuitKey(name, toVersion); err != nil {
		return Circuit{}, err
	}
	if version == toVersion {
		return Circuit{}, invalidf("source and target version must differ for circuit %q", name)
	}

	var result Circuit
	err := s.withLock(func() (bool, error) {
		source := findCircuit(s.data.Circuits, name, version)
		if source == nil {
			return false, notFoundf("circuit %q version %d does not exist", name, version)
		}
		if !source.Frozen {
			return false, notFrozenf("circuit %q version %d must be frozen before it can be copied as a new draft", name, version)
		}
		target := findCircuit(s.data.Circuits, name, toVersion)
		if target != nil {
			if target.Frozen {
				return false, conflictf("circuit %q version %d already exists and is frozen", name, toVersion)
			}
			if !circuitCopyMatches(*source, *target) {
				return false, conflictf("circuit %q version %d already exists with content that differs from the source", name, toVersion)
			}
			result = circuitFromPersist(*target)
			return false, nil
		}
		record := persistCircuit{
			Name: name, Version: toVersion,
			Constraints: source.Constraints, PublicInputs: source.PublicInputs, PrivateInputs: source.PrivateInputs,
			Frozen: false, Description: source.Description,
		}
		if source.Definition != nil {
			def := *source.Definition
			record.Definition = &def
		}
		s.data.Circuits = append(s.data.Circuits, record)
		result = circuitFromPersist(record)
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

// maxInputWires is the largest declared input count that stays representable
// on the running platform. Wire numbering always reserves wire 0 for the
// constant one, so the wires a version names are 0 plus its public and private
// inputs: the declared counts must satisfy 1 + public + private <= M, where M
// is the largest positive value the platform's int can hold. The check is done
// without ever adding the counts, so a layout whose sum would wrap (e.g.
// public=M with one private input) is refused rather than accepted through
// integer wraparound as a small, representable layout.
const maxInputWires = math.MaxInt

// inputLayoutRepresentable reports whether the constant wire plus the public
// and private inputs fits inside the platform's int. Callers already guarantee
// both counts are non-negative.
func inputLayoutRepresentable(publicInputs, privateInputs int) bool {
	return publicInputs <= maxInputWires-1 && privateInputs <= maxInputWires-1-publicInputs
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
	if !inputLayoutRepresentable(publicInputs, privateInputs) {
		return invalidf("input layout is not representable: 1 constant wire + %d public + %d private inputs exceeds the platform limit of %d wires",
			publicInputs, privateInputs, maxInputWires)
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

func circuitFromPersist(c persistCircuit) Circuit {
	return Circuit{
		Name: c.Name, Version: c.Version,
		Constraints: c.Constraints, PublicInputs: c.PublicInputs, PrivateInputs: c.PrivateInputs,
		Frozen: c.Frozen, Description: c.Description,
	}
}

// circuitCopyMatches reports whether target is the same draft content that a
// copy of source would create: same description, same three counts and a
// semantically equal constraint definition. Definition equality follows the
// existing normalization semantics (terms merged per wire, coefficients
// reduced modulo p, zeros dropped, wires sorted ascending); a missing
// definition differs from a present one.
func circuitCopyMatches(source, target persistCircuit) bool {
	if source.Description != target.Description {
		return false
	}
	if source.Constraints != target.Constraints ||
		source.PublicInputs != target.PublicInputs ||
		source.PrivateInputs != target.PrivateInputs {
		return false
	}
	return definitionsEqual(source.Definition, target.Definition, source.PublicInputs, source.PrivateInputs)
}

// definitionsEqual compares two stored definitions for semantic equality.
// Both nil is equal; one nil and the other not is different. When both are
// present they are parsed through the existing normalization pipeline and
// compared by modulus, constraint count and each side's wire/coefficient
// pairs. The public/private layout is the source's; the caller has already
// confirmed the counts match.
func definitionsEqual(d1, d2 *persistDefinition, public, private int) bool {
	if d1 == nil && d2 == nil {
		return true
	}
	if d1 == nil || d2 == nil {
		return false
	}
	p1, err := definitionFromPersist(*d1, public, private)
	if err != nil {
		return false
	}
	p2, err := definitionFromPersist(*d2, public, private)
	if err != nil {
		return false
	}
	if p1.modulus != p2.modulus || len(p1.constraints) != len(p2.constraints) {
		return false
	}
	for i := range p1.constraints {
		if !canonicalTermsEqual(p1.constraints[i].a, p2.constraints[i].a) ||
			!canonicalTermsEqual(p1.constraints[i].b, p2.constraints[i].b) ||
			!canonicalTermsEqual(p1.constraints[i].c, p2.constraints[i].c) {
			return false
		}
	}
	return true
}

// canonicalTermsEqual reports whether two normalized term slices are
// identical wire-by-wire and coefficient-by-coefficient.
func canonicalTermsEqual(t1, t2 []parsedTerm) bool {
	if len(t1) != len(t2) {
		return false
	}
	for i := range t1 {
		if t1[i].wire != t2[i].wire || t1[i].value != t2[i].value {
			return false
		}
	}
	return true
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
