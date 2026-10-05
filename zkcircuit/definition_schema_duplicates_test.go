package zkcircuit

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// escapedKey re-spells the first byte of key as a JSON \u00XX escape, built at
// runtime so the source itself never has to carry a backslash escape. The
// decoded key is identical to key.
func escapedKey(key string) string {
	return string(rune(92)) + u00xx(key[0]) + key[1:]
}

func u00xx(b byte) string {
	const hex = "0123456789abcdef"
	return "u00" + string(hex[b>>4]) + string(hex[b&0xf])
}

// secondKeyEscaped returns doc with its second occurrence of "key": re-spelled
// with key's first byte JSON-escaped. The document keeps two members whose
// unescaped names are equal.
func secondKeyEscaped(doc, key string) string {
	needle := `"` + key + `":`
	first := strings.Index(doc, needle)
	if first < 0 {
		return doc
	}
	second := strings.Index(doc[first+len(needle):], needle)
	if second < 0 {
		return doc
	}
	at := first + len(needle) + second
	replacement := `"` + escapedKey(key) + `":`
	return doc[:at] + replacement + doc[at+len(needle):]
}

// TestDuplicateFieldsRejectedByJSONUnescapedName pins the duplicate-field
// contract shared by both read paths after the repeated nested walks were
// collapsed into one document-wide scan. A field named twice is a duplicate by
// its JSON-unescaped name — so a second spelling through a \uXXXX escape is a
// duplicate too — even when both occurrences carry the same value. The rule
// holds at the definition, the constraint and the term level, and deep into a
// later constraint. The import path rejects it as an invalid argument and
// attributes the document-wide scan to the document itself (no "constraint
// #N:" position); the committed-data path rejects the same damage as
// corruption.
func TestDuplicateFieldsRejectedByJSONUnescapedName(t *testing.T) {
	const pub, priv, count = 1, 1, 1
	cases := []struct {
		name  string
		doc   string
		field string
	}{
		{"top field raw", `{"modulus":"7","modulus":"11","constraints":[]}`, "modulus"},
		{"top field identical value", `{"modulus":"7","modulus":"7","constraints":[]}`, "modulus"},
		{"constraint side raw", `{"modulus":"7","constraints":[{"a":[],"a":[],"b":[],"c":[]}]}`, "a"},
		{"constraint side identical value", `{"modulus":"7","constraints":[{"b":[],"b":[],"a":[],"c":[]}]}`, "b"},
		{"term wire raw", `{"modulus":"7","constraints":[{"a":[{"wire":1,"wire":2,"coeff":"1"}],"b":[],"c":[]}]}`, "wire"},
		{"term wire identical value", `{"modulus":"7","constraints":[{"a":[{"wire":1,"wire":1,"coeff":"1"}],"b":[],"c":[]}]}`, "wire"},
		{"term coeff raw", `{"modulus":"7","constraints":[{"a":[{"wire":1,"coeff":"1","coeff":"2"}],"b":[],"c":[]}]}`, "coeff"},
		{"term coeff identical value", `{"modulus":"7","constraints":[{"a":[{"wire":1,"coeff":"1","coeff":"1"}],"b":[],"c":[]}]}`, "coeff"},
		// The single document-wide scan must still reach a nested object far
		// from the document start.
		{"duplicate side in later constraint", `{"modulus":"7","constraints":[
			{"a":[],"b":[],"c":[]},{"a":[],"a":[],"b":[],"c":[]}]}`, "a"},
		{"duplicate term key in later constraint", `{"modulus":"7","constraints":[
			{"a":[],"b":[],"c":[]},{"a":[{"wire":1,"wire":1,"coeff":"1"}],"b":[],"c":[]}]}`, "wire"},
	}
	// Escaped-spelling variants: re-spell the second occurrence of each raw
	// case's key through a \uXXXX escape; it is the same JSON field name.
	escaped := make([]struct {
		name  string
		doc   string
		field string
	}, 0, len(cases))
	for _, tc := range cases {
		escaped = append(escaped, struct {
			name  string
			doc   string
			field string
		}{tc.name + " escaped", secondKeyEscaped(tc.doc, tc.field), tc.field})
	}
	all := append(cases, escaped...)

	for _, tc := range all {
		t.Run(tc.name+"/import", func(t *testing.T) {
			_, err := parseDefinitionJSON([]byte(tc.doc), pub, priv, count)
			if err == nil {
				t.Fatalf("duplicate field %q accepted on import\n%s", tc.field, tc.doc)
			}
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("want ErrInvalidArgument, got %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "contains duplicate field") || !strings.Contains(msg, `"`+tc.field+`"`) {
				t.Fatalf("error does not name duplicated field %q: %v", tc.field, err)
			}
			// The document-wide scan reports against the document as a whole;
			// it must not be re-attributed to a constraint/term position.
			if strings.Contains(msg, "constraint #") {
				t.Fatalf("nested duplicate gained a position prefix: %v", err)
			}
		})
		t.Run(tc.name+"/stored", func(t *testing.T) {
			var p persistDefinition
			err := json.Unmarshal([]byte(tc.doc), &p)
			if err == nil {
				t.Fatalf("duplicate field %q accepted in committed data\n%s", tc.field, tc.doc)
			}
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), "contains duplicate field") {
				t.Fatalf("error does not identify a duplicate: %v", err)
			}
		})
	}
}

// TestEscapedKeyHelperSanity confirms the test helper really produces a JSON
// escape that decodes back to the original field name.
func TestEscapedKeyHelperSanity(t *testing.T) {
	for _, key := range []string{"modulus", "a", "b", "wire", "coeff"} {
		var got string
		if err := json.Unmarshal([]byte(`"`+escapedKey(key)+`"`), &got); err != nil {
			t.Fatalf("escape for %q is not valid JSON: %v", key, err)
		}
		if got != key {
			t.Fatalf("escaped %q decoded to %q", key, got)
		}
	}
}

// TestDeeplyNestedButWellFormedScannedOnce is a guard against a future change
// dropping the document-wide scan: a document with distinct keys at every
// level and several terms per side must still parse on both paths.
func TestDeeplyNestedButWellFormedScannedOnce(t *testing.T) {
	doc := `{"modulus":"7","constraints":[
		{"a":[{"wire":0,"coeff":"1"},{"wire":1,"coeff":"2"}],"b":[{"wire":1,"coeff":"3"}],"c":[{"wire":0,"coeff":"3"}]},
		{"a":[],"b":[],"c":[]}]}`
	if _, err := parseDefinitionJSON([]byte(doc), 1, 1, 2); err != nil {
		t.Fatalf("well-formed document rejected: %v", err)
	}
	var p persistDefinition
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatalf("well-formed committed definition rejected: %v", err)
	}
}
