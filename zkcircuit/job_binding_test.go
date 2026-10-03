package zkcircuit

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Regression tests for the compiled-artifact binding accepted on job
// submission. A non-empty CompiledHash belongs to exactly one circuit
// name+version (the artifact's), is matched as the exact string and is part
// of the job identity for idempotence/conflict checks. An empty hash keeps
// the register-only behavior forever: no auto-compile, no auto-binding.
//
// Every submission here uses a non-blank id, a positive attempt and kind
// "prove", so the observed failures are binding failures and not request
// validation noise.

// compiledVersion creates a frozen version with a setup and a compiled
// artifact, returning that artifact's full hash.
func compiledVersion(t *testing.T, s *Store, name string, v int, def string) string {
	t.Helper()
	seedFrozenSource(t, s, name, v, 1, 1, 1, def)
	a, err := s.GetArtifact(name, v)
	if err != nil {
		t.Fatalf("artifact for %q v%d: %v", name, v, err)
	}
	return a.Hash
}

// frozenSetupUncompiled creates a frozen version with a setup but without a
// compiled artifact (a definition is imported, so it could be compiled
// later).
func frozenSetupUncompiled(t *testing.T, s *Store, name string, v int) {
	t.Helper()
	seedDraftWithDef(t, s, name, v, 1, 1, validDef)
	if _, err := s.FreezeCircuit(name, v); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSetup(name, v); err != nil {
		t.Fatal(err)
	}
}

func boundProveJob(id, circuit string, version int, hash string) Job {
	return Job{ID: id, Circuit: circuit, Version: version, Kind: "prove", Attempt: 1, CompiledHash: hash}
}

// A successful binding stores the pinned version and the full hash; the
// submission result, GetJob and ListJobs agree, including after reopen.
func TestSubmitJobBoundHashRoundTrips(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := compiledVersion(t, s, "c", 1, validDef)

	submitted := boundProveJob("j-bound", "c", 1, h)
	stored, err := s.SubmitJob(submitted)
	if err != nil {
		t.Fatalf("submit with own hash: %v", err)
	}
	if stored != submitted {
		t.Fatalf("submit result differs from request:\n got %+v\nwant %+v", stored, submitted)
	}
	if stored.CompiledHash != h {
		t.Fatalf("stored hash:\n got %q\nwant %q", stored.CompiledHash, h)
	}

	got, err := s.GetJob("j-bound")
	if err != nil {
		t.Fatal(err)
	}
	if got != stored {
		t.Fatalf("GetJob disagrees with submit result:\n got %+v\nwant %+v", got, stored)
	}
	jobs, err := s.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0] != stored {
		t.Fatalf("ListJobs disagrees with submit result: %+v", jobs)
	}

	// The binding is committed state and survives a fresh process.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got, err = s2.GetJob("j-bound")
	if err != nil {
		t.Fatal(err)
	}
	if got != stored {
		t.Fatalf("binding lost on reopen:\n got %+v\nwant %+v", got, stored)
	}
	jobs, _ = s2.ListJobs()
	if len(jobs) != 1 || jobs[0] != stored {
		t.Fatalf("listed binding wrong after reopen: %+v", jobs)
	}
}

// Identical constraint definitions and identical input counts do not make
// hashes interchangeable: each name+version has its own artifact, and a
// foreign hash is an artifact mismatch against a new job id without touching
// any record.
func TestArtifactHashCannotBeBorrowedAcrossNameOrVersion(t *testing.T) {
	s := openTestStore(t)
	h1 := compiledVersion(t, s, "c", 1, validDef)
	h2 := compiledVersion(t, s, "c", 2, validDef)
	hOther := compiledVersion(t, s, "other", 1, validDef)

	// The identity (name and version) is folded into the hash even though the
	// definitions and the 1/1/1 counts are byte-for-byte identical.
	if h1 == h2 || h1 == hOther || h2 == hOther {
		t.Fatalf("artifacts of different name/version share a hash: %q %q %q", h1, h2, hOther)
	}

	cases := []struct {
		name    string
		circuit string
		version int
		hash    string
	}{
		{"same name other version borrows hash", "c", 1, h2},
		{"other version borrows v1 hash", "c", 2, h1},
		{"same version other name borrows hash", "other", 1, h1},
		{"target borrows other-name hash", "c", 1, hOther},
		{"unrelated foreign hash", "c", 1, "deadbeef"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := fmt.Sprintf("j-bad-%d", i)
			before, err := readDataFile(t, s.Dir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.SubmitJob(boundProveJob(id, tc.circuit, tc.version, tc.hash)); !errors.Is(err, ErrArtifactMismatch) {
				t.Fatalf("want artifact mismatch, got %v", err)
			}
			if _, err := s.GetJob(id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("rejected job %q left a record: %v", id, err)
			}
			jobs, _ := s.ListJobs()
			if len(jobs) != 0 {
				t.Fatalf("rejected binding stored a job: %+v", jobs)
			}
			after, err := readDataFile(t, s.Dir())
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatalf("rejected binding changed committed state")
			}
			// The version's own artifact is untouched.
			if a, err := s.GetArtifact(tc.circuit, tc.version); err != nil || a.Hash == tc.hash {
				t.Fatalf("artifact state wrong after mismatch: %+v err=%v", a, err)
			}
		})
	}

	// Positive controls: each version binds only its own hash.
	for i, target := range []struct {
		id, name string
		v        int
		hash     string
	}{
		{"j1", "c", 1, h1}, {"j2", "c", 2, h2}, {"j3", "other", 1, hOther},
	} {
		if stored, err := s.SubmitJob(boundProveJob(target.id, target.name, target.v, target.hash)); err != nil {
			t.Fatalf("own-hash submit #%d: %v", i, err)
		} else if stored.CompiledHash != target.hash || stored.Version != target.v || stored.Circuit != target.name {
			t.Fatalf("bound record wrong: %+v", stored)
		}
	}
	jobs, _ := s.ListJobs()
	if len(jobs) != 3 {
		t.Fatalf("want 3 jobs, got %+v", jobs)
	}
}

