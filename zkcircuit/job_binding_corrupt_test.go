package zkcircuit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression tests for the on-disk expression of a job's compiled-artifact
// binding. A stored compiled_hash member is optional (legacy register-only
// records) and an explicit "" keeps meaning "unbound", but once the member is
// present it must be a JSON string: null must not silently read as the empty
// string — that would display a submitted binding as an unbound job until the
// next normal write silently dropped the anomaly. A null/number/boolean/array
// /object value, or a repeated member (even with the same value, even spelled
// through a JSON escape), is data corruption: the directory fails closed,
// names the offending job, leaves data.json byte-for-byte in place and cannot
// be laundered by an unrelated write.

// seedJobsStore writes a closed directory with:
//   - a frozen, setup-registered and compiled circuit "c" v1 and a bound job
//     "j-bound" carrying its artifact hash;
//   - a frozen, setup-registered but uncompiled circuit "d" v1 and an
//     unbound register-only job "j-loose".
//
// It returns the directory and "c" v1's artifact hash.
func seedJobsStore(t *testing.T) (dir string, hash string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	hash = compiledVersion(t, s, "c", 1, validDef)
	frozenSetupUncompiled(t, s, "d", 1)
	if _, err := s.SubmitJob(boundProveJob("j-bound", "c", 1, hash)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitJob(boundProveJob("j-loose", "d", 1, "")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, hash
}

func firstJob(env map[string]any, id string) map[string]any {
	for _, item := range env["jobs"].([]any) {
		job := item.(map[string]any)
		if job["id"] == id {
			return job
		}
	}
	return nil
}

// damageJob rewrites one job record through the generic JSON map and returns
// the bytes now on disk.
func damageJob(t *testing.T, dir, id string, mutate func(map[string]any)) []byte {
	t.Helper()
	env := readGenericEnv(t, dir)
	job := firstJob(env, id)
	if job == nil {
		t.Fatalf("job %q not found in seeded envelope", id)
	}
	mutate(job)
	return writeGenericEnv(t, dir, env)
}

// requireJobCorrupt opens dir and asserts a data-corrupt failure that names
// both the damaged field and the job id, with data.json left exactly as bad.
func requireJobCorrupt(t *testing.T, dir string, bad []byte, id, field string) {
	t.Helper()
	s, err := Open(dir)
	if err == nil {
		s.Close()
		t.Fatalf("damaged job record was accepted")
	}
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	if !strings.Contains(err.Error(), id) {
		t.Fatalf("error does not name the damaged job %q: %v", id, err)
	}
	if field != "" && !strings.Contains(err.Error(), field) {
		t.Fatalf("error does not name the damaged field %q: %v", field, err)
	}
	left, rerr := os.ReadFile(filepath.Join(dir, dirDataFile))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(left) != string(bad) {
		t.Fatalf("refused read modified data.json\nwant: %q\n got: %q", bad, left)
	}
	// A second open still refuses.
	if s2, err := Open(dir); !errors.Is(err, ErrDataCorrupt) {
		if s2 != nil {
			s2.Close()
		}
		t.Fatalf("reopen accepted the damage: %v", err)
	}
}

// TestStoredCompiledHashWrongTypeRefused covers every non-string JSON type the
// headline bug used to swallow: null on a bound job reads as unbound, and the
// other types each fail to decode into the string field. A null on the
// unbound job against the uncompiled version is corruption too — it must not
// be explained away as "the job never carried a hash".
func TestStoredCompiledHashWrongTypeRefused(t *testing.T) {
	cases := []struct {
		name  string
		id    string
		value any
	}{
		{"null on bound job", "j-bound", nil},
		{"number on bound job", "j-bound", float64(3)},
		{"boolean on bound job", "j-bound", true},
		{"array on bound job", "j-bound", []any{"x"}},
		{"object on bound job", "j-bound", map[string]any{"x": "y"}},
		// The target version has no artifact at all: a null here is still a
		// malformed saved binding, never a legitimate register-only record
		// (those simply omit the member).
		{"null on unbound uncompiled job", "j-loose", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := seedJobsStore(t)
			bad := damageJob(t, dir, tc.id, func(job map[string]any) {
				job["compiled_hash"] = tc.value
			})
			requireJobCorrupt(t, dir, bad, tc.id, "compiled_hash")
		})
	}
}

// compactJobRecord extracts the job record with the given id from a raw
// envelope and returns it compacted plus an embed function that re-inserts a
// replacement one-for-one.
func compactJobRecord(t *testing.T, raw []byte, id string) (record []byte, embed func([]byte) []byte) {
	t.Helper()
	var wire struct {
		Jobs []json.RawMessage `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	var orig []byte
	for _, j := range wire.Jobs {
		var probe struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(j, &probe); err != nil {
			t.Fatal(err)
		}
		if probe.ID == id {
			orig = []byte(j)
			break
		}
	}
	if orig == nil {
		t.Fatalf("job %q not found", id)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, orig); err != nil {
		t.Fatal(err)
	}
	return compact.Bytes(), func(rep []byte) []byte {
		return bytes.Replace(raw, orig, rep, 1)
	}
}

// TestStoredCompiledHashDuplicateRefused: the member appearing twice is
// corruption regardless of whether the values differ or are identical, and
// regardless of a JSON escape spelling the same key name. The last value must
// never decide the binding.
func TestStoredCompiledHashDuplicateRefused(t *testing.T) {
	dir, hash := seedJobsStore(t)
	raw, err := os.ReadFile(filepath.Join(dir, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	rec, embed := compactJobRecord(t, raw, "j-bound")

	// escapedKey spells the key's first byte as \u00XX: JSON decoding makes it
	// the identical member name, so the occurrence is still a duplicate.
	escapedKey := func(key string) string {
		return fmt.Sprintf("\"\\u%04x%s\"", key[0], key[1:])
	}
	cases := []struct {
		name   string
		member string
	}{
		{"different value", fmt.Sprintf("%q:%q", "compiled_hash", "a-totally-different-hash")},
		{"same value", fmt.Sprintf("%q:%q", "compiled_hash", hash)},
		{"escaped key same value", escapedKey("compiled_hash") + ":" + fmt.Sprintf("%q", hash)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			badRec := appendRecordField(t, rec, tc.member)
			var probe map[string]json.RawMessage
			if err := json.Unmarshal(badRec, &probe); err != nil {
				t.Fatalf("crafted record is not valid JSON: %v", err)
			}
			bad := embed(badRec)
			if err := os.WriteFile(filepath.Join(dir, dirDataFile), bad, 0o644); err != nil {
				t.Fatal(err)
			}
			requireJobCorrupt(t, dir, bad, "j-bound", "compiled_hash")
		})
	}
}

// TestOmittedAndExplicitEmptyCompiledHashStayUnbound pins the two legal
// "unbound" encodings: a record that omits compiled_hash and one that carries
// an explicit "" both load as unbound. The writer never backfills either
// field, and a later compile of the pinned version does not bind the old job.
func TestOmittedAndExplicitEmptyCompiledHashStayUnbound(t *testing.T) {
	dir, _ := seedJobsStore(t)

	// The writer's register-only encoding omits the member outright.
	raw, err := os.ReadFile(filepath.Join(dir, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := compactJobRecord(t, raw, "j-loose")
	if bytes.Contains(rec, []byte("compiled_hash")) {
		t.Fatalf("register-only job persisted a compiled_hash member: %s", rec)
	}

	// An explicit empty string is the second legal unbound encoding.
	env := readGenericEnv(t, dir)
	firstJob(env, "j-loose")["compiled_hash"] = ""
	if err := os.WriteFile(filepath.Join(dir, dirDataFile),
		mustMarshalIndent(t, env), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("explicit empty compiled_hash must load: %v", err)
	}
	got, err := s.GetJob("j-loose")
	if err != nil || got.CompiledHash != "" {
		t.Fatalf("explicit empty binding read wrong: %+v %v", got, err)
	}
	// The bound job next to it still returns its exact hash.
	bound, err := s.GetJob("j-bound")
	if err != nil || bound.CompiledHash == "" {
		t.Fatalf("bound job lost next to explicit-empty record: %+v %v", bound, err)
	}

	// Compiling the previously uncompiled version never backfills the old
	// register-only job.
	if a, err := s.CompileCircuit("d", 1); err != nil {
		t.Fatalf("compile: %v", err)
	} else if a.Hash == "" {
		t.Fatal("compile returned an empty hash")
	}
	got, _ = s.GetJob("j-loose")
	if got.CompiledHash != "" {
		t.Fatalf("later compilation backfilled the binding: %+v", got)
	}
	// A normal commit must not invent a binding for the legacy record.
	if _, err := s.SubmitJob(boundProveJob("j-other", "c", 1, bound.CompiledHash)); err != nil {
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
	for _, id := range []string{"j-loose", "j-other"} {
		j, err := s2.GetJob(id)
		if err != nil {
			t.Fatalf("get %q: %v", id, err)
		}
		want := ""
		if id == "j-other" {
			want = bound.CompiledHash
		}
		if j.CompiledHash != want {
			t.Fatalf("job %q hash = %q, want %q", id, j.CompiledHash, want)
		}
	}
}

func mustMarshalIndent(t *testing.T, env map[string]any) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

// TestCorruptBindingFailsWholeDirectory: one job's null binding poisons every
// operation even while a healthy job and healthy circuits sit beside it — no
// partial job output, no new submission, and an unrelated circuit change can
// neither succeed nor rewrite the directory. The damaged bytes survive every
// refused operation, so a later write cannot make the anomaly "disappear".
func TestCorruptBindingFailsWholeDirectory(t *testing.T) {
	dir, hash := seedJobsStore(t)
	bad := damageJob(t, dir, "j-bound", func(job map[string]any) {
		job["compiled_hash"] = nil
	})

	s, err := Open(dir)
	if err == nil {
		defer s.Close()
		if _, lerr := s.ListJobs(); !errors.Is(lerr, ErrDataCorrupt) {
			t.Fatalf("ListJobs: want ErrDataCorrupt, got %v", lerr)
		}
		// The healthy job must not be returned on its own either.
		if _, gerr := s.GetJob("j-loose"); !errors.Is(gerr, ErrDataCorrupt) {
			t.Fatalf("GetJob on the healthy job: want ErrDataCorrupt, got %v", gerr)
		}
		// A new valid submission is refused before writing.
		if _, serr := s.SubmitJob(boundProveJob("j-new", "c", 1, hash)); !errors.Is(serr, ErrDataCorrupt) {
			t.Fatalf("SubmitJob: want ErrDataCorrupt, got %v", serr)
		}
		// An unrelated draft change cannot launder the file through a rewrite.
		if _, cerr := s.CreateCircuit(Circuit{Name: "other", Version: 1, Constraints: 1,
			Description: "d"}); !errors.Is(cerr, ErrDataCorrupt) {
			t.Fatalf("CreateCircuit: want ErrDataCorrupt, got %v", cerr)
		}
		// Nor can compiling the other version.
		if _, xerr := s.CompileCircuit("d", 1); !errors.Is(xerr, ErrDataCorrupt) {
			t.Fatalf("CompileCircuit: want ErrDataCorrupt, got %v", xerr)
		}
	} else if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("Open: want ErrDataCorrupt, got %v", err)
	}

	left, err := os.ReadFile(filepath.Join(dir, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(bad) {
		t.Fatalf("refused operations rewrote the directory\nwant: %q\n got: %q", bad, left)
	}
}

// TestCorruptBindingDiscoveredAfterOpen: the directory opens cleanly, the file
// is damaged on disk while the handle is open, and the next normal data
// operation fails closed instead of silently committing the de-bound record.
func TestCorruptBindingDiscoveredAfterOpen(t *testing.T) {
	dir, _ := seedJobsStore(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.GetJob("j-bound"); err != nil {
		t.Fatalf("first read should be healthy: %v", err)
	}

	bad := damageJob(t, dir, "j-bound", func(job map[string]any) {
		job["compiled_hash"] = nil
	})

	// Reads reload under lock and must fail.
	if _, err := s.ListJobs(); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("ListJobs after tamper: want ErrDataCorrupt, got %v", err)
	}
	if got, err := s.GetJob("j-bound"); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetJob after tamper: want ErrDataCorrupt, got %+v %v", got, err)
	}
	// A normal write must not quietly normalize the null away.
	if _, err := s.CreateCircuit(Circuit{Name: "other", Version: 1, Constraints: 1,
		Description: "d"}); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("CreateCircuit after tamper: want ErrDataCorrupt, got %v", err)
	}
	left, err := os.ReadFile(filepath.Join(dir, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(bad) {
		t.Fatalf("tampered data.json was overwritten")
	}
}
