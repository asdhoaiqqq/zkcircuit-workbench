// Package zkcircuit implements circuit versioning and proving job orchestration.
package zkcircuit

import "sort"

// Circuit is one versioned arithmetic circuit.
type Circuit struct {
	Name          string
	Version       int
	Description   string
	Constraints   int
	PublicInputs  int
	PrivateInputs int
	Frozen        bool
}

// Job is one prove or verify attempt bound to one circuit version.
type Job struct {
	ID       string
	Circuit  string
	Version  int
	Kind     string
	Attempt  int
	Artifact string
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