// A non-empty hash against a target version without a compiled artifact is
// ErrArtifactMissing — whether the version merely has not been compiled yet
// or is a counts-only version that can never compile. Submission never
// triggers a compile, so the counts-only case must not surface
// ErrDefinitionMissing. Nothing is written.
func TestSubmitJobHashRequiresCompiledTarget(t *testing.T) {
	s := openTestStore(t)
	frozenSetupUncompiled(t, s, "defined", 1)
	// Counts-only frozen version with a setup: register-only jobs stay legal,
	// but it has no artifact.
	readyCircuit(t, s, "bare", 1)

	cases := []struct {
		name    string
		circuit string
	}{
		{"defined but not yet compiled", "defined"},
		{"counts only, no artifact possible", "bare"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := "j-missing-" + tc.circuit
			before, err := readDataFile(t, s.Dir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.SubmitJob(boundProveJob(id, tc.circuit, 1, "some-hash")); !errors.Is(err, ErrArtifactMissing) {
				t.Fatalf("want compiled artifact missing, got %v", err)
			}
			if _, err := s.GetJob(id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("refused job %q left a record: %v", id, err)
			}
			jobs, _ := s.ListJobs()
			if len(jobs) != 0 {
				t.Fatalf("refused submission stored a job: %+v", jobs)
			}
			after, err := readDataFile(t, s.Dir())
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatalf("rejected submission changed committed state")
			}
		})
	}
}

// The hash matches the exact submitted string: surrounding whitespace or
// changed letter case is a mismatch, never normalized into a match.
func TestSubmitJobHashMatchesExactly(t *testing.T) {
	s := openTestStore(t)
	h := compiledVersion(t, s, "c", 1, validDef)
	upper := strings.ToUpper(h)
	if upper == h {
		t.Fatalf("test hash %q has no letters to flip the case of", h)
	}
	// Flip the first lowercase hex letter deterministically; the first
	// character of a hex hash can be a digit, whose "case" is unchanged.
	flipped := h
	for i := 0; i < len(h); i++ {
		if c := h[i]; c >= 'a' && c <= 'f' {
			flipped = h[:i] + strings.ToUpper(h[i:i+1]) + h[i+1:]
			break
		}
	}
	if flipped == h {
		t.Fatalf("test hash %q has no a-f letter to flip", h)
	}

	variants := []struct {
		name string
		hash string
	}{
		{"leading space", " " + h},
		{"trailing space", h + " "},
		{"trailing tab", h + "\t"},
		{"trailing newline", h + "\n"},
		{"upper case", upper},
		{"single letter case flip", flipped},
	}
	for i, tc := range variants {
		t.Run(tc.name, func(t *testing.T) {
			id := fmt.Sprintf("j-exact-%d", i)
			if _, err := s.SubmitJob(boundProveJob(id, "c", 1, tc.hash)); !errors.Is(err, ErrArtifactMismatch) {
				t.Fatalf("want artifact mismatch for %q, got %v", tc.name, err)
			}
			if _, err := s.GetJob(id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("normalized hash %q was accepted and stored", tc.name)
			}
		})
	}
	jobs, _ := s.ListJobs()
	if len(jobs) != 0 {
		t.Fatalf("exact-match failures left jobs: %+v", jobs)
	}

	// The untouched exact string is the only accepted form.
	stored, err := s.SubmitJob(boundProveJob("j-exact-ok", "c", 1, h))
	if err != nil {
		t.Fatalf("exact hash rejected: %v", err)
	}
	if stored.CompiledHash != h {
		t.Fatalf("bound hash altered: got %q want %q", stored.CompiledHash, h)
	}
}

