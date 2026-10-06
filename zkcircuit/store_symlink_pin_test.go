//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package zkcircuit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Physical-directory pinning across symlink changes. Once Open succeeds the
// store is bound to the actual directory the entry path named at that moment;
// repointing or removing an entry symlink afterwards must never move reads,
// writes, the lock, artifact bindings or Dir onto another directory, while a
// fresh Open through the changed entry binds to the data then named.

// symlinkReplace atomically (as far as the tests need) repoints link at
// target: the link itself is replaced, never its target directory.
func symlinkReplace(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Remove(link); err != nil {
		t.Fatalf("remove symlink %q: %v", link, err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %q -> %q: %v", link, target, err)
	}
}

// The data directory itself is a symlink. After the link is repointed at a
// second populated directory, the old store keeps reading and writing the
// first one: its record, its compiled-artifact hash (same circuit name and
// version, different modulus definition) and its lock. The second
// directory's data.json must stay byte-for-byte unchanged, while a new
// Open through the repointed entry binds to the second directory.
func TestStorePinnedWhenDataDirSymlinkRetargeted(t *testing.T) {
	root := t.TempDir()
	d1 := filepath.Join(root, "real1")
	d2 := filepath.Join(root, "real2")
	art1 := seedBenchData(t, d1, "first description", validDef)
	art2 := seedBenchData(t, d2, "second description", altDef)
	if art1.Hash == art2.Hash {
		t.Fatal("seed artifacts must differ so a swapped directory is observable")
	}
	d2Before, err := os.ReadFile(filepath.Join(d2, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}

	entry := filepath.Join(root, "entry")
	if err := os.Symlink(d1, entry); err != nil {
		t.Fatal(err)
	}
	s, err := Open(entry)
	if err != nil {
		t.Fatalf("Open through symlink: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if got := s.Dir(); got != d1 {
		t.Fatalf("Dir = %q, want the physical target %q", got, d1)
	}

	symlinkReplace(t, entry, d2)

	if got := s.Dir(); got != d1 {
		t.Fatalf("Dir after repoint = %q, want %q", got, d1)
	}
	c, err := s.GetCircuit("c", 1)
	if err != nil {
		t.Fatalf("GetCircuit after repoint: %v", err)
	}
	if c.Description != "first description" {
		t.Fatalf("read %q after the link moved; want the first directory", c.Description)
	}
	a, err := s.CompileCircuit("c", 1)
	if err != nil {
		t.Fatalf("CompileCircuit after repoint: %v", err)
	}
	if a.Hash != art1.Hash {
		t.Fatalf("artifact hash %q belongs to the repointed directory; want %q", a.Hash, art1.Hash)
	}
	if a.Hash == art2.Hash {
		t.Fatal("pinned store crossed into the second directory's definition/hash")
	}
	if _, err := s.SubmitJob(Job{ID: "j1", Circuit: "c", Version: 1, Kind: "prove", Attempt: 1, CompiledHash: art1.Hash}); err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "late", Version: 1, Constraints: 1, Description: "only in first"}); err != nil {
		t.Fatalf("CreateCircuit after repoint: %v", err)
	}

	d1Data, err := os.ReadFile(filepath.Join(d1, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(d1Data), "only in first") || !strings.Contains(string(d1Data), art1.Hash) {
		t.Fatalf("writes did not land in the original directory:\n%s", d1Data)
	}
	d2After, err := os.ReadFile(filepath.Join(d2, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(d2After) != string(d2Before) {
		t.Fatalf("the repointed-to directory's data.json changed:\nbefore:\n%s\nafter:\n%s", d2Before, d2After)
	}

	// A fresh Open through the changed entry binds to the second directory;
	// the old store's binding is unaffected.
	s2, err := Open(entry)
	if err != nil {
		t.Fatalf("Open through repointed symlink: %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	if got := s2.Dir(); got != d2 {
		t.Fatalf("new store Dir = %q, want %q", got, d2)
	}
	c2, err := s2.GetCircuit("c", 1)
	if err != nil {
		t.Fatalf("new store GetCircuit: %v", err)
	}
	if c2.Description != "second description" {
		t.Fatalf("new store read %q, want the second directory", c2.Description)
	}
	a2, err := s2.CompileCircuit("c", 1)
	if err != nil {
		t.Fatalf("new store CompileCircuit: %v", err)
	}
	if a2.Hash != art2.Hash {
		t.Fatalf("new store hash %q, want %q", a2.Hash, art2.Hash)
	}
	if c, err := s.GetCircuit("c", 1); err != nil || c.Description != "first description" {
		t.Fatalf("old store changed when a second store opened: %+v, %v", c, err)
	}
}

// Removing the entry symlink altogether leaves the store bound to the
// original directory: reads and writes keep working there.
func TestStorePinnedWhenDataDirSymlinkRemoved(t *testing.T) {
	root := t.TempDir()
	d1 := filepath.Join(root, "real1")
	seedBenchData(t, d1, "first description", validDef)
	entry := filepath.Join(root, "entry")
	if err := os.Symlink(d1, entry); err != nil {
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
	if _, err := s.GetCircuit("c", 1); err != nil {
		t.Fatalf("reading after the entry link vanished: %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "late", Version: 1, Constraints: 1, Description: "still in first"}); err != nil {
		t.Fatalf("writing after the entry link vanished: %v", err)
	}
	if got := s.Dir(); got != d1 {
		t.Fatalf("Dir = %q, want %q", got, d1)
	}
	data, err := os.ReadFile(filepath.Join(d1, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "still in first") {
		t.Fatalf("write did not land in the original directory:\n%s", data)
	}
}

// A symlink in a parent directory is pinned exactly like the data directory
// itself: repointing the parent moves neither reads nor writes.
func TestStorePinnedWhenParentSymlinkRetargeted(t *testing.T) {
	root := t.TempDir()
	wp1 := filepath.Join(root, "wp1")
	wp2 := filepath.Join(root, "wp2")
	seedBenchData(t, filepath.Join(wp1, "bench-data"), "first description", validDef)
	art2 := seedBenchData(t, filepath.Join(wp2, "bench-data"), "second description", altDef)
	d2Before, err := os.ReadFile(filepath.Join(wp2, "bench-data", dirDataFile))
	if err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(root, "w")
	if err := os.Symlink(wp1, link); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(link, "bench-data"))
	if err != nil {
		t.Fatalf("Open through parent symlink: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if got, want := s.Dir(), filepath.Join(wp1, "bench-data"); got != want {
		t.Fatalf("Dir = %q, want %q", got, want)
	}

	symlinkReplace(t, link, wp2)

	c, err := s.GetCircuit("c", 1)
	if err != nil {
		t.Fatalf("GetCircuit after parent repoint: %v", err)
	}
	if c.Description != "first description" {
		t.Fatalf("read %q from the repointed parent; want the first", c.Description)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "late", Version: 1, Constraints: 1, Description: "only in first"}); err != nil {
		t.Fatalf("write after parent repoint: %v", err)
	}
	d2After, err := os.ReadFile(filepath.Join(wp2, "bench-data", dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(d2After) != string(d2Before) {
		t.Fatalf("second directory changed while operating through the old parent:\nbefore:\n%s\nafter:\n%s", d2Before, d2After)
	}

	s2, err := Open(filepath.Join(link, "bench-data"))
	if err != nil {
		t.Fatalf("reopen through repointed parent: %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	if got, want := s2.Dir(), filepath.Join(wp2, "bench-data"); got != want {
		t.Fatalf("new store Dir = %q, want %q", got, want)
	}
	a2, err := s2.CompileCircuit("c", 1)
	if err != nil {
		t.Fatalf("new store compile: %v", err)
	}
	if a2.Hash != art2.Hash {
		t.Fatalf("new store hash %q, want the second directory's %q", a2.Hash, art2.Hash)
	}
}

// A ".." appearing after a symlink is resolved where the file system walk
// actually reaches it, not by folding the literal spelling. With
// entry -> a/real, "entry/../x/data" reaches a/x/data via the link target;
// the lexically folded root/x/data holds a decoy that must never be opened.
func TestStoreDotDotAfterSymlinkResolvesKernelWise(t *testing.T) {
	root := t.TempDir()
	aReal := filepath.Join(root, "a", "real")
	kernelData := filepath.Join(root, "a", "x", "data")
	lexicalData := filepath.Join(root, "x", "data")
	if err := os.MkdirAll(aReal, 0o755); err != nil {
		t.Fatal(err)
	}
	seedBenchData(t, kernelData, "KERNEL directory", validDef)
	seedBenchData(t, lexicalData, "LEXICAL directory", altDef)
	lexicalBefore, err := os.ReadFile(filepath.Join(lexicalData, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}

	entry := filepath.Join(root, "entry")
	if err := os.Symlink(aReal, entry); err != nil {
		t.Fatal(err)
	}
	// Build the raw spelling (no lexical Clean): filepath.Join would fold
	// "entry/.." before Open ever saw the symlink.
	arg := filepath.Join(root, "entry") + string(os.PathSeparator) + ".." + string(os.PathSeparator) + filepath.Join("x", "data")
	s, err := Open(arg)
	if err != nil {
		t.Fatalf("Open %q: %v", arg, err)
	}
	t.Cleanup(func() { s.Close() })
	if got, want := s.Dir(), kernelData; got != want {
		t.Fatalf("Dir = %q, want kernel-resolved %q", got, want)
	}
	c, err := s.GetCircuit("c", 1)
	if err != nil {
		t.Fatalf("GetCircuit: %v", err)
	}
	if c.Description != "KERNEL directory" {
		t.Fatalf("read %q; the lexical fold opened the wrong data directory", c.Description)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "late", Version: 1, Constraints: 1, Description: "kernel write"}); err != nil {
		t.Fatal(err)
	}
	kd, err := os.ReadFile(filepath.Join(kernelData, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(kd), "kernel write") {
		t.Fatalf("write did not land at the kernel-resolved directory:\n%s", kd)
	}
	ld, err := os.ReadFile(filepath.Join(lexicalData, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(ld) != string(lexicalBefore) {
		t.Fatal("the lexically folded decoy directory was touched")
	}
}

// A not-yet-existing data subdirectory named through ".." after a parent
// symlink is created under the physical parent the walk reaches (a/new/data),
// never under the lexically folded location (root/new/data).
func TestStoreDotDotAfterSymlinkCreatesAtKernelParent(t *testing.T) {
	root := t.TempDir()
	aReal := filepath.Join(root, "a", "real")
	if err := os.MkdirAll(aReal, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(root, "entry")
	if err := os.Symlink(aReal, entry); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "a", "new", "data")
	arg := filepath.Join(root, "entry") + string(os.PathSeparator) + ".." + string(os.PathSeparator) + filepath.Join("new", "data")
	s, err := Open(arg)
	if err != nil {
		t.Fatalf("Open %q: %v", arg, err)
	}
	t.Cleanup(func() { s.Close() })
	if got := s.Dir(); got != want {
		t.Fatalf("Dir = %q, want %q", got, want)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Fatalf("data directory not created at the physical parent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a directory appeared at the lexically folded location: %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1, Description: "created via link"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetCircuit("c", 1); err != nil || got.Description != "created via link" {
		t.Fatalf("readback = %+v, %v", got, err)
	}
}

// A dangling entry symlink names the data directory to be created: Open
// creates and opens the link target's physical directory and pins it.
func TestStoreCreatesThroughDanglingSymlinkAndPinsTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	entry := filepath.Join(root, "entry")
	if err := os.Symlink(target, entry); err != nil {
		t.Fatal(err)
	}
	s, err := Open(entry)
	if err != nil {
		t.Fatalf("Open dangling symlink: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if got := s.Dir(); got != target {
		t.Fatalf("Dir = %q, want created target %q", got, target)
	}
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		t.Fatalf("target directory not created: %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 1, Description: "at target"}); err != nil {
		t.Fatal(err)
	}

	// Repointing the link after creation cannot move the store.
	other := filepath.Join(root, "other")
	symlinkReplace(t, entry, other)
	if _, err := s.CreateCircuit(Circuit{Name: "d", Version: 1, Constraints: 1, Description: "still target"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(target, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "still target") {
		t.Fatalf("write followed the repointed dangling link:\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(other, dirDataFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the repointed-to directory was created or written")
	}
}

// After the entry link moves, corruption of the ORIGINAL data.json keeps
// being reported as data corruption by the pinned store: queries and writes
// fail with ErrDataCorrupt, the damaged file is preserved byte-for-byte, and
// the healthy directory the link now points at is neither read nor written.
func TestStoreCorruptOriginalStillRejectedAfterSymlinkRetarget(t *testing.T) {
	root := t.TempDir()
	d1 := filepath.Join(root, "real1")
	d2 := filepath.Join(root, "real2")
	seedBenchData(t, d1, "first description", validDef)
	seedBenchData(t, d2, "second description", altDef)
	d2Before, err := os.ReadFile(filepath.Join(d2, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}

	entry := filepath.Join(root, "entry")
	if err := os.Symlink(d1, entry); err != nil {
		t.Fatal(err)
	}
	s, err := Open(entry)
	if err != nil {
		t.Fatalf("Open through symlink: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	symlinkReplace(t, entry, d2)

	origFile := filepath.Join(d1, dirDataFile)
	corrupt := []byte(`{"format":1, broken`)
	if err := os.WriteFile(origFile, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCircuit("c", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt from the damaged original, got %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "x", Version: 1, Constraints: 1, Description: "x"}); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("write against damaged original: want ErrDataCorrupt, got %v", err)
	}
	left, err := os.ReadFile(origFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(corrupt) {
		t.Fatalf("damaged original file was overwritten:\n%s", left)
	}
	d2After, err := os.ReadFile(filepath.Join(d2, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(d2After) != string(d2Before) {
		t.Fatal("the healthy repointed-to data.json was touched")
	}
}

// Normal commits made by another instance opened through the original
// directory's real path stay visible to the link-pinned store, even though
// the entry link has since moved elsewhere.
func TestStorePinnedInstanceSeesCommitsViaRealPath(t *testing.T) {
	root := t.TempDir()
	d1 := filepath.Join(root, "real1")
	d2 := filepath.Join(root, "real2")
	seedBenchData(t, d1, "first description", validDef)
	seedBenchData(t, d2, "second description", altDef)
	entry := filepath.Join(root, "entry")
	if err := os.Symlink(d1, entry); err != nil {
		t.Fatal(err)
	}
	s, err := Open(entry)
	if err != nil {
		t.Fatalf("Open through symlink: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	symlinkReplace(t, entry, d2)

	other, err := Open(d1)
	if err != nil {
		t.Fatalf("Open real path: %v", err)
	}
	if _, err := other.CreateCircuit(Circuit{Name: "external", Version: 1, Constraints: 1, Description: "committed via real path"}); err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetCircuit("external", 1)
	if err != nil {
		t.Fatalf("pinned store did not see the real-path commit: %v", err)
	}
	if got.Description != "committed via real path" {
		t.Fatalf("read %q, want the externally committed record", got.Description)
	}
	// The pinned store's own commit lands beside the external one, in d1.
	if _, err := s.CreateCircuit(Circuit{Name: "reply", Version: 1, Constraints: 1, Description: "pinned reply"}); err != nil {
		t.Fatalf("pinned write: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(d1, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "committed via real path") || !strings.Contains(string(data), "pinned reply") {
		t.Fatalf("both commits are not present in the original directory:\n%s", data)
	}
}

// A relative entry is anchored to the working directory at Open time and
// then pinned to the physical target; a later repoint of the relative link
// does not move the store.
func TestStoreRelativeSymlinkPinnedFromOpenCwd(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	d1 := filepath.Join(root, "real1")
	d2 := filepath.Join(root, "real2")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	seedBenchData(t, d1, "first description", validDef)
	seedBenchData(t, d2, "second description", altDef)
	if err := os.Symlink(d1, filepath.Join(work, "bench")); err != nil {
		t.Fatal(err)
	}

	t.Chdir(work)
	s, err := Open("bench")
	if err != nil {
		t.Fatalf("Open relative symlink: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if got := s.Dir(); got != d1 {
		t.Fatalf("Dir = %q, want %q", got, d1)
	}
	symlinkReplace(t, filepath.Join(work, "bench"), d2)
	c, err := s.GetCircuit("c", 1)
	if err != nil {
		t.Fatalf("GetCircuit after repoint: %v", err)
	}
	if c.Description != "first description" {
		t.Fatalf("read %q from the repointed relative link; want first", c.Description)
	}
}
