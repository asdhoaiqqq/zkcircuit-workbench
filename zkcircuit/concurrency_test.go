package zkcircuit

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// In-process: two open handles on one directory are independent open file
// descriptions with separate flock state. Concurrent creators on disjoint
// keys must all have their committed records preserved.
func TestTwoHandlesConcurrentCreates(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	defer s2.Close()

	const n = 60
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_, e := s1.CreateCircuit(Circuit{Name: fmt.Sprintf("a%03d", i), Version: 1, Constraints: 1, Description: "x"})
			errs <- e
		}(i)
		go func(i int) {
			defer wg.Done()
			_, e := s2.CreateCircuit(Circuit{Name: fmt.Sprintf("b%03d", i), Version: 1, Constraints: 1, Description: "x"})
			errs <- e
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatalf("concurrent create: %v", e)
		}
	}

	s3, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s3.Close()
	list, err := s3.ListCircuits()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2*n {
		t.Fatalf("lost committed records: want %d, got %d", 2*n, len(list))
	}
}

// The draft/freeze race can only end in one of two complete states:
//  1. a complete updated description was committed and then frozen, or
//  2. the freeze landed first and every later update was rejected.
//
// A half-updated record must never appear, and the on-disk state after
// reopen must equal the final in-memory state.
func TestFreezeUpdateRaceNoPartialState(t *testing.T) {
	for iter := 0; iter < 25; iter++ {
		dir := t.TempDir()
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1, Description: "desc-1"}); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			for i := 2; i <= 200; i++ {
				_, err := s.UpdateCircuit(Circuit{Name: "c", Version: 1, Constraints: i, Description: fmt.Sprintf("desc-%d", i)})
				if err != nil {
					if !errors.Is(err, ErrFrozen) {
						t.Errorf("unexpected update error: %v", err)
					}
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			s.FreezeCircuit("c", 1)
		}()
		close(start)
		wg.Wait()

		got, err := s.GetCircuit("c", 1)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Frozen {
			t.Fatalf("iter %d: version not frozen after race", iter)
		}
		wantDesc := fmt.Sprintf("desc-%d", got.Constraints)
		if got.Description != wantDesc {
			t.Fatalf("iter %d: partial update visible: constraints=%d description=%q (want %q)",
				iter, got.Constraints, got.Description, wantDesc)
		}
		if _, err := s.UpdateCircuit(Circuit{Name: "c", Version: 1, Constraints: 777, Description: "hax"}); !errors.Is(err, ErrFrozen) {
			t.Fatalf("post-freeze update: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}

		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("iter %d: reopen: %v", iter, err)
		}
		disk, err := s2.GetCircuit("c", 1)
		s2.Close()
		if err != nil || disk != got {
			t.Fatalf("iter %d: disk state %+v != memory state %+v (err=%v)", iter, disk, got, err)
		}
	}
}

// Concurrent submission of identical create requests must never conflict or
// duplicate.
func TestConcurrentIdempotentCreate(t *testing.T) {
	dir := t.TempDir()
	s1, _ := Open(dir)
	s2, _ := Open(dir)
	defer s1.Close()
	defer s2.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	c := Circuit{Name: "same", Version: 1, Constraints: 8, Description: "d"}
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, e := s1.CreateCircuit(c); errs <- e }()
		go func() { defer wg.Done(); _, e := s2.CreateCircuit(c); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatalf("idempotent concurrent create failed: %v", e)
		}
	}
	list, _ := s1.ListCircuits()
	if len(list) != 1 {
		t.Fatalf("want exactly 1 record, got %d: %+v", len(list), list)
	}
}

// Concurrent identical job submissions likewise collapse to one job.
func TestConcurrentIdempotentSubmit(t *testing.T) {
	dir := t.TempDir()
	s1, _ := Open(dir)
	s2, _ := Open(dir)
	defer s1.Close()
	defer s2.Close()
	readyCircuit(t, s1, "c", 1)

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	job := Job{ID: "j1", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1}
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, e := s1.SubmitJob(job); errs <- e }()
		go func() { defer wg.Done(); _, e := s2.SubmitJob(job); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatalf("idempotent concurrent submit failed: %v", e)
		}
	}
	jobs, _ := s1.ListJobs()
	if len(jobs) != 1 || jobs[0] != job {
		t.Fatalf("want exactly one pinned job, got %+v", jobs)
	}
}

// --- cross-process tests via re-exec -------------------------------------

// TestMain recognizes ZKCIRCUIT_HELPER=<mode>: instead of running tests the
// process performs one scripted workbench session, printing "ACK …" lines
// for operations that returned successfully. The parent verifies every ACK
// survived after the helpers exit (or are killed).
func TestMain(m *testing.M) {
	switch mode := os.Getenv("ZKCIRCUIT_HELPER"); mode {
	case "writer":
		helperWriter()
	case "crash-writer":
		helperCrashWriter()
	case "setup-job-writer":
		helperSetupJobWriter()
	default:
		os.Exit(m.Run())
	}
}

