package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// CLI regression coverage for constraint import wire bounds: a definition
// file whose terms reference wires outside the draft's declared input layout
// must be refused with exit code 1 and an invalid-argument message naming
// the allowed range — never reported as success and never misclassified as
// data-directory corruption (exit 3) — even when the out-of-range term
// carries a zero coefficient, a modulus-multiple coefficient or cancels
// against a twin term. A refused file must leave the previously imported
// definition, the counts and the description exactly as they were, so the
// version still freezes, compiles and checks inputs against the old
// definition.
func TestCLIImportRejectsOutOfRangeWireWithZeroishCoeff(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "bench")

	type res struct {
		code int
		out  string
		err  string
	}
	call := func(args ...string) res {
		c, o, e := runCapture(t, args...)
		return res{c, o, e}
	}

	if r := call("circuit-create", "--dir", dir, "--name", "mul", "--version", "1",
		"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1",
		"--description", "original"); r.code != 0 {
		t.Fatalf("create: %+v", r)
	}
	defPath := writeFile(t, work, "def.json", cliDef) // mod 7: wire1 * wire2 = 6
	if r := call("constraint-import", "--dir", dir, "--name", "mul", "--version", "1",
		"--file", defPath); r.code != 0 {
		t.Fatalf("seed import: %+v", r)
	}

	// Every refusal mode: legal wires are 0, 1, 2 for this layout. Each file
	// also carries a different modulus (11), so any partial application
	// would be visible in the later compile.
	badFiles := map[string]string{
		"zero coefficient": `{"modulus":"11","constraints":[
			{"a":[{"wire":3,"coeff":"0"}],"b":[],"c":[]}]}`,
		"modulus multiple": `{"modulus":"11","constraints":[
			{"a":[],"b":[{"wire":5,"coeff":"11"}],"c":[]}]}`,
		"cancelling pair": `{"modulus":"11","constraints":[
			{"a":[],"b":[],"c":[{"wire":4,"coeff":"6"},{"wire":4,"coeff":"5"}]}]}`,
		"negative wire": `{"modulus":"11","constraints":[
			{"a":[{"wire":-1,"coeff":"0"}],"b":[],"c":[]}]}`,
	}
	for name, doc := range badFiles {
		badPath := writeFile(t, work, "bad-"+strings.ReplaceAll(name, " ", "-")+".json", doc)
		r := call("constraint-import", "--dir", dir, "--name", "mul", "--version", "1",
			"--file", badPath)
		if r.code != 1 {
			t.Fatalf("%s: want exit 1, got %+v", name, r)
		}
		if !strings.Contains(r.err, "invalid argument") || !strings.Contains(r.err, "outside [0,2]") {
			t.Fatalf("%s: rejection must name the wire range as invalid argument, got %+v", name, r)
		}
		if strings.Contains(r.err, "data corrupt") || strings.Contains(r.err, "read failed") {
			t.Fatalf("%s: a bad import file must not be reported as data corruption, got %+v", name, r)
		}
		if strings.Contains(r.out, "constraints imported") {
			t.Fatalf("%s: refused import must not report success, got %+v", name, r)
		}
	}

	// The circuit record is untouched: counts and description as created.
	if r := call("circuit-get", "--dir", dir, "--name", "mul", "--version", "1"); r.code != 0 ||
		!strings.Contains(r.out, "constraints=1 public_inputs=1 private_inputs=1") ||
		!strings.Contains(r.out, `description="original"`) {
		t.Fatalf("circuit record changed by refused imports: %+v", r)
	}

	// The version still freezes, compiles and checks against the original
	// modulus-7 definition, not anything from the refused files.
	if r := call("circuit-freeze", "--dir", dir, "--name", "mul", "--version", "1"); r.code != 0 {
		t.Fatalf("freeze: %+v", r)
	}
	r := call("circuit-compile", "--dir", dir, "--name", "mul", "--version", "1")
	if r.code != 0 || !strings.Contains(r.out, "modulus=7") {
		t.Fatalf("compile after refused imports must use the preserved definition: %+v", r)
	}
	hash := strings.TrimSpace(strings.SplitN(strings.SplitAfter(r.out, "hash=")[1], "\n", 2)[0])
	goodPath := writeFile(t, work, "good.json", cliGoodInput)
	if r := call("input-check", "--dir", dir, "--name", "mul", "--version", "1",
		"--hash", hash, "--file", goodPath); r.code != 0 || !strings.Contains(r.out, "satisfied=true") {
		t.Fatalf("check against the preserved definition: %+v", r)
	}
	badInput := writeFile(t, work, "unsat.json", `{"public":["2"],"private":["4"]}`)
	if r := call("input-check", "--dir", dir, "--name", "mul", "--version", "1",
		"--hash", hash, "--file", badInput); r.code != 0 ||
		!strings.Contains(r.out, "satisfied=false") || !strings.Contains(r.out, "first_failure=1") {
		t.Fatalf("unsatisfied verdict against the preserved definition: %+v", r)
	}
}

