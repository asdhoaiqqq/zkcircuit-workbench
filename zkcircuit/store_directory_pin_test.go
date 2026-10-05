package zkcircuit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Relative data-directory pinning. Once Open("bench-data") succeeds the
// directory is fixed for the store's lifetime; a later process working
// directory change must never move reads, writes, the lock, artifact/job
// bindings or Dir onto another bench-data.

const altDef = `{"modulus":"11","constraints":[
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"10"}]}]}`

// seedBenchData creates and closes a fully populated bench-data store at the
// given absolute directory (no chdir involved): one frozen, setup-registered
// and compiled circuit "c" v1 carrying desc, built from def.
func seedBenchData(t *testing.T, absDir, desc, def string) Artifact {
	t.Helper()
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		t.Fatal(err)
	}
	defPath := filepath.Join(t.TempDir(), "def.json")
	if err := os.WriteFile(defPath, []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(absDir)
	if err != nil {
		t.Fatalf("seed Open: %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1,
		PublicInputs: 1, PrivateInputs: 1, Description: desc}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportConstraints("c", 1, defPath); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSetup("c", 1); err != nil {
		t.Fatal(err)
	}
	a, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return a
}

// With a bench-data in each of two working directories, a store opened
// against the relative name in the first keeps operating on the first after
// the process moves to the second: reads return the first record, writes
// land in the first directory, and compiled-artifact / job bindings resolve
// against the first directory's frozen version even though the second holds
// the same name and version with a different (different-modulus) artifact.
func TestStorePinsRelativeDataDirectoryAcrossChdir(t *testing.T) {
	root := t.TempDir()
	w1 := filepath.Join(root, "w1")
	w2 := filepath.Join(root, "w2")
	art1 := seedBenchData(t, filepath.Join(w1, "bench-data"), "first description", validDef)
	art2 := seedBenchData(t, filepath.Join(w2, "bench-data"), "second description", altDef)
	if art1.Hash == art2.Hash {
		t.Fatal("seed artifacts must differ so a swapped directory is observable")
	}
	w2DataBefore, err := os.ReadFile(filepath.Join(w2, "bench-data", dirDataFile))
	if err != nil {
		t.Fatal(err)
	}

	t.Chdir(w1)
	s, err := Open("bench-data")
	if err != nil {
		t.Fatalf("Open relative: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if got := s.Dir(); got != filepath.Join(w1, "bench-data") {
		t.Fatalf("Dir = %q, want %q", got, filepath.Join(w1, "bench-data"))
	}

	// Move the process to the second working directory; the store must not
	// follow.
	t.Chdir(w2)

	if got := s.Dir(); got != filepath.Join(w1, "bench-data") {
		t.Fatalf("Dir after chdir = %q, want %q", got, filepath.Join(w1, "bench-data"))
	}
	c, err := s.GetCircuit("c", 1)
	if err != nil {
		t.Fatalf("GetCircuit: %v", err)
	}
	if c.Description != "first description" {
		t.Fatalf("read %q from the second directory; want the first directory's record", c.Description)
	}

	// Recompiling after the chdir must return the first directory's frozen
	// artifact hash, not the same-named version's artifact in the second.
	a, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatalf("CompileCircuit: %v", err)
	}
	if a.Hash != art1.Hash {
		t.Fatalf("artifact hash %q belongs to the second directory; want %q", a.Hash, art1.Hash)
	}
	if a.Hash == art2.Hash {
		t.Fatal("pinned store returned the second directory's artifact")
	}
	gotArt, err := s.GetArtifact("c", 1)
	if err != nil || gotArt.Hash != art1.Hash {
		t.Fatalf("GetArtifact = %+v, %v", gotArt, err)
	}

	// A job bound to that hash registers against the first directory's
	// version and is queryable from the same store afterwards.
	job, err := s.SubmitJob(Job{ID: "j1", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: art1.Hash})
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	if job.CompiledHash != art1.Hash {
		t.Fatalf("bound hash %q, want %q", job.CompiledHash, art1.Hash)
	}
	if _, err := s.GetJob("j1"); err != nil {
		t.Fatalf("GetJob: %v", err)
	}

	// A draft-only update round-trips through the first directory.
	if _, err := s.CreateCircuit(Circuit{Name: "draft", Version: 1, Constraints: 2, Description: "d"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateCircuit(Circuit{Name: "draft", Version: 1, Constraints: 3, Description: "edited in first"}); err != nil {
		t.Fatal(err)
	}

	w1Data, err := os.ReadFile(filepath.Join(w1, "bench-data", dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(w1Data), "first description") || !strings.Contains(string(w1Data), "edited in first") || !strings.Contains(string(w1Data), art1.Hash) || !strings.Contains(string(w1Data), `"j1"`) {
		t.Fatalf("first directory data.json missing pinned writes:\n%s", w1Data)
	}
	w2DataAfter, err := os.ReadFile(filepath.Join(w2, "bench-data", dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(w2DataAfter) != string(w2DataBefore) {
		t.Fatalf("second directory data.json changed while operating the first store:\nbefore:\n%s\nafter:\n%s", w2DataBefore, w2DataAfter)
	}
}

// When the new working directory has no bench-data, the store keeps reading
// and saving the original directory: no not-found errors, no save failures,
// and no second bench-data created in the new working directory.
func TestStoreStaysOnOriginalWhenNewCwdHasNoDataDir(t *testing.T) {
	root := t.TempDir()
	w1 := filepath.Join(root, "w1")
	w2 := filepath.Join(root, "w2")
	if err := os.MkdirAll(w2, 0o755); err != nil {
		t.Fatal(err)
	}
	seedBenchData(t, filepath.Join(w1, "bench-data"), "first description", validDef)

	t.Chdir(w1)
	s, err := Open("bench-data")
	if err != nil {
		t.Fatalf("Open relative: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	t.Chdir(w2)

	if _, err := s.GetCircuit("c", 1); err != nil {
		t.Fatalf("reading the original circuit after moving to an empty cwd: %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "late", Version: 1, Constraints: 1, Description: "saved in first"}); err != nil {
		t.Fatalf("saving after moving to an empty cwd: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w2, "bench-data")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a bench-data appeared in the new cwd: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(w1, "bench-data", dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "saved in first") {
		t.Fatalf("write did not land in the original directory:\n%s", data)
	}
}

// A relative directory that does not exist at Open time is still created,
// and the created directory is pinned the same way.
func TestStoreCreatesRelativeDirThenPinsIt(t *testing.T) {
	root := t.TempDir()
	w1 := filepath.Join(root, "w1")
	w2 := filepath.Join(root, "w2")
	if err := os.MkdirAll(w1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(w2, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Chdir(w1)
	s, err := Open("fresh-data")
	if err != nil {
		t.Fatalf("Open missing relative dir: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if fi, err := os.Stat(filepath.Join(w1, "fresh-data")); err != nil || !fi.IsDir() {
		t.Fatalf("Open did not create the relative directory: %v", err)
	}

	t.Chdir(w2)
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1, Description: "only here"}); err != nil {
		t.Fatalf("write after chdir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w2, "fresh-data")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a fresh-data appeared in the new cwd")
	}
	if got, err := s.GetCircuit("c", 1); err != nil || got.Description != "only here" {
		t.Fatalf("readback after chdir = %+v, %v", got, err)
	}
}

// Corruption of the originally opened file is still detected after a chdir:
// the next operation fails with ErrDataCorrupt, leaves the corrupt file in
// place, and never falls back to the intact file in the new working
// directory nor overwrites the damage with an empty state.
func TestStoreCorruptionOfOriginalNotBypassedByChdir(t *testing.T) {
	root := t.TempDir()
	w1 := filepath.Join(root, "w1")
	w2 := filepath.Join(root, "w2")
	seedBenchData(t, filepath.Join(w1, "bench-data"), "first description", validDef)
	seedBenchData(t, filepath.Join(w2, "bench-data"), "second description", altDef)

	t.Chdir(w1)
	s, err := Open("bench-data")
	if err != nil {
		t.Fatalf("Open relative: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	t.Chdir(w2)

	origFile := filepath.Join(w1, "bench-data", dirDataFile)
	corrupt := []byte(`{"format":1, broken`)
	if err := os.WriteFile(origFile, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCircuit("c", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt from the damaged original file, got %v", err)
	}
	left, err := os.ReadFile(origFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(corrupt) {
		t.Fatalf("damaged original file was overwritten:\n%s", left)
	}
	w2Data, err := os.ReadFile(filepath.Join(w2, "bench-data", dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(w2Data), "second description") {
		t.Fatal("the new cwd's intact data file was touched")
	}
}

// Constraint definition and input files keep being resolved against the
// working directory at call time, not against the pinned data directory's
// location: the store's pinning must not drag them along.
func TestStoreRelativeDefinitionAndInputFollowCallCwd(t *testing.T) {
	root := t.TempDir()
	w1 := filepath.Join(root, "w1")
	w2 := filepath.Join(root, "w2")
	seedBenchData(t, filepath.Join(w1, "bench-data"), "first description", validDef)
	if err := os.MkdirAll(w2, 0o755); err != nil {
		t.Fatal(err)
	}

	// In the first directory a same-named relative file is unusable junk.
	if err := os.WriteFile(filepath.Join(w1, "def.json"), []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	// In the second directory the same relative name holds a valid document.
	if err := os.WriteFile(filepath.Join(w2, "def.json"), []byte(validDef), 0o644); err != nil {
		t.Fatal(err)
	}
	// And a witness file resolvable only from the second directory.
	if err := os.WriteFile(filepath.Join(w2, "witness.json"),
		[]byte(`{"public":["2"],"private":["3"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(w1)
	s, err := Open("bench-data")
	if err != nil {
		t.Fatalf("Open relative: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.CreateCircuit(Circuit{Name: "d", Version: 1, Constraints: 1,
		PublicInputs: 1, PrivateInputs: 1, Description: "d"}); err != nil {
		t.Fatal(err)
	}

	// From the first cwd the relative definition must resolve to its junk
	// file (proving paths follow the call-time cwd in both directions).
	if _, err := s.ImportConstraints("d", 1, "def.json"); err == nil {
		t.Fatal("relative def.json in the first cwd should have been the junk file")
	}

	t.Chdir(w2)
	if _, err := s.ImportConstraints("d", 1, "def.json"); err != nil {
		t.Fatalf("relative def.json should resolve in the new cwd: %v", err)
	}
	if _, err := s.FreezeCircuit("d", 1); err != nil {
		t.Fatal(err)
	}
	a, err := s.CompileCircuit("d", 1)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.CheckInputFile("d", 1, a.Hash, "witness.json")
	if err != nil {
		t.Fatalf("relative witness.json should resolve in the new cwd: %v", err)
	}
	if !res.Satisfied {
		t.Fatalf("witness from the new cwd not evaluated: %+v", res)
	}
}
