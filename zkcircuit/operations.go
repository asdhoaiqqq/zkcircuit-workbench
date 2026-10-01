package zkcircuit

import (
	"fmt"
	"math/big"
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
		// An imported definition pins the constraint count and the wire
		// range. Changing the counts so the definition would become illegal
		// rejects the whole update; the stored definition stays exactly as
		// imported.
		if existing.Definition != nil {
			if len(existing.Definition) != c.Constraints {
				return false, invalidf("cannot change the constraint count of %q version %d to %d: its imported definition has %d constraints",
					c.Name, c.Version, c.Constraints, len(existing.Definition))
			}
			maxWire := c.PublicInputs + c.PrivateInputs
			for _, con := range existing.Definition {
				for _, side := range []struct {
					name  string
					terms []persistTerm
				}{{"a", con.A}, {"b", con.B}, {"c", con.C}} {
					for _, t := range side.terms {
						if t.Wire > maxWire {
							return false, invalidf("cannot change the input counts of %q version %d: its definition references wire %d but only %d inputs are declared",
								c.Name, c.Version, t.Wire, maxWire)
						}
					}
				}
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
// Resubmitting the same id with the same request returns the stored job;
// the same id with different content yields ErrConflict. A stored job stays
// pinned to the version submitted, regardless of circuit versions added
// later.
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
		record := persistJob{
			ID: job.ID, Circuit: job.Circuit, Version: job.Version,
			Kind: job.Kind, Attempt: job.Attempt, Artifact: job.Artifact,
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

// ImportConstraints stores a constraint definition for a draft circuit
// version, replacing any previously imported definition wholesale.
//
// The version must exist and remain a draft: unknown versions yield
// ErrNotFound, frozen versions yield ErrFrozen. The definition's constraint
// count must equal the version's declared constraint count, and every wire
// must refer to a declared input (wire 0 is the constant one); violations
// yield ErrInvalidArgument and leave the stored definition untouched. The
// definition is canonicalized on import, so later whitespace, term order or
// coefficient-multiple variants never change the stored form.
func (s *Store) ImportConstraints(name string, version int, def ConstraintDefinition) (Circuit, error) {
	if err := validateCircuitKey(name, version); err != nil {
		return Circuit{}, err
	}
	m, err := validateDefinitionShape(def)
	if err != nil {
		return Circuit{}, err
	}

	var result Circuit
	err = s.withLock(func() (bool, error) {
		existing := findCircuit(s.data.Circuits, name, version)
		if existing == nil {
			return false, notFoundf("circuit %q version %d does not exist", name, version)
		}
		if existing.Frozen {
			return false, frozenf("circuit %q version %d is frozen and its constraint definition cannot be replaced", name, version)
		}
		if len(def.Constraints) != existing.Constraints {
			return false, invalidf("constraint definition has %d constraints but circuit %q version %d declares %d",
				len(def.Constraints), name, version, existing.Constraints)
		}
		maxWire := existing.PublicInputs + existing.PrivateInputs
		for i, c := range def.Constraints {
			for _, side := range []struct {
				name  string
				terms []Term
			}{{"a", c.A}, {"b", c.B}, {"c", c.C}} {
				for _, t := range side.terms {
					if t.Wire > maxWire {
						return false, invalidf("constraint #%d field %q references wire %d but circuit %q version %d declares only %d inputs (wires 1..%d)",
							i+1, side.name, t.Wire, name, version, maxWire, maxWire)
					}
				}
			}
		}
		existing.Modulus = def.Modulus
		existing.Definition = canonicalizeConstraints(def.Constraints, m)
		existing.Artifact = ""
		result = circuitFromPersist(*existing)
		return true, nil
	})
	return result, err
}

// CompileCircuit compiles the imported constraint definition of a frozen
// circuit version into a content-addressed artifact.
//
// Only frozen versions are accepted (ErrNotFrozen); a frozen version
// without an imported definition reports ErrDefinitionMissing. Repeated
// compilation returns the same artifact and adds no record; artifacts of
// other versions are never touched. The artifact binds name, version,
// modulus, input partition, constraint count and the canonical constraints.
func (s *Store) CompileCircuit(name string, version int) (Artifact, error) {
	if err := validateCircuitKey(name, version); err != nil {
		return Artifact{}, err
	}
	var result Artifact
	err := s.withLock(func() (bool, error) {
		c := findCircuit(s.data.Circuits, name, version)
		if c == nil {
			return false, notFoundf("circuit %q version %d does not exist", name, version)
		}
		if !c.Frozen {
			return false, notFrozenf("circuit %q version %d must be frozen before compilation", name, version)
		}
		if c.Modulus == "" || len(c.Definition) == 0 {
			return false, StoreError{Kind: ErrDefinitionMissing.Kind,
				Detail: fmt.Sprintf("circuit %q version %d has no constraint definition imported", name, version)}
		}
		digest := artifactDigest(c.Name, c.Version, c.Modulus, c.PublicInputs, c.PrivateInputs, len(c.Definition), c.Definition)
		if c.Artifact != "" {
			if c.Artifact != digest {
				return false, corruptf("artifact recorded for %q version %d no longer matches its constraint definition", name, version)
			}
			result = artifactFromPersist(*c)
			return false, nil
		}
		c.Artifact = digest
		result = artifactFromPersist(*c)
		return true, nil
	})
	return result, err
}

// CheckCircuit evaluates a witness against the compiled artifact of a frozen
// circuit version.
//
// Failure reasons are distinguishable: ErrNotFound (version unknown),
// ErrNotFrozen (version still a draft), ErrArtifactMissing (no compiled
// artifact) and ErrArtifactMismatch (the supplied hash is not bound to this
// version). The witness arrays must match the declared public/private input
// counts and hold decimal integers interpreted modulo the circuit modulus;
// violations yield ErrInputFormat. The result reports satisfaction, the
// bound artifact hash and the 1-based index of the first failing
// constraint. Private values are never included in the result, and no
// proving job is created.
func (s *Store) CheckCircuit(name string, version int, artifactHash string, public, private []string) (CheckResult, error) {
	if err := validateCircuitKey(name, version); err != nil {
		return CheckResult{}, err
	}
	if strings.TrimSpace(artifactHash) == "" {
		return CheckResult{}, invalidf("artifact hash must not be empty")
	}
	var result CheckResult
	err := s.withLockRead(func() error {
		c := findCircuit(s.data.Circuits, name, version)
		if c == nil {
			return notFoundf("circuit %q version %d does not exist", name, version)
		}
		if !c.Frozen {
			return notFrozenf("circuit %q version %d is not frozen", name, version)
		}
		if c.Artifact == "" {
			return StoreError{Kind: ErrArtifactMissing.Kind,
				Detail: fmt.Sprintf("circuit %q version %d has no compiled artifact", name, version)}
		}
		if strings.ToLower(strings.TrimSpace(artifactHash)) != c.Artifact {
			return StoreError{Kind: ErrArtifactMismatch.Kind,
				Detail: fmt.Sprintf("artifact hash %q is not bound to circuit %q version %d", artifactHash, name, version)}
		}
		if c.Modulus == "" || len(c.Definition) == 0 {
			return corruptf("circuit %q version %d has an artifact but no constraint definition", name, version)
		}
		m, ok := new(big.Int).SetString(c.Modulus, 10)
		if !ok {
			return corruptf("circuit %q version %d has an invalid modulus", name, version)
		}
		if len(public) != c.PublicInputs {
			return inputFormatf("public input count %d does not match declared count %d", len(public), c.PublicInputs)
		}
		if len(private) != c.PrivateInputs {
			return inputFormatf("private input count %d does not match declared count %d", len(private), c.PrivateInputs)
		}
		values := make([]*big.Int, 1+c.PublicInputs+c.PrivateInputs)
		values[0] = big.NewInt(1)
		for i, raw := range public {
			v, err := parseWitnessValue(raw, m)
			if err != nil {
				return err
			}
			values[1+i] = v
		}
		for i, raw := range private {
			v, err := parseWitnessValue(raw, m)
			if err != nil {
				return err
			}
			values[1+c.PublicInputs+i] = v
		}
		failed := checkConstraints(c.Definition, values, m)
		result = CheckResult{
			Satisfied:        failed == 0,
			ArtifactHash:     c.Artifact,
			FailedConstraint: failed,
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
		stored.Attempt == request.Attempt
}

func circuitFromPersist(c persistCircuit) Circuit {
	return Circuit{
		Name: c.Name, Version: c.Version,
		Constraints: c.Constraints, PublicInputs: c.PublicInputs, PrivateInputs: c.PrivateInputs,
		Frozen: c.Frozen, Description: c.Description,
	}
}

func artifactFromPersist(c persistCircuit) Artifact {
	return Artifact{
		Name: c.Name, Version: c.Version,
		Modulus: c.Modulus, Constraints: len(c.Definition), Hash: c.Artifact,
	}
}

func setupFromPersist(p persistSetup) Setup {
	return Setup{Name: p.Name, Version: p.Version}
}

func jobFromPersist(j persistJob) Job {
	return Job{
		ID: j.ID, Circuit: j.Circuit, Version: j.Version,
		Kind: j.Kind, Attempt: j.Attempt, Artifact: j.Artifact,
	}
}