// The binding belongs to the job id. Identical resubmits are idempotent;
// changing or removing the hash, or adding one to a previously unbound job,
// is ErrConflict with the stored job preserved. The existing-id conflict is
// reported before the new request's artifact is validated.
func TestSubmitJobBindingConflictPreservesStoredJob(t *testing.T) {
	cases := []struct {
		name string
		// run seeds a fresh store, submits the original job and returns it
		// together with the conflicting resubmission.
		run func(t *testing.T, s *Store) (original, conflict Job)
	}{
		{
			name: "changed hash conflicts",
			run: func(t *testing.T, s *Store) (Job, Job) {
				h1 := compiledVersion(t, s, "c", 1, validDef)
				h2 := compiledVersion(t, s, "c", 2, validDef)
				original := boundProveJob("j", "c", 1, h1)
				if _, err := s.SubmitJob(original); err != nil {
					t.Fatal(err)
				}
				return original, boundProveJob("j", "c", 1, h2)
			},
		},
		{
			name: "removed hash conflicts",
			run: func(t *testing.T, s *Store) (Job, Job) {
				h1 := compiledVersion(t, s, "c", 1, validDef)
				original := boundProveJob("j", "c", 1, h1)
				if _, err := s.SubmitJob(original); err != nil {
					t.Fatal(err)
				}
				return original, boundProveJob("j", "c", 1, "")
			},
		},
		{
			name: "adding hash to unbound job conflicts",
			run: func(t *testing.T, s *Store) (Job, Job) {
				h1 := compiledVersion(t, s, "c", 1, validDef)
				original := boundProveJob("j", "c", 1, "")
				if _, err := s.SubmitJob(original); err != nil {
					t.Fatal(err)
				}
				return original, boundProveJob("j", "c", 1, h1)
			},
		},
		{
			name: "conflict precedes artifact mismatch",
			run: func(t *testing.T, s *Store) (Job, Job) {
				h1 := compiledVersion(t, s, "c", 1, validDef)
				original := boundProveJob("j", "c", 1, h1)
				if _, err := s.SubmitJob(original); err != nil {
					t.Fatal(err)
				}
				// Sanity: for a fresh id this foreign hash is a real mismatch…
				if _, err := s.SubmitJob(boundProveJob("fresh-id", "c", 1, "foreign-hash")); !errors.Is(err, ErrArtifactMismatch) {
					t.Fatalf("sanity mismatch: %v", err)
				}
				// …but for the existing id the conflict is reported first.
				return original, boundProveJob("j", "c", 1, "foreign-hash")
			},
		},
		{
			name: "conflict precedes artifact missing",
			run: func(t *testing.T, s *Store) (Job, Job) {
				h1 := compiledVersion(t, s, "c", 1, validDef)
				// A frozen version with a setup but no artifact: a fresh id with
				// any non-empty hash hits ErrArtifactMissing…
				frozenSetupUncompiled(t, s, "d", 1)
				original := boundProveJob("j", "c", 1, h1)
				if _, err := s.SubmitJob(original); err != nil {
					t.Fatal(err)
				}
				if _, err := s.SubmitJob(boundProveJob("fresh-id", "d", 1, "any-hash")); !errors.Is(err, ErrArtifactMissing) {
					t.Fatalf("sanity missing: %v", err)
				}
				// …repurposing the existing id is a conflict first, not a new
				// binding failure against "d" v1.
				return original, boundProveJob("j", "d", 1, "any-hash")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			original, conflictReq := tc.run(t, s)

			if _, err := s.SubmitJob(conflictReq); !errors.Is(err, ErrConflict) {
				t.Fatalf("want conflict, got %v", err)
			}
			got, err := s.GetJob("j")
			if err != nil {
				t.Fatal(err)
			}
			if got != original {
				t.Fatalf("stored job changed after conflict:\n got %+v\nwant %+v", got, original)
			}
			jobs, _ := s.ListJobs()
			// The sanity-check fresh ids were refused and stored nothing, so the
			// conflict leaves exactly the original job.
			if len(jobs) != 1 {
				t.Fatalf("want exactly one job, got %+v", jobs)
			}

			// The identical original request still returns the same record.
			again, err := s.SubmitJob(original)
			if err != nil || again != original {
				t.Fatalf("identical resubmit after conflict: %+v %v", again, err)
			}
		})
	}
}

