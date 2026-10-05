package zkcircuit

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// These tests pin the shared structural decode (definition_schema.go) on both
// read paths after the nested-object checks were consolidated into one
// whole-document tokenization: the import path tags every shape failure as
// ErrInvalidArgument and names constraints/terms by position, while the
// committed-data path tags the identical damage as ErrDataCorrupt.

func decodeBothWays(t *testing.T, doc string) (ierr, serr error) {
	t.Helper()
	_, ierr = decodeDefinitionShape([]byte(doc), importDefinitionSource)
	var p persistDefinition
	if jerr := json.Unmarshal([]byte(doc), &p); jerr != nil {
		serr = jerr
	}
	return ierr, serr
}

// A field repeated through a JSON \uXXXX escape is still a duplicate, keys
// being compared after unescaping — including two identical values — at every
// level of the document. The finding is a document-level one (named for the
// whole definition, never prefixed with a constraint position) on both paths.
func TestDefinitionShapeRejectsRepeatedFields(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string // unescaped field named in the message
	}{
		{"top repeated", `{"modulus":"7","modulus":"7","constraints":[]}`, "modulus"},
		{"top repeated different value", `{"modulus":"7","modulus":"11","constraints":[]}`, "modulus"},
		{"top repeated escaped", "{\"modulus\":\"7\",\"\\u006dodulus\":\"11\",\"constraints\":[]}", "modulus"},
		{"constraints repeated", `{"modulus":"7","constraints":[],"constraints":[]}`, "constraints"},
		{"side repeated", `{"modulus":"7","constraints":[{"a":[],"a":[],"b":[],"c":[]}]}`, "a"},
		{"side repeated escaped", `{"modulus":"7","constraints":[{"a":[],"b":[],"c":[],"\u0063":[]}]}`, "c"},
		{"term wire repeated", `{"modulus":"7","constraints":[{"a":[{"wire":1,"wire":1,"coeff":"1"}],"b":[],"c":[]}]}`, "wire"},
		{"term wire repeated escaped", `{"modulus":"7","constraints":[{"a":[{"wire":1,"\u0077ire":2,"coeff":"1"}],"b":[],"c":[]}]}`, "wire"},
		{"term coeff repeated", `{"modulus":"7","constraints":[{"a":[{"wire":1,"coeff":"1","coeff":"2"}],"b":[],"c":[]}]}`, "coeff"},
		{"deep repeat in second constraint", `{"modulus":"7","constraints":[
			{"a":[],"b":[],"c":[]},
			{"a":[{"wire":1,"wire":2,"coeff":"1"}],"b":[],"c":[]}]}`, "wire"},
		// A repeat buried inside an unknown field's value is still damage.
		{"repeat inside unknown term field", `{"modulus":"7","constraints":[{"a":[
			{"wire":1,"coeff":"1","z":{"y":1,"y":2}}],"b":[],"c":[]}]}`, "y"},
		// …and inside a wrongly typed container, which is still part of the
		// document grammar the one walk covers.
		{"repeat inside non-object constraint", `{"modulus":"7","constraints":[[{"x":1,"x":2}]]}`, "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ierr, serr := decodeBothWays(t, tc.doc)
			if ierr == nil || serr == nil {
				t.Fatalf("expected rejection on both paths: import=%v stored=%v", ierr, serr)
			}
			if !errors.Is(ierr, ErrInvalidArgument) {
				t.Fatalf("import want ErrInvalidArgument, got %v", ierr)
			}
			if !errors.Is(serr, ErrDataCorrupt) {
				t.Fatalf("stored want ErrDataCorrupt, got %v", serr)
			}
			for label, err := range map[string]error{"import": ierr, "stored": serr} {
				msg := err.Error()
				if !strings.Contains(msg, "contains duplicate field "+strconv.Quote(tc.want)) {
					t.Errorf("%s message missing duplicate field %q: %s", label, tc.want, msg)
				}
				if strings.Contains(msg, "constraint #") {
					t.Errorf("%s duplicate must be document-level, got constraint prefix: %s", label, msg)
				}
			}
		})
	}
}

