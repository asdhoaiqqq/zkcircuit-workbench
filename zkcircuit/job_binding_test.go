package zkcircuit

import (
	"errors"
	"strings"
	"testing"
)

// compiledCircuit registers a fully prepared circuit version: draft with an
// imported definition, frozen, trusted setup recorded and artifact compiled.
// It returns the compiled artifact whose hash jobs may bind to.
func compiledCircuit(t *testing.T, s *Store, name string, v int) Artifact {
	t.Helper()
	seedDraftWithDef(t, s, name, v, 1, 1, validDef)
	if _, err := s.FreezeCircuit(name, v); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSetup(name, v); err != nil {
		t.Fatal(err)
	}
	a, err := s.CompileCircuit(name, v)
	if err != nil {
		t.Fatalf("compile %s v%d: %v", name, v, err)
	}
	return a
}

func jobIDs(t *testing.T, s *Store) []string {
	t.Helper()
	jobs, err := s.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
	}
	return ids
}

func TestSubmitJobBindsCompiledHash(t *testing.T) {
	s := openTestStore(t)
	a := compiledCircuit(t, s, "c", 1)

	job := Job{ID: "job-1", Circuit: "c", Version: 1, Kind: "prove", Attempt: 2, CompiledHash: a.Hash}
	stored, err := s.SubmitJob(job)
	if err != nil {
		t.Fatalf("submit bound job: %v", err)
	}
	if stored != job {
		t.Fatalf("stored job differs from request: %+v", stored)
	}
	// The binding is visible identically through by-id lookup and listing.
	got, err := s.GetJob("job-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != stored || got.CompiledHash != a.Hash || got.Version != 1 {
		t.Fatalf("lookup lost binding: %+v", got)
	}
	jobs, err := s.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0] != stored {
		t.Fatalf("list lost binding: %+v", jobs)
	}
}

func TestSubmitJobHashScopedToNameAndVersion(t *testing.T) {
	s := openTestStore(t)
	// Same name, other version; same version, other name — all with identical
	// constraint definitions and input counts. None may borrow c v1's hash.
	a1 := compiledCircuit(t, s, "c", 1)
	a2 := compiledCircuit(t, s, "c", 2)
	ad := compiledCircuit(t, s, "d", 1)
	if a1.Hash == a2.Hash || a1.Hash == ad.Hash {
		t.Fatalf("test requires distinct artifact hashes, got %q / %q / %q", a1.Hash, a2.Hash, ad.Hash)
	}

	foreign := []struct {
		name string
		job  Job
	}{
		{"same name other version", Job{ID: "j-v2", Circuit: "c", Version: 2, Kind: "prove", Attempt: 1, CompiledHash: a1.Hash}},
		{"other name same version", Job{ID: "j-d1", Circuit: "d", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: a1.Hash}},
		{"cross both ways", Job{ID: "j-x", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: a2.Hash}},
		{"other name's hash", Job{ID: "j-y", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: ad.Hash}},
	}
	for _, tc := range foreign {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.SubmitJob(tc.job); !errors.Is(err, ErrArtifactMismatch) {
				t.Fatalf("want artifact mismatch, got %v", err)
			}
			if _, err := s.GetJob(tc.job.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("refused submission left a job: %v", err)
			}
		})
	}
	if ids := jobIDs(t, s); len(ids) != 0 {
		t.Fatalf("foreign-hash submissions stored jobs: %v", ids)
	}

	// Each version accepts its own hash.
	for _, bound := range []struct {
		id   string
		name string
		v    int
		hash string
	}{
		{"j-ok-c1", "c", 1, a1.Hash},
		{"j-ok-c2", "c", 2, a2.Hash},
		{"j-ok-d1", "d", 1, ad.Hash},
	} {
		job := Job{ID: bound.id, Circuit: bound.name, Version: bound.v, Kind: "prove", Attempt: 1, CompiledHash: bound.hash}
		stored, err := s.SubmitJob(job)
		if err != nil || stored != job {
			t.Fatalf("own-hash binding for %s v%d: %+v %v", bound.name, bound.v, stored, err)
		}
	}
}

