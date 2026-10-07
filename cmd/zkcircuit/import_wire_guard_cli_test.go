package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// This file is the command-line regression guard for the wire-range rule at
// import time. An out-of-range wire is a bad REQUEST even when its coefficient
// is zero, a whole multiple of the modulus, or cancelled by a twin term: the
// constraint-import command must exit 1, name the allowed wire range on
// stderr, print no success line, and never classify the problem as data
// directory corruption (exit 3). The definition already saved for the draft
// must remain the one later get/freeze/compile/input-check operate on.

// cliRes is one captured command invocation.
type cliRes struct {
	code int
	out  string
	err  string
}

// runCLI invokes a command and captures its exit code and both streams.
func runCLI(t *testing.T, args ...string) cliRes {
	t.Helper()
	code, out, errOut := runCapture(t, args...)
	return cliRes{code: code, out: out, err: errOut}
}

func TestCLIImportIllegalWireRejectedAndKeepsOldDefinition(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "bench")

	// modulus 7, one constraint w1·w2 = 6; legal layout is wires [0,2].
	goodDef := writeFile(t, work, "good.json", `{"modulus":"7","constraints":[
		{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`)

	call := func(args ...string) cliRes { return runCLI(t, args...) }

	if r := call("circuit-create", "--dir", dir, "--name", "mul", "--version", "1",
		"--constraints", "1", "--public-inputs", "1", "--private-inputs", "1",
		"--description", "original"); r.code != 0 {
		t.Fatalf("create: %+v", r)
	}
	if r := call("constraint-import", "--dir", dir, "--name", "mul", "--version", "1",
		"--file", goodDef); r.code != 0 {
		t.Fatalf("seed import: %+v", r)
	}

	// Every bad document keeps modulus 7 and exactly one constraint and would
	// be a perfectly legal definition but for the out-of-range wire. Each
	// disguise would otherwise make the offending term vanish.
	badDefs := map[string]string{
		"zero past end on a": `{"modulus":"7","constraints":[
			{"a":[{"wire":3,"coeff":"0"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`,
		"modulus multiple past end on a": `{"modulus":"7","constraints":[
			{"a":[{"wire":3,"coeff":"7"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`,
		"cancelling pair past end on a": `{"modulus":"7","constraints":[
			{"a":[{"wire":3,"coeff":"3"},{"wire":3,"coeff":"-3"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`,
		"zero past end on b": `{"modulus":"7","constraints":[
			{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":3,"coeff":"0"}],"c":[{"wire":0,"coeff":"6"}]}]}`,
		"modulus multiple past end on c": `{"modulus":"7","constraints":[
			{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":3,"coeff":"14"}]}]}`,
		"cancelling pair past end on c": `{"modulus":"7","constraints":[
			{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":3,"coeff":"5"},{"wire":3,"coeff":"2"}]}]}`,
		"negative wire zero on a": `{"modulus":"7","constraints":[
			{"a":[{"wire":-1,"coeff":"0"}],"b":[{"wire":2,"coeff":"1"}],"c":[{"wire":0,"coeff":"6"}]}]}`,
		"negative wire multiple on b": `{"modulus":"7","constraints":[
			{"a":[{"wire":1,"coeff":"1"}],"b":[{"wire":-2,"coeff":"14"}],"c":[{"wire":0,"coeff":"6"}]}]}`,
	}
	for name, body := range badDefs {
		path := writeFile(t, work, "bad_"+sanitizeCLI(name)+".json", body)
		r := call("constraint-import", "--dir", dir, "--name", "mul", "--version", "1", "--file", path)
		if r.code != 1 {
			t.Fatalf("%s: want exit 1, got %d (out=%q err=%q)", name, r.code, r.out, r.err)
		}
		if r.code == 3 || strings.Contains(r.err, "read failed") || strings.Contains(r.err, "corrupt") {
			t.Fatalf("%s: an illegal wire is a bad request, not directory corruption: %q", name, r.err)
		}
		if !strings.Contains(r.err, "outside") || !strings.Contains(r.err, "[0,2]") {
			t.Fatalf("%s: error must name the allowed wire range [0,2], got %q", name, r.err)
		}
		if strings.TrimSpace(r.out) != "" {
			t.Fatalf("%s: rejected import must not print a success line: %q", name, r.out)
		}
	}

	// The saved counts and description are untouched.
	if r := call("circuit-get", "--dir", dir, "--name", "mul", "--version", "1"); r.code != 0 ||
		!strings.Contains(r.out, "constraints=1") ||
		!strings.Contains(r.out, "public_inputs=1") ||
		!strings.Contains(r.out, "private_inputs=1") ||
		!strings.Contains(r.out, `description="original"`) {
		t.Fatalf("circuit-get after refused imports: %+v", r)
	}

	// Freeze and compile the ORIGINAL definition; its hash is then usable.
	if r := call("circuit-freeze", "--dir", dir, "--name", "mul", "--version", "1"); r.code != 0 {
		t.Fatalf("freeze: %+v", r)
	}
	comp := call("circuit-compile", "--dir", dir, "--name", "mul", "--version", "1")
	if comp.code != 0 || !strings.Contains(comp.out, "hash=") {
		t.Fatalf("compile original definition: %+v", comp)
	}
	hash := strings.TrimSpace(strings.SplitAfter(comp.out, "hash=")[1])

	// Input checking runs against the original w1·w2 = 6 definition: 2·3 = 6
	// satisfies; 2·2 = 4 ≠ 6 fails at constraint 1. These verdicts prove the
	// failed files neither replaced nor partially merged into the definition.
	sat := writeFile(t, work, "sat.json", `{"public":["2"],"private":["3"]}`)
	if r := call("input-check", "--dir", dir, "--name", "mul", "--version", "1",
		"--hash", hash, "--file", sat); r.code != 0 || !strings.Contains(r.out, "satisfied=true") {
		t.Fatalf("satisfied check against surviving definition: %+v", r)
	}
	unsat := writeFile(t, work, "unsat.json", `{"public":["2"],"private":["2"]}`)
	if r := call("input-check", "--dir", dir, "--name", "mul", "--version", "1",
		"--hash", hash, "--file", unsat); r.code != 0 ||
		!strings.Contains(r.out, "satisfied=false") || !strings.Contains(r.out, "first_failure=1") {
		t.Fatalf("unsatisfied check against surviving definition: %+v", r)
	}
}

