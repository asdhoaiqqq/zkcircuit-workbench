package zkcircuit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// These tests pin the top-level envelope rule for the five committed fields
// (format, circuits, setups, jobs, artifacts): a difference in ASCII letter
// case only — including a difference hidden behind a JSON string escape —
// still names the same field. Two such spellings in one envelope corrupt the
// whole directory read, regardless of order or of whether the two values
// agree, while a single spelling in any case variant keeps reading exactly as
// the lowercase canonical key would.

// envKv is one top-level envelope member read in document order: keyToken is
// the exact JSON key token to emit (quotes included, escapes preserved) and
// val is its raw JSON value.
type envKv struct {
	keyToken string
	val      json.RawMessage
}

// readOrderedEnvelope tokenizes data.json into its ordered top-level members
// without a map, so a test can re-emit the envelope with chosen key spellings
// and a chosen member order.
func readOrderedEnvelope(t *testing.T, dir string) []envKv {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)))
	dec.UseNumber()
	open, err := dec.Token()
	if err != nil || open != json.Delim('{') {
		t.Fatalf("seeded envelope is not an object: %v", err)
	}
	var kvs []envKv
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			t.Fatal(err)
		}
		kvs = append(kvs, envKv{keyToken: strconv.Quote(keyTok.(string)), val: val})
	}
	if closeTok, err := dec.Token(); err != nil || closeTok != json.Delim('}') {
		t.Fatalf("seeded envelope is not valid JSON: %v", err)
	}
	return kvs
}

// envelopeValues indexes the seeded envelope's values by canonical key.
func envelopeValues(kvs []envKv) map[string]json.RawMessage {
	vals := make(map[string]json.RawMessage, len(kvs))
	for _, kv := range kvs {
		var key string
		if err := json.Unmarshal([]byte(kv.keyToken), &key); err != nil {
			panic(err)
		}
		vals[key] = kv.val
	}
	return vals
}

