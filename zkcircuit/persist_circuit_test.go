package zkcircuit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the read-time contract for a stored circuit record: the
// seven scalar fields (name, version, constraints, public_inputs,
// private_inputs, frozen, description) must each be present exactly once
// with their declared JSON type. A missing, null, mistyped or duplicated
// field is data corruption, never a zero value to continue with — a dropped
// "frozen" would otherwise silently unfreeze the version and let a damaged
// record be modified again.

// fieldDamage is one map-expressible damage shape for a circuit record
// field: a nil value deletes the field, nilMarker sets it to JSON null and
// anything else replaces it (used for wrong JSON types).
type fieldDamage struct {
	name  string
	field string
	value any
}

// circuitRecordDamage enumerates the damage shapes for each required scalar
// field in turn: deleted, null and a wrong JSON type.
func circuitRecordDamage() []fieldDamage {
	var out []fieldDamage
	wrong := map[string]any{
		"name":           1,
		"version":        "1",
		"constraints":    1.5,
		"public_inputs":  true,
		"private_inputs": "0",
		"frozen":         "true",
		"description":    0,
	}
	for _, field := range []string{
		"name", "version", "constraints", "public_inputs", "private_inputs",
		"frozen", "description",
	} {
		out = append(out,
			fieldDamage{"missing " + field, field, nil},
			fieldDamage{"null " + field, field, nilMarker},
			fieldDamage{"wrong type " + field, field, wrong[field]},
		)
	}
	return out
}

// nilMarker distinguishes "set the field to JSON null" from "delete it" in
// the table above (a nil interface would be ambiguous).
var nilMarker = &struct{}{}

// TestCorruptPersistedCircuitRecordRefused damages each required scalar
// field in turn and requires the whole directory read to fail as corruption,
// naming the damaged field, leaving data.json byte-for-byte in place.
func TestCorruptPersistedCircuitRecordRefused(t *testing.T) {
	for _, tc := range circuitRecordDamage() {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			// A frozen+compiled record with a definition, plus a healthy
			// second record that must not make the damage tolerable.
			seedDraftWithDef(t, s, "c", 1, 1, 1,
				`{"modulus":"7","constraints":[{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`)
			if _, err := s.FreezeCircuit("c", 1); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CompileCircuit("c", 1); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateCircuit(Circuit{Name: "healthy", Version: 1, Constraints: 3, Description: "ok"}); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			env := readGenericEnv(t, dir)
			circuit := env["circuits"].([]any)[0].(map[string]any)
			if tc.value == nil {
				delete(circuit, tc.field)
			} else if tc.value == nilMarker {
				circuit[tc.field] = nil
			} else {
				circuit[tc.field] = tc.value
			}
			bad := writeGenericEnv(t, dir, env)

			got, err := Open(dir)
			if err == nil {
				got.Close()
				t.Fatalf("damaged circuit record was accepted")
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), `"`+tc.field+`"`) {
				t.Fatalf("error does not name the damaged field %q: %v", tc.field, err)
			}
			left, rerr := os.ReadFile(filepath.Join(dir, dirDataFile))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(left) != string(bad) {
				t.Fatal("refused read modified data.json")
			}
		})
	}
}

// TestDuplicateCircuitRecordFieldRefused writes duplicate members directly
// (a parsed map cannot hold them): the same field twice is corruption even
// when both values are identical, and a key spelled through JSON escapes
// still names the same field.
func TestDuplicateCircuitRecordFieldRefused(t *testing.T) {
	record := func(members string) []byte {
		return []byte(`{"format":1,"circuits":[{` + members + `}],"setups":[],"jobs":[]}`)
	}
	base := `"name":"c","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":true,"description":"d"`
	cases := []struct {
		name    string
		content []byte
		field   string
	}{
		{"same value twice", record(base + `,"frozen":true`), "frozen"},
		{"conflicting values", record(
			`"name":"c","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":true,"description":"d","frozen":false`), "frozen"},
		{"escaped key duplicate", record(
			`"name":"c","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":true,"description":"d","fro\u007Aen":true`), "frozen"},
		{"escaped name duplicate", record(
			`"name":"c","nam\u0065":"c","version":1,"constraints":1,"public_inputs":0,"private_inputs":0,"frozen":true,"description":"d"`), "name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeDataFile(t, dir, tc.content)

			s, err := Open(dir)
			if err == nil {
				s.Close()
				t.Fatalf("duplicate field %q was accepted", tc.field)
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), `"`+tc.field+`"`) {
				t.Fatalf("error does not name the duplicated field %q: %v", tc.field, err)
			}
			got, rerr := readDataFile(t, dir)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(got) != string(tc.content) {
				t.Fatal("refused read modified data.json")
			}
		})
	}
}

// TestCircuitRecordExplicitZeroValuesLegal pins the other half: an explicit
// public/private input count of 0, frozen:false and the empty description
// are ordinary saved values, not damage, and must survive a reload.
func TestCircuitRecordExplicitZeroValuesLegal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "z", Version: 1, Constraints: 1,
		PublicInputs: 0, PrivateInputs: 0, Description: ""}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("explicit zero/false/empty values rejected on reload: %v", err)
	}
	defer s2.Close()
	c, err := s2.GetCircuit("z", 1)
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicInputs != 0 || c.PrivateInputs != 0 || c.Frozen || c.Description != "" {
		t.Fatalf("explicit zero values did not survive reload: %+v", c)
	}
}

// TestCorruptCircuitRecordBlocksAllOperations: with one damaged record in
// the directory, reads return nothing (no partial results) and mutating
// operations fail without committing; data.json stays byte-for-byte intact.
func TestCorruptCircuitRecordBlocksAllOperations(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "good", Version: 1, Constraints: 2, Description: "g"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "bad", Version: 1, Constraints: 2, Description: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	env := readGenericEnv(t, dir)
	for _, c := range env["circuits"].([]any) {
		rec := c.(map[string]any)
		if rec["name"] == "bad" {
			delete(rec, "frozen")
		}
	}
	bad := writeGenericEnv(t, dir, env)

	s2, err := Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		if s2 != nil {
			s2.Close()
		}
		t.Fatalf("open over damaged record: want ErrDataCorrupt, got %v", err)
	}
	left, _ := os.ReadFile(filepath.Join(dir, dirDataFile))
	if string(left) != string(bad) {
		t.Fatal("refused open modified data.json")
	}
}

// TestCorruptCircuitRecordDetectedAfterOpen damages the file while the store
// is already open: the next read and the next mutating operation must both
// re-read, report corruption and leave the file untouched.
func TestCorruptCircuitRecordDetectedAfterOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "c", Version: 1, Constraints: 2, Description: "d"}); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	env := readGenericEnv(t, dir)
	env["circuits"].([]any)[0].(map[string]any)["version"] = nil
	bad := writeGenericEnv(t, dir, env)

	if _, err := s.ListCircuits(); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("list after damage: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.GetCircuit("c", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("get after damage: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.FreezeCircuit("c", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("freeze after damage: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "new", Version: 1, Constraints: 1}); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("create after damage: want ErrDataCorrupt, got %v", err)
	}
	left, _ := os.ReadFile(filepath.Join(dir, dirDataFile))
	if string(left) != string(bad) {
		t.Fatal("failed operation committed over the damaged file")
	}
}
