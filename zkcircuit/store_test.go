package zkcircuit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func dataFilePath(dir string) string { return filepath.Join(dir, dirDataFile) }

func readDataFile(t *testing.T, dir string) ([]byte, error) {
	t.Helper()
	return os.ReadFile(dataFilePath(dir))
}

func writeDataFile(t *testing.T, dir string, content []byte) {
	t.Helper()
	if err := os.WriteFile(dataFilePath(dir), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCorruptDataRefused: corrupt, truncated or unsupported-format files
// must be reported as read failures, left byte-for-byte untouched, and never
// treated as an empty directory or overwritten by a subsequent open.
func TestCorruptDataRefused(t *testing.T) {
	// Seed a valid store first so "empty file" is a truncation case, not a
	// never-initialized directory.
	seed := t.TempDir()
	s, err := Open(seed)
	if err != nil {
		t.Fatal(err)
	}
	readyCircuit(t, s, "c", 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	valid, err := readDataFile(t, seed)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		content []byte
	}{
		{"garbage", []byte("this is not json at all")},
		{"empty", []byte{}},
		{"whitespace only", []byte("   \n\t ")},
		{"truncated json", valid[:len(valid)/3]},
		{"cut mid object", append(append([]byte(nil), valid[:len(valid)-80]...), []byte("\n  }")...)},
		{"unsupported format", []byte(`{"format":999,"circuits":[],"setups":[],"jobs":[]}`)},
		{"missing format", []byte(`{"circuits":[],"setups":[],"jobs":[]}`)},
		{"duplicate circuit", []byte(`{"format":1,"circuits":[` +
			`{"name":"c","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":true,"description":"d"},` +
			`{"name":"c","version":1,"constraints":2,"public_inputs":0,"private_inputs":0,"frozen":true,"description":"d"}` +
			`],"setups":[],"jobs":[]}`)},
		{"setup on unknown circuit", []byte(`{"format":1,"circuits":[],"setups":[{"name":"c","version":1}],"jobs":[]}`)},
		{"setup not frozen", []byte(`{"format":1,"circuits":[` +
			`{"name":"c","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":false,"description":"d"}` +
			`],"setups":[{"name":"c","version":1}],"jobs":[]}`)},
		{"job without setup", []byte(`{"format":1,"circuits":[` +
			`{"name":"c","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":true,"description":"d"}` +
			`],"setups":[],"jobs":[{"id":"j","circuit":"c","version":1,"kind":"prove","attempt":1}]}`)},
		{"duplicate job", []byte(`{"format":1,"circuits":[` +
			`{"name":"c","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":true,"description":"d"}` +
			`],"setups":[{"name":"c","version":1}],"jobs":[` +
			`{"id":"j","circuit":"c","version":1,"kind":"prove","attempt":1},` +
			`{"id":"j","circuit":"c","version":1,"kind":"prove","attempt":1}]}`)},
		{"bad counts", []byte(`{"format":1,"circuits":[` +
			`{"name":"c","version":1,"constraints":0,"public_inputs":0,"private_inputs":0,"frozen":false,"description":"d"}` +
			`],"setups":[],"jobs":[]}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeDataFile(t, dir, tc.content)

			s, err := Open(dir)
			if err == nil {
				s.Close()
				t.Fatalf("expected read failure, open succeeded")
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			got, rerr := readDataFile(t, dir)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(got) != string(tc.content) {
				t.Fatalf("open modified the damaged file\nwant: %q\n got: %q", tc.content, got)
			}
			// A second open still refuses; operations never clobber the file.
			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("reopen on damaged dir: want corruption, got %v", err)
			}
			if got, _ := readDataFile(t, dir); string(got) != string(tc.content) {
				t.Fatal("damaged file was overwritten after refusal")
			}
		})
	}
}

func TestEmptyDirectoryOpensClean(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("fresh empty directory should open cleanly: %v", err)
	}
	if list, err := s.ListCircuits(); err != nil || len(list) != 0 {
		t.Fatalf("fresh store not empty: %+v %v", list, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// No data file is written while nothing has been committed.
	if _, err := os.Stat(dataFilePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no data file before first commit, got err=%v", err)
	}
}

// TestStaleTempFileIgnored: a data.json.tmp left behind by a process killed
// mid-commit is never read as state; the committed data.json remains the
// source of truth, and the next successful commit replaces the stale temp.
func TestStaleTempFileIgnored(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	readyCircuit(t, s, "c", 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dirTmpFile), []byte("{garbage partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("stale temp must not block open: %v", err)
	}
	c, err := s2.GetCircuit("c", 1)
	if err != nil || !c.Frozen {
		t.Fatalf("committed state wrong with stale temp present: %+v %v", c, err)
	}
	if _, err := s2.CreateCircuit(Circuit{Name: "c", Version: 2, Constraints: 2, Description: "d"}); err != nil {
		t.Fatal(err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	// After a commit the temp name must not exist as a junk file (it was
	// renamed away into the new data.json).
	if raw, err := os.ReadFile(filepath.Join(dir, dirTmpFile)); err == nil {
		t.Fatalf("stale temp survived commit: %q", raw)
	}
	s3, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s3.Close()
	list, err := s3.ListCircuits()
	if err != nil || len(list) != 2 {
		t.Fatalf("commit over stale temp incomplete: %+v %v", list, err)
	}
}

// TestCommitsAreCompleteAfterReopen checks the file on disk is always a
// complete envelope that independently passes validation.
func TestCommitsAreCompleteAfterReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		c := Circuit{Name: "c", Version: i, Constraints: i, Description: "d"}
		if _, err := s.CreateCircuit(c); err != nil {
			t.Fatal(err)
		}
		// Every intermediate committed file must be independently loadable.
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("intermediate reopen at %d: %v", i, err)
		}
		list, err := s2.ListCircuits()
		if err != nil || len(list) != i {
			t.Fatalf("intermediate state incomplete: %d records err=%v", len(list), err)
		}
		s2.Close()
	}
	s.Close()
}