func TestSubmitJobArtifactMissingAndMismatchLeaveNoTrace(t *testing.T) {
	s := openTestStore(t)
	a := compiledCircuit(t, s, "c", 1)
	// Frozen, setup recorded, definition imported — but never compiled.
	seedDraftWithDef(t, s, "u", 1, 1, 1, validDef)
	if _, err := s.FreezeCircuit("u", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSetup("u", 1); err != nil {
		t.Fatal(err)
	}

	keeper := Job{ID: "keep", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: a.Hash}
	if _, err := s.SubmitJob(keeper); err != nil {
		t.Fatal(err)
	}

	// Non-empty hash against an uncompiled version -> artifact missing.
	missing := Job{ID: "j-missing", Circuit: "u", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: a.Hash}
	if _, err := s.SubmitJob(missing); !errors.Is(err, ErrArtifactMissing) {
		t.Fatalf("want artifact missing, got %v", err)
	}
	// Compiled target, hash that belongs to nobody -> mismatch.
	mismatch := Job{ID: "j-mismatch", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: strings.Repeat("0", 64)}
	if _, err := s.SubmitJob(mismatch); !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("want artifact mismatch, got %v", err)
	}

	// Neither failure added a job or touched the existing record.
	if ids := jobIDs(t, s); len(ids) != 1 || ids[0] != "keep" {
		t.Fatalf("failed submissions changed the job list: %v", ids)
	}
	got, err := s.GetJob("keep")
	if err != nil || got != keeper {
		t.Fatalf("existing job changed by failed submissions: %+v %v", got, err)
	}
}

func TestSubmitJobHashMatchedExactly(t *testing.T) {
	s := openTestStore(t)
	a := compiledCircuit(t, s, "c", 1)

	// Flip the case of the first hex letter; the hash always contains one.
	flipped := a.Hash
	for i, r := range a.Hash {
		if r >= 'a' && r <= 'f' {
			flipped = a.Hash[:i] + strings.ToUpper(string(r)) + a.Hash[i+1:]
			break
		}
	}
	if flipped == a.Hash {
		t.Fatalf("hash %q has no letter to flip", a.Hash)
	}

	variants := []struct {
		name string
		hash string
	}{
		{"leading space", " " + a.Hash},
		{"trailing space", a.Hash + " "},
		{"surrounding whitespace", "\t " + a.Hash + "\n"},
		{"case changed", flipped},
	}
	for i, tc := range variants {
		t.Run(tc.name, func(t *testing.T) {
			job := Job{ID: "j-exact-" + string(rune('a'+i)), Circuit: "c", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: tc.hash}
			if _, err := s.SubmitJob(job); !errors.Is(err, ErrArtifactMismatch) {
				t.Fatalf("want artifact mismatch, got %v", err)
			}
			if _, err := s.GetJob(job.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("near-miss hash was accepted or left a trace: %v", err)
			}
		})
	}
	if ids := jobIDs(t, s); len(ids) != 0 {
		t.Fatalf("near-miss hashes stored jobs: %v", ids)
	}
}

