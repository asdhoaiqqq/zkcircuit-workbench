// Package zkcircuit implements circuit versioning and proving job orchestration.
package zkcircuit

import "sort"

// Circuit is one versioned arithmetic circuit.
type Circuit struct {
	Name          string `json:"name"`
	Version       int    `json:"version"`
	Constraints   int    `json:"constraints"`
	PublicInputs  int    `json:"public_inputs"`
	PrivateInputs int    `json:"private_inputs"`
	Frozen        bool   `json:"frozen"`
	// Description is the human-readable description bound to this version.
	Description string `json:"description"`
}

// Job is one prove or verify attempt bound to one circuit version.
type Job struct {
	ID       string `json:"id"`
	Circuit  string `json:"circuit"`
	Version  int    `json:"version"`
	Kind     string `json:"kind"`
	Attempt  int    `json:"attempt"`
	Artifact string `json:"artifact"`
	// CompiledHash optionally binds the job to the compiled artifact of the
	// pinned circuit version. Empty means unbound; a non-empty value must
	// equal that version's artifact hash exactly.
	CompiledHash string `json:"compiled_hash,omitempty"`
}

// Setup is a trusted setup record registered for one frozen circuit version.
type Setup struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// Validate refuses to run a job against a moved or unfrozen circuit.
func Validate(circuit Circuit, job Job, trustedSetup bool) error {
	if circuit.Name == "" || job.Circuit != circuit.Name {
		return errInvalid("job references an unknown circuit")
	}
	if job.Version != circuit.Version {
		return errInvalid("job pinned to a stale circuit version")
	}
	if !circuit.Frozen {
		return errInvalid("circuit must be frozen before proving")
	}
	if job.Kind == "prove" && !trustedSetup {
		return errInvalid("proving requires a recorded trusted setup")
	}
	if circuit.Constraints <= 0 {
		return errInvalid("circuit has no constraints")
	}
	return nil
}

// WitnessCost orders circuits by constraint load, largest first.
func WitnessCost(circuits []Circuit) []string {
	sorted := append([]Circuit(nil), circuits...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Constraints == sorted[j].Constraints {
			return sorted[i].Name < sorted[j].Name
		}
		return sorted[i].Constraints > sorted[j].Constraints
	})
	var names []string
	for _, circuit := range sorted {
		names = append(names, circuit.Name)
	}
	return names
}

type errInvalid string

func (e errInvalid) Error() string { return string(e) }
