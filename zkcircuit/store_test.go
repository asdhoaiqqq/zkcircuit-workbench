package zkcircuit

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func draft(name string, version int) Circuit {
	return Circuit{Name: name, Version: version, Description: "desc", Constraints: 100, PublicInputs: 2, PrivateInputs: 3}
}

func freezeAndSetup(t *testing.T, s *Store, name string, version int) {
	t.Helper()
	if _, err := s.FreezeCircuit(name, version); err != nil {
		t.Fatalf("FreezeCircuit: %v", err)
	}
	if _, err := s.RegisterTrustedSetup(name, version); err != nil {
		t.Fatalf("RegisterTrustedSetup: %v", err)
	}
}

// ---------------------------------------------------------------------------
// validation
// ---------------------------------------------------------------------------

func TestCreateValidation(t *testing.T) {
	cases := []struct {
		name    string
		circuit Circuit
	}{
		{"blank name", Circuit{Name: "   ", Version: 1, Constraints: 1}},
		{"empty name", Circuit{Name: "", Version: 1, Constraints: 1}},
		{"zero version", Circuit{Name: "a", Version: 0, Constraints: 1}},
		{"negative version", Circuit{Name: "a", Version: -1, Constraints: 1}},
		{"zero constraints", Circuit{Name: "a", Version: 1, Constraints: 0}},
		{"negative constraints", Circuit{Name: "a", Version: 1, Constraints: -1}},
		{"negative public", Circuit{Name: "a", Version: 1, Constraints: 1, PublicInputs: -1}},
		{"negative private", Circuit{Name: "a", Version: 1, Constraints: 1, PrivateInputs: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			_, err := s.CreateCircuit(tc.circuit)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("want ErrInvalidInput, got %v", err)
			}
			circuits, _ := s.ListCircuits()
			if len(circuits) != 0 {
				t.Fatalf("rejected operation left %d circuits", len(circuits))
			}
		})
	}
}