// TestCLIImportConstantOnlyLayoutGuardsWireOne pins the zero-input boundary
// end to end: a 0/0 draft still has the constant wire 0, but a reference to
// wire 1 — even with a zero or modulus-multiple coefficient — is out of range
// and rejected with exit 1, while a legal wire-0 zero term imports fine.
func TestCLIImportConstantOnlyLayoutGuardsWireOne(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "bench")

	call := func(args ...string) cliRes { return runCLI(t, args...) }
	if r := call("circuit-create", "--dir", dir, "--name", "k", "--version", "1",
		"--constraints", "1", "--public-inputs", "0", "--private-inputs", "0"); r.code != 0 {
		t.Fatalf("create 0/0: %+v", r)
	}

	// Legal: only the constant wire, including a zero/modulus-multiple term on
	// it; the import succeeds and reports one constraint.
	legal := writeFile(t, work, "legal.json", `{"modulus":"11","constraints":[
		{"a":[{"wire":0,"coeff":"0"}],"b":[{"wire":0,"coeff":"11"}],"c":[]}]}`)
	if r := call("constraint-import", "--dir", dir, "--name", "k", "--version", "1",
		"--file", legal); r.code != 0 || !strings.Contains(r.out, "constraints=1") {
		t.Fatalf("legal constant-wire reference: %+v", r)
	}

	// Illegal: wire 1 is one past the 0/0 layout; the range named is [0,0].
	for name, body := range map[string]string{
		"zero":     `{"modulus":"11","constraints":[{"a":[{"wire":1,"coeff":"0"}],"b":[],"c":[]}]}`,
		"multiple": `{"modulus":"11","constraints":[{"a":[],"b":[{"wire":1,"coeff":"22"}],"c":[]}]}`,
		"cancel":   `{"modulus":"11","constraints":[{"a":[],"b":[],"c":[{"wire":1,"coeff":"4"},{"wire":1,"coeff":"-4"}]}]}`,
	} {
		path := writeFile(t, work, "bad1_"+name+".json", body)
		r := call("constraint-import", "--dir", dir, "--name", "k", "--version", "1", "--file", path)
		if r.code != 1 || !strings.Contains(r.err, "outside [0,0]") {
			t.Fatalf("%s: want exit 1 naming outside [0,0], got code=%d err=%q", name, r.code, r.err)
		}
		if strings.TrimSpace(r.out) != "" {
			t.Fatalf("%s: rejected import must not report success: %q", name, r.out)
		}
	}
}

// sanitizeCLI makes a file-name/label fragment from free text.
func sanitizeCLI(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, s)
}
