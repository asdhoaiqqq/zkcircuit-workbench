package zkcircuit

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateCircuitValidation(t *testing.T) {
	s := openTestStore(t)

	cases := []struct {
		name string
		c    Circuit
		want error
	}{
		{"empty name", Circuit{Name: "  ", Version: 1, Constraints: 1}, ErrInvalidArgument},
		{"zero version", Circuit{Name: "c", Version: 0, Constraints: 1}, ErrInvalidArgument},
		{"negative version", Circuit{Name: "c", Version: -1, Constraints: 1}, ErrInvalidArgument},
		{"zero constraints", Circuit{Name: "c", Version: 1, Constraints: 0}, ErrInvalidArgument},
		{"negative public", Circuit{Name: "c", Version: 1, Constraints: 1, PublicInputs: -1}, ErrInvalidArgument},
		{"negative private", Circuit{Name: "c", Version: 1, Constraints: 1, PrivateInputs: -2}, ErrInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.CreateCircuit(tc.c); !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}

	// Zero counts rejected on update as well.
	if _, err := s.UpdateCircuit(Circuit{Name: "c", Version: 1, Constraints: 0}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("update validation: %v", err)
	}
	// Nothing was created despite the rejected attempts.
	list, err := s.ListCircuits()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("rejected creates left records: %+v", list)
	}
}

func TestCreateIdempotenceAndConflict(t *testing.T) {
	s := openTestStore(t)
	c := Circuit{Name: "transfer", Version: 3, Constraints: 2048, PublicInputs: 2, PrivateInputs: 4, Description: "d3"}

	first, err := s.CreateCircuit(c)
	if err != nil {
		t.Fatal(err)
	}
	if first.Frozen {
		t.Fatal("new circuit must be a draft")
	}
	// Same name+version with the identical description returns the stored
	// record, regardless of the other (ignored-on-existing) count arguments.
	same := Circuit{Name: "transfer", Version: 3, Constraints: 9999, PublicInputs: 9, PrivateInputs: 9, Description: "d3"}
	got, err := s.CreateCircuit(same)
	if err != nil {
		t.Fatalf("identical re-create: %v", err)
	}
	if got.Constraints != 2048 || got.PublicInputs != 2 || got.PrivateInputs != 4 || got.Frozen {
		t.Fatalf("re-create altered stored record: %+v", got)
	}
	// Different description is a conflict and changes nothing.
	conflict := c
	conflict.Description = "different"
	if _, err := s.CreateCircuit(conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	got, err = s.GetCircuit("transfer", 3)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "d3" {
		t.Fatalf("conflicting create mutated description: %q", got.Description)
	}

	// Multiple versions of the same name coexist.
	v4 := Circuit{Name: "transfer", Version: 4, Constraints: 4096, Description: "d4"}
	if _, err := s.CreateCircuit(v4); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetCircuit("transfer", 3)
	if err != nil || got.Version != 3 {
		t.Fatalf("v3 displaced by v4: %+v %v", got, err)
	}
}

func TestUpdateRequiresDraft(t *testing.T) {
	s := openTestStore(t)

	if _, err := s.UpdateCircuit(Circuit{Name: "ghost", Version: 1, Constraints: 10}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: want not found, got %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 100, Description: "orig"}); err != nil {
		t.Fatal(err)
	}
	updated, err := s.UpdateCircuit(Circuit{Name: "c", Version: 1, Constraints: 200, PublicInputs: 1, Description: "next"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Constraints != 200 || updated.PublicInputs != 1 || updated.Description != "next" || updated.Frozen {
		t.Fatalf("bad update result: %+v", updated)
	}
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateCircuit(Circuit{Name: "c", Version: 1, Constraints: 300, Description: "next"}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("update frozen: want frozen, got %v", err)
	}
	got, _ := s.GetCircuit("c", 1)
	if got.Constraints != 200 || got.Description != "next" || !got.Frozen {
		t.Fatalf("frozen record mutated by rejected update: %+v", got)
	}
	// A later version cannot replace the frozen one.
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 2, Constraints: 1}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetCircuit("c", 1)
	if got.Constraints != 200 || !got.Frozen {
		t.Fatalf("new version displaced frozen v1: %+v", got)
	}
}

func TestFreezeIdempotentAndImmutable(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.FreezeCircuit("nope", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 10, Description: "d"}); err != nil {
		t.Fatal(err)
	}
	f1, err := s.FreezeCircuit("c", 1)
	if err != nil || !f1.Frozen {
		t.Fatalf("freeze: %+v %v", f1, err)
	}
	f2, err := s.FreezeCircuit("c", 1)
	if err != nil || !f2.Frozen || f2 != f1 {
		t.Fatalf("re-freeze not idempotent: %+v %v", f2, err)
	}
}