func helperOpen(dir string) *Store {
	s, err := Open(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper open: %v\n", err)
		os.Exit(10)
	}
	return s
}

func helperWriter() {
	dir := os.Getenv("ZKCIRCUIT_DIR")
	prefix := os.Getenv("ZKCIRCUIT_PREFIX")
	n, _ := strconv.Atoi(os.Getenv("ZKCIRCUIT_N"))
	s := helperOpen(dir)
	defer s.Close()
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for v := 1; v <= n; v++ {
		if _, err := s.CreateCircuit(Circuit{Name: prefix, Version: v, Constraints: v, Description: prefix}); err != nil {
			fmt.Fprintf(os.Stderr, "helper create: %v\n", err)
			out.Flush()
			os.Exit(11)
		}
		if _, err := s.FreezeCircuit(prefix, v); err != nil {
			fmt.Fprintf(os.Stderr, "helper freeze: %v\n", err)
			out.Flush()
			os.Exit(12)
		}
		fmt.Fprintf(out, "ACK circuit %s %d\n", prefix, v)
		out.Flush()
	}
}

func helperSetupJobWriter() {
	dir := os.Getenv("ZKCIRCUIT_DIR")
	prefix := os.Getenv("ZKCIRCUIT_PREFIX")
	n, _ := strconv.Atoi(os.Getenv("ZKCIRCUIT_N"))
	s := helperOpen(dir)
	defer s.Close()
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for v := 1; v <= n; v++ {
		var last error
		for i := 0; i < 1000; i++ {
			_, last = s.RecordSetup(prefix, v)
			if last == nil || (!errors.Is(last, ErrNotFrozen) && !errors.Is(last, ErrNotFound)) {
				break
			}
		}
		if last != nil {
			fmt.Fprintf(os.Stderr, "helper setup: %v\n", last)
			out.Flush()
			os.Exit(13)
		}
		job := Job{ID: fmt.Sprintf("%s-job-%d", prefix, v), Circuit: prefix, Version: v, Kind: "prove", Attempt: 1}
		stored, err := s.SubmitJob(job)
		if err != nil {
			fmt.Fprintf(os.Stderr, "helper submit: %v\n", err)
			out.Flush()
			os.Exit(14)
		}
		fmt.Fprintf(out, "ACK job %s\n", stored.ID)
		out.Flush()
	}
}

func helperCrashWriter() {
	dir := os.Getenv("ZKCIRCUIT_DIR")
	prefix := os.Getenv("ZKCIRCUIT_PREFIX")
	s := helperOpen(dir)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for v := 1; ; v++ {
		if _, err := s.CreateCircuit(Circuit{Name: prefix, Version: v, Constraints: v, Description: prefix}); err != nil {
			fmt.Fprintf(os.Stderr, "crash writer create: %v\n", err)
			return
		}
		if _, err := s.FreezeCircuit(prefix, v); err != nil {
			fmt.Fprintf(os.Stderr, "crash writer freeze: %v\n", err)
			return
		}
		fmt.Fprintf(out, "ACK circuit %s %d\n", prefix, v)
		out.Flush()
	}
}

// safeBuffer is a bytes.Buffer safe for the os/exec writer goroutine and the
// test goroutine to touch concurrently.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *safeBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Bytes()
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type helperProc struct {
	cmd *exec.Cmd
	out *safeBuffer
	err *safeBuffer
}

func startHelper(t *testing.T, dir, mode string, extra ...string) *helperProc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(),
		"ZKCIRCUIT_HELPER="+mode,
		"ZKCIRCUIT_DIR="+dir,
	)
	cmd.Env = append(cmd.Env, extra...)
	h := &helperProc{cmd: cmd, out: &safeBuffer{}, err: &safeBuffer{}}
	cmd.Stdout = h.out
	cmd.Stderr = h.err
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	return h
}

func (h *helperProc) wait(t *testing.T) []ack {
	t.Helper()
	if err := h.cmd.Wait(); err != nil {
		t.Fatalf("helper exited: %v\nstderr:\n%s", err, h.err.String())
	}
	return parseAcks(t, h.out.Bytes())
}

type ack struct {
	kind string
	args string
}

func parseAcks(t *testing.T, raw []byte) []ack {
	t.Helper()
	var acks []ack
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "ACK ") {
			t.Fatalf("unexpected helper output: %q", line)
		}
		rest := strings.TrimPrefix(line, "ACK ")
		parts := strings.SplitN(rest, " ", 2)
		acks = append(acks, ack{kind: parts[0], args: parts[1]})
	}
	return acks
}

// parseAcksLenient is used on output of SIGKILLed processes: a final line
// could in principle be cut mid-write, so incomplete last lines are ignored
// rather than fatal.
func parseAcksLenient(t *testing.T, raw []byte) []ack {
	t.Helper()
	var acks []ack
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "ACK ") {
			continue
		}
		rest := strings.TrimPrefix(line, "ACK ")
		parts := strings.SplitN(rest, " ", 2)
		if len(parts) != 2 {
			continue
		}
		acks = append(acks, ack{kind: parts[0], args: parts[1]})
	}
	return acks
}

