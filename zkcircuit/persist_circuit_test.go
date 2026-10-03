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

// These tests pin the read-time contract for a circuit record already
// committed in data.json: name, version, constraints, public_inputs,
// private_inputs, frozen and description must each be present exactly once
// with the exact JSON type the writer emits. A missing key, null, wrong type
// or repeated key (including a key spelled through a JSON escape, or repeated
// with the same value) is data corruption: the whole directory fails to read
// rather than silently filling 0/false/"".

// seedClosedStore creates a store, optionally seeds a complete circuit
// (definition imported, then frozen) and closes it, returning its directory.
func seedClosedStore(t *testing.T, frozen bool) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const goodDef = `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`
	seedDraftWithDef(t, s, "mul", 1, 1, 1, goodDef)
	if frozen {
		if _, err := s.FreezeCircuit("mul", 1); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RecordSetup("mul", 1); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompileCircuit("mul", 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// setCircuitField replaces one top-level field of the first circuit record.
func setCircuitField(field string, value any) func(map[string]any) {
	return func(env map[string]any) {
		readFirstCircuit(env)[field] = value
	}
}

func deleteCircuitField(field string) func(map[string]any) {
	return func(env map[string]any) {
		delete(readFirstCircuit(env), field)
	}
}

func readFirstCircuit(env map[string]any) map[string]any {
	return env["circuits"].([]any)[0].(map[string]any)
}

func requireCorruptAndUntouched(t *testing.T, dir string, bad []byte, wantField string) {
	t.Helper()
	s, err := Open(dir)
	if err == nil {
		s.Close()
		t.Fatalf("damaged circuit record was accepted")
	}
	var se StoreError
	if !errors.As(err, &se) || se.Kind != ErrDataCorrupt.Kind {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	if wantField != "" && !strings.Contains(err.Error(), wantField) {
		t.Fatalf("error does not name the damaged field %q: %v", wantField, err)
	}
	left, rerr := os.ReadFile(filepath.Join(dir, dirDataFile))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(left) != string(bad) {
		t.Fatalf("refused read modified data.json\nwant: %q\n got: %q", bad, left)
	}
	if s2, err := Open(dir); err == nil {
		s2.Close()
		t.Fatalf("reopen accepted the damage")
	} else if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("reopen: want ErrDataCorrupt, got %v", err)
	}
}

// TestCorruptCircuitRecordFieldsRefused damages each required field of a
// committed circuit record in turn; whether the version is a draft or frozen
// (trusted setup + compiled artifact), the directory must be unreadable.
func TestCorruptCircuitRecordFieldsRefused(t *testing.T) {
	damage := []struct {
		name      string
		mutate    func(map[string]any)
		wantField string
	}{
		{"missing name", deleteCircuitField("name"), `"name"`},
		{"null name", setCircuitField("name", nil), `"name"`},
		{"numeric name", setCircuitField("name", 7), `"name"`},
		{"missing version", deleteCircuitField("version"), `"version"`},
		{"null version", setCircuitField("version", nil), `"version"`},
		{"string version", setCircuitField("version", "1"), `"version"`},
		{"float version", setCircuitField("version", 1.5), `"version"`},
		{"missing constraints", deleteCircuitField("constraints"), `"constraints"`},
		{"null constraints", setCircuitField("constraints", nil), `"constraints"`},
		{"string constraints", setCircuitField("constraints", "1"), `"constraints"`},
		{"missing public_inputs", deleteCircuitField("public_inputs"), `"public_inputs"`},
		{"null public_inputs", setCircuitField("public_inputs", nil), `"public_inputs"`},
		{"float public_inputs", setCircuitField("public_inputs", 0.5), `"public_inputs"`},
		{"string public_inputs", setCircuitField("public_inputs", "0"), `"public_inputs"`},
		{"missing private_inputs", deleteCircuitField("private_inputs"), `"private_inputs"`},
		{"null private_inputs", setCircuitField("private_inputs", nil), `"private_inputs"`},
		{"bool private_inputs", setCircuitField("private_inputs", true), `"private_inputs"`},
		// The headline damage: a frozen record whose frozen flag is deleted or
		// null must not come back as an editable draft.
		{"missing frozen", deleteCircuitField("frozen"), `"frozen"`},
		{"null frozen", setCircuitField("frozen", nil), `"frozen"`},
		{"string frozen", setCircuitField("frozen", "true"), `"frozen"`},
		{"numeric frozen", setCircuitField("frozen", 1), `"frozen"`},
		{"missing description", deleteCircuitField("description"), `"description"`},
		{"null description", setCircuitField("description", nil), `"description"`},
		{"numeric description", setCircuitField("description", 1), `"description"`},
	}

	for _, frozen := range []bool{false, true} {
		state := "draft"
		if frozen {
			state = "frozen-setup-compiled"
		}
		t.Run(state, func(t *testing.T) {
			for _, tc := range damage {
				t.Run(tc.name, func(t *testing.T) {
					dir := seedClosedStore(t, frozen)
					env := readGenericEnv(t, dir)
					tc.mutate(env)
					bad := writeGenericEnv(t, dir, env)
					requireCorruptAndUntouched(t, dir, bad, tc.wantField)
				})
			}
		})
	}
}

// compactCircuitRecord extracts the first circuit record from a raw envelope
// and returns it compacted, plus a function to re-embed a replacement.
func compactCircuitRecord(t *testing.T, raw []byte) (record []byte, embed func([]byte) []byte) {
	t.Helper()
	var wire struct {
		Circuits []json.RawMessage `json:"circuits"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	orig := []byte(wire.Circuits[0])
	var compact bytes.Buffer
	if err := json.Compact(&compact, orig); err != nil {
		t.Fatal(err)
	}
	return compact.Bytes(), func(rep []byte) []byte {
		return bytes.Replace(raw, orig, rep, 1)
	}
}

// appendRecordField returns a compact record JSON copy with an extra raw
// member inserted before the closing brace: `...,"field":value`.
func appendRecordField(t *testing.T, record []byte, member string) []byte {
	t.Helper()
	if len(record) == 0 || record[len(record)-1] != '}' {
		t.Fatalf("record does not end with }: %s", record)
	}
	out := make([]byte, 0, len(record)+len(member)+1)
	out = append(out, record[:len(record)-1]...)
	out = append(out, ',')
	out = append(out, member...)
	out = append(out, '}')
	// Must stay parseable JSON.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("crafted record is not valid JSON: %v (%s)", err, out)
	}
	return out
}

// TestDuplicateCircuitFieldsRefused: every field appearing twice is
// corruption — with a differing value, with the same value, and when one
// occurrence spells the key through a JSON escape (encoding/json decodes the
// escaped key to the same string and would otherwise keep one value).
func TestDuplicateCircuitFieldsRefused(t *testing.T) {
	fields := []string{
		"name", "version", "constraints", "public_inputs",
		"private_inputs", "frozen", "description",
	}
	values := map[string]string{
		"name": `"other"`, "version": `2`, "constraints": `9`,
		"public_inputs": `4`, "private_inputs": `5`,
		"description": `"changed"`,
	}
	// escapedKey spells one byte of the key as \u00XX: JSON decoding makes it
	// the identical member name, so it is still a duplicate.
	escapedKey := func(key string) string {
		return fmt.Sprintf("\"\\u%04x%s\"", key[0], key[1:])
	}

	for _, frozen := range []bool{false, true} {
		// The duplicated value differs from the stored one: flip frozen.
		values["frozen"] = `true`
		if frozen {
			values["frozen"] = `false`
		}
		state := "draft"
		if frozen {
			state = "frozen"
		}
		t.Run(state, func(t *testing.T) {
			for _, field := range fields {
				t.Run(field+"/different value", func(t *testing.T) {
					dir := seedClosedStore(t, frozen)
					raw, err := os.ReadFile(filepath.Join(dir, dirDataFile))
					if err != nil {
						t.Fatal(err)
					}
					rec, embed := compactCircuitRecord(t, raw)
					badRec := appendRecordField(t, rec, fmt.Sprintf("%q:%s", field, values[field]))
					bad := embed(badRec)
					if err := os.WriteFile(filepath.Join(dir, dirDataFile), bad, 0o644); err != nil {
						t.Fatal(err)
					}
					requireCorruptAndUntouched(t, dir, bad, field)
				})
				t.Run(field+"/same value", func(t *testing.T) {
					dir := seedClosedStore(t, frozen)
					raw, err := os.ReadFile(filepath.Join(dir, dirDataFile))
					if err != nil {
						t.Fatal(err)
					}
					rec, embed := compactCircuitRecord(t, raw)
					var probe map[string]json.RawMessage
					if err := json.Unmarshal(rec, &probe); err != nil {
						t.Fatal(err)
					}
					badRec := appendRecordField(t, rec,
						fmt.Sprintf("%q:%s", field, string(probe[field])))
					bad := embed(badRec)
					if err := os.WriteFile(filepath.Join(dir, dirDataFile), bad, 0o644); err != nil {
						t.Fatal(err)
					}
					requireCorruptAndUntouched(t, dir, bad, field)
				})
				t.Run(field+"/escaped key", func(t *testing.T) {
					dir := seedClosedStore(t, frozen)
					raw, err := os.ReadFile(filepath.Join(dir, dirDataFile))
					if err != nil {
						t.Fatal(err)
					}
					rec, embed := compactCircuitRecord(t, raw)
					var probe map[string]json.RawMessage
					if err := json.Unmarshal(rec, &probe); err != nil {
						t.Fatal(err)
					}
					// The second occurrence escapes one byte of the key; it is
					// the same JSON field name.
					badRec := appendRecordField(t, rec,
						escapedKey(field)+":"+string(probe[field]))
					bad := embed(badRec)
					if err := os.WriteFile(filepath.Join(dir, dirDataFile), bad, 0o644); err != nil {
						t.Fatal(err)
					}
					requireCorruptAndUntouched(t, dir, bad, field)
				})
			}
		})
	}
}

// TestUnknownCircuitFieldRefused: a committed record may only carry the
// fields the writer emits; an unknown member is refused instead of being
// silently ignored (a typo next to a real required field must not pass).
func TestUnknownCircuitFieldRefused(t *testing.T) {
	for _, field := range []string{`"note":"x"`, `"frzoen":true`} {
		t.Run(field, func(t *testing.T) {
			dir := seedClosedStore(t, true)
			raw, err := os.ReadFile(filepath.Join(dir, dirDataFile))
			if err != nil {
				t.Fatal(err)
			}
			rec, embed := compactCircuitRecord(t, raw)
			badRec := appendRecordField(t, rec, field)
			bad := embed(badRec)
			if err := os.WriteFile(filepath.Join(dir, dirDataFile), bad, 0o644); err != nil {
				t.Fatal(err)
			}
			requireCorruptAndUntouched(t, dir, bad, "unknown field")
		})
	}
}

// TestDuplicateEnvelopeFieldRefused: a repeated top-level envelope key is
// also refused rather than resolved to its last value.
func TestDuplicateEnvelopeFieldRefused(t *testing.T) {
	dir := seedClosedStore(t, false)
	path := filepath.Join(dir, dirDataFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	body := bytes.TrimSuffix(compact.Bytes(), []byte("}"))
	bad := append(append([]byte(nil), body...), []byte(`,"jobs":[]}`)...)
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	requireCorruptAndUntouched(t, dir, bad, "jobs")
}

// TestExplicitZeroFalseEmptyAreLegal pins the other half of the contract: an
// explicitly stored public/private count of 0, frozen:false and an empty
// description are ordinary values and must survive a reload unchanged — they
// must never be mistaken for a missing field.
func TestExplicitZeroFalseEmptyAreLegal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "zero", Version: 1, Constraints: 3,
		PublicInputs: 0, PrivateInputs: 0, Frozen: false, Description: ""}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Hand-assert the on-disk record carries the explicit fields.
	env := readGenericEnv(t, dir)
	rec := readFirstCircuit(env)
	for _, key := range []string{"name", "version", "constraints", "public_inputs",
		"private_inputs", "frozen", "description"} {
		if _, ok := rec[key]; !ok {
			t.Fatalf("writer omitted %q", key)
		}
	}
	if rec["public_inputs"] != float64(0) || rec["private_inputs"] != float64(0) ||
		rec["frozen"] != false || rec["description"] != "" {
		t.Fatalf("unexpected stored record: %+v", rec)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("explicit 0/false/\"\" rejected on reload: %v", err)
	}
	defer s2.Close()
	got, err := s2.GetCircuit("zero", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.PublicInputs != 0 || got.PrivateInputs != 0 || got.Frozen || got.Description != "" {
		t.Fatalf("reloaded record drifted: %+v", got)
	}
	// The draft remains genuinely editable: an empty-description update with
	// zero inputs succeeds, proving frozen:false was not conflated with
	// "unset".
	zero := 0
	empty := ""
	if _, err := s2.UpdateCircuitPartial("zero", 1, PartialCircuit{
		PublicInputs: &zero, Description: &empty,
	}); err != nil {
		t.Fatalf("draft update refused: %v", err)
	}
}

// TestCorruptRecordFailsWholeDirectory: one damaged record poisons every
// read even while a healthy record sits next to it — no partial results, and
// mutating, freezing and compiling operations neither succeed nor commit.
func TestCorruptRecordFailsWholeDirectory(t *testing.T) {
	dir := seedClosedStore(t, true)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Add a second, healthy record while the store is open.
	if _, err := s.CreateCircuit(Circuit{Name: "good", Version: 9, Constraints: 1,
		Description: "fine"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	env := readGenericEnv(t, dir)
	readFirstCircuit(env)["frozen"] = nil // damage the first (frozen) record
	bad := writeGenericEnv(t, dir, env)

	before, err := os.ReadFile(filepath.Join(dir, dirDataFile))
	if err != nil || string(before) != string(bad) {
		t.Fatalf("setup mismatch")
	}

	// Reads: whole-directory and single-record queries both fail.
	s2, err := Open(dir)
	if err == nil {
		defer s2.Close()
		if _, lerr := s2.ListCircuits(); !errors.Is(lerr, ErrDataCorrupt) {
			t.Fatalf("ListCircuits: want ErrDataCorrupt, got %v", lerr)
		}
		if _, gerr := s2.GetCircuit("good", 9); !errors.Is(gerr, ErrDataCorrupt) {
			t.Fatalf("GetCircuit on the healthy record: want ErrDataCorrupt, got %v", gerr)
		}
		if _, _, serr := s2.GetSetup("mul", 1); !errors.Is(serr, ErrDataCorrupt) {
			t.Fatalf("GetSetup: want ErrDataCorrupt, got %v", serr)
		}
		// Mutations on the healthy draft never commit.
		if _, uerr := s2.UpdateCircuit(Circuit{Name: "good", Version: 9, Constraints: 4,
			Description: "x"}); !errors.Is(uerr, ErrDataCorrupt) {
			t.Fatalf("UpdateCircuit: want ErrDataCorrupt, got %v", uerr)
		}
		if _, ferr := s2.FreezeCircuit("good", 9); !errors.Is(ferr, ErrDataCorrupt) {
			t.Fatalf("FreezeCircuit: want ErrDataCorrupt, got %v", ferr)
		}
		if _, ierr := s2.ImportConstraints("good", 9, writeTempJSON(t,
			`{"modulus":"7","constraints":[]}`)); !errors.Is(ierr, ErrDataCorrupt) {
			t.Fatalf("ImportConstraints: want ErrDataCorrupt, got %v", ierr)
		}
		if _, aerr := s2.CreateCircuit(Circuit{Name: "new", Version: 1, Constraints: 1,
			Description: "d"}); !errors.Is(aerr, ErrDataCorrupt) {
			t.Fatalf("CreateCircuit: want ErrDataCorrupt, got %v", aerr)
		}
		if _, cerr := s2.CompileCircuit("mul", 1); !errors.Is(cerr, ErrDataCorrupt) {
			t.Fatalf("CompileCircuit: want ErrDataCorrupt, got %v", cerr)
		}
	} else if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("Open: want ErrDataCorrupt, got %v", err)
	}

	left, err := os.ReadFile(filepath.Join(dir, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(bad) {
		t.Fatalf("refused operation committed state")
	}
}

// TestCorruptionDiscoveredAfterOpen: the directory opens cleanly, the file is
// damaged by an external editor, and the next operation's re-read must fail
// closed without committing.
func TestCorruptionDiscoveredAfterOpen(t *testing.T) {
	dir := seedClosedStore(t, false)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// First read is healthy.
	if _, err := s.GetCircuit("mul", 1); err != nil {
		t.Fatal(err)
	}

	// External damage while the handle stays open: drop the frozen flag.
	env := readGenericEnv(t, dir)
	delete(readFirstCircuit(env), "frozen")
	bad := writeGenericEnv(t, dir, env)

	// A read path that reloads under the shared lock must fail.
	if _, err := s.ListCircuits(); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("ListCircuits after tamper: want ErrDataCorrupt, got %v", err)
	}
	// A mutating path must not succeed, and crucially must not "repair" the
	// file by committing in-memory state over the damage.
	if _, err := s.FreezeCircuit("mul", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("FreezeCircuit after tamper: want ErrDataCorrupt, got %v", err)
	}
	left, err := os.ReadFile(filepath.Join(dir, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(bad) {
		t.Fatalf("tampered data.json was overwritten")
	}
}

// TestFrozenRecordTamperingCannotReopenAsDraft is the exact bug from the
// repair rule: frozen deleted/null must not make a frozen version editable.
func TestFrozenRecordTamperingCannotReopenAsDraft(t *testing.T) {
	for _, mode := range []string{"deleted", "null"} {
		t.Run(mode, func(t *testing.T) {
			dir := seedClosedStore(t, true)
			env := readGenericEnv(t, dir)
			if mode == "deleted" {
				delete(readFirstCircuit(env), "frozen")
			} else {
				readFirstCircuit(env)["frozen"] = nil
			}
			bad := writeGenericEnv(t, dir, env)

			s, err := Open(dir)
			if err == nil {
				defer s.Close()
				t.Fatalf("damaged frozen record opened")
			}
			if !errors.Is(err, ErrDataCorrupt) || !strings.Contains(err.Error(), `"frozen"`) {
				t.Fatalf("want ErrDataCorrupt naming frozen, got %v", err)
			}
			left, _ := os.ReadFile(filepath.Join(dir, dirDataFile))
			if string(left) != string(bad) {
				t.Fatalf("data.json changed")
			}
		})
	}
}
