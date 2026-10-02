package zkcircuit

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// seedCompiledVersion creates a draft with one constraint matching def,
// freezes it, records its trusted setup and compiles it, returning the
// artifact with the hash a job may bind to.
func seedCompiledVersion(t *testing.T, s *Store, name string, v int, def string) Artifact {
	t.Helper()
	seedDraftWithDef(t, s, name, v, 1, 1, def)
	if _, err := s.FreezeCircuit(name, v); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSetup(name, v); err != nil {
		t.Fatal(err)
	}
	a, err := s.CompileCircuit(name, v)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// seedBareVersion creates a frozen version with a trusted setup but no
// imported definition and no compiled artifact.
func seedBareVersion(t *testing.T, s *Store, name string, v int) {
	t.Helper()
	if _, err := s.CreateCircuit(Circuit{Name: name, Version: v, Constraints: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit(name, v); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSetup(name, v); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitJobArtifactBinding(t *testing.T) {
	s := openTestStore(t)
	artifact := seedCompiledVersion(t, s, "c", 1, validDef)
	seedBareVersion(t, s, "bare", 1)
	// A second compiled version with a different hash, to prove jobs cannot
	// borrow another version's artifact.
	artifact2 := seedCompiledVersion(t, s, "c", 2,
		`{"modulus":"13","constraints":[{"a":[],"b":[],"c":[]}]}`)

	// The exact artifact hash binds the job and is returned everywhere.
	j, err := s.SubmitJob(Job{ID: "j1", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: artifact.Hash})
	if err != nil {
		t.Fatalf("bound submit: %v", err)
	}
	if j.CompiledHash != artifact.Hash {
		t.Fatalf("submitted job lost its binding: %+v", j)
	}
	got, err := s.GetJob("j1")
	if err != nil || got.CompiledHash != artifact.Hash {
		t.Fatalf("get job binding: %+v %v", got, err)
	}
	jobs, err := s.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, jj := range jobs {
		if jj.ID == "j1" {
			found = true
			if jj.CompiledHash != artifact.Hash {
				t.Fatalf("listed job lost its binding: %+v", jj)
			}
		}
	}
	if !found {
		t.Fatal("j1 missing from list")
	}

	// A wrong hash and an arbitrary string are both mismatch errors.
	for _, h := range []string{artifact.Hash + "00", "not-a-hash"} {
		if _, err := s.SubmitJob(Job{ID: "jbad", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: h}); !errors.Is(err, ErrArtifactMismatch) {
			t.Fatalf("hash %q: want artifact mismatch, got %v", h, err)
		}
	}
	// Another version's artifact cannot be borrowed.
	if _, err := s.SubmitJob(Job{ID: "jborrow", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: artifact2.Hash}); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("foreign artifact: want mismatch, got %v", err)
	}
	// A version with no artifact at all reports the missing error.
	if _, err := s.SubmitJob(Job{ID: "jbare", Circuit: "bare", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: artifact.Hash}); !errors.Is(err, ErrArtifactMissing) {
		t.Fatalf("no artifact: want missing, got %v", err)
	}
	// An unbound submission on the bare version keeps the old behavior.
	if _, err := s.SubmitJob(Job{ID: "junbound", Circuit: "bare", Version: 1, Kind: "prove", Attempt: 1}); err != nil {
		t.Fatalf("unbound submit on counts-only version: %v", err)
	}
	// Rejected submissions left no jobs behind.
	for _, id := range []string{"jbad", "jborrow", "jbare"} {
		if _, err := s.GetJob(id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("rejected job %q left a trace: %v", id, err)
		}
	}
}

func TestSubmitJobBindingConflictPrecedence(t *testing.T) {
	s := openTestStore(t)
	artifact := seedCompiledVersion(t, s, "c", 1, validDef)

	if _, err := s.SubmitJob(Job{ID: "j1", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: artifact.Hash}); err != nil {
		t.Fatal(err)
	}
	// Same id with the same hash is idempotent.
	if again, err := s.SubmitJob(Job{ID: "j1", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: artifact.Hash}); err != nil || again.CompiledHash != artifact.Hash {
		t.Fatalf("identical resubmit: %+v %v", again, err)
	}
	// A changed hash — even one that matches no artifact — is a conflict,
	// not an artifact error: the duplicate check runs first.
	if _, err := s.SubmitJob(Job{ID: "j1", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: "does-not-exist"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed hash resubmit: want conflict, got %v", err)
	}
	// Switching from bound to unbound is also a conflict.
	if _, err := s.SubmitJob(Job{ID: "j1", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1}); !errors.Is(err, ErrConflict) {
		t.Fatalf("bound->unbound resubmit: want conflict, got %v", err)
	}
	got, _ := s.GetJob("j1")
	if got.CompiledHash != artifact.Hash {
		t.Fatalf("conflicting resubmit changed the binding: %q", got.CompiledHash)
	}

	// An unbound job resubmitted with a hash conflicts the same way.
	if _, err := s.SubmitJob(Job{ID: "j2", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitJob(Job{ID: "j2", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: artifact.Hash}); !errors.Is(err, ErrConflict) {
		t.Fatalf("unbound->bound resubmit: want conflict, got %v", err)
	}
	jobs, _ := s.ListJobs()
	if len(jobs) != 2 {
		t.Fatalf("want exactly two jobs, got %+v", jobs)
	}
}

func TestJobBindingSurvivesReopenAndNewVersions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	artifact := seedCompiledVersion(t, s, "c", 1, validDef)
	if _, err := s.SubmitJob(Job{ID: "j1", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: artifact.Hash}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got, err := s2.GetJob("j1")
	if err != nil || got.CompiledHash != artifact.Hash || got.Version != 1 {
		t.Fatalf("bound job lost after reopen: %+v %v", got, err)
	}
	jobs, err := s2.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].CompiledHash != artifact.Hash {
		t.Fatalf("listed binding lost after reopen: %+v", jobs)
	}

	// A new circuit version (with its own artifact) leaves the job pinned.
	seedCompiledVersion(t, s2, "c", 2, `{"modulus":"13","constraints":[{"a":[],"b":[],"c":[]}]}`)
	got, _ = s2.GetJob("j1")
	if got.Version != 1 || got.CompiledHash != artifact.Hash {
		t.Fatalf("job drifted after a new version: %+v", got)
	}
}

func TestUnboundJobNotFilledByLaterCompile(t *testing.T) {
	s := openTestStore(t)
	// Counts-only frozen version with a setup but no definition/artifact.
	if _, err := s.CreateCircuit(Circuit{Name: "bare", Version: 1, Constraints: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit("bare", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSetup("bare", 1); err != nil {
		t.Fatal(err)
	}
	j, err := s.SubmitJob(Job{ID: "j1", Circuit: "bare", Version: 1, Kind: "prove", Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if j.CompiledHash != "" {
		t.Fatalf("unbound job shows a binding: %+v", j)
	}
	// A draft sibling imports the definition and compiles; the job on the
	// frozen version must stay unbound regardless.
	seedDraftWithDef(t, s, "bare2", 1, 1, 1, validDef)
	if _, err := s.FreezeCircuit("bare2", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompileCircuit("bare2", 1); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob("j1")
	if err != nil {
		t.Fatal(err)
	}
	if got.CompiledHash != "" {
		t.Fatalf("later compilation filled the binding: %q", got.CompiledHash)
	}
}

func TestBoundJobRejectedWhenArtifactTampered(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	artifact := seedCompiledVersion(t, s, "c", 1, validDef)
	if _, err := s.SubmitJob(Job{ID: "j1", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: artifact.Hash}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"artifact removed", func(env map[string]any) { delete(env, "artifacts") }},
		{"artifact hash changed", func(env map[string]any) {
			arts := env["artifacts"].([]any)
			arts[0].(map[string]any)["hash"] = "deadbeef"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(dataFilePath(dir))
			if err != nil {
				t.Fatal(err)
			}
			var env map[string]any
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatal(err)
			}
			tc.mutate(env)
			tampered, err := json.MarshalIndent(env, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			tampered = append(tampered, '\n')
			if err := os.WriteFile(dataFilePath(dir), tampered, 0o644); err != nil {
				t.Fatal(err)
			}

			s2, err := Open(dir)
			if err == nil {
				s2.Close()
				t.Fatal("want read failure, got nil")
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			after, err := os.ReadFile(dataFilePath(dir))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, tampered) {
				t.Fatal("failed open modified the data file")
			}

			// Restore the committed file for the next case.
			if err := os.WriteFile(dataFilePath(dir), raw, 0o644); err != nil {
				t.Fatal(err)
			}
		})
	}
}