func TestSubmitJobBindingConflictKeepsOriginal(t *testing.T) {
	s := openTestStore(t)
	a1 := compiledCircuit(t, s, "c", 1)
	a2 := compiledCircuit(t, s, "c", 2)

	bound := Job{ID: "job-bound", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: a1.Hash}
	stored, err := s.SubmitJob(bound)
	if err != nil {
		t.Fatal(err)
	}
	// Identical resubmission returns the original record and adds nothing.
	again, err := s.SubmitJob(bound)
	if err != nil || again != stored {
		t.Fatalf("idempotent resubmit: %+v %v", again, err)
	}
	if ids := jobIDs(t, s); len(ids) != 1 {
		t.Fatalf("resubmit duplicated the job: %v", ids)
	}

	// Same id, hash swapped for another real artifact's hash -> conflict.
	swapped := bound
	swapped.CompiledHash = a2.Hash
	if _, err := s.SubmitJob(swapped); !errors.Is(err, ErrConflict) {
		t.Fatalf("hash swap: want conflict, got %v", err)
	}
	// Same id, hash dropped -> conflict.
	dropped := bound
	dropped.CompiledHash = ""
	if _, err := s.SubmitJob(dropped); !errors.Is(err, ErrConflict) {
		t.Fatalf("hash drop: want conflict, got %v", err)
	}
	got, _ := s.GetJob("job-bound")
	if got != stored {
		t.Fatalf("conflicting resubmits mutated the stored job: %+v", got)
	}

	// A job registered unbound cannot gain a hash later.
	unbound := Job{ID: "job-unbound", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1}
	if _, err := s.SubmitJob(unbound); err != nil {
		t.Fatal(err)
	}
	gain := unbound
	gain.CompiledHash = a1.Hash
	if _, err := s.SubmitJob(gain); !errors.Is(err, ErrConflict) {
		t.Fatalf("hash gain: want conflict, got %v", err)
	}
	got, _ = s.GetJob("job-unbound")
	if got != unbound {
		t.Fatalf("unbound job gained a hash: %+v", got)
	}

	// A conflicting resubmit whose new hash is itself invalid still reports
	// the conflict first, not a fresh binding failure.
	bogus := bound
	bogus.Attempt = 9
	bogus.CompiledHash = "not-a-real-hash"
	if _, err := s.SubmitJob(bogus); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict must precede artifact validation, got %v", err)
	}
	got, _ = s.GetJob("job-bound")
	if got != stored {
		t.Fatalf("bogus resubmit mutated the stored job: %+v", got)
	}
	if ids := jobIDs(t, s); len(ids) != 2 {
		t.Fatalf("conflict handling changed the job list: %v", ids)
	}
}

func TestSubmitJobEmptyHashStaysUnbound(t *testing.T) {
	s := openTestStore(t)

	// Frozen with setup and definition but not yet compiled: an unbound job
	// registers fine, and later compilation does not retro-bind it.
	seedDraftWithDef(t, s, "late", 1, 1, 1, validDef)
	if _, err := s.FreezeCircuit("late", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSetup("late", 1); err != nil {
		t.Fatal(err)
	}
	early := Job{ID: "j-early", Circuit: "late", Version: 1, Kind: "prove", Attempt: 1}
	if _, err := s.SubmitJob(early); err != nil {
		t.Fatalf("unbound job before compilation: %v", err)
	}
	if _, err := s.CompileCircuit("late", 1); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob("j-early")
	if err != nil || got.CompiledHash != "" {
		t.Fatalf("compilation retro-bound an old job: %+v %v", got, err)
	}

	// Already compiled: an empty hash is stored as-is, never auto-filled.
	compiledCircuit(t, s, "c", 1)
	unbound := Job{ID: "j-unbound", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1}
	stored, err := s.SubmitJob(unbound)
	if err != nil {
		t.Fatalf("unbound job on compiled version: %v", err)
	}
	if stored.CompiledHash != "" {
		t.Fatalf("empty hash auto-bound to artifact: %+v", stored)
	}

	// Counts-only version (no definition ever imported): unbound jobs remain
	// register-only and are accepted.
	if _, err := s.CreateCircuit(Circuit{Name: "bare", Version: 1, Constraints: 5, Description: "counts only"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit("bare", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSetup("bare", 1); err != nil {
		t.Fatal(err)
	}
	bare := Job{ID: "j-bare", Circuit: "bare", Version: 1, Kind: "prove", Attempt: 1}
	stored, err = s.SubmitJob(bare)
	if err != nil || stored != bare {
		t.Fatalf("counts-only unbound job: %+v %v", stored, err)
	}

	jobs, err := s.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("want 3 unbound jobs, got %+v", jobs)
	}
	for _, j := range jobs {
		if j.CompiledHash != "" {
			t.Fatalf("job %q unexpectedly bound: %+v", j.ID, j)
		}
	}
}
