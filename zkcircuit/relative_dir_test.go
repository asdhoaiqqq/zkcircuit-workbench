package zkcircuit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Regression: a Store opened on a relative path must keep operating on the
// directory that was opened, wherever the process working directory moves
// afterwards. The directory is pinned to an absolute path at Open time;
// reads, commits, artifact lookups and the corruption check never re-resolve
// against the new working directory.

const defMod7 = `{"modulus":"7","constraints":[
	{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`
const defMod11 = `{"modulus":"11","constraints":[
	{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"10"}]}]}`

// seedPinnedStore creates dir, opens it by absolute path and commits one
// frozen, compiled circuit "c" v1 (description desc, definition defJSON) plus
// one editable draft "draft" v1. It returns the compiled artifact's hash.
func seedPinnedStore(t *testing.T, dir, desc, defJSON string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	defPath := filepath.Join(t.TempDir(), "def.json")
	if err := os.WriteFile(defPath, []byte(defJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
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
	art, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "draft", Version: 1, Constraints: 1,
		Description: desc + " draft"}); err != nil {
		t.Fatal(err)
	}
	return art.Hash
}

func TestStorePinsDirectoryAcrossChdir(t *testing.T) {
	root := t.TempDir()
	wd1 := filepath.Join(root, "wd1")
	wd2 := filepath.Join(root, "wd2")
	wd3 := filepath.Join(root, "wd3") // never gets a bench-data
	for _, d := range []string{wd1, wd2, wd3} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dir1 := filepath.Join(wd1, "bench-data")
	dir2 := filepath.Join(wd2, "bench-data")

	// Both working directories hold a bench-data with the same circuit
	// name and version but a different description and definition, hence a
	// different compiled hash.
	hash1 := seedPinnedStore(t, dir1, "first", defMod7)
	hash2 := seedPinnedStore(t, dir2, "second", defMod11)
	if hash1 == hash2 {
		t.Fatal("seeded stores must compile to different hashes")
	}
	before2, err := readDataFile(t, dir2)
	if err != nil {
		t.Fatal(err)
	}

	// Open the first store by relative path from its working directory.
	t.Chdir(wd1)
	s, err := Open("bench-data")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Dir must locate the opened directory even after the working
	// directory moves: it is the absolute path of wd1's bench-data.
	if got := s.Dir(); got != dir1 {
		t.Fatalf("Dir() = %q, want %q", got, dir1)
	}

	// Move elsewhere: every operation must still hit the first directory.
	t.Chdir(wd2)

	c, err := s.GetCircuit("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if c.Description != "first" {
		t.Fatalf("description = %q, want %q (the opened directory's record)", c.Description, "first")
	}
	art, err := s.GetArtifact("c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if art.Hash != hash1 {
		t.Fatalf("artifact hash = %q, want the opened directory's %q", art.Hash, hash1)
	}

	// A draft edit commits to the first directory only.
	if _, err := s.UpdateCircuit(Circuit{Name: "draft", Version: 1, Constraints: 1,
		Description: "updated in first"}); err != nil {
		t.Fatal(err)
	}
	check, err := Open(dir1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := check.GetCircuit("draft", 1)
	if err != nil {
		t.Fatal(err)
	}
	check.Close()
	if got.Description != "updated in first" {
		t.Fatalf("first directory draft description = %q, want %q", got.Description, "updated in first")
	}
	if after, _ := readDataFile(t, dir2); string(after) != string(before2) {
		t.Fatal("second directory's data file changed; writes must stay in the opened directory")
	}

	// Constraint definition files still resolve against the working
	// directory at call time, not against the pinned data directory.
	if err := os.WriteFile(filepath.Join(wd2, "def.json"), []byte(defMod11), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "imp", Version: 1, Constraints: 1,
		PublicInputs: 1, PrivateInputs: 1}); err != nil {
		t.Fatal(err)
	}
	def, err := s.ImportConstraints("imp", 1, "def.json")
	if err != nil {
		t.Fatalf("relative definition file must resolve against the current working directory: %v", err)
	}
	if def.Modulus != "11" {
		t.Fatalf("imported modulus = %q, want %q from wd2's def.json", def.Modulus, "11")
	}

	// A working directory without bench-data changes nothing: reads and
	// commits keep working against the opened directory, and no second
	// data directory appears at the new location.
	t.Chdir(wd3)
	if c, err := s.GetCircuit("c", 1); err != nil || c.Description != "first" {
		t.Fatalf("read from wd3: got %+v, %v", c, err)
	}
	if _, err := s.UpdateCircuit(Circuit{Name: "draft", Version: 1, Constraints: 1,
		Description: "updated from wd3"}); err != nil {
		t.Fatalf("commit from wd3 must not fail: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wd3, "bench-data")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no bench-data may be created at the new working directory, stat err=%v", err)
	}

	// Corruption in the opened directory is still reported from there: the
	// next operation fails with ErrDataCorrupt instead of falling back to
	// the second directory's healthy data, and the damaged file is left in
	// place.
	t.Chdir(wd2)
	writeDataFile(t, dir1, []byte("garbage not json"))
	if _, err := s.ListCircuits(); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("corrupt opened directory: want ErrDataCorrupt, got %v", err)
	}
	if got, _ := readDataFile(t, dir1); string(got) != "garbage not json" {
		t.Fatal("damaged file in the opened directory was overwritten")
	}
	if after, _ := readDataFile(t, dir2); string(after) != string(before2) {
		t.Fatal("second directory's data file changed after the corruption check")
	}
}

// A relative path that does not exist yet at Open time is created, and the
// store then keeps using that created directory after the working directory
// moves.
func TestStorePinsNewlyCreatedDirectory(t *testing.T) {
	root := t.TempDir()
	wd1 := filepath.Join(root, "wd1")
	wd2 := filepath.Join(root, "wd2")
	for _, d := range []string{wd1, wd2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(wd1)
	s, err := Open("bench-data")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1,
		Description: "created fresh"}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(wd2)
	c, err := s.GetCircuit("c", 1)
	if err != nil || c.Description != "created fresh" {
		t.Fatalf("got %+v, %v", c, err)
	}
	if _, err := s.UpdateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1,
		Description: "still fresh"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wd2, "bench-data")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no bench-data may appear at the new working directory, stat err=%v", err)
	}
	if _, err := os.Stat(dataFilePath(filepath.Join(wd1, "bench-data"))); err != nil {
		t.Fatalf("data file must live in the originally created directory: %v", err)
	}
}