// Precedence of findings is fixed: syntax first, then an unknown top-level
// field, then the first repeated key anywhere, and only afterwards the
// level-by-level required-field/type checks. These orderings were observable
// before the consolidation and must not move.
func TestDefinitionShapeFailurePrecedence(t *testing.T) {
	// Syntax is judged before any shape on both paths. (At the
	// persistDefinition level a JSON syntax error surfaces as encoding/json's
	// own SyntaxError; the envelope's read turns it into data corruption.)
	ierr, serr := decodeBothWays(t, `{bad`)
	if !strings.Contains(ierr.Error(), "is not valid JSON") {
		t.Fatalf("import syntax should be a shape rejection: %v", ierr)
	}
	if serr == nil {
		t.Fatalf("stored malformed document should fail to decode")
	}

	// An unknown field at the definition root outranks a duplicate nested in
	// the constraints: the top-level field set is judged before the deferred
	// first duplicate.
	ierr, serr = decodeBothWays(t, `{"modulus":"7","z":1,"constraints":[
		{"a":[{"wire":1,"wire":2,"coeff":"1"}],"b":[],"c":[]}]}`)
	if !strings.Contains(ierr.Error(), `constraint definition has unknown field "z"`) {
		t.Fatalf("unknown top-level field should win, got %v", ierr)
	}
	if !strings.Contains(serr.Error(), `stored constraint definition has unknown field "z"`) {
		t.Fatalf("stored unknown top-level field should win, got %v", serr)
	}

	// A constraint-level unknown field does NOT outrank a duplicate elsewhere:
	// nested unknowns are local, while the duplicate is document-level.
	ierr, serr = decodeBothWays(t, `{"modulus":"7","constraints":[
		{"a":[],"b":[],"c":[],"z":1},
		{"a":[{"wire":1,"wire":2,"coeff":"1"}],"b":[],"c":[]}]}`)
	if !strings.Contains(ierr.Error(), `contains duplicate field "wire"`) {
		t.Fatalf("document-level duplicate should beat a later local unknown: %v", ierr)
	}

	// A deep duplicate outranks a missing required side on a later
	// constraint (the whole document is walked before local checks run).
	ierr, serr = decodeBothWays(t, `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"wire":2,"coeff":"1"}],"b":[],"c":[]},
		{"b":[],"c":[]}]}`)
	if !strings.Contains(ierr.Error(), `contains duplicate field "wire"`) {
		t.Fatalf("deep duplicate should win over later missing side, got %v", ierr)
	}
	if !strings.Contains(serr.Error(), `contains duplicate field "wire"`) {
		t.Fatalf("stored deep duplicate should win, got %v", serr)
	}

	// With no duplicate, a local failure is attributed to its constraint.
	ierr, serr = decodeBothWays(t, `{"modulus":"7","constraints":[
		{"a":[],"b":[],"c":[]},{"a":[],"b":[]}]}`)
	if !strings.HasPrefix(ierr.Error(), "constraint #2:") {
		t.Fatalf("local failure should carry its constraint position, got %v", ierr)
	}
	if !strings.Contains(serr.Error(), "stored constraint is missing required field") {
		t.Fatalf("stored local failure wording changed: %v", serr)
	}
}

// The one-pass decode preserves every accepted value exactly: modulus text,
// constraint order, side order and each term's wire/coefficient, including
// explicit empty arrays and the constant wire 0.
func TestDefinitionShapeAcceptsAndPreserves(t *testing.T) {
	doc := `{"modulus":"7","constraints":[
		{"a":[],"b":[{"wire":0,"coeff":"1"}],"c":[]},
		{"a":[{"wire":2,"coeff":"8"},{"wire":2,"coeff":"-1"}],"b":[],"c":[{"wire":1,"coeff":"0"}]}]}`
	shape, err := decodeDefinitionShape([]byte(doc), importDefinitionSource)
	if err != nil {
		t.Fatal(err)
	}
	if shape.modulus != "7" || len(shape.constraints) != 2 {
		t.Fatalf("shape header wrong: %+v", shape)
	}
	c0 := shape.constraints[0]
	if len(c0.a) != 0 || len(c0.b) != 1 || c0.b[0].wire != 0 || c0.b[0].coeff != "1" || len(c0.c) != 0 {
		t.Fatalf("empty array / wire 0 not preserved: %+v", c0)
	}
	c1a := shape.constraints[1].a
	if len(c1a) != 2 || c1a[0].coeff != "8" || c1a[1].coeff != "-1" {
		t.Fatalf("term order/coeff not preserved: %+v", c1a)
	}

	// The stored form renders the same shape (empty arrays stay arrays).
	var p persistDefinition
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Constraints[0].A) != 0 || p.Constraints[0].A == nil {
		t.Fatalf("empty side must be a non-nil empty slice, got %#v", p.Constraints[0].A)
	}
	if p.Constraints[0].B[0].Wire != 0 || p.Constraints[0].B[0].Coeff != "1" {
		t.Fatalf("wire-0 term lost: %+v", p.Constraints[0].B[0])
	}
}

// BenchmarkDecodeDefinitionShape exercises the single structural pass over a
// large definition; the point of the consolidation is that nested objects are
// tokenized once rather than once per enclosing level.
func BenchmarkDecodeDefinitionShape(b *testing.B) {
	var sb strings.Builder
	sb.WriteString(`{"modulus":"2147483647","constraints":[`)
	const constraints, terms = 500, 40
	for i := 0; i < constraints; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"a":[`)
		for j := 0; j < terms; j++ {
			if j > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(`{"wire":1,"coeff":"1"}`)
		}
		sb.WriteString(`],"b":[],"c":[]}`)
	}
	sb.WriteString(`]}`)
	raw := []byte(sb.String())
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		if _, err := decodeDefinitionShape(raw, importDefinitionSource); err != nil {
			b.Fatal(err)
		}
	}
}