func TestSetupScopedToFrozenVersion(t *testing.T) {
	s := openTestStore(t)
	mk := func(name string, v int, frozen bool) {
		t.Helper()
		if _, err := s.CreateCircuit(Circuit{Name: name, Version: v, Constraints: 64, Description: fmt.Sprintf("%s%d", name, v)}); err != nil {
			t.Fatal(err)
		}
		if frozen {
			if _, err := s.FreezeCircuit(name, v); err != nil {
				t.Fatal(err)
			}
		}
	}

	if _, err := s.RecordSetup("ghost", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	mk("c", 1, false)
	if _, err := s.RecordSetup("c", 1); !errors.Is(err, ErrNotFrozen) {
		t.Fatalf("want not frozen, got %v", err)
	}
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSetup("c", 1); err != nil {
		t.Fatalf("record setup: %v", err)
	}
	// Idempotent: repeat registration is harmless.
	if _, err := s.RecordSetup("c", 1); err != nil {
		t.Fatalf("repeat setup: %v", err)
	}
	if setup, found, err := s.GetSetup("c", 1); err != nil || !found || setup.Version != 1 {
		t.Fatalf("get setup: %+v found=%v err=%v", setup, found, err)
	}
	if before := countSetups(t, s); before != 1 {
		t.Fatalf("want 1 setup record, got %d", before)
	}
	// A sibling frozen version cannot borrow the setup.
	mk("c", 2, true)
	if _, err := s.SubmitJob(Job{ID: "jv2", Circuit: "c", Version: 2, Kind: "prove", Attempt: 1}); !errors.Is(err, ErrSetupMissing) {
		t.Fatalf("want setup missing for v2, got %v", err)
	}
	if _, found, err := s.GetSetup("c", 2); err != nil || found {
		t.Fatalf("v2 should have no setup, found=%v err=%v", found, err)
	}
	if _, err := s.RecordSetup("c", 2); err != nil {
		t.Fatalf("record v2 setup: %v", err)
	}
	if got, err := s.SubmitJob(Job{ID: "jv2", Circuit: "c", Version: 2, Kind: "prove", Attempt: 1}); err != nil {
		t.Fatalf("v2 job after its own setup: %v", got)
	}
	if countSetups(t, s) != 2 {
		t.Fatalf("want 2 setups, got %d", countSetups(t, s))
	}
}

func countSetups(t *testing.T, s *Store) int {
	t.Helper()
	// No exported setup-list API; count through repeated job eligibility is
	// indirect, so inspect the committed envelope directly.
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := lockShared(s.lock); err != nil {
		t.Fatal(err)
	}
	defer unlockLock(s.lock)
	if err := s.loadLocked(); err != nil {
		t.Fatal(err)
	}
	return len(s.data.Setups)
}

func readyCircuit(t *testing.T, s *Store, name string, v int) {
	t.Helper()
	if _, err := s.CreateCircuit(Circuit{Name: name, Version: v, Constraints: 128, Description: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit(name, v); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSetup(name, v); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitJobFailuresAreDistinguishableAndLeaveNoTrace(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateCircuit(Circuit{Name: "draft", Version: 1, Constraints: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "frozen", Version: 1, Constraints: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit("frozen", 1); err != nil {
		t.Fatal(err)
	}
	readyCircuit(t, s, "ready", 1)

	cases := []struct {
		name string
		job  Job
		want error
	}{
		{"blank id", Job{ID: "   ", Circuit: "ready", Version: 1, Kind: "prove", Attempt: 1}, ErrInvalidArgument},
		{"blank circuit", Job{ID: "j", Circuit: "  ", Version: 1, Kind: "prove", Attempt: 1}, ErrInvalidArgument},
		{"zero version", Job{ID: "j", Circuit: "ready", Version: 0, Kind: "prove", Attempt: 1}, ErrInvalidArgument},
		{"zero attempt", Job{ID: "j", Circuit: "ready", Version: 1, Kind: "prove", Attempt: 0}, ErrInvalidArgument},
		{"bad kind", Job{ID: "j", Circuit: "ready", Version: 1, Kind: "verify", Attempt: 1}, ErrUnsupportedKind},
		{"unknown circuit", Job{ID: "j", Circuit: "ghost", Version: 1, Kind: "prove", Attempt: 1}, ErrNotFound},
		{"unknown version", Job{ID: "j", Circuit: "ready", Version: 9, Kind: "prove", Attempt: 1}, ErrNotFound},
		{"not frozen", Job{ID: "j", Circuit: "draft", Version: 1, Kind: "prove", Attempt: 1}, ErrNotFrozen},
		{"no setup", Job{ID: "j", Circuit: "frozen", Version: 1, Kind: "prove", Attempt: 1}, ErrSetupMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.SubmitJob(tc.job)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if _, err := s.GetJob(tc.job.ID); tc.job.ID != "" && strings.TrimSpace(tc.job.ID) != "" && !errors.Is(err, ErrNotFound) {
				t.Fatalf("refused job %q left a trace: %v / %v", tc.name, err, tc.job.ID)
			}
		})
	}
	jobs, _ := s.ListJobs()
	if len(jobs) != 0 {
		t.Fatalf("refused submissions stored jobs: %+v", jobs)
	}
}

func TestSubmitJobIdempotenceConflictAndPinning(t *testing.T) {
	s := openTestStore(t)
	readyCircuit(t, s, "c", 1)

	j1 := Job{ID: "job-1", Circuit: "c", Version: 1, Kind: "prove", Attempt: 2}
	stored, err := s.SubmitJob(j1)
	if err != nil {
		t.Fatal(err)
	}
	if stored != j1 {
		t.Fatalf("stored job differs: %+v", stored)
	}
	again, err := s.SubmitJob(j1)
	if err != nil || again != stored {
		t.Fatalf("resubmit not idempotent: %+v %v", again, err)
	}
	// Same id, different request content -> conflict.
	diff := j1
	diff.Attempt = 3
	if _, err := s.SubmitJob(diff); !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	diff = j1
	diff.Version = 2
	if _, err := s.SubmitJob(diff); !errors.Is(err, ErrConflict) {
		t.Fatalf("version drift resubmit: want conflict, got %v", err)
	}
	got, _ := s.GetJob("job-1")
	if got.Attempt != 2 || got.Version != 1 {
		t.Fatalf("conflicting resubmit mutated stored job: %+v", got)
	}

	// Later circuit versions never change what the stored job binds to.
	readyCircuit(t, s, "c", 2)
	got, _ = s.GetJob("job-1")
	if got.Circuit != "c" || got.Version != 1 {
		t.Fatalf("job binding drifted: %+v", got)
	}
	jobs, _ := s.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("want exactly one job, got %+v", jobs)
	}
}

func TestListCircuitOrdering(t *testing.T) {
	s := openTestStore(t)
	plan := []struct {
		name string
		v    int
	}{
		{"zeta", 2}, {"alpha", 9}, {"alpha", 2}, {"zeta", 1}, {"alpha", 10},
	}
	for _, p := range plan {
		if _, err := s.CreateCircuit(Circuit{Name: p.name, Version: p.v, Constraints: 1, Description: "d"}); err != nil {
			t.Fatal(err)
		}
	}
	circuits, err := s.ListCircuits()
	if err != nil {
		t.Fatal(err)
	}
	got := make([][2]string, 0, len(circuits))
	for _, c := range circuits {
		got = append(got, [2]string{c.Name, fmt.Sprint(c.Version)})
	}
	want := [][2]string{{"alpha", "2"}, {"alpha", "9"}, {"alpha", "10"}, {"zeta", "1"}, {"zeta", "2"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("circuit order:\n got %v\nwant %v", got, want)
	}
}

func TestListJobsOrdering(t *testing.T) {
	s := openTestStore(t)
	readyCircuit(t, s, "c", 1)
	ids := []string{"j9", "j1", "j10", "j2"}
	for _, id := range ids {
		if _, err := s.SubmitJob(Job{ID: id, Circuit: "c", Version: 1, Kind: "prove", Attempt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := s.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(jobs))
	for i, j := range jobs {
		got[i] = j.ID
	}
	want := []string{"j1", "j10", "j2", "j9"}
	if !sort.StringsAreSorted(got) || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("job order: got %v want %v", got, want)
	}
}

func TestReopenPreservesState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	readyCircuit(t, s, "c", 1)
	if _, err := s.SubmitJob(Job{ID: "j1", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 2, Constraints: 7, Description: "draft-v2"}); err != nil {
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
	c, err := s2.GetCircuit("c", 1)
	if err != nil || !c.Frozen || c.Constraints != 128 {
		t.Fatalf("frozen circuit lost: %+v %v", c, err)
	}
	if setup, found, _ := s2.GetSetup("c", 1); !found {
		t.Fatalf("setup lost: %+v", setup)
	}
	j, err := s2.GetJob("j1")
	if err != nil || j.Version != 1 {
		t.Fatalf("job lost: %+v %v", j, err)
	}
	c2, err := s2.GetCircuit("c", 2)
	if err != nil || c2.Frozen || c2.Constraints != 7 {
		t.Fatalf("draft lost: %+v %v", c2, err)
	}
}

func TestRejectedOperationWritesNothing(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	readyCircuit(t, s, "c", 1)
	if _, err := s.SubmitJob(Job{ID: "j1", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	dataBefore, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}

	// A batch of clearly rejected operations.
	rejected := []func() error{
		func() error {
			_, e := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1, Description: "other"})
			return e
		},
		func() error { _, e := s.UpdateCircuit(Circuit{Name: "c", Version: 1, Constraints: 5}); return e },
		func() error { _, e := s.FreezeCircuit("ghost", 1); return e },
		func() error { _, e := s.RecordSetup("ghost", 1); return e },
		func() error {
			_, e := s.SubmitJob(Job{ID: "jbad", Circuit: "ghost", Version: 1, Kind: "prove", Attempt: 1})
			return e
		},
	}
	for i, call := range rejected {
		if err := call(); err == nil {
			t.Fatalf("rejected call #%d unexpectedly succeeded", i)
		}
	}
	dataAfter, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(dataBefore) != string(dataAfter) {
		t.Fatalf("rejected operations changed the committed file\nbefore: %s\nafter:  %s", dataBefore, dataAfter)
	}
}
