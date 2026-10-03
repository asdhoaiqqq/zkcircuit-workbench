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
// malformed witness (missing arrays, wrong lengths or illegal values) yields
// ErrInvalidInput and never reaches evaluation.
//
// On success the verdict reports satisfaction, the bound artifact hash and,
// when unsatisfied, the 1-based index of the first failing constraint. No
// private input value is included in the result and no proof job is created.
func (s *Store) CheckInput(name string, version int, hash string, witness Witness) (CheckResult, error) {
	if err := validateCheckRequest(name, version, hash); err != nil {
		return CheckResult{}, err
	}
	return s.checkWitness(name, version, hash, func(parsed *canonicalDefinition) (Witness, error) {
		if err := validateWitness(witness, parsed.public, parsed.private); err != nil {
			return Witness{}, err
		}
		return witness, nil
	})
}

// CheckInputFile is CheckInput with the witness read from a JSON file. All
// gating still runs against the live store, so a missing artifact or version
// is reported even when the file itself is malformed. An unreadable file is
// reported before any store access, exactly like a malformed request.
func (s *Store) CheckInputFile(name string, version int, hash, path string) (CheckResult, error) {
	if err := validateCheckRequest(name, version, hash); err != nil {
		return CheckResult{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return CheckResult{}, inputFormatf("cannot read input file %q: %v", path, err)
	}
	return s.checkWitness(name, version, hash, func(parsed *canonicalDefinition) (Witness, error) {
		return parseWitness(raw, parsed.public, parsed.private)
	})
}

// validateCheckRequest checks the arguments both input-check entries share:
// a well-formed circuit key and a non-empty artifact hash.
func validateCheckRequest(name string, version int, hash string) error {
	if err := validateCircuitKey(name, version); err != nil {
		return err
	}
	if hash == "" {
		return invalidf("an artifact hash is required to check inputs")
	}
	return nil
}

// checkWitness runs the gating and evaluation shared by CheckInput and
// CheckInputFile. The version must exist, be frozen and have a compiled
// artifact whose hash equals the requested one; only then does makeWitness
// produce the assignment (validating a directly passed witness or parsing
// the file JSON, respectively), which is evaluated against the version's
// definition. The verdict binds the artifact's own hash and never creates a
// proof job or mutates the store.
func (s *Store) checkWitness(name string, version int, hash string, makeWitness func(*canonicalDefinition) (Witness, error)) (CheckResult, error) {
	var result CheckResult
	err := s.withLockRead(func() error {
		circuit := findCircuit(s.data.Circuits, name, version)
		if circuit == nil {
			return notFoundf("circuit %q version %d does not exist", name, version)
		}
		if !circuit.Frozen {
			return notFrozenf("circuit %q version %d must be frozen before inputs can be checked", name, version)
		}
		artifact := findArtifact(s.data.Artifacts, name, version)
		if artifact == nil {
			return artifactMissingf("circuit %q version %d has no compiled artifact", name, version)
		}
		if artifact.Hash != hash {
			return artifactMismatchf("artifact hash %q does not belong to circuit %q version %d (bound hash %q)",
				hash, name, version, artifact.Hash)
		}
		if circuit.Definition == nil {
			return corruptf("artifact for %q v%d exists but its definition is missing", name, version)
		}
		parsed, perr := definitionFromPersist(*circuit.Definition, circuit.PublicInputs, circuit.PrivateInputs)
		if perr != nil {
			return corruptf("stored definition for %q v%d is unreadable: %v", name, version, perr)
		}
		witness, werr := makeWitness(parsed)
		if werr != nil {
			return werr
		}
		failure := parsed.evaluate(witness)
		result = CheckResult{Satisfied: failure == 0, Hash: artifact.Hash, FirstFailure: failure}
		return nil
	})
	return result, err
}

// validateWitness applies the witness grammar and layout rules for an
// in-API call.
func validateWitness(w Witness, public, private int) error {
	if len(w.Public) != public {
		return inputFormatf("public input length mismatch: got %d values, version declares %d", len(w.Public), public)
	}
	if len(w.Private) != private {
		return inputFormatf("private input length mismatch: got %d values, version declares %d", len(w.Private), private)
	}
	for i, v := range w.Public {
		if _, err := parseBigSignedDecimal(v); err != nil {
			return inputFormatf("public input #%d value %q is not a decimal integer: %v", i+1, v, err)
		}
	}
	for i, v := range w.Private {
		if _, err := parseBigSignedDecimal(v); err != nil {
			return inputFormatf("private input #%d value is not a decimal integer: %v", i+1, err)
		}
	}
	return nil
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