// writeEnvelope emits members in the given order, compacting values, and
// returns the bytes now on disk.
func writeEnvelope(t *testing.T, dir string, kvs []envKv) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, kv := range kvs {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(kv.keyToken)
		buf.WriteByte(':')
		var compact bytes.Buffer
		if err := json.Compact(&compact, kv.val); err != nil {
			t.Fatalf("envelope value is not valid JSON: %v", err)
		}
		buf.Write(compact.Bytes())
	}
	buf.WriteString("}\n")
	out := buf.Bytes()
	if err := os.WriteFile(filepath.Join(dir, dirDataFile), out, 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

// caseSpellings returns JSON key tokens naming field under non-canonical
// case spellings: fully upper, a deterministic mixed case, and the first
// letter escaped for both the lower and upper forms.
func caseSpellings(field string) map[string]string {
	upper := strings.ToUpper(field)
	mixed := make([]byte, len(field))
	for i := 0; i < len(field); i++ {
		c := field[i]
		if i%2 == 0 {
			c -= 'a' - 'A'
		}
		mixed[i] = c
	}
	return map[string]string{
		"upper":         strconv.Quote(upper),
		"mixed":         strconv.Quote(string(mixed)),
		"escaped-lower": fmt.Sprintf(`"\u%04x%s"`, field[0], field[1:]),
		"escaped-upper": fmt.Sprintf(`"\u%04x%s"`, upper[0], upper[1:]),
	}
}

// seedBoundStore creates a fully consistent directory — frozen circuit with
// definition, trusted setup, compiled artifact and a hash-bound job — and
// returns it after close.
func seedBoundStore(t *testing.T) (dir, hash string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := compiledVersion(t, s, "mul", 1, validDef)
	if _, err := s.SubmitJob(boundProveJob("j1", "mul", 1, h)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, h
}

// requireTopLevelDupCorrupt opens dir, requires a data-corrupt error that
// names both the duplicated top-level field (canonical lowercase) and its
// actual second spelling, and requires data.json to stay byte-for-byte.
func requireTopLevelDupCorrupt(t *testing.T, dir string, bad []byte, canonical, secondToken string) {
	t.Helper()
	s, err := Open(dir)
	if err == nil {
		s.Close()
		t.Fatalf("duplicate case-variant top-level field %q was accepted", canonical)
	}
	var se StoreError
	if !errors.As(err, &se) || se.Kind != ErrDataCorrupt.Kind {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "duplicate top-level field") {
		t.Fatalf("error does not identify a duplicated top-level field: %v", err)
	}
	if !strings.Contains(msg, strconv.Quote(canonical)) {
		t.Fatalf("error does not name the canonical field %q: %v", canonical, err)
	}
	var second string
	if uerr := json.Unmarshal([]byte(secondToken), &second); uerr == nil && second != canonical {
		if !strings.Contains(msg, strconv.Quote(second)) {
			t.Fatalf("error does not name the second spelling %q: %v", second, err)
		}
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
		t.Fatalf("reopen accepted the duplicated top-level field")
	} else if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("reopen: want ErrDataCorrupt, got %v", err)
	}
}

// dupEnvelope builds bytes in which field appears twice: the canonical
// member keeps its normal position with firstToken/firstVal, and a second
// member with secondToken/secondVal is appended at the end, so the first
// spelling always precedes the second in document order.
func dupEnvelope(t *testing.T, vals map[string]json.RawMessage, field, firstToken, secondToken string,
	firstVal, secondVal json.RawMessage) []byte {
	t.Helper()
	var kvs []envKv
	for _, f := range envelopeTopFields {
		val := vals[f]
		if len(val) == 0 && f != field {
			continue // a member the seeded envelope itself omitted (artifacts)
		}
		token := strconv.Quote(f)
		if f == field {
			token, val = firstToken, firstVal
		}
		kvs = append(kvs, envKv{keyToken: token, val: val})
	}
	kvs = append(kvs, envKv{keyToken: secondToken, val: secondVal})
	var buf bytes.Buffer
	emit := func(kv envKv) {
		buf.WriteString(kv.keyToken)
		buf.WriteByte(':')
		var compact bytes.Buffer
		if err := json.Compact(&compact, kv.val); err != nil {
			t.Fatalf("duplicate value is not valid JSON: %v", err)
		}
		buf.Write(compact.Bytes())
	}
	buf.WriteByte('{')
	for i, kv := range kvs {
		if i > 0 {
			buf.WriteByte(',')
		}
		emit(kv)
	}
	buf.WriteString("}\n")
	return buf.Bytes()
}

func writeRawEnvelope(t *testing.T, dir string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, dirDataFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTopLevelFieldCaseVariantDuplicateRefused is the matrix: every one of
// the five known fields appearing under two case spellings is rejected,
// whichever spelling comes first and even with byte-identical values.
func TestTopLevelFieldCaseVariantDuplicateRefused(t *testing.T) {
	type pair struct {
		name        string
		first, last string // JSON key tokens, in document order
	}
	pairsFor := func(field string, sp map[string]string) []pair {
		canonical := strconv.Quote(field)
		return []pair{
			{"canonical then upper", canonical, sp["upper"]},
			{"upper then canonical", sp["upper"], canonical},
			{"mixed then escaped-lower", sp["mixed"], sp["escaped-lower"]},
			{"escaped-upper then canonical", sp["escaped-upper"], canonical},
			{"upper twice", sp["upper"], sp["upper"]},
		}
	}
	for _, field := range envelopeTopFields {
		spellings := caseSpellings(field)
		t.Run(field, func(t *testing.T) {
			for _, tc := range pairsFor(field, spellings) {
				t.Run(tc.name, func(t *testing.T) {
					dir, _ := seedBoundStore(t)
					vals := envelopeValues(readOrderedEnvelope(t, dir))
					val := vals[field]
					bad := dupEnvelope(t, vals, field, tc.first, tc.last, val, val)
					writeRawEnvelope(t, dir, bad)
					requireTopLevelDupCorrupt(t, dir, bad, field, tc.last)
				})
			}
		})
	}
}

// TestTopLevelDuplicateCannotPickAValue covers the dangerous value shapes:
// a complete record beside an empty array or null (both orders), an
// unsupported format number beside 1 (both orders), and an array of illegal
// records beside an empty array. Neither side may be selected.
func TestTopLevelDuplicateCannotPickAValue(t *testing.T) {
	illegalRecord := json.RawMessage(`[{"name":"mul","version":1,"constraints":1,` +
		`"public_inputs":1,"private_inputs":1,"description":"x"}]`) // missing "frozen"

	cases := []struct {
		name      string
		field     string
		first     json.RawMessage // appears first
		second    json.RawMessage // appears last
		reversed  bool            // upper-case spelling comes first, canonical last
		wantField string
	}{
		{"full circuits then empty", "circuits", nil, json.RawMessage("[]"), false, "circuits"},
		{"empty circuits then full", "circuits", json.RawMessage("[]"), nil, true, "circuits"},
		{"full circuits then null", "circuits", nil, json.RawMessage("null"), false, "circuits"},
		{"null circuits then full", "circuits", json.RawMessage("null"), nil, true, "circuits"},
		{"bad-record circuits beside empty", "circuits", illegalRecord, json.RawMessage("[]"), false, "circuits"},
		{"empty beside bad-record circuits", "circuits", json.RawMessage("[]"), illegalRecord, true, "circuits"},
		{"format 1 then 999", "format", json.RawMessage("1"), json.RawMessage("999"), false, "format"},
		{"format 999 then 1", "format", json.RawMessage("999"), json.RawMessage("1"), true, "format"},
		{"format 1 then null", "format", json.RawMessage("1"), json.RawMessage("null"), false, "format"},
		{"setups full then null", "setups", nil, json.RawMessage("null"), false, "setups"},
		{"jobs empty then full", "jobs", json.RawMessage("[]"), nil, true, "jobs"},
		{"artifacts null then full", "artifacts", json.RawMessage("null"), nil, true, "artifacts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := seedBoundStore(t)
			vals := envelopeValues(readOrderedEnvelope(t, dir))
			first, second := tc.first, tc.second
			if first == nil {
				first = vals[tc.field]
			}
			if second == nil {
				second = vals[tc.field]
			}
			spellings := caseSpellings(tc.field)
			canonical := strconv.Quote(tc.field)
			firstToken, secondToken := canonical, spellings["upper"]
			if tc.reversed {
				firstToken, secondToken = spellings["upper"], canonical
			}
			bad := dupEnvelope(t, vals, tc.field, firstToken, secondToken, first, second)
			writeRawEnvelope(t, dir, bad)
			requireTopLevelDupCorrupt(t, dir, bad, tc.wantField, secondToken)
		})
	}
}

// TestSingleCaseVariantTopFieldReadsNormally: one occurrence under an upper,
// mixed or escaped spelling is the same single field and must load with
// version records, artifact hashes and the job binding all intact.
func TestSingleCaseVariantTopFieldReadsNormally(t *testing.T) {
	for _, field := range envelopeTopFields {
		for variant, token := range caseSpellings(field) {
			t.Run(field+"/"+variant, func(t *testing.T) {
				dir, h := seedBoundStore(t)
				kvs := readOrderedEnvelope(t, dir)
				for i := range kvs {
					var key string
					if err := json.Unmarshal([]byte(kvs[i].keyToken), &key); err != nil {
						t.Fatal(err)
					}
					if key == field {
						kvs[i].keyToken = token
					}
				}
				writeEnvelope(t, dir, kvs)

				s, err := Open(dir)
				if err != nil {
					t.Fatalf("single %s spelling %s rejected: %v", field, token, err)
				}
				defer s.Close()
				c, err := s.GetCircuit("mul", 1)
				if err != nil {
					t.Fatalf("circuit unreadable under %s: %v", token, err)
				}
				if !c.Frozen || c.Constraints != 1 || c.PublicInputs != 1 || c.PrivateInputs != 1 {
					t.Fatalf("version record drifted under %s: %+v", token, c)
				}
				if setup, found, err := s.GetSetup("mul", 1); err != nil || !found {
					t.Fatalf("setup lost under %s: %+v %v", token, setup, err)
				}
				a, err := s.GetArtifact("mul", 1)
				if err != nil {
					t.Fatalf("artifact lost under %s: %v", token, err)
				}
				if a.Hash != h {
					t.Fatalf("artifact hash drifted: got %q want %q", a.Hash, h)
				}
				j, err := s.GetJob("j1")
				if err != nil {
					t.Fatalf("bound job lost under %s: %v", token, err)
				}
				if j.CompiledHash != h {
					t.Fatalf("job binding drifted: got %q want %q", j.CompiledHash, h)
				}
			})
		}
	}
}

// TestSingleUpperCaseCircuitsStillStrictlyValidated proves the case-fold
// recognition cannot bypass record-level strictness: one CIRCUITS array
// containing a record the strict decoder rejects is still corrupt.
func TestSingleUpperCaseCircuitsStillStrictlyValidated(t *testing.T) {
	dir, _ := seedBoundStore(t)
	vals := envelopeValues(readOrderedEnvelope(t, dir))
	kvs := []envKv{
		{keyToken: `"FORMAT"`, val: vals["format"]},
		{keyToken: `"CIRCUITS"`, val: json.RawMessage(`[{
			"name":"mul","version":1,"constraints":1,
			"public_inputs":1,"private_inputs":1,"description":"x"}]`)},
		{keyToken: `"SETUPS"`, val: json.RawMessage("[]")},
		{keyToken: `"JOBS"`, val: json.RawMessage("[]")},
		{keyToken: `"ARTIFACTS"`, val: json.RawMessage("null")},
	}
	bad := writeEnvelope(t, dir, kvs)
	s, err := Open(dir)
	if err == nil {
		s.Close()
		t.Fatalf("illegal record under CIRCUITS was accepted")
	}
	if !errors.Is(err, ErrDataCorrupt) || !strings.Contains(err.Error(), `"frozen"`) {
		t.Fatalf("want corruption naming the missing frozen flag, got %v", err)
	}
	left, _ := os.ReadFile(filepath.Join(dir, dirDataFile))
	if string(left) != string(bad) {
		t.Fatalf("data.json modified after refused read")
	}

	// The legal null/empty readings still work under upper-case keys.
	dir2, _ := seedBoundStore(t)
	vals2 := envelopeValues(readOrderedEnvelope(t, dir2))
	kvs2 := []envKv{
		{keyToken: `"FORMAT"`, val: vals2["format"]},
		{keyToken: `"CIRCUITS"`, val: json.RawMessage("[]")},
		{keyToken: `"SETUPS"`, val: json.RawMessage("null")},
		{keyToken: `"JOBS"`, val: json.RawMessage("null")},
	}
	writeEnvelope(t, dir2, kvs2)
	s2, err := Open(dir2)
	if err != nil {
		t.Fatalf("upper-case empty/null arrays rejected: %v", err)
	}
	defer s2.Close()
	if list, err := s2.ListCircuits(); err != nil || len(list) != 0 {
		t.Fatalf("want no circuits, got %+v err=%v", list, err)
	}
}

// TestArtifactsOmissionCompatibility keeps the pre-existing readings: on a
// directory whose jobs carry no artifact binding, artifacts may be absent,
// an explicit empty array or null — under the canonical key and under case
// variants — and all load as "no artifact".
func TestArtifactsOmissionCompatibility(t *testing.T) {
	dir := seedClosedStore(t, true)
	srcVals := envelopeValues(readOrderedEnvelope(t, dir))
	for _, tc := range []struct {
		name  string
		token string
		val   json.RawMessage
		omit  bool
	}{
		{"absent", `"artifacts"`, nil, true},
		{"empty array", `"artifacts"`, json.RawMessage("[]"), false},
		{"null", `"artifacts"`, json.RawMessage("null"), false},
		{"upper null", `"ARTIFACTS"`, json.RawMessage("null"), false},
		{"mixed empty", `"ArTiFaCtS"`, json.RawMessage("[]"), false},
		{"escaped null", caseSpellings("artifacts")["escaped-lower"], json.RawMessage("null"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := filepath.Join(t.TempDir(), "bench")
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
			kvs := []envKv{
				{keyToken: `"format"`, val: srcVals["format"]},
				{keyToken: `"circuits"`, val: srcVals["circuits"]},
				{keyToken: `"setups"`, val: srcVals["setups"]},
				{keyToken: `"jobs"`, val: json.RawMessage("[]")},
			}
			if !tc.omit {
				kvs = append(kvs, envKv{keyToken: tc.token, val: tc.val})
			}
			writeEnvelope(t, d, kvs)
			s, err := Open(d)
			if err != nil {
				t.Fatalf("artifacts %s rejected: %v", tc.name, err)
			}
			if _, err := s.GetArtifact("mul", 1); !errors.Is(err, ErrArtifactMissing) {
				t.Fatalf("artifacts %s: want ErrArtifactMissing, got %v", tc.name, err)
			}
			if list, err := s.ListCircuits(); err != nil || len(list) != 1 {
				t.Fatalf("circuit listing wrong under artifacts %s: %+v %v", tc.name, list, err)
			}
			s.Close()
		})
	}
}

// TestCaseVariantDuplicateDiscoveredAfterOpen: an already-open directory
// whose file gains a case-variant duplicate must fail closed on the next
// ordinary read and on the next mutation, without serving cached data or
// committing.
func TestCaseVariantDuplicateDiscoveredAfterOpen(t *testing.T) {
	dir := seedClosedStore(t, false)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.GetCircuit("mul", 1); err != nil {
		t.Fatal(err)
	}

	vals := envelopeValues(readOrderedEnvelope(t, dir))
	bad := dupEnvelope(t, vals, "circuits", `"circuits"`, `"CIRCUITS"`,
		vals["circuits"], json.RawMessage("[]"))
	writeRawEnvelope(t, dir, bad)

	if _, err := s.ListCircuits(); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("ListCircuits after tamper: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.GetCircuit("mul", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("GetCircuit after tamper: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.FreezeCircuit("mul", 1); !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("FreezeCircuit after tamper: want ErrDataCorrupt, got %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "new", Version: 1, Constraints: 1,
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

// TestUnknownTopFieldNotCaughtByCaseRule: the new case folding applies to
// exactly the five known fields. A similar-looking but distinct key stays an
// unknown member and is ignored as before, even when it superficially
// resembles a known field in upper case; a repeated unknown key keeps the
// pre-existing exact-spelling duplicate rejection.
func TestUnknownTopFieldNotCaughtByCaseRule(t *testing.T) {
	dir, _ := seedBoundStore(t)
	vals := envelopeValues(readOrderedEnvelope(t, dir))
	// "CIRCUIT" looks like an upper "circuits" but is shorter, and the
	// escaped key decodes to "cİrcuits" (dotted capital I), not "circuits".
	kvs := []envKv{
		{keyToken: `"format"`, val: vals["format"]},
		{keyToken: `"circuits"`, val: vals["circuits"]},
		{keyToken: `"setups"`, val: vals["setups"]},
		{keyToken: `"jobs"`, val: vals["jobs"]},
		{keyToken: `"artifacts"`, val: vals["artifacts"]},
		{keyToken: `"CIRCUIT"`, val: json.RawMessage("[]")},
		{keyToken: `"cİrcuits"`, val: json.RawMessage("1")},
	}
	writeEnvelope(t, dir, kvs)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("unknown top-level fields rejected: %v", err)
	}
	defer s.Close()
	if list, err := s.ListCircuits(); err != nil || len(list) != 1 {
		t.Fatalf("real circuits swallowed by unknown keys: %+v %v", list, err)
	}

	// A repeated unknown key under the exact same spelling keeps failing with
	// the pre-existing unknown-key wording, and its values are never walked
	// as a known field.
	dir2, _ := seedBoundStore(t)
	vals2 := envelopeValues(readOrderedEnvelope(t, dir2))
	bad := writeEnvelope(t, dir2, []envKv{
		{keyToken: `"format"`, val: vals2["format"]},
		{keyToken: `"circuits"`, val: vals2["circuits"]},
		{keyToken: `"setups"`, val: vals2["setups"]},
		{keyToken: `"jobs"`, val: vals2["jobs"]},
		{keyToken: `"artifacts"`, val: vals2["artifacts"]},
		{keyToken: `"note"`, val: json.RawMessage("1")},
		{keyToken: `"note"`, val: json.RawMessage("2")},
	})
	s2, err := Open(dir2)
	if err == nil {
		s2.Close()
		t.Fatalf("repeated unknown key accepted")
	}
	if !errors.Is(err, ErrDataCorrupt) || !strings.Contains(err.Error(), `duplicate field "note"`) {
		t.Fatalf("want the pre-existing duplicate-field wording for unknown keys, got %v", err)
	}
	left, _ := os.ReadFile(filepath.Join(dir2, dirDataFile))
	if string(left) != string(bad) {
		t.Fatalf("data.json modified after refused read")
	}
}

// TestCaseVariantDirectoryCommitCanonicalizes: a valid directory whose keys
// were hand-written in upper case stays fully operational, and the next
// commit writes the canonical lowercase keys back without losing state.
func TestCaseVariantDirectoryCommitCanonicalizes(t *testing.T) {
	dir, h := seedBoundStore(t)
	kvs := readOrderedEnvelope(t, dir)
	for i := range kvs {
		var key string
		if err := json.Unmarshal([]byte(kvs[i].keyToken), &key); err != nil {
			t.Fatal(err)
		}
		if tok, ok := map[string]string{
			"format": `"FORMAT"`, "circuits": `"CIRCUITS"`, "setups": `"SETUPS"`,
			"jobs": `"JOBS"`, "artifacts": `"ARTIFACTS"`,
		}[key]; ok {
			kvs[i].keyToken = tok
		}
	}
	writeEnvelope(t, dir, kvs)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("upper-case envelope rejected: %v", err)
	}
	if _, err := s.CreateCircuit(Circuit{Name: "second", Version: 2, Constraints: 1,
		Description: "d"}); err != nil {
		t.Fatalf("mutation on upper-case directory: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, dirDataFile))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, key := range envelopeTopFields {
		if !strings.Contains(text, strconv.Quote(key)+":") {
			t.Fatalf("committed file lost canonical key %q", key)
		}
		if strings.Contains(text, strings.ToUpper(strconv.Quote(key))+":") {
			t.Fatalf("committed file kept upper-case key for %q", key)
		}
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if list, err := s2.ListCircuits(); err != nil || len(list) != 2 {
		t.Fatalf("state lost across canonicalization: %+v %v", list, err)
	}
	j, err := s2.GetJob("j1")
	if err != nil || j.CompiledHash != h {
		t.Fatalf("binding lost across canonicalization: %+v %v", j, err)
	}
}
