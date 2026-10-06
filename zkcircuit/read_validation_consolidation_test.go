package zkcircuit

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the read-time contract that survives consolidating a
// stored definition's validation: a definition is fully checked while its
// circuit record is read (whether or not the version is frozen or compiled),
// the artifact pass reuses that one validation result instead of
// re-normalizing, and every artifact/definition disagreement is still a
// whole-directory corruption with its historical cause.

// semanticDamage replaces committed fields through the generic envelope so a
// test can target nested definition or artifact values without byte offsets.
func semanticDamage(t *testing.T, dir string, mutate func(map[string]any)) {
	t.Helper()
	env := readGenericEnv(t, dir)
	mutate(env)
	writeGenericEnv(t, dir, env)
}

func firstArtifact(env map[string]any) map[string]any {
	return env["artifacts"].([]any)[0].(map[string]any)
}

// TestDraftDefinitionSemanticallyValidatedOnLoad: shape damage on a draft is
// covered elsewhere; here a never-frozen, never-compiled version carrying a
// definition must still run the full semantic check (prime modulus, counts,
// wire range) at open, not only once an artifact exists.
func TestDraftDefinitionSemanticallyValidatedOnLoad(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]any)
		wantMsg string
	}{
		{"non-prime modulus", func(env map[string]any) {
			firstDefinition(t, env)["modulus"] = "9"
		}, "has a corrupt constraint definition"},
		{"declared count mismatch", func(env map[string]any) {
			env["circuits"].([]any)[0].(map[string]any)["constraints"] = 2
		}, "definition is incompatible with its declared counts"},
		{"wire outside declared layout", func(env map[string]any) {
			circuit := env["circuits"].([]any)[0].(map[string]any)
			circuit["public_inputs"] = 0
			circuit["private_inputs"] = 0
		}, "has a corrupt constraint definition"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			// A one-constraint definition over modulus 7 using public wire 1.
			seedDraftWithDef(t, s, "c", 1, 1, 1, `{"modulus":"7","constraints":[
				{"a":[{"wire":1,"coeff":"1"}],"b":[],"c":[]}]}`)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			semanticDamage(t, dir, tc.mutate)
			_, err = Open(dir)
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestDefinitionDamageOutranksArtifactDamage keeps the historical precedence:
// the circuit pass validates every definition before any artifact is checked,
// so a simultaneously damaged definition and artifact reports the definition
// cause, never an artifact hash/count complaint.
func TestDefinitionDamageOutranksArtifactDamage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bench")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedDraftWithDef(t, s, "c", 1, 1, 1, `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`)
	if _, err := s.FreezeCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompileCircuit("c", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	semanticDamage(t, dir, func(env map[string]any) {
		firstDefinition(t, env)["modulus"] = "9" // non-prime
		art := firstArtifact(env)
		art["hash"] = strings.Repeat("0", 64)
		art["constraints"] = 2
	})
	_, err = Open(dir)
	if !errors.Is(err, ErrDataCorrupt) {
		t.Fatalf("want ErrDataCorrupt, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "has a corrupt constraint definition") {
		t.Fatalf("definition damage must be reported first, got %q", msg)
	}
	if strings.Contains(msg, "recompute") || strings.Contains(msg, "disagrees") {
		t.Fatalf("artifact finding leaked ahead of the definition error: %q", msg)
	}
}

// TestArtifactDefinitionDisagreementsStayCorrupt covers the artifact causes
// that must refuse the read as data corruption (not as a missing artifact an
// operation could proceed without): constraint count disagreeing with the
// version, modulus disagreeing with the definition, and a hash that does not
// recompute.
func TestArtifactDefinitionDisagreementsStayCorrupt(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]any)
		wantMsg string
	}{
		{"artifact count disagrees", func(env map[string]any) {
			firstArtifact(env)["constraints"] = 2
		}, "constraint count 2 disagrees with the version's 1"},
		{"artifact modulus disagrees", func(env map[string]any) {
			firstArtifact(env)["modulus"] = 11
		}, "is inconsistent with its stored definition"},
		{"artifact hash does not recompute", func(env map[string]any) {
			firstArtifact(env)["hash"] = strings.Repeat("0", 64)
		}, "does not recompute from the definition"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bench")
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			seedDraftWithDef(t, s, "c", 1, 1, 1, `{"modulus":"7","constraints":[
				{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`)
			if _, err := s.FreezeCircuit("c", 1); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CompileCircuit("c", 1); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			semanticDamage(t, dir, tc.mutate)
			_, err = Open(dir)
			if !errors.Is(err, ErrDataCorrupt) {
				t.Fatalf("want ErrDataCorrupt, got %v", err)
			}
			if errors.Is(err, ErrArtifactMissing) {
				t.Fatalf("artifact/definition disagreement must not read as a missing artifact: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.wantMsg)
			}
		})
	}
}