// TestCLIImportAcceptsZeroCoeffOnLegalWires pins the other half of the
// contract at the command line: zero coefficients, modulus multiples and
// cancelling pairs on in-range wires import successfully and compile to the
// identical artifact hash as the same definition with those terms removed.
func TestCLIImportAcceptsZeroCoeffOnLegalWires(t *testing.T) {
	work := t.TempDir()

	type res struct {
		code int
		out  string
		err  string
	}
	call := func(args ...string) res {
		c, o, e := runCapture(t, args...)
		return res{c, o, e}
	}

	// Control: the plain definition.
	dirPlain := filepath.Join(work, "plain")
	if r := call("circuit-create", "--dir", dirPlain, "--name", "mul", "--version", "1",
		"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1"); r.code != 0 {
		t.Fatalf("create plain: %+v", r)
	}
	if r := call("constraint-import", "--dir", dirPlain, "--name", "mul", "--version", "1",
		"--file", writeFile(t, work, "plain.json", cliDef)); r.code != 0 {
		t.Fatalf("import plain: %+v", r)
	}

	// Variant: same circuit, same layout, but the file pads every side with
	// zero coefficients, modulus multiples and a cancelling pair — all on
	// legal wires 0, 1, 2.
	variant := `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"8"},{"wire":0,"coeff":"0"}],
		 "b":[{"wire":2,"coeff":"1"},{"wire":1,"coeff":"7"},{"wire":1,"coeff":"-7"}],
		 "c":[{"wire":0,"coeff":"6"},{"wire":2,"coeff":"-7000000000000000000000000"}]}]}`
	dirVariant := filepath.Join(work, "variant")
	if r := call("circuit-create", "--dir", dirVariant, "--name", "mul", "--version", "1",
		"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1"); r.code != 0 {
		t.Fatalf("create variant: %+v", r)
	}
	r := call("constraint-import", "--dir", dirVariant, "--name", "mul", "--version", "1",
		"--file", writeFile(t, work, "variant.json", variant))
	if r.code != 0 || !strings.Contains(r.out, "modulus=7 constraints=1") {
		t.Fatalf("variant import must succeed with the same shape: %+v", r)
	}

	compile := func(dir string) string {
		t.Helper()
		if r := call("circuit-freeze", "--dir", dir, "--name", "mul", "--version", "1"); r.code != 0 {
			t.Fatalf("freeze: %+v", r)
		}
		r := call("circuit-compile", "--dir", dir, "--name", "mul", "--version", "1")
		if r.code != 0 {
			t.Fatalf("compile: %+v", r)
		}
		return strings.TrimSpace(strings.SplitN(strings.SplitAfter(r.out, "hash=")[1], "\n", 2)[0])
	}
	hashPlain, hashVariant := compile(dirPlain), compile(dirVariant)
	if hashPlain != hashVariant {
		t.Fatalf("zero-coefficient padding changed the artifact hash:\n plain   %s\n variant %s", hashPlain, hashVariant)
	}

	// The same input gives the same verdict under both compilations.
	goodPath := writeFile(t, work, "good.json", cliGoodInput)
	for _, d := range []string{dirPlain, dirVariant} {
		if r := call("input-check", "--dir", d, "--name", "mul", "--version", "1",
			"--hash", hashPlain, "--file", goodPath); r.code != 0 || !strings.Contains(r.out, "satisfied=true") {
			t.Fatalf("check in %s: %+v", d, r)
		}
	}
}