// Two real processes create disjoint circuit versions; two more then
// register setups and submit jobs concurrently. Every acknowledged record
// must be present exactly once in the final directory.
func TestCrossProcessWriters(t *testing.T) {
	if os.Getenv("ZKCIRCUIT_HELPER") != "" {
		t.Skip()
	}
	dir := filepath.Join(t.TempDir(), "bench")
	const per = 25

	writers := []*helperProc{
		startHelper(t, dir, "writer", "ZKCIRCUIT_PREFIX=aaa", "ZKCIRCUIT_N="+strconv.Itoa(per)),
		startHelper(t, dir, "writer", "ZKCIRCUIT_PREFIX=bbb", "ZKCIRCUIT_N="+strconv.Itoa(per)),
	}
	var allAcks []ack
	for _, h := range writers {
		allAcks = append(allAcks, h.wait(t)...)
	}

	jobWriters := []*helperProc{
		startHelper(t, dir, "setup-job-writer", "ZKCIRCUIT_PREFIX=aaa", "ZKCIRCUIT_N="+strconv.Itoa(per)),
		startHelper(t, dir, "setup-job-writer", "ZKCIRCUIT_PREFIX=bbb", "ZKCIRCUIT_N="+strconv.Itoa(per)),
	}
	for _, h := range jobWriters {
		allAcks = append(allAcks, h.wait(t)...)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("final open: %v", err)
	}
	defer s.Close()
	circuits, err := s.ListCircuits()
	if err != nil {
		t.Fatal(err)
	}
	if len(circuits) != 2*per {
		t.Fatalf("want %d circuits, got %d", 2*per, len(circuits))
	}
	jobs, err := s.ListJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2*per {
		t.Fatalf("want %d jobs, got %d", 2*per, len(jobs))
	}
	for _, c := range circuits {
		if !c.Frozen {
			t.Fatalf("circuit %s v%d not frozen", c.Name, c.Version)
		}
	}

	seen := make(map[string]int)
	for _, a := range allAcks {
		seen[a.kind+":"+a.args]++
	}
	for token, n := range seen {
		if n != 1 {
			t.Fatalf("ack %q observed %d times", token, n)
		}
	}
	for _, a := range allAcks {
		switch a.kind {
		case "circuit":
			parts := strings.Split(a.args, " ")
			v, _ := strconv.Atoi(parts[1])
			if _, err := s.GetCircuit(parts[0], v); err != nil {
				t.Fatalf("acknowledged circuit %s missing: %v", a.args, err)
			}
		case "job":
			if _, err := s.GetJob(a.args); err != nil {
				t.Fatalf("acknowledged job %s missing: %v", a.args, err)
			}
		}
	}
}

// Kill writers mid-flight with SIGKILL. Reopening afterwards must show a
// complete state, and every record acknowledged before the kill must still
// be present and complete.
func TestCrossProcessCrashRecovery(t *testing.T) {
	if os.Getenv("ZKCIRCUIT_HELPER") != "" {
		t.Skip()
	}
	dir := filepath.Join(t.TempDir(), "bench")

	const victims = 6
	procs := make([]*helperProc, 0, victims)
	for i := 0; i < victims; i++ {
		procs = append(procs, startHelper(t, dir, "crash-writer",
			"ZKCIRCUIT_PREFIX="+fmt.Sprintf("p%02d", i)))
	}

	// Wait until every victim has acknowledged at least one durable commit,
	// then kill them staggered so some die mid-commit and others between
	// commits.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ready := 0
		for _, p := range procs {
			if p.out.Len() > 0 {
				ready++
			}
		}
		if ready == victims {
			break
		}
		time.Sleep(time.Millisecond)
	}
	for i, p := range procs {
		if err := p.cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
		if i == 1 {
			// Let the middle victims keep running a few ms longer so kills
			// land at genuinely different commit points.
			time.Sleep(15 * time.Millisecond)
		}
	}

	var acks []ack
	for _, p := range procs {
		_ = p.cmd.Wait()
		acks = append(acks, parseAcksLenient(t, p.out.Bytes())...)
	}
	if len(acks) == 0 {
		t.Fatal("no acknowledged commits before kills; test setup is broken")
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen after SIGKILL storm: %v", err)
	}
	defer s.Close()
	for _, a := range acks {
		parts := strings.Split(a.args, " ")
		v, _ := strconv.Atoi(parts[1])
		c, err := s.GetCircuit(parts[0], v)
		if err != nil {
			t.Fatalf("acknowledged record %q lost after crash: %v", a.args, err)
		}
		if !c.Frozen || c.Constraints != v || c.Description != parts[0] {
			t.Fatalf("acknowledged record %q incomplete on disk: %+v", a.args, c)
		}
	}
	list, _ := s.ListCircuits()
	if len(list) < len(acks) {
		t.Fatalf("records vanished: %d acknowledged, %d on disk", len(acks), len(list))
	}
	t.Logf("crash storm: %d acknowledged records before kill, %d records final", len(acks), len(list))
}