func TestNameIsTrimmed(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(Circuit{Name: "  foo  ", Version: 1, Description: "d", Constraints: 1}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetCircuit("foo", 1)
	if err != nil {
		t.Fatalf("GetCircuit with trimmed name: %v", err)
	}
	if got.Name != "foo" {
		t.Fatalf("name = %q, want foo", got.Name)
	}
	// Same name after trimming: idempotent, not a second record.
	if _, err := s.CreateCircuit(Circuit{Name: "foo", Version: 1, Description: "d", Constraints: 1}); err != nil {
		t.Fatal(err)
	}
	circuits, _ := s.ListCircuits()
	if len(circuits) != 1 {
		t.Fatalf("want 1 circuit, got %d", len(circuits))
	}
}

// ---------------------------------------------------------------------------
// create / update / freeze
// ---------------------------------------------------------------------------

func TestCreateIdempotentAndConflict(t *testing.T) {
	s := openTestStore(t)
	c := draft("c", 1)
	first, err := s.CreateCircuit(c)
	if err != nil {
		t.Fatal(err)
	}
	// Identical request: returns the original record.
	second, err := s.CreateCircuit(c)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("idempotent create returned a different record: %+v vs %+v", first, second)
	}
	// Different description: conflict.
	conflict := c
	conflict.Description = "other"
	if _, err := s.CreateCircuit(conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	circuits, _ := s.ListCircuits()
	if len(circuits) != 1 {
		t.Fatalf("conflict created a record: %d circuits", len(circuits))
	}
	if circuits[0].Description != "desc" {
		t.Fatalf("conflict changed description to %q", circuits[0].Description)
	}
}

func TestVersionsCoexist(t *testing.T) {
	s := openTestStore(t)
	for _, v := range []int{1, 2, 3} {
		c := draft("c", v)
		if _, err := s.CreateCircuit(c); err != nil {
			t.Fatal(err)
		}
	}
	circuits, _ := s.ListCircuits()
	if len(circuits) != 3 {
		t.Fatalf("want 3 coexisting versions, got %d", len(circuits))
	}
}

func TestUpdateDraft(t *testing.T) {
	s := openTestStore(t)
	c := draft("c", 1)
	if _, err := s.CreateCircuit(c); err != nil {
		t.Fatal(err)
	}
	c.Description = "new desc"
	c.Constraints = 999
	got, err := s.UpdateCircuit(c)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "new desc" || got.Constraints != 999 {
		t.Fatalf("update not applied: %+v", got)
	}
}

func TestUpdateFrozenRejected(t *testing.T) {
	s := openTestStore(t)
	c := draft("c", 1)
	if _, err := s.CreateCircuit(c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	c.Description = "hacked"
	if _, err := s.UpdateCircuit(c); !errors.Is(err, ErrCircuitFrozen) {
		t.Fatalf("want ErrCircuitFrozen, got %v", err)
	}
	got, _ := s.GetCircuit("c", 1)
	if got.Description != "desc" {
		t.Fatalf("frozen circuit changed to %q", got.Description)
	}
}

func TestUpdateMissing(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.UpdateCircuit(draft("c", 1)); !errors.Is(err, ErrCircuitNotFound) {
		t.Fatalf("want ErrCircuitNotFound, got %v", err)
	}
}

func TestFreezeIdempotent(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(draft("c", 1)); err != nil {
		t.Fatal(err)
	}
	first, err := s.FreezeCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.FreezeCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Frozen || !second.Frozen {
		t.Fatal("frozen flag not set")
	}
	if first != second {
		t.Fatalf("idempotent freeze returned different records: %+v vs %+v", first, second)
	}
}

func TestFreezeMissing(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.FreezeCircuit("c", 1); !errors.Is(err, ErrCircuitNotFound) {
		t.Fatalf("want ErrCircuitNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// trusted setup
// ---------------------------------------------------------------------------

func TestSetupRequiresFrozen(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(draft("c", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterTrustedSetup("c", 1); !errors.Is(err, ErrCircuitNotFrozen) {
		t.Fatalf("want ErrCircuitNotFrozen, got %v", err)
	}
	setups, _ := s.ListSetups()
	if len(setups) != 0 {
		t.Fatalf("rejected setup registration left %d records", len(setups))
	}
}

func TestSetupIdempotent(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(draft("c", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	first, err := s.RegisterTrustedSetup("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RegisterTrustedSetup("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("idempotent setup returned different records: %+v vs %+v", first, second)
	}
	setups, _ := s.ListSetups()
	if len(setups) != 1 {
		t.Fatalf("want 1 setup, got %d", len(setups))
	}
}

func TestSetupBelongsToVersion(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(draft("c", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(draft("c", 2)); err != nil {
		t.Fatal(err)
	}
	freezeAndSetup(t, s, "c", 1)
	if _, err := s.FreezeCircuit("c", 2); err != nil {
		t.Fatal(err)
	}
	// v2 is frozen but has no setup: it cannot borrow v1's setup.
	_, err := s.SubmitJob(Job{ID: "j", Circuit: "c", Version: 2, Kind: "prove", Attempt: 1})
	if !errors.Is(err, ErrTrustedSetupMissing) {
		t.Fatalf("want ErrTrustedSetupMissing, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// jobs
// ---------------------------------------------------------------------------

func TestJobSubmitHappyPathAndIdempotent(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(draft("c", 1)); err != nil {
		t.Fatal(err)
	}
	freezeAndSetup(t, s, "c", 1)
	job := Job{ID: "job-1", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1}
	first, err := s.SubmitJob(job)
	if err != nil {
		t.Fatal(err)
	}
	if first.Artifact != "accepted" {
		t.Fatalf("artifact = %q, want accepted", first.Artifact)
	}
	second, err := s.SubmitJob(job)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("idempotent submit returned different records: %+v vs %+v", first, second)
	}
	jobs, _ := s.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("want 1 job, got %d", len(jobs))
	}
}

func TestJobRejectionReasonsDistinguishable(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(draft("c", 1)); err != nil {
		t.Fatal(err)
	}
	freezeAndSetup(t, s, "c", 1)

	cases := []struct {
		name string
		job  Job
		want error
	}{
		{"missing version", Job{ID: "j1", Circuit: "c", Version: 9, Kind: "prove", Attempt: 1}, ErrCircuitNotFound},
		{"not frozen", Job{ID: "j2", Circuit: "c", Version: 2, Kind: "prove", Attempt: 1}, ErrCircuitNotFrozen},
		{"no setup", Job{ID: "j3", Circuit: "c", Version: 3, Kind: "prove", Attempt: 1}, ErrTrustedSetupMissing},
	}
	// v2 exists but is not frozen; v3 is frozen without setup.
	if _, err := s.CreateCircuit(draft("c", 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(draft("c", 3)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit("c", 3); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.SubmitJob(tc.job)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
	jobs, _ := s.ListJobs()
	if len(jobs) != 0 {
		t.Fatalf("rejected jobs left %d records", len(jobs))
	}
}

func TestJobValidation(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(draft("c", 1)); err != nil {
		t.Fatal(err)
	}
	freezeAndSetup(t, s, "c", 1)
	cases := []struct {
		name string
		job  Job
	}{
		{"blank id", Job{ID: "   ", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1}},
		{"empty id", Job{ID: "", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1}},
		{"blank circuit", Job{ID: "j", Circuit: "  ", Version: 1, Kind: "prove", Attempt: 1}},
		{"zero version", Job{ID: "j", Circuit: "c", Version: 0, Kind: "prove", Attempt: 1}},
		{"zero attempt", Job{ID: "j", Circuit: "c", Version: 1, Kind: "prove", Attempt: 0}},
		{"negative attempt", Job{ID: "j", Circuit: "c", Version: 1, Kind: "prove", Attempt: -1}},
		{"verify kind", Job{ID: "j", Circuit: "c", Version: 1, Kind: "verify", Attempt: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.SubmitJob(tc.job)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("want ErrInvalidInput, got %v", err)
			}
		})
	}
	jobs, _ := s.ListJobs()
	if len(jobs) != 0 {
		t.Fatalf("rejected jobs left %d records", len(jobs))
	}
}

func TestJobConflict(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(draft("c", 1)); err != nil {
		t.Fatal(err)
	}
	freezeAndSetup(t, s, "c", 1)
	job := Job{ID: "j", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1}
	if _, err := s.SubmitJob(job); err != nil {
		t.Fatal(err)
	}
	job.Attempt = 2
	if _, err := s.SubmitJob(job); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	jobs, _ := s.ListJobs()
	if len(jobs) != 1 || jobs[0].Attempt != 1 {
		t.Fatalf("conflict changed the stored job: %+v", jobs)
	}
}

func TestJobPinsVersion(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(draft("c", 1)); err != nil {
		t.Fatal(err)
	}
	freezeAndSetup(t, s, "c", 1)
	if _, err := s.SubmitJob(Job{ID: "j", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	// A new version appears after submission; the job must stay pinned to v1.
	if _, err := s.CreateCircuit(draft("c", 2)); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.ListJobs()
	if len(jobs) != 1 || jobs[0].Version != 1 {
		t.Fatalf("job drifted: %+v", jobs)
	}
}

// ---------------------------------------------------------------------------
// ordering
// ---------------------------------------------------------------------------

func TestOrdering(t *testing.T) {
	s := openTestStore(t)
	for _, c := range []Circuit{
		draft("b", 1), draft("a", 2), draft("a", 1), draft("b", 2),
	} {
		if _, err := s.CreateCircuit(c); err != nil {
			t.Fatal(err)
		}
	}
	circuits, _ := s.ListCircuits()
	got := make([]string, 0, len(circuits))
	for _, c := range circuits {
		got = append(got, fmt.Sprintf("%s/%d", c.Name, c.Version))
	}
	want := []string{"a/1", "a/2", "b/1", "b/2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("circuit order = %v, want %v", got, want)
	}

	// Freeze+setup each version so jobs can be submitted.
	for _, c := range circuits {
		freezeAndSetup(t, s, c.Name, c.Version)
	}
	for _, id := range []string{"j2", "j1", "j3"} {
		if _, err := s.SubmitJob(Job{ID: id, Circuit: "a", Version: 1, Kind: "prove", Attempt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	jobs, _ := s.ListJobs()
	var ids []string
	for _, j := range jobs {
		ids = append(ids, j.ID)
	}
	if strings.Join(ids, ",") != "j1,j2,j3" {
		t.Fatalf("job order = %v, want j1,j2,j3", ids)
	}
}

// ---------------------------------------------------------------------------
// persistence
// ---------------------------------------------------------------------------

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(draft("c", 1)); err != nil {
		t.Fatal(err)
	}
	freezeAndSetup(t, s, "c", 1)
	if _, err := s.SubmitJob(Job{ID: "j", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1}); err != nil {
		t.Fatal(err)
	}

	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	circuits, _ := again.ListCircuits()
	if len(circuits) != 1 || !circuits[0].Frozen {
		t.Fatalf("circuits lost after reopen: %+v", circuits)
	}
	setups, _ := again.ListSetups()
	if len(setups) != 1 {
		t.Fatalf("setups lost after reopen: %+v", setups)
	}
	jobs, _ := again.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("jobs lost after reopen: %+v", jobs)
	}
}

func TestEmptyStore(t *testing.T) {
	s := openTestStore(t)
	circuits, err := s.ListCircuits()
	if err != nil {
		t.Fatal(err)
	}
	if circuits == nil || len(circuits) != 0 {
		t.Fatalf("empty store returned %+v", circuits)
	}
	jobs, err := s.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("empty store returned %+v", jobs)
	}
}

// ---------------------------------------------------------------------------
// corruption / crash recovery
// ---------------------------------------------------------------------------

func TestCorruptDataReportedAndPreserved(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(draft("c", 1)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, circuitsFile)
	original := []byte("{not valid json")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := s.ListCircuits(); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("want ErrCorruptData, got %v", err)
	}
	// The corrupt file must be left exactly as-is.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(original) {
		t.Fatalf("corrupt file was modified: %q", raw)
	}
	// And the store must not recover on its own.
	if _, err := s.ListCircuits(); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("second read: want ErrCorruptData, got %v", err)
	}
}

func TestTruncatedAndEmptyFiles(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content []byte
	}{
		{"truncated", []byte(`{"format_version":1,"circuits":[{"name":"c","version":1`)},
		{"empty", []byte{}},
		{"unsupported version", []byte(`{"format_version":2,"circuits":[]}`)},
		{"wrong shape", []byte(`{"format_version":1,"circuits":{}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, circuitsFile), tc.content, 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.ListCircuits(); !errors.Is(err, ErrCorruptData) {
				t.Fatalf("want ErrCorruptData, got %v", err)
			}
			raw, _ := os.ReadFile(filepath.Join(dir, circuitsFile))
			if string(raw) != string(tc.content) {
				t.Fatalf("file was modified")
			}
		})
	}
}

func TestLeftoverTempFileIgnored(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".tmp-orphan"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(draft("c", 1)); err != nil {
		t.Fatalf("orphan temp file broke operation: %v", err)
	}
	circuits, _ := s.ListCircuits()
	if len(circuits) != 1 {
		t.Fatalf("want 1 circuit, got %d", len(circuits))
	}
}

// ---------------------------------------------------------------------------
// concurrency
// ---------------------------------------------------------------------------

func TestConcurrentCreates(t *testing.T) {
	s := openTestStore(t)
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := draft(fmt.Sprintf("c-%d", i), 1)
			if _, err := s.CreateCircuit(c); err != nil {
				t.Errorf("create %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	circuits, _ := s.ListCircuits()
	if len(circuits) != n {
		t.Fatalf("want %d circuits, got %d", n, len(circuits))
	}
}

func TestConcurrentJobSubmitIdempotent(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(draft("c", 1)); err != nil {
		t.Fatal(err)
	}
	freezeAndSetup(t, s, "c", 1)
	job := Job{ID: "j", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1}
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.SubmitJob(job); err != nil {
				t.Errorf("submit: %v", err)
			}
		}()
	}
	wg.Wait()
	jobs, _ := s.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("want 1 job, got %d", len(jobs))
	}
}

func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "zkcircuit")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/zkcircuit")
	cmd.Dir = filepath.Join(wd, "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}
	return bin
}

func runCLI(t *testing.T, bin string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("CLI %v: %v\n%s", args, err, out)
	}
}

// runCLIQuiet runs the CLI without failing the test on error. Used for
// operations that are expected to be rejected under a race.
func runCLIQuiet(bin string, args ...string) {
	_ = exec.Command(bin, args...).Run()
}

func TestConcurrentProcesses(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	const n = 10
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("c-%d", i)
			runCLI(t, bin, "circuit", "create", "-dir", dir, "-name", name, "-version", "1",
				"-desc", "d", "-constraints", "100", "-public", "1", "-private", "1")
			runCLI(t, bin, "circuit", "freeze", "-dir", dir, "-name", name, "-version", "1")
			runCLI(t, bin, "setup", "register", "-dir", dir, "-name", name, "-version", "1")
			runCLI(t, bin, "job", "submit", "-dir", dir, "-id", fmt.Sprintf("j-%d", i),
				"-name", name, "-version", "1", "-attempt", "1")
		}(i)
	}
	wg.Wait()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	circuits, _ := s.ListCircuits()
	if len(circuits) != n {
		t.Fatalf("want %d circuits, got %d", n, len(circuits))
	}
	setups, _ := s.ListSetups()
	if len(setups) != n {
		t.Fatalf("want %d setups, got %d", n, len(setups))
	}
	jobs, _ := s.ListJobs()
	if len(jobs) != n {
		t.Fatalf("want %d jobs, got %d", n, len(jobs))
	}
}

func TestConcurrentProcessesSameCircuit(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	const n = 10
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runCLI(t, bin, "circuit", "create", "-dir", dir, "-name", "shared", "-version", "1",
				"-desc", "d", "-constraints", "100", "-public", "1", "-private", "1")
			runCLI(t, bin, "circuit", "freeze", "-dir", dir, "-name", "shared", "-version", "1")
			runCLI(t, bin, "setup", "register", "-dir", dir, "-name", "shared", "-version", "1")
			runCLI(t, bin, "job", "submit", "-dir", dir, "-id", fmt.Sprintf("j-%d", i),
				"-name", "shared", "-version", "1", "-attempt", "1")
		}(i)
	}
	wg.Wait()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	circuits, _ := s.ListCircuits()
	if len(circuits) != 1 {
		t.Fatalf("want 1 circuit, got %d", len(circuits))
	}
	setups, _ := s.ListSetups()
	if len(setups) != 1 {
		t.Fatalf("want 1 setup, got %d", len(setups))
	}
	jobs, _ := s.ListJobs()
	if len(jobs) != n {
		t.Fatalf("want %d jobs, got %d", n, len(jobs))
	}
}

// TestConcurrentModifyFreeze hammers the draft-update vs freeze race.
// Whatever interleaving wins, the frozen record must be either the
// original draft or one fully-written update — never a partial mix.
func TestConcurrentModifyFreeze(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	runCLI(t, bin, "circuit", "create", "-dir", dir, "-name", "race", "-version", "1",
		"-desc", "desc-0", "-constraints", "100", "-public", "2", "-private", "3")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for k := 1; k <= 30; k++ {
			// An update that lands after the freeze is rejected; that is
			// the expected outcome of the race, not a test failure.
			runCLIQuiet(bin, "circuit", "update", "-dir", dir, "-name", "race", "-version", "1",
				"-desc", fmt.Sprintf("desc-%d", k))
		}
	}()
	go func() {
		defer wg.Done()
		runCLI(t, bin, "circuit", "freeze", "-dir", dir, "-name", "race", "-version", "1")
	}()
	wg.Wait()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetCircuit("race", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Frozen {
		t.Fatal("circuit was not frozen")
	}
	if got.Constraints != 100 || got.PublicInputs != 2 || got.PrivateInputs != 3 {
		t.Fatalf("counts corrupted: %+v", got)
	}
	if !strings.HasPrefix(got.Description, "desc-") {
		t.Fatalf("description is not a fully written value: %q", got.Description)
	}
}