// An empty hash means unbound for the job's whole life: it is accepted on a
// not-yet-compiled version without compiling it, a later compilation never
// backfills the hash, an already-compiled target never auto-selects its
// artifact, counts-only versions keep their register-only ability, and no
// setup is auto-registered.
func TestEmptyHashLeavesJobUnbound(t *testing.T) {
	t.Run("uncompiled version does not compile or bind", func(t *testing.T) {
		dir := t.TempDir()
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		frozenSetupUncompiled(t, s, "c", 1)
		req := boundProveJob("ju", "c", 1, "")
		stored, err := s.SubmitJob(req)
		if err != nil {
			t.Fatalf("register-only submit: %v", err)
		}
		if stored != req || stored.CompiledHash != "" {
			t.Fatalf("unbound job stored a binding: %+v", stored)
		}
		// Submission must not have compiled the version on the side.
		if _, err := s.GetArtifact("c", 1); !errors.Is(err, ErrArtifactMissing) {
			t.Fatalf("submit auto-compiled the version: %v", err)
		}
		// Compiling afterwards never backfills the old job's binding.
		a, err := s.CompileCircuit("c", 1)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetJob("ju")
		if got.CompiledHash != "" {
			t.Fatalf("later compilation backfilled the hash: %+v", got)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s2, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer s2.Close()
		got, _ = s2.GetJob("ju")
		if got.CompiledHash != "" {
			t.Fatalf("empty binding did not survive reopen: %+v", got)
		}
		if a2, err := s2.GetArtifact("c", 1); err != nil || a2.Hash != a.Hash {
			t.Fatalf("compiled artifact state after reopen: %+v %v", a2, err)
		}
	})

	t.Run("compiled version does not auto-select artifact", func(t *testing.T) {
		s := openTestStore(t)
		h := compiledVersion(t, s, "c", 1, validDef)
		stored, err := s.SubmitJob(boundProveJob("ju", "c", 1, ""))
		if err != nil {
			t.Fatalf("unbound submit against compiled version: %v", err)
		}
		if stored.CompiledHash != "" {
			t.Fatalf("empty hash auto-bound to %q", stored.CompiledHash)
		}
		got, _ := s.GetJob("ju")
		if got.CompiledHash != "" {
			t.Fatalf("queried unbound job carries the artifact hash: %+v", got)
		}
		// A sibling explicit binding on the same version still works and stays
		// distinct.
		bound, err := s.SubmitJob(boundProveJob("jb", "c", 1, h))
		if err != nil {
			t.Fatal(err)
		}
		if bound.CompiledHash != h {
			t.Fatalf("explicit binding wrong: %+v", bound)
		}
		jobs, _ := s.ListJobs()
		if len(jobs) != 2 {
			t.Fatalf("want 2 jobs, got %+v", jobs)
		}
		for _, j := range jobs {
			want := h
			if j.ID == "ju" {
				want = ""
			}
			if j.CompiledHash != want {
				t.Fatalf("job %q hash = %q, want %q", j.ID, j.CompiledHash, want)
			}
		}
	})

	t.Run("counts-only version keeps register-only ability", func(t *testing.T) {
		s := openTestStore(t)
		readyCircuit(t, s, "bare", 1)
		stored, err := s.SubmitJob(boundProveJob("jb", "bare", 1, ""))
		if err != nil {
			t.Fatalf("counts-only register-only submit: %v", err)
		}
		if stored.CompiledHash != "" {
			t.Fatalf("counts-only job carries a hash: %+v", stored)
		}
		// This version can never compile (no definition imported); the job
		// remains valid and unbound.
		if _, err := s.CompileCircuit("bare", 1); !errors.Is(err, ErrDefinitionMissing) {
			t.Fatalf("want definition missing, got %v", err)
		}
		got, err := s.GetJob("jb")
		if err != nil || got != stored {
			t.Fatalf("counts-only job after failed compile: %+v %v", got, err)
		}
	})

	t.Run("missing setup is not auto-registered even with empty hash", func(t *testing.T) {
		s := openTestStore(t)
		// Frozen and compiled, but deliberately no trusted setup.
		seedDraftWithDef(t, s, "ns", 1, 1, 1, validDef)
		if _, err := s.FreezeCircuit("ns", 1); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompileCircuit("ns", 1); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SubmitJob(boundProveJob("jx", "ns", 1, "")); !errors.Is(err, ErrSetupMissing) {
			t.Fatalf("want trusted setup missing, got %v", err)
		}
		if _, found, err := s.GetSetup("ns", 1); err != nil || found {
			t.Fatalf("submit auto-registered a setup: found=%v err=%v", found, err)
		}
		if _, err := s.GetJob("jx"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("refused job left a record: %v", err)
		}
	})
}
