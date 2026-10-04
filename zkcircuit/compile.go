package zkcircuit

import (
	"fmt"
	"os"
)

// This file holds the constraint lifecycle operations: importing a draft
// definition from a JSON file, compiling a frozen version into a hashed
// artifact, and checking an input assignment against an artifact.

// ImportConstraints reads a definition JSON file and stores it for one draft
// circuit version, replacing any previously imported definition as a whole.
//
// The version must exist and be a draft; a frozen version's definition can
// never be replaced (ErrFrozen). The file is parsed and fully validated
// against the version's declared counts before anything is committed, so a
// rejected import leaves the previous definition byte-for-byte in place.
// Unknown versions yield ErrNotFound; malformed or illegal definitions
// yield ErrInvalidArgument.
func (s *Store) ImportConstraints(name string, version int, path string) (Definition, error) {
	if err := validateCircuitKey(name, version); err != nil {
		return Definition{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Definition{}, invalidf("cannot read constraint definition file %q: %v", path, err)
	}

	var result Definition
	err = s.withLock(func() (bool, error) {
		circuit := findCircuit(s.data.Circuits, name, version)
		if circuit == nil {
			return false, notFoundf("circuit %q version %d does not exist", name, version)
		}
		if circuit.Frozen {
			return false, frozenf("circuit %q version %d is frozen; its constraint definition cannot be replaced", name, version)
		}
		parsed, perr := parseDefinitionJSON(raw, circuit.PublicInputs, circuit.PrivateInputs, circuit.Constraints)
		if perr != nil {
			return false, perr
		}
		persisted := parsed.toPersist()
		circuit.Definition = &persisted
		result = parsed.toExport()
		return true, nil
	})
	return result, err
}

// GetDefinition returns the imported definition of one version. A version
// without one yields ErrDefinitionMissing, and an unknown version
// ErrNotFound.
func (s *Store) GetDefinition(name string, version int) (Definition, error) {
	if err := validateCircuitKey(name, version); err != nil {
		return Definition{}, err
	}
	var result Definition
	err := s.withLockRead(func() error {
		circuit := findCircuit(s.data.Circuits, name, version)
		if circuit == nil {
			return notFoundf("circuit %q version %d does not exist", name, version)
		}
		if circuit.Definition == nil {
			return definitionMissingf("circuit %q version %d has no constraint definition", name, version)
		}
		parsed, perr := definitionFromPersist(*circuit.Definition, circuit.PublicInputs, circuit.PrivateInputs)
		if perr != nil {
			return corruptf("circuit %q v%d stored definition cannot be read: %v", name, version, perr)
		}
		result = parsed.toExport()
		return nil
	})
	return result, err
}

// CompileCircuit compiles a frozen circuit version into an artifact.
//
// Only frozen versions compile: unknown versions yield ErrNotFound, drafts
// ErrNotFrozen. A version whose counts were registered but which never had a
// definition imported yields ErrDefinitionMissing. Repeated compilation of
// the same version is idempotent and returns the identical artifact; other
// versions' artifacts are unaffected.
func (s *Store) CompileCircuit(name string, version int) (Artifact, error) {
	if err := validateCircuitKey(name, version); err != nil {
		return Artifact{}, err
	}
	var result Artifact
	err := s.withLock(func() (bool, error) {
		circuit := findCircuit(s.data.Circuits, name, version)
		if circuit == nil {
			return false, notFoundf("circuit %q version %d does not exist", name, version)
		}
		if !circuit.Frozen {
			return false, notFrozenf("circuit %q version %d must be frozen before compilation", name, version)
		}
		if existing := findArtifact(s.data.Artifacts, name, version); existing != nil {
			result = artifactFromPersist(*existing)
			return false, nil
		}
		if circuit.Definition == nil {
			return false, definitionMissingf("circuit %q version %d has only registered counts and no constraint definition; import one before freezing",
				name, version)
		}
		parsed, perr := definitionFromPersist(*circuit.Definition, circuit.PublicInputs, circuit.PrivateInputs)
		if perr != nil {
			return false, corruptf("stored definition for %q v%d is unreadable: %v", name, version, perr)
		}
		if !parsed.compatibleWith(circuit.Constraints, circuit.PublicInputs, circuit.PrivateInputs) {
			return false, corruptf("stored definition for %q v%d no longer matches its declared counts", name, version)
		}
		record := persistArtifact{
			Name:        name,
			Version:     version,
			Modulus:     parsed.modulus,
			Constraints: len(parsed.constraints),
			Hash:        artifactHash(name, version, parsed),
		}
		s.data.Artifacts = append(s.data.Artifacts, record)
		result = artifactFromPersist(record)
		return true, nil
	})
	return result, err
}

// GetArtifact returns the compiled artifact for one version. A version
// without one yields ErrArtifactMissing, and an unknown version ErrNotFound.
func (s *Store) GetArtifact(name string, version int) (Artifact, error) {
	if err := validateCircuitKey(name, version); err != nil {
		return Artifact{}, err
	}
	var result Artifact
	err := s.withLockRead(func() error {
		circuit := findCircuit(s.data.Circuits, name, version)
		if circuit == nil {
			return notFoundf("circuit %q version %d does not exist", name, version)
		}
		existing := findArtifact(s.data.Artifacts, name, version)
		if existing == nil {
			return artifactMissingf("circuit %q version %d has no compiled artifact", name, version)
		}
		result = artifactFromPersist(*existing)
		return nil
	})
	return result, err
}

// CheckInput tests one input assignment against the named version's
// compiled artifact. hash must equal that artifact's hash, binding the check
// to an explicit compilation result.
//
// Gating errors are reported distinctly: unknown version ErrNotFound,
// unfrozen version ErrNotFrozen, no compiled artifact ErrArtifactMissing and
// a hash that does not belong to the target version ErrArtifactMismatch. A
// malformed witness (wrong lengths or illegal values; a nil slice stands for
// an empty one) yields ErrInvalidInput and never reaches evaluation.
//
// The witness strings are converted to field residues once, while they are
// validated, and evaluation reuses that result. On success the verdict
// reports satisfaction, the bound artifact hash and, when unsatisfied, the
// 1-based index of the first failing constraint. No private input value is
// included in the result and no proof job is created.
//
// CheckInputFile applies exactly the same gating, binding and evaluation;
// only the witness representation differs.
func (s *Store) CheckInput(name string, version int, hash string, witness Witness) (CheckResult, error) {
	if err := validateCheckRequest(name, version, hash); err != nil {
		return CheckResult{}, err
	}
	var result CheckResult
	err := s.withLockRead(func() error {
		def, boundHash, gerr := s.boundCheckDefinition(name, version, hash)
		if gerr != nil {
			return gerr
		}
		parsed, perr := parseWitnessValues(witness.Public, witness.Private, def.public, def.private, def.modulus)
		if perr != nil {
			return tagInputFormatError(perr)
		}
		result = evaluateCheck(def, parsed, boundHash)
		return nil
	})
	return result, err
}

// CheckInputFile is CheckInput with the witness read from a JSON file. It
// shares every gating, binding and evaluation rule with CheckInput; the only
// differences are how the witness is supplied (a strict JSON document that
// must explicitly contain both string arrays, even when empty) and that an
// unreadable or malformed file is reported as an input format error.
//
// The file is read first, so a missing or unreadable file keeps its read
// error; but once the file can be read, all binding gating still runs
// against the live store — an unknown version, a draft, a missing artifact
// or a wrong hash is reported before any complaint about the file's
// contents.
func (s *Store) CheckInputFile(name string, version int, hash, path string) (CheckResult, error) {
	if err := validateCheckRequest(name, version, hash); err != nil {
		return CheckResult{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return CheckResult{}, inputFormatf("cannot read input file %q: %v", path, err)
	}
	var result CheckResult
	err = s.withLockRead(func() error {
		def, boundHash, gerr := s.boundCheckDefinition(name, version, hash)
		if gerr != nil {
			return gerr
		}
		pubValues, privValues, derr := witnessArraysFromDocument(raw)
		if derr != nil {
			return tagInputFormatError(derr)
		}
		parsed, perr := parseWitnessValues(pubValues, privValues, def.public, def.private, def.modulus)
		if perr != nil {
			return tagInputFormatError(perr)
		}
		result = evaluateCheck(def, parsed, boundHash)
		return nil
	})
	return result, err
}

// validateCheckRequest covers the request-shape checks common to both
// entry points: an explicit non-whitespace circuit key and a non-empty
// artifact hash.
func validateCheckRequest(name string, version int, hash string) error {
	if err := validateCircuitKey(name, version); err != nil {
		return err
	}
	if hash == "" {
		return invalidf("an artifact hash is required to check inputs")
	}
	return nil
}

// boundCheckDefinition resolves and verifies everything a check is bound
// to, in the required order: the version must exist and be frozen, its
// compiled artifact must exist, the supplied hash must equal that
// artifact's hash (artifacts of other names or versions never qualify), and
// the stored definition behind the artifact must be readable. It returns
// the canonical definition and the bound artifact hash.
//
// Callers must hold the store read lock.
func (s *Store) boundCheckDefinition(name string, version int, hash string) (*canonicalDefinition, string, error) {
	circuit := findCircuit(s.data.Circuits, name, version)
	if circuit == nil {
		return nil, "", notFoundf("circuit %q version %d does not exist", name, version)
	}
	if !circuit.Frozen {
		return nil, "", notFrozenf("circuit %q version %d must be frozen before inputs can be checked", name, version)
	}
	artifact := findArtifact(s.data.Artifacts, name, version)
	if artifact == nil {
		return nil, "", artifactMissingf("circuit %q version %d has no compiled artifact", name, version)
	}
	if artifact.Hash != hash {
		return nil, "", artifactMismatchf("artifact hash %q does not belong to circuit %q version %d (bound hash %q)",
			hash, name, version, artifact.Hash)
	}
	if circuit.Definition == nil {
		return nil, "", corruptf("artifact for %q v%d exists but its definition is missing", name, version)
	}
	parsed, perr := definitionFromPersist(*circuit.Definition, circuit.PublicInputs, circuit.PrivateInputs)
	if perr != nil {
		return nil, "", corruptf("stored definition for %q v%d is unreadable: %v", name, version, perr)
	}
	return parsed, artifact.Hash, nil
}

// evaluateCheck runs the one shared verdict: the 1-based index of the first
// failing constraint, or zero when every constraint holds. The witness is
// already parsed into field residues by parseWitnessValues, so this performs
// no numeric conversion of its own.
func evaluateCheck(def *canonicalDefinition, witness parsedWitness, hash string) CheckResult {
	failure := def.evaluate(witness)
	return CheckResult{Satisfied: failure == 0, Hash: hash, FirstFailure: failure}
}

// --- lookup / mapping helpers ---------------------------------------------

func findArtifact(as []persistArtifact, name string, version int) *persistArtifact {
	for i := range as {
		if as[i].Name == name && as[i].Version == version {
			return &as[i]
		}
	}
	return nil
}

func artifactFromPersist(a persistArtifact) Artifact {
	return Artifact{
		Name: a.Name, Version: a.Version,
		Modulus: int(a.Modulus), Constraints: a.Constraints, Hash: a.Hash,
	}
}

func definitionMissingf(format string, args ...any) error {
	return StoreError{Kind: ErrDefinitionMissing.Kind, Detail: fmt.Sprintf(format, args...)}
}

func artifactMissingf(format string, args ...any) error {
	return StoreError{Kind: ErrArtifactMissing.Kind, Detail: fmt.Sprintf(format, args...)}
}

func artifactMismatchf(format string, args ...any) error {
	return StoreError{Kind: ErrArtifactMismatch.Kind, Detail: fmt.Sprintf(format, args...)}
}
