package zkcircuit

import (
	"errors"
	"strings"
	"testing"
)

// Regression tests for the compiled-artifact binding as expressed in a
// committed job record. compiled_hash may be omitted (legacy register-only
// records) or be an explicit string ("" = unbound, anything else = the exact
// artifact hash). Any other value — null above all — is damage to a binding
// the record claims to carry, never "the user did not provide a hash": the
// directory read is refused as data corruption, the error names the job, and
// the file is left byte-for-byte in place.

// seedBoundJobStore commits a frozen, compiled circuit with a trusted setup
// and two jobs — "j-bound" carrying the version's artifact hash and "j-plain"
// left unbound — and returns the data directory and the artifact hash.
func seedBoundJobStore(t *testing.T) (dir, hash string) {
	t.Helper()
	dir = t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	hash = compiledVersion(t, s, "c", 1, validDef)
	if _, err := s.SubmitJob(boundProveJob("j-bound", "c", 1, hash)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitJob(boundProveJob("j-plain", "c", 1, "")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, hash
}

// rewriteBoundHash replaces the committed j-bound binding field with the
// given raw JSON text, returning the damaged file content.
func rewriteBoundHash(t *testing.T, dir, hash, replacement string) string {
	t.Helper()
	valid, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	field := `"compiled_hash": "` + hash + `"`
	if !strings.Contains(string(valid), field) {
		t.Fatalf("committed file does not carry the bound field %q", field)
	}
	bad := strings.Replace(string(valid), field, `"compiled_hash": `+replacement, 1)
	writeDataFile(t, dir, []byte(bad))
	return bad
}

// A compiled_hash that is present but not a JSON string is data corruption:
// null, numbers, booleans, arrays and objects are all refused, the error
// names the job whose binding field is damaged, and the file is untouched.
func TestJobCompiledHashNonStringIsCorrupt(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"null", "null"},
		{"number", "5"},
		{"boolean", "true"},
		{"array", `["deadbeef"]`},
		{"object", `{"hash":"deadbeef"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, h := seedBoundJobStore(t)
			bad := rewriteBoundHash(t, dir, h, tc.value)

			_, err := Open(dir)
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), `"j-bound"`) {
				t.Fatalf("error does not name the damaged job: %v", err)
			}
			if !strings.Contains(err.Error(), "compiled_hash") {
				t.Fatalf("error does not name the binding field: %v", err)
			}
			got, rerr := readDataFile(t, dir)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(got) != bad {
				t.Fatalf("open modified the damaged file")
			}
			// A second open still refuses; nothing recovers the field by
			// guessing from the current artifact.
			if _, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("reopen on damaged dir: want corruption, got %v", err)
			}
		})
	}
}

// A repeated compiled_hash key is refused — compared after JSON unescaping
// and regardless of whether both values agree — instead of silently
// resolving the binding to the last value.
func TestJobCompiledHashDuplicateIsCorrupt(t *testing.T) {
	_, h := seedBoundJobStore(t)
	cases := []struct {
		name        string
		replacement string
	}{
		{"identical values", `"compiled_hash": "` + h + `", "compiled_hash": "` + h + `"`},
		{"differing values", `"compiled_hash": "` + h + `", "compiled_hash": "deadbeef"`},
		{"null then value", `"compiled_hash": null, "compiled_hash": "` + h + `"`},
		{"escaped key spelling", `"compiled_hash": "` + h + `", "compiled\u005fhash": "` + h + `"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, h := seedBoundJobStore(t)
			field := `"compiled_hash": "` + h + `"`
			valid, err := readDataFile(t, dir)
			if err != nil {
				t.Fatal(err)
			}
			bad := strings.Replace(string(valid), field, tc.replacement, 1)
			writeDataFile(t, dir, []byte(bad))

			_, err = Open(dir)
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), "compiled_hash") {
				t.Fatalf("error does not name the duplicated field: %v", err)
			}
			got, rerr := readDataFile(t, dir)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(got) != bad {
				t.Fatalf("open modified the damaged file")
			}
		})
	}
}

// An explicit empty string remains the legal unbound state: the directory
// opens and the job reads back unbound.
func TestJobCompiledHashExplicitEmptyStaysUnbound(t *testing.T) {
	dir, h := seedBoundJobStore(t)
	rewriteBoundHash(t, dir, h, `""`)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("explicit empty binding must load: %v", err)
	}
	defer s.Close()
	got, err := s.GetJob("j-bound")
	if err != nil {
		t.Fatal(err)
	}
	if got.CompiledHash != "" {
		t.Fatalf("explicit empty hash read as bound: %+v", got)
	}
}

// Once a directory is open, damaging the binding on disk is caught by the
// very next operation: reads return no partial job list, new jobs are not
// committed and unrelated circuit changes cannot rewrite the directory.
func TestOpenStoreRefusesAfterJobBindingCorruption(t *testing.T) {
	dir, h := seedBoundJobStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bad := rewriteBoundHash(t, dir, h, "null")

	if _, err := s.GetJob("j-bound"); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetJob after corruption: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.ListJobs(); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("ListJobs after corruption: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.SubmitJob(boundProveJob("j-new", "c", 1, h)); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("SubmitJob after corruption: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "other", Version: 1, Constraints: 1}); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("unrelated CreateCircuit after corruption: want ErrDataCorrupt, got %v", err)
	}
	got, rerr := readDataFile(t, dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != bad {
		t.Fatalf("damaged file was rewritten by a refused operation")
	}
}

// Legal records keep their exact shape across further commits: the bound job
// keeps its hash verbatim and the unbound job's omitted field is not
// backfilled.
func TestJobBindingFieldShapeSurvivesCommits(t *testing.T) {
	dir, h := seedBoundJobStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitJob(boundProveJob("j-third", "c", 1, "")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := readDataFile(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly one compiled_hash field exists: j-bound's. The two unbound jobs
	// stay omitted rather than being rewritten with an explicit value.
	if n := strings.Count(string(raw), `"compiled_hash"`); n != 1 {
		t.Fatalf("committed file carries %d compiled_hash fields, want 1:\n%s", n, raw)
	}
	if !strings.Contains(string(raw), `"compiled_hash": "`+h+`"`) {
		t.Fatalf("bound hash not preserved verbatim:\n%s", raw)
	}
}
