package zkcircuit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Symbolic-link data-directory pinning. Once Open through a symlinked entry
// path succeeds, the store is bound to the actual directory it reached:
// retargeting or removing the entry link afterwards must never move reads,
// writes, the lock, artifact/job bindings or Dir onto the newly pointed-at
// data. A fresh Open through the changed entry gets its own binding.

// retarget replaces the symlink at entry with one pointing at target.
func retarget(t *testing.T, entry, target string) {
	t.Helper()
	if err := os.Remove(entry); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, entry); err != nil {
		t.Fatal(err)
	}
}

// The data directory itself is a symlink. After the entry is retargeted to a
// second, already populated directory, the open store keeps reading and
// committing to the original directory: same-name same-version circuits in
// the two directories never share constraint definitions or artifact hashes,
// the second directory's data.json stays byte-identical, and Dir keeps
// reporting the original actual directory.
func TestStorePinsDataDirectoryThroughRetargetedSymlink(t *testing.T) {
	root := t.TempDir()
	real1 := filepath.Join(root, "real1")
	real2 := filepath.Join(root, "real2")
	art1 := seedBenchData(t, real1, "first description", validDef)
	art2 := seedBenchData(t, real2, "second description", altDef)
	if art1.Hash == art2.Hash {
		t.Fatal("seed artifacts must differ so a swapped directory is observable")
	}
	entry := filepath.Join(root, "entry")
	if err := os.Symlink(real1, entry); err != nil {
		t.Fatal(err)
	}
	data2Before, err := os.ReadFile(filepath.Join(real2, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}

	s, err := Open(entry)
	if err != nil {
		t.Fatalf("Open through symlink: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if got := s.Dir(); got != real1 {
		t.Fatalf("Dir = %q, want the actual directory %q", got, real1)
	}

	// Repoint the entry link at the second directory.
	retarget(t, entry, real2)

	if got := s.Dir(); got != real1 {
		t.Fatalf("Dir after retarget = %q, want %q", got, real1)
	}
	c, err := s.GetCircuit("c", 1)
	if err != nil {
		t.Fatalf("GetCircuit: %v", err)
	}
	if c.Description != "first description" {
		t.Fatalf("read %q from the retargeted directory; want the original record", c.Description)
	}

	// Recompiling must return the original directory's artifact hash, not the
	// same-named version's artifact behind the new link target.
	a, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatalf("CompileCircuit: %v", err)
	}
	if a.Hash != art1.Hash {
		t.Fatalf("artifact hash %q belongs to the new target; want %q", a.Hash, art1.Hash)
	}
	if a.Hash == art2.Hash {
		t.Fatal("pinned store returned the retargeted directory's artifact")
	}

	// A job bound to that hash registers against the original directory.
	if _, err := s.SubmitJob(Job{ID: "j1", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: art1.Hash}); err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "draft", Version: 1, Constraints: 2, Description: "committed to first"}); err != nil {
		t.Fatal(err)
	}

	data1, err := os.ReadFile(filepath.Join(real1, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data1), "committed to first") || !strings.Contains(string(data1), `"j1"`) {
		t.Fatalf("original directory data.json missing pinned writes:\n%s", data1)
	}
	data2After, err := os.ReadFile(filepath.Join(real2, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(data2After) != string(data2Before) {
		t.Fatalf("new link target's data.json changed while operating the pinned store:\nbefore:\n%s\nafter:\n%s", data2Before, data2After)
	}

	// A new Open through the retargeted entry binds to the second directory;
	// the first store's binding is unaffected.
	s2, err := Open(entry)
	if err != nil {
		t.Fatalf("Open through retargeted symlink: %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	if got := s2.Dir(); got != real2 {
		t.Fatalf("second store Dir = %q, want %q", got, real2)
	}
	c2, err := s2.GetCircuit("c", 1)
	if err != nil {
		t.Fatalf("second store GetCircuit: %v", err)
	}
	if c2.Description != "second description" {
		t.Fatalf("second store read %q, want the new target's record", c2.Description)
	}
	if got := s.Dir(); got != real1 {
		t.Fatalf("first store Dir moved after second Open: %q", got)
	}
}

// Removing the entry symlink outright likewise leaves the open store on the
// original directory.
func TestStoreStaysOnOriginalWhenEntrySymlinkRemoved(t *testing.T) {
	root := t.TempDir()
	real1 := filepath.Join(root, "real1")
	seedBenchData(t, real1, "first description", validDef)
	entry := filepath.Join(root, "entry")
	if err := os.Symlink(real1, entry); err != nil {
		t.Fatal(err)
	}

	s, err := Open(entry)
	if err != nil {
		t.Fatalf("Open through symlink: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if err := os.Remove(entry); err != nil {
		t.Fatal(err)
	}
	if got := s.Dir(); got != real1 {
		t.Fatalf("Dir after link removal = %q, want %q", got, real1)
	}
	if _, err := s.GetCircuit("c", 1); err != nil {
		t.Fatalf("read after link removal: %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "late", Version: 1, Constraints: 1, Description: "still first"}); err != nil {
		t.Fatalf("write after link removal: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(real1, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "still first") {
		t.Fatalf("write did not land in the original directory:\n%s", data)
	}
}

// A symlink in a parent component pins the same way: the store binds to the
// actual directory beneath the link's target.
func TestStorePinsDataDirectoryThroughParentSymlink(t *testing.T) {
	root := t.TempDir()
	realParent1 := filepath.Join(root, "real-parent1")
	realParent2 := filepath.Join(root, "real-parent2")
	art1 := seedBenchData(t, filepath.Join(realParent1, "bench-data"), "first description", validDef)
	art2 := seedBenchData(t, filepath.Join(realParent2, "bench-data"), "second description", altDef)
	if art1.Hash == art2.Hash {
		t.Fatal("seed artifacts must differ so a swapped directory is observable")
	}
	linkParent := filepath.Join(root, "parent-link")
	if err := os.Symlink(realParent1, linkParent); err != nil {
		t.Fatal(err)
	}

	s, err := Open(filepath.Join(linkParent, "bench-data"))
	if err != nil {
		t.Fatalf("Open through parent symlink: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	wantDir := filepath.Join(realParent1, "bench-data")
	if got := s.Dir(); got != wantDir {
		t.Fatalf("Dir = %q, want %q", got, wantDir)
	}

	retarget(t, linkParent, realParent2)

	if got := s.Dir(); got != wantDir {
		t.Fatalf("Dir after parent retarget = %q, want %q", got, wantDir)
	}
	c, err := s.GetCircuit("c", 1)
	if err != nil {
		t.Fatalf("GetCircuit: %v", err)
	}
	if c.Description != "first description" {
		t.Fatalf("read %q after parent retarget; want the original record", c.Description)
	}
	a, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatalf("CompileCircuit: %v", err)
	}
	if a.Hash != art1.Hash {
		t.Fatalf("artifact hash %q belongs to the new parent target; want %q", a.Hash, art1.Hash)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "draft", Version: 1, Constraints: 2, Description: "committed to first"}); err != nil {
		t.Fatal(err)
	}
	data2, err := os.ReadFile(filepath.Join(realParent2, "bench-data", dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data2), "committed to first") {
		t.Fatal("write leaked into the retargeted parent's directory")
	}
}

// A ".." after a symlink is walked the way the filesystem walks it — through
// the link's target — not folded out of the path text first. With
// real/sub as the link target, link/../data names real/data, never the
// lexically collapsed root/data.
func TestStoreResolvesDotDotAfterSymlinkPhysically(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "real", "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	physical := seedBenchData(t, filepath.Join(root, "real", "data"), "physical description", validDef)
	lexical := seedBenchData(t, filepath.Join(root, "data"), "lexical description", altDef)
	if physical.Hash == lexical.Hash {
		t.Fatal("seed artifacts must differ so the two resolutions are distinguishable")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(sub, link); err != nil {
		t.Fatal(err)
	}

	// Build the path textually: filepath.Join would fold the ".." away
	// lexically before Open ever sees the symlink.
	s, err := Open(link + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "data")
	if err != nil {
		t.Fatalf("Open through link/..: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	wantDir := filepath.Join(root, "real", "data")
	if got := s.Dir(); got != wantDir {
		t.Fatalf("Dir = %q, want the physically reached %q", got, wantDir)
	}
	c, err := s.GetCircuit("c", 1)
	if err != nil {
		t.Fatalf("GetCircuit: %v", err)
	}
	if c.Description != "physical description" {
		t.Fatalf("read %q; the path was folded lexically onto the wrong directory", c.Description)
	}
	a, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatalf("CompileCircuit: %v", err)
	}
	if a.Hash != physical.Hash {
		t.Fatalf("artifact hash %q, want the physically reached directory's %q", a.Hash, physical.Hash)
	}
}

// A data directory that does not exist yet, named through a valid parent
// symlink, is created beneath the actual parent and pinned there.
func TestStoreCreatesMissingDirUnderResolvedParent(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real-parent")
	if err := os.MkdirAll(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	linkParent := filepath.Join(root, "parent-link")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Fatal(err)
	}

	s, err := Open(filepath.Join(linkParent, "fresh-data"))
	if err != nil {
		t.Fatalf("Open missing dir through parent symlink: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	wantDir := filepath.Join(realParent, "fresh-data")
	if got := s.Dir(); got != wantDir {
		t.Fatalf("Dir = %q, want %q", got, wantDir)
	}
	if fi, err := os.Stat(wantDir); err != nil || !fi.IsDir() {
		t.Fatalf("directory not created under the actual parent: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(linkParent, "fresh-data")); err != nil {
		t.Fatalf("created directory not visible through the entry link: %v", err)
	}

	// Retargeting the parent link afterwards cannot move the store.
	otherParent := filepath.Join(root, "other-parent")
	seedBenchData(t, filepath.Join(otherParent, "fresh-data"), "other description", validDef)
	retarget(t, linkParent, otherParent)

	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1, Description: "only here"}); err != nil {
		t.Fatalf("write after parent retarget: %v", err)
	}
	got, err := s.GetCircuit("c", 1)
	if err != nil || got.Description != "only here" {
		t.Fatalf("readback after parent retarget = %+v, %v", got, err)
	}
	data, err := os.ReadFile(filepath.Join(wantDir, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "only here") {
		t.Fatalf("write did not land beneath the original actual parent:\n%s", data)
	}
}

// Corruption of the originally opened file is still reported after the entry
// link is retargeted to a healthy directory: the next operation fails with
// ErrDataCorrupt, leaves the corrupt file in place, and never falls back to
// the intact data behind the new link target.
func TestStoreCorruptionOfOriginalNotBypassedBySymlinkRetarget(t *testing.T) {
	root := t.TempDir()
	real1 := filepath.Join(root, "real1")
	real2 := filepath.Join(root, "real2")
	seedBenchData(t, real1, "first description", validDef)
	seedBenchData(t, real2, "second description", altDef)
	entry := filepath.Join(root, "entry")
	if err := os.Symlink(real1, entry); err != nil {
		t.Fatal(err)
	}

	s, err := Open(entry)
	if err != nil {
		t.Fatalf("Open through symlink: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	retarget(t, entry, real2)

	origFile := filepath.Join(real1, dirDataFile)
	corrupt := []byte(`{"format":1, broken`)
	if err := os.WriteFile(origFile, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCircuit("c", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt from the damaged original file, got %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "d", Version: 1, Constraints: 1, Description: "d"}); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt on write against the damaged original, got %v", err)
	}
	left, err := os.ReadFile(origFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(corrupt) {
		t.Fatalf("damaged original file was overwritten:\n%s", left)
	}
	data2, err := os.ReadFile(filepath.Join(real2, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data2), "second description") {
		t.Fatal("the new link target's intact data file was touched")
	}
}

// Commits from another instance opened through the real directory path stay
// visible to the symlink-opened store: pinning freezes the location, not a
// snapshot of the data.
func TestStoreSeesCommitsThroughRealPathAfterPinning(t *testing.T) {
	root := t.TempDir()
	real1 := filepath.Join(root, "real1")
	seedBenchData(t, real1, "first description", validDef)
	entry := filepath.Join(root, "entry")
	if err := os.Symlink(real1, entry); err != nil {
		t.Fatal(err)
	}

	s, err := Open(entry)
	if err != nil {
		t.Fatalf("Open through symlink: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	s2, err := Open(real1)
	if err != nil {
		t.Fatalf("Open through real path: %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	if _, err := s2.CreateCircuit(Circuit{Name: "shared", Version: 1, Constraints: 1, Description: "via real path"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetCircuit("shared", 1)
	if err != nil || got.Description != "via real path" {
		t.Fatalf("commit through the real path not visible via the pinned store: %+v, %v", got, err)
	}
}
